package eval

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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
		fakeSecurityResolution(harness.SecurityPolicy{Level: harness.SandboxIsolated}),
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

func TestCopyOutputsRecordsDirectoryShapedSourceSymlinkWithoutFollowing(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	target := filepath.Join(root, "agent-output")
	mustWrite(t, filepath.Join(target, "secret.txt"), "must not be followed\n")
	source := filepath.Join(root, "outputs")
	if err := os.Symlink(target, source); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "collected")

	if err := copyOutputs(source, destination); err != nil {
		t.Fatalf("copyOutputs() crashed on directory-shaped source symlink: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destination, "secret.txt")); err == nil {
		t.Fatal("directory-shaped source symlink was followed")
	}
	metadata := filepath.Join(destination, ".shuhari-symlink.json")
	contents, err := os.ReadFile(metadata)
	if err != nil {
		t.Fatalf("source symlink metadata missing: %v", err)
	}
	if !strings.Contains(string(contents), `"link": "."`) || !strings.Contains(string(contents), "not followed") {
		t.Fatalf("source symlink metadata = %s", contents)
	}
}

func TestRenderArtifactUsesMetadataForNonText(t *testing.T) {
	t.Parallel()

	outputDir := t.TempDir()
	contents := []byte{'G', 'I', 'T', 0, 0xff, 'P', 'A', 'C', 'K'}
	path := filepath.Join(outputDir, "repository", "objects.pack")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, contents, 0o644); err != nil {
		t.Fatal(err)
	}

	artifact, err := renderArtifact(outputDir)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	for _, want := range []string{
		`"path":"repository/objects.pack"`,
		fmt.Sprintf(`"size":%d`, len(contents)),
		`"sha256":"` + hex.EncodeToString(digest[:]) + `"`,
		`"reason":"non-text"`,
	} {
		if !strings.Contains(artifact, want) {
			t.Fatalf("artifact metadata omitted %q: %s", want, artifact)
		}
	}
	if strings.Contains(artifact, string(contents)) {
		t.Fatal("artifact inlined non-text bytes")
	}
}

func TestRenderArtifactUsesMetadataForGitInternals(t *testing.T) {
	t.Parallel()

	outputDir := t.TempDir()
	contents := "repositoryformatversion = 0\n"
	mustWrite(t, filepath.Join(outputDir, "repository", ".git", "config"), contents)

	artifact, err := renderArtifact(outputDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"path":"repository/.git/config"`, `"reason":"git-internal"`} {
		if !strings.Contains(artifact, want) {
			t.Fatalf("artifact metadata omitted %q: %s", want, artifact)
		}
	}
	if strings.Contains(artifact, contents) {
		t.Fatal("artifact inlined a Git-internal text file")
	}
}

func TestRenderArtifactInlinesSmallText(t *testing.T) {
	t.Parallel()

	outputDir := t.TempDir()
	contents := "analysis complete\n"
	mustWrite(t, filepath.Join(outputDir, "result.txt"), contents)

	artifact, err := renderArtifact(outputDir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(artifact, "--- file: result.txt") || !strings.Contains(artifact, strings.TrimSpace(contents)) {
		t.Fatalf("artifact omitted inline text: %s", artifact)
	}
}

func TestRenderArtifactUsesMetadataForOversizedText(t *testing.T) {
	t.Parallel()

	outputDir := t.TempDir()
	contents := strings.Repeat("text\n", 14_000)
	mustWrite(t, filepath.Join(outputDir, "large.txt"), contents)

	artifact, err := renderArtifact(outputDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"path":"large.txt"`, `"reason":"oversized-text"`} {
		if !strings.Contains(artifact, want) {
			t.Fatalf("artifact metadata omitted %q: %s", want, artifact)
		}
	}
	if strings.Contains(artifact, contents) {
		t.Fatal("artifact inlined oversized text")
	}
}

func TestRenderArtifactBoundsAggregateInlineTextAndKeepsResponse(t *testing.T) {
	t.Parallel()

	outputDir := t.TempDir()
	response := "final response\n"
	first := strings.Repeat("a", 40*1024)
	second := strings.Repeat("b", 40*1024)
	mustWrite(t, filepath.Join(outputDir, "response.md"), response)
	mustWrite(t, filepath.Join(outputDir, "a.txt"), first)
	mustWrite(t, filepath.Join(outputDir, "b.txt"), second)

	artifact, err := renderArtifact(outputDir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(artifact, strings.TrimSpace(response)) {
		t.Fatal("artifact did not prioritize the final response")
	}
	if !strings.Contains(artifact, first) {
		t.Fatal("artifact did not inline text within the aggregate budget")
	}
	if strings.Contains(artifact, second) || !strings.Contains(artifact, `"path":"b.txt"`) || !strings.Contains(artifact, `"reason":"text-budget"`) {
		t.Fatalf("artifact did not represent excess text as metadata: %s", artifact)
	}
}

func TestPackfileDominatedOutputKeepsJudgePromptSmall(t *testing.T) {
	t.Parallel()

	outputDir := t.TempDir()
	pack := bytes.Repeat([]byte{0xff, 0x00, 0x80, 0x01}, 212_500)
	packPath := filepath.Join(outputDir, "repository", ".git", "objects", "pack", "pack-a.pack")
	if err := os.MkdirAll(filepath.Dir(packPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(packPath, pack, 0o644); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(outputDir, "result.txt"), "done\n")

	artifact, err := renderArtifact(outputDir)
	if err != nil {
		t.Fatal(err)
	}
	legacyArtifact := fmt.Sprintf("--- file: repository/.git/objects/pack/pack-a.pack (%d bytes) ---\n%s", len(pack), string(pack))
	before, err := structuredJudgePrompt(graderPrompt, []judgeInput{{ID: "large", Trial: 1, Assertions: []string{"correct"}, A: legacyArtifact, B: "baseline"}})
	if err != nil {
		t.Fatal(err)
	}
	after, err := structuredJudgePrompt(graderPrompt, []judgeInput{{ID: "large", Trial: 1, Assertions: []string{"correct"}, A: artifact, B: "baseline"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(before) <= 1_048_576 {
		t.Fatalf("legacy-shaped prompt = %d bytes, want over the agent input limit", len(before))
	}
	if len(after) >= 10_000 || len(after)*100 >= len(before) {
		t.Fatalf("metadata prompt did not collapse enough: before=%d after=%d", len(before), len(after))
	}
}
