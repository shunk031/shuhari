package eval

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkspaceNamingAndIterationAllocation(t *testing.T) {
	if got := safeName(" .case/with spaces "); got != "case-with-spaces" {
		t.Fatalf("safeName() = %q", got)
	}
	if got := safeName("...///"); got != "case" {
		t.Fatalf("empty safeName() = %q", got)
	}
	root := filepath.Join(t.TempDir(), "workspace")
	if err := os.MkdirAll(filepath.Join(root, "iteration-2"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "iteration-nope"), 0o755); err != nil {
		t.Fatal(err)
	}
	suite := Suite{Name: "demo", Root: t.TempDir()}
	iteration, err := createIteration(suite, root)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(iteration) != "iteration-3" {
		t.Fatalf("iteration = %q", iteration)
	}
	if got := runDirectory(iteration, "a/b", variantWithSkill, 1); !strings.HasSuffix(got, filepath.Join("eval-a-b", variantWithSkill)) {
		t.Fatalf("trial 1 directory = %q", got)
	}
	if got := runDirectory(iteration, "a/b", variantWithSkill, 2); !strings.HasSuffix(got, filepath.Join("eval-a-b", variantWithSkill, "trials", "2")) {
		t.Fatalf("trial 2 directory = %q", got)
	}
}

func TestStageFixturesAndCopyOutputsHandleMissingAndInvalidRoots(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "fixture.txt"), "fixture\n")
	suite := Suite{Root: root}
	files, err := stageFixtures(suite, Case{Files: []string{"fixture.txt"}}, filepath.Join(t.TempDir(), "work"))
	if err != nil || len(files) != 1 || files[0] != "fixture.txt" {
		t.Fatalf("stageFixtures() = %#v, %v", files, err)
	}
	if _, err := stageFixtures(suite, Case{Files: []string{"missing.txt"}}, t.TempDir()); err == nil {
		t.Fatal("stageFixtures accepted a missing fixture")
	}

	destination := filepath.Join(t.TempDir(), "outputs")
	if err := copyOutputs(filepath.Join(t.TempDir(), "missing"), destination); err != nil {
		t.Fatal(err)
	}
	if err := copyOutputs(filepath.Join(root, "fixture.txt"), destination); err == nil {
		t.Fatal("copyOutputs accepted a regular file root")
	}
	linkRoot := filepath.Join(t.TempDir(), "link-root")
	if err := os.Symlink("fixture.txt", linkRoot); err != nil {
		t.Fatal(err)
	}
	if err := copyOutputs(linkRoot, destination); err != nil {
		t.Fatal(err)
	}
	metadata, err := os.ReadFile(filepath.Join(destination, ".shuhari-symlink.json"))
	if err != nil || !strings.Contains(string(metadata), "fixture.txt") {
		t.Fatalf("root symlink metadata = %s, %v", metadata, err)
	}
}

func TestRenderArtifactRecordsMetadataAndPreservesOrder(t *testing.T) {
	output := t.TempDir()
	mustWrite(t, filepath.Join(output, "other.txt"), "other\n")
	mustWrite(t, filepath.Join(output, "response.md"), "response")
	mustWrite(t, filepath.Join(output, ".git", "config"), "internal")
	if err := os.WriteFile(filepath.Join(output, "binary.bin"), []byte{'a', 0, 'b'}, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(output, "huge.txt"), bytes.Repeat([]byte{'x'}, maxInlineArtifactBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("response.md", filepath.Join(output, "inside")); err != nil {
		t.Fatal(err)
	}
	artifact, err := renderArtifact(output)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(artifact, "--- file: response.md") || !strings.Contains(artifact, `"reason":"git-internal"`) || !strings.Contains(artifact, `"reason":"non-text"`) || !strings.Contains(artifact, `"reason":"oversized-text"`) || !strings.Contains(artifact, "symlink: inside -> response.md") {
		t.Fatalf("rendered artifact = %s", artifact)
	}
	if strings.Contains(artifact, "--- file: other.txt") && strings.Index(artifact, "--- file: response.md") > strings.Index(artifact, "--- file: other.txt") {
		t.Fatal("response.md was not rendered first")
	}
}

func TestArtifactMetadataAndPathSafety(t *testing.T) {
	if got := artifactMetadataReason(".git/config", []byte("text")); got != "git-internal" {
		t.Fatalf("git metadata reason = %q", got)
	}
	if got := artifactMetadataReason("binary", []byte{'a', 0}); got != "non-text" {
		t.Fatalf("binary metadata reason = %q", got)
	}
	if got := artifactMetadataReason("large", bytes.Repeat([]byte{'x'}, maxInlineArtifactBytes+1)); got != "oversized-text" {
		t.Fatalf("large metadata reason = %q", got)
	}
	if got := artifactMetadataReason("normal", []byte("ok")); got != "" {
		t.Fatalf("normal metadata reason = %q", got)
	}
	root := t.TempDir()
	inside := filepath.Join(root, "inside")
	if err := os.WriteFile(inside, []byte("inside"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink("inside", link); err != nil {
		t.Fatal(err)
	}
	if !symlinkStaysWithin(root, link, "inside") || symlinkStaysWithin(root, link, "/etc/passwd") || symlinkStaysWithin(root, link, "../../etc/passwd") {
		t.Fatal("symlink safety classification was incorrect")
	}
	if !pathWithin(root, inside) || pathWithin(root, filepath.Join(root, "..", "outside")) {
		t.Fatal("pathWithin classification was incorrect")
	}
}

func TestSuiteDigestCoversSkillAndInstructionInputs(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "SKILL.md"), "skill\n")
	mustWrite(t, filepath.Join(root, "evals", "evals.json"), "evals\n")
	skillSuite := Suite{Kind: "skill", Root: root, TargetPath: root, EvalPath: filepath.Join(root, "evals", "evals.json")}
	digest, err := suiteDigest(skillSuite)
	if err != nil || len(digest) != sha256.Size*2 {
		t.Fatalf("skill digest = %q, %v", digest, err)
	}
	if _, err := hex.DecodeString(digest); err != nil {
		t.Fatal(err)
	}
	instructions := filepath.Join(root, "AGENTS.md")
	customEval := filepath.Join(t.TempDir(), "custom.json")
	mustWrite(t, instructions, "instructions\n")
	mustWrite(t, customEval, "custom evals\n")
	instructionSuite := Suite{Kind: "instructions", Root: root, TargetPath: instructions, EvalPath: customEval}
	if digest, err := suiteDigest(instructionSuite); err != nil || digest == "" {
		t.Fatalf("instruction digest = %q, %v", digest, err)
	}
	if err := os.Symlink("SKILL.md", filepath.Join(root, "unsafe")); err != nil {
		t.Fatal(err)
	}
	if _, err := suiteDigest(skillSuite); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("suiteDigest() error = %v, want symlink rejection", err)
	}
}

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
