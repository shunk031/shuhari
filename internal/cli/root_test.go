package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shunk031/shuhari/internal/cache"
	"github.com/shunk031/shuhari/internal/harness"
)

func TestRootHasOnlyThePublicCommandTree(t *testing.T) {
	t.Parallel()

	root := NewRoot(Options{Version: "test"})
	for _, path := range [][]string{{"eval", "skill"}, {"eval", "instructions"}, {"check", "trigger"}} {
		command, _, err := root.Find(path)
		if err != nil || command == nil || command.Name() != path[len(path)-1] {
			t.Fatalf("Find(%v) = %v, %v", path, command, err)
		}
	}
	if len(root.Commands()) != 2 {
		t.Fatalf("root commands = %d, want 2", len(root.Commands()))
	}
}

func TestPublicCommandsDefaultToNeutralIsolatedSandbox(t *testing.T) {
	t.Parallel()

	root := NewRoot(Options{Version: "test"})
	for _, path := range [][]string{{"eval", "skill"}, {"eval", "instructions"}, {"check", "trigger"}} {
		command, _, err := root.Find(path)
		if err != nil {
			t.Fatal(err)
		}
		flag := command.Flag("sandbox")
		if flag == nil || flag.DefValue != "isolated" {
			t.Fatalf("%v --sandbox default = %#v, want isolated", path, flag)
		}
	}
}

func TestEvalSkillRejectsRemovedCodexSandboxValuesWithReplacement(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "demo")
	if err := os.MkdirAll(filepath.Join(root, "evals"), 0o755); err != nil {
		t.Fatal(err)
	}
	for path, contents := range map[string]string{
		filepath.Join(root, "SKILL.md"):            "---\nname: demo\ndescription: Demo\n---\n",
		filepath.Join(root, "evals", "evals.json"): `{"skill_name":"demo","evals":[{"id":1,"prompt":"do it","expected_output":"done"}]}`,
	} {
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	for _, test := range []struct {
		value       string
		replacement string
	}{
		{value: "workspace-write", replacement: "isolated"},
		{value: "danger-full-access", replacement: "unsandboxed"},
	} {
		t.Run(test.value, func(t *testing.T) {
			command := NewRoot(Options{
				Version: "test",
				Cache:   cache.Store{Root: t.TempDir()},
				HarnessFactory: func(string, harness.Config) (harness.Harness, error) {
					return nil, nil
				},
			})
			command.SetArgs([]string{"eval", "skill", "--sandbox", test.value, root})
			command.SetOut(&bytes.Buffer{})
			command.SetErr(&bytes.Buffer{})
			err := command.Execute()
			if err == nil || !strings.Contains(err.Error(), test.replacement) {
				t.Fatalf("Execute() error = %v, want replacement %q", err, test.replacement)
			}
		})
	}
}

func TestEvalSkillValidateOnlyDoesNotStartAgent(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "demo")
	if err := os.MkdirAll(filepath.Join(root, "evals"), 0o755); err != nil {
		t.Fatal(err)
	}
	for path, contents := range map[string]string{
		filepath.Join(root, "SKILL.md"):            "---\nname: demo\ndescription: Demo\n---\n",
		filepath.Join(root, "evals", "evals.json"): `{"skill_name":"demo","evals":[{"id":1,"prompt":"do it","expected_output":"done"}]}`,
	} {
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	command := NewRoot(Options{Version: "test", HarnessFactory: func(string, harness.Config) (harness.Harness, error) {
		return nil, errors.New("agent must not be created")
	}})
	command.SetArgs([]string{"eval", "skill", "--validate-only", root})
	var stdout bytes.Buffer
	command.SetOut(&stdout)
	command.SetErr(&bytes.Buffer{})
	if err := command.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if stdout.String() != "demo: valid\n" {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestResolveSkillPathsDeduplicatesFilesFromOneSkill(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "demo")
	if err := os.MkdirAll(filepath.Join(root, "references"), 0o755); err != nil {
		t.Fatal(err)
	}
	for path, contents := range map[string]string{
		filepath.Join(root, "SKILL.md"):               "---\nname: demo\ndescription: demo\n---\n",
		filepath.Join(root, "references", "guide.md"): "guide\n",
	} {
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	paths, err := resolveSkillPaths([]string{filepath.Join(root, "SKILL.md"), filepath.Join(root, "references", "guide.md")})
	if err != nil {
		t.Fatalf("resolveSkillPaths() error = %v", err)
	}
	if len(paths) != 1 || paths[0] != root {
		t.Fatalf("paths = %#v, want [%q]", paths, root)
	}
}
