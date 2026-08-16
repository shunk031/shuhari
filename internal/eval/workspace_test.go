package eval

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shunk031/shuhari/internal/harness"
)

type symlinkOutputHarness struct{ fakeHarness }

func (h *symlinkOutputHarness) Run(_ context.Context, request harness.Request) (harness.Result, error) {
	outputDir := filepath.Join(request.WorkDir, "outputs", "dotfiles-inspection")
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return harness.Result{}, err
	}
	if err := os.WriteFile(filepath.Join(outputDir, "AGENTS.md"), []byte("shared guidance\n"), 0o644); err != nil {
		return harness.Result{}, err
	}
	if err := os.Symlink("AGENTS.md", filepath.Join(outputDir, "CLAUDE.md")); err != nil {
		return harness.Result{}, err
	}
	return harness.Result{Response: "done", Transcript: []byte("{}\n")}, nil
}

func TestExecuteTaskCollectsProductionShapeSymlink(t *testing.T) {
	t.Parallel()

	iteration := t.TempDir()
	result, err := executeTask(
		context.Background(),
		Suite{Kind: harness.TargetSkill, Name: "demo"},
		&symlinkOutputHarness{},
		Config{Timeout: time.Second},
		iteration,
		runTask{Case: Case{ID: "case-symlink", Prompt: "inspect dotfiles"}, Trial: 2, Variant: variantWithoutSkill},
	)
	if err != nil {
		t.Fatalf("executeTask() rejected agent symlink output: %v", err)
	}
	collected := filepath.Join(result.OutputPath, "dotfiles-inspection", "CLAUDE.md")
	if info, err := os.Lstat(collected); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("collected production-shape symlink: info=%v err=%v", info, err)
	}
	if !strings.Contains(result.Artifact, "dotfiles-inspection/CLAUDE.md") {
		t.Fatalf("grading artifact omitted symlink entry: %s", result.Artifact)
	}
}

func TestCopyOutputsPreservesInTreeRelativeSymlink(t *testing.T) {
	t.Parallel()

	source := filepath.Join(t.TempDir(), "outputs")
	destination := filepath.Join(t.TempDir(), "collected")
	mustWrite(t, filepath.Join(source, "dotfiles-inspection", "AGENTS.md"), "shared guidance\n")
	link := filepath.Join(source, "dotfiles-inspection", "CLAUDE.md")
	if err := os.Symlink("AGENTS.md", link); err != nil {
		t.Fatal(err)
	}

	if err := copyOutputs(source, destination); err != nil {
		t.Fatalf("copyOutputs() error = %v", err)
	}
	collectedLink := filepath.Join(destination, "dotfiles-inspection", "CLAUDE.md")
	info, err := os.Lstat(collectedLink)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("collected link mode = %v, want symlink", info.Mode())
	}
	target, err := os.Readlink(collectedLink)
	if err != nil {
		t.Fatal(err)
	}
	if target != "AGENTS.md" {
		t.Fatalf("collected link target = %q", target)
	}
	artifact, err := renderArtifact(destination)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(artifact, "dotfiles-inspection/CLAUDE.md") || !strings.Contains(artifact, "AGENTS.md") {
		t.Fatalf("artifact omitted preserved symlink: %s", artifact)
	}
}

func TestCopyOutputsRecordsUnsafeSymlinksWithoutFollowing(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	source := filepath.Join(root, "outputs")
	destination := filepath.Join(t.TempDir(), "collected")
	outside := filepath.Join(root, "outside.txt")
	mustWrite(t, outside, "must not be collected\n")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	links := map[string]string{
		"escaping-link": "../outside.txt",
		"absolute-link": outside,
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(source, name)); err != nil {
			t.Fatal(err)
		}
	}

	if err := copyOutputs(source, destination); err != nil {
		t.Fatalf("copyOutputs() error = %v", err)
	}
	for name, target := range links {
		collected := filepath.Join(destination, name)
		info, err := os.Lstat(collected)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("unsafe link %q was preserved", name)
		}
		contents, err := os.ReadFile(collected)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{name, target, "not followed"} {
			if !strings.Contains(string(contents), want) {
				t.Fatalf("metadata for %q omitted %q: %s", name, want, contents)
			}
		}
		if strings.Contains(string(contents), "must not be collected") {
			t.Fatalf("unsafe link %q leaked target contents", name)
		}
	}
}

func TestCopyOutputsDoesNotTraverseDirectorySymlink(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	source := filepath.Join(root, "outputs")
	destination := filepath.Join(t.TempDir(), "collected")
	outsideDir := filepath.Join(root, "outside")
	mustWrite(t, filepath.Join(outsideDir, "secret.txt"), "outside secret\n")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideDir, filepath.Join(source, "linked-directory")); err != nil {
		t.Fatal(err)
	}

	if err := copyOutputs(source, destination); err != nil {
		t.Fatalf("copyOutputs() error = %v", err)
	}
	info, err := os.Lstat(filepath.Join(destination, "linked-directory"))
	if err != nil {
		t.Fatal(err)
	}
	if info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("outside directory link was traversable after collection: %v", info.Mode())
	}
	if _, err := os.Stat(filepath.Join(destination, "linked-directory", "secret.txt")); err == nil {
		t.Fatal("outside directory content was copied")
	}
	artifact, err := renderArtifact(destination)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(artifact, "outside secret") {
		t.Fatal("outside directory content reached the grading artifact")
	}
}
