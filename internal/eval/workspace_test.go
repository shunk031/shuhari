package eval

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCopyOutputsPreservesInternalAndRecordsEscapingSymlinks(t *testing.T) {
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "response.md"), []byte("result\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("response.md", filepath.Join(source, "internal.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(source, "outside.md")); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "outputs")
	if err := copyOutputs(source, destination); err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(filepath.Join(destination, "internal.md")); err != nil || target != "response.md" {
		t.Fatalf("internal symlink = %q, %v", target, err)
	}
	metadata, err := os.ReadFile(filepath.Join(destination, "outside.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(metadata), `"type": "symlink"`) || !strings.Contains(string(metadata), "/etc/passwd") {
		t.Fatalf("escaping symlink metadata = %s", metadata)
	}
}

func TestAgentJudgePromptDoesNotRenderArtifacts(t *testing.T) {
	prompt, err := structuredJudgePrompt(graderPrompt, []agentJudgeInput{{ID: "one", Trial: 1, Side: "A", Assertions: []string{"The result is useful."}}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(prompt, "response.md") || strings.Contains(prompt, "The artifact says") {
		t.Fatalf("grader prompt rendered artifact content: %s", prompt)
	}
	if !strings.Contains(prompt, `"side":"A"`) {
		t.Fatalf("grader prompt omitted blind side: %s", prompt)
	}
}
