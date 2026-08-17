package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

func TestValidateOnlyRejectsRemovedCodexSandboxValuesWithReplacement(t *testing.T) {
	root := writeValidationFixtures(t)

	for _, test := range []struct {
		value       string
		replacement string
	}{
		{value: "workspace-write", replacement: "isolated"},
		{value: "danger-full-access", replacement: "unsandboxed"},
	} {
		t.Run(test.value, func(t *testing.T) {
			commands := [][]string{
				{"eval", "skill", "--validate-only", "--sandbox", test.value, root},
				{"eval", "instructions", "--validate-only", "--sandbox", test.value, filepath.Join(root, "AGENTS.md")},
				{"check", "trigger", "--validate-only", "--sandbox", test.value, root},
			}
			for _, arguments := range commands {
				command := NewRoot(Options{Version: "test"})
				command.SetArgs(arguments)
				command.SetOut(&bytes.Buffer{})
				command.SetErr(&bytes.Buffer{})
				err := command.Execute()
				if err == nil || !strings.Contains(err.Error(), test.replacement) {
					t.Fatalf("Execute(%v) error = %v, want replacement %q", arguments, err, test.replacement)
				}
			}
		})
	}
}

func TestExplicitSandboxFlagWinsOverWeakerEnvironmentValue(t *testing.T) {
	t.Setenv("SHUHARI_SANDBOX", "unsandboxed")
	t.Setenv(harness.NoCredentialBoundaryAcknowledgementEnv, "1")
	root := writeValidationFixtures(t)
	command := NewRoot(Options{Version: "test"})
	command.SetArgs([]string{"eval", "skill", "--validate-only", "--sandbox", "isolated", root})
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	if err := command.Execute(); err != nil {
		t.Fatalf("explicit --sandbox was overridden by SHUHARI_SANDBOX: %v", err)
	}
}

func TestValidateOnlyRejectsUnacknowledgedUnsandboxedPolicy(t *testing.T) {
	t.Setenv(harness.NoCredentialBoundaryAcknowledgementEnv, "")
	root := writeValidationFixtures(t)
	command := NewRoot(Options{Version: "test"})
	command.SetArgs([]string{"eval", "skill", "--validate-only", "--sandbox", "unsandboxed", "--network=true", root})
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	err := command.Execute()
	if err == nil || !errors.Is(err, harness.ErrUnsupportedSecurityPolicy) {
		t.Fatalf("Execute() error = %v, want ErrUnsupportedSecurityPolicy", err)
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

func writeValidationFixtures(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "demo")
	if err := os.MkdirAll(filepath.Join(root, "evals"), 0o755); err != nil {
		t.Fatal(err)
	}
	for path, contents := range map[string]string{
		filepath.Join(root, "SKILL.md"):               "---\nname: demo\ndescription: Demo\n---\n",
		filepath.Join(root, "evals", "evals.json"):    `{"skill_name":"demo","evals":[{"id":1,"prompt":"do it","expected_output":"done"}]}`,
		filepath.Join(root, "evals", "triggers.json"): `{"skill_name":"demo","cases":[{"id":"yes","prompt":"do it","should_trigger":true},{"id":"no","prompt":"explain it","should_trigger":false}]}`,
		filepath.Join(root, "AGENTS.md"):              "Keep output concise.\n",
		filepath.Join(root, "AGENTS.evals.json"):      `{"instructions_name":"demo","evals":[{"id":1,"prompt":"do it","expected_output":"done"}]}`,
	} {
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}
