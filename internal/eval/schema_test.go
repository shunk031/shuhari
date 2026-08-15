package eval

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSkillSuite(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "demo")
	mustWrite(t, filepath.Join(root, "SKILL.md"), "---\nname: demo\ndescription: Demo skill\n---\n")
	mustWrite(t, filepath.Join(root, "evals", "files", "input.txt"), "fixture")
	mustWrite(t, filepath.Join(root, "evals", "evals.json"), `{
  "skill_name": "demo",
  "evals": [{
    "id": 1,
    "prompt": "process the fixture",
    "expected_output": "a useful result",
    "files": ["evals/files/input.txt"],
    "assertions": ["The answer is useful"],
    "required_actions": ["web_search", "file_change"]
  }]
}`)

	suite, err := LoadSkillSuite(root)
	if err != nil {
		t.Fatalf("LoadSkillSuite() error = %v", err)
	}
	if suite.Name != "demo" || len(suite.Cases) != 1 || suite.Cases[0].ID != "1" {
		t.Fatalf("suite = %#v", suite)
	}
	if got := suite.Cases[0].Files[0]; got != "evals/files/input.txt" {
		t.Fatalf("fixture path = %q", got)
	}
}

func TestLoadSkillSuiteRejectsEscapingFixture(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "demo")
	mustWrite(t, filepath.Join(root, "SKILL.md"), "---\nname: demo\ndescription: Demo skill\n---\n")
	mustWrite(t, filepath.Join(root, "evals", "evals.json"), `{
  "skill_name": "demo",
  "evals": [{
    "id": "escape",
    "prompt": "read it",
    "expected_output": "contents",
    "files": ["../secret.txt"]
  }]
}`)

	if _, err := LoadSkillSuite(root); err == nil {
		t.Fatal("LoadSkillSuite() accepted a fixture outside the skill root")
	}
}

func TestLoadInstructionsSuite(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	instructions := filepath.Join(root, "AGENTS.md")
	mustWrite(t, instructions, "Always verify the result.\n")
	mustWrite(t, filepath.Join(root, "AGENTS.evals.json"), `{
  "instructions_name": "project-guidance",
  "evals": [{
    "id": "verify",
    "prompt": "make a change",
    "expected_output": "a verified change",
    "assertions": ["The response reports verification"]
  }]
}`)

	suite, err := LoadInstructionsSuite(instructions, "")
	if err != nil {
		t.Fatalf("LoadInstructionsSuite() error = %v", err)
	}
	if suite.Name != "project-guidance" || suite.TargetPath != instructions {
		t.Fatalf("suite = %#v", suite)
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
