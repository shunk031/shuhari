package eval

import (
	"os"
	"path/filepath"
	"testing"
)

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

func mustWrite(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
