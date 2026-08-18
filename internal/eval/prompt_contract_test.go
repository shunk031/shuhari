package eval

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDerivedPromptsRetainGradingContractAnchors(t *testing.T) {
	t.Parallel()

	contract := readPromptContractFile(t, "docs/grading-contract.md")
	grader := readPromptContractFile(t, "internal/eval/prompts/grader.md")
	comparator := readPromptContractFile(t, "internal/eval/prompts/comparator.md")
	application := readPromptContractFile(t, "internal/trigger/prompts/application.md")

	for _, anchor := range []string{
		"evidence_references",
		"verbatim",
		"inclusive",
		"forbidden_patterns",
		"negated_clause",
		"blind",
	} {
		if !strings.Contains(contract, anchor) {
			t.Fatalf("grading contract lost anchor %q", anchor)
		}
		if !strings.Contains(grader, anchor) {
			t.Errorf("grader prompt lost contract anchor %q", anchor)
		}
	}

	for _, anchor := range []string{"cases", "id", "trial", "side", "assertion_results", "evidence", "evidence_references", "absence"} {
		if !strings.Contains(grader, anchor) {
			t.Errorf("grader prompt does not state output field %q", anchor)
		}
	}
	for _, anchor := range []string{"cases", "id", "trial", "preferred", "reason", "A", "B", "tie"} {
		if !strings.Contains(comparator, anchor) {
			t.Errorf("comparator prompt does not state output field or choice %q", anchor)
		}
	}
	for _, anchor := range []string{"verdict", "evidence", "applied", "declined", "ambiguous"} {
		if !strings.Contains(application, anchor) {
			t.Errorf("application prompt does not state output field or verdict %q", anchor)
		}
	}
}

func readPromptContractFile(t *testing.T, relative string) string {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller() failed")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
	contents, err := os.ReadFile(filepath.Join(root, relative))
	if err != nil {
		t.Fatalf("read %s: %v", relative, err)
	}
	return string(contents)
}
