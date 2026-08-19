package eval

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCaseIDAcceptsStringsAndIntegersOnly(t *testing.T) {
	for _, test := range []struct {
		name    string
		input   string
		want    caseID
		wantErr string
	}{
		{name: "string", input: `"case"`, want: "case"},
		{name: "integer", input: `42`, want: "42"},
		{name: "blank string", input: `" "`, wantErr: "must not be empty"},
		{name: "fraction", input: `1.5`, wantErr: "must be an integer"},
		{name: "boolean", input: `true`, wantErr: "must be a string or number"},
		{name: "malformed", input: `{`, wantErr: "unexpected end"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var got caseID
			err := json.Unmarshal([]byte(test.input), &got)
			if test.wantErr == "" {
				if err != nil || got != test.want {
					t.Fatalf("caseID = %q, error = %v, want %q", got, err, test.want)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want %q", err, test.wantErr)
			}
		})
	}
}

func TestLoadSkillSuiteUsesReferenceAssertionStrings(t *testing.T) {
	root := filepath.Join(t.TempDir(), "demo")
	mustWrite(t, filepath.Join(root, "SKILL.md"), "---\nname: demo\ndescription: Demo skill\n---\n")
	mustWrite(t, filepath.Join(root, "evals", "evals.json"), `{"skill_name":"demo","evals":[{"id":1,"prompt":"do it","expected_output":"done","assertions":["The result is useful."]}]}`)
	suite, err := LoadSkillSuite(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := suite.Cases[0].effectiveAssertions(); len(got) != 1 || got[0] != "The result is useful." {
		t.Fatalf("assertions = %#v", got)
	}
}

func TestLoadSkillSuiteRejectsGroundingFields(t *testing.T) {
	root := filepath.Join(t.TempDir(), "demo")
	mustWrite(t, filepath.Join(root, "SKILL.md"), "---\nname: demo\ndescription: Demo skill\n---\n")
	mustWrite(t, filepath.Join(root, "evals", "evals.json"), `{"skill_name":"demo","evals":[{"id":1,"prompt":"do it","expected_output":"done","assertions":[{"text":"claim","forbidden_patterns":["command"]}]}]}`)
	if _, err := LoadSkillSuite(root); err == nil {
		t.Fatal("LoadSkillSuite accepted a removed grounding field")
	}
}

func TestLoadSkillSuiteRejectsMalformedCases(t *testing.T) {
	tests := []struct {
		name  string
		evals string
		files map[string]string
		want  string
	}{
		{name: "mismatched skill name", evals: `{"skill_name":"other","evals":[{"id":1,"prompt":"do it","expected_output":"done"}]}`, want: "does not match"},
		{name: "no cases", evals: `{"skill_name":"demo","evals":[]}`, want: "at least one"},
		{name: "empty id", evals: `{"skill_name":"demo","evals":[{"id":"","prompt":"do it","expected_output":"done"}]}`, want: "id must not be empty"},
		{name: "duplicate id", evals: `{"skill_name":"demo","evals":[{"id":1,"prompt":"a","expected_output":"a"},{"id":1,"prompt":"b","expected_output":"b"}]}`, want: "duplicate"},
		{name: "path collision", evals: `{"skill_name":"demo","evals":[{"id":"a/b","prompt":"a","expected_output":"a"},{"id":"a\\b","prompt":"b","expected_output":"b"}]}`, want: "same workspace path"},
		{name: "blank prompt", evals: `{"skill_name":"demo","evals":[{"id":1,"prompt":" ","expected_output":"done"}]}`, want: "prompt and expected_output"},
		{name: "missing fixture", evals: `{"skill_name":"demo","evals":[{"id":1,"prompt":"do it","expected_output":"done","files":["missing.md"]}]}`, want: "inspect fixture"},
		{name: "absolute fixture", evals: `{"skill_name":"demo","evals":[{"id":1,"prompt":"do it","expected_output":"done","files":["/tmp/file"]}]}`, want: "must be relative"},
		{name: "escaping fixture", evals: `{"skill_name":"demo","evals":[{"id":1,"prompt":"do it","expected_output":"done","files":["../file"]}]}`, want: "escapes target root"},
		{name: "blank assertion", evals: `{"skill_name":"demo","evals":[{"id":1,"prompt":"do it","expected_output":"done","assertions":[" "]}]}`, want: "empty assertion"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "demo")
			mustWrite(t, filepath.Join(root, "SKILL.md"), "---\nname: demo\ndescription: Demo\n---\n")
			mustWrite(t, filepath.Join(root, "evals", "evals.json"), test.evals)
			for path, contents := range test.files {
				mustWrite(t, filepath.Join(root, path), contents)
			}
			if _, err := LoadSkillSuite(root); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("LoadSkillSuite() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestLoadInstructionsSuiteUsesDefaultAndExplicitEvalPaths(t *testing.T) {
	root := t.TempDir()
	instructions := filepath.Join(root, "AGENTS.md")
	mustWrite(t, instructions, "Keep output concise.\n")
	defaultEval := filepath.Join(root, "AGENTS.evals.json")
	mustWrite(t, defaultEval, `{"instructions_name":"demo","evals":[{"id":"one","prompt":"do it","expected_output":"done"}]}`)
	suite, err := LoadInstructionsSuite(instructions, "")
	if err != nil {
		t.Fatal(err)
	}
	if suite.Name != "demo" || suite.TargetPath != instructions || suite.EvalPath != defaultEval {
		t.Fatalf("default suite = %#v", suite)
	}

	explicitEval := filepath.Join(root, "custom.json")
	mustWrite(t, explicitEval, `{"instructions_name":"custom","evals":[{"id":2,"prompt":"do it","expected_output":"done"}]}`)
	explicit, err := LoadInstructionsSuite(instructions, explicitEval)
	if err != nil {
		t.Fatal(err)
	}
	if explicit.Name != "custom" || explicit.EvalPath != explicitEval {
		t.Fatalf("explicit suite = %#v", explicit)
	}
}

func TestLoadInstructionsSuiteRejectsInvalidInputs(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "missing.md")
	if _, err := LoadInstructionsSuite(missing, ""); err == nil || !strings.Contains(err.Error(), "inspect instructions") {
		t.Fatalf("missing instructions error = %v", err)
	}
	directory := filepath.Join(root, "directory")
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadInstructionsSuite(directory, ""); err == nil || !strings.Contains(err.Error(), "not a file") {
		t.Fatalf("directory instructions error = %v", err)
	}
	instructions := filepath.Join(root, "AGENTS.md")
	mustWrite(t, instructions, "instructions\n")
	mustWrite(t, filepath.Join(root, "AGENTS.evals.json"), `{"instructions_name":"","evals":[]}`)
	if _, err := LoadInstructionsSuite(instructions, ""); err == nil || !strings.Contains(err.Error(), "instructions_name") {
		t.Fatalf("blank name error = %v", err)
	}
	badEval := filepath.Join(root, "bad.json")
	mustWrite(t, badEval, `{"instructions_name":"demo","evals":[]} trailing`)
	if _, err := LoadInstructionsSuite(instructions, badEval); err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("bad eval error = %v", err)
	}
}

func mustWrite(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
