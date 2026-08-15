package harness

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseCodexTrace(t *testing.T) {
	t.Parallel()

	trace := []byte(`{"type":"thread.started","thread_id":"thread-1"}
{"type":"item.completed","item":{"id":"item-1","type":"command_execution","command":"sed -n '1,80p' .agents/skills/demo/SKILL.md","aggregated_output":"","exit_code":0,"status":"completed"}}
{"type":"item.completed","item":{"id":"item-2","type":"web_search","query":"official Go documentation"}}
{"type":"item.completed","item":{"id":"item-3","type":"agent_message","text":"finished"}}
{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":2,"output_tokens":4,"reasoning_output_tokens":1}}
`)

	result, err := parseCodexTrace(trace, Target{Kind: TargetSkill, Name: "demo"})
	if err != nil {
		t.Fatalf("parseCodexTrace() error = %v", err)
	}
	if result.Response != "finished" {
		t.Fatalf("Response = %q, want finished", result.Response)
	}
	if !result.TargetRead {
		t.Fatal("TargetRead = false, want true")
	}
	if result.Usage.TotalTokens() != 15 {
		t.Fatalf("TotalTokens() = %d, want 15", result.Usage.TotalTokens())
	}
	if len(result.Actions) != 1 || result.Actions[0] != ActionWebSearch {
		t.Fatalf("Actions = %#v, want web_search", result.Actions)
	}
}

func TestParseCodexTraceRequiresSuccessfulSkillReadBeforeResponse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		command string
		status  string
		exit    int
		before  bool
		want    bool
	}{
		{name: "failed read", command: "cat .agents/skills/demo/SKILL.md", status: "failed", exit: 1, before: true, want: false},
		{name: "path mention", command: "echo .agents/skills/demo/SKILL.md", status: "completed", exit: 0, before: true, want: false},
		{name: "read after response", command: "cat .agents/skills/demo/SKILL.md", status: "completed", exit: 0, before: false, want: false},
		{name: "read after changing directory", command: "cd .agents/skills/demo && sed -n '1,80p' SKILL.md", status: "completed", exit: 0, before: true, want: true},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			command := fmt.Sprintf(`{"type":"item.completed","item":{"id":"read","type":"command_execution","command":%q,"aggregated_output":"","exit_code":%d,"status":%q}}`, test.command, test.exit, test.status)
			message := `{"type":"item.completed","item":{"id":"answer","type":"agent_message","text":"done"}}`
			lines := []string{message, command}
			if test.before {
				lines = []string{command, message}
			}
			lines = append(lines, `{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1,"reasoning_output_tokens":0}}`)
			result, err := parseCodexTrace([]byte(strings.Join(lines, "\n")+"\n"), Target{Kind: TargetSkill, Name: "demo"})
			if err != nil {
				t.Fatalf("parseCodexTrace() error = %v", err)
			}
			if result.TargetRead != test.want {
				t.Fatalf("TargetRead = %v, want %v", result.TargetRead, test.want)
			}
		})
	}
}

func TestParseCodexTraceRejectsMalformedOrIncompleteTrace(t *testing.T) {
	t.Parallel()

	for name, trace := range map[string]string{
		"malformed JSONL":    "not-json\n",
		"missing completion": `{"type":"item.completed","item":{"id":"answer","type":"agent_message","text":"done"}}` + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := parseCodexTrace([]byte(trace), Target{}); err == nil {
				t.Fatal("parseCodexTrace() accepted an invalid trace")
			}
		})
	}
}

func TestContainsOrderedActions(t *testing.T) {
	t.Parallel()

	observed := []Action{ActionWebSearch, ActionGitHubSearch, ActionFileChange}
	if !ContainsOrderedActions(observed, []Action{ActionWebSearch, ActionFileChange}) {
		t.Fatal("ordered subsequence was not accepted")
	}
	if ContainsOrderedActions(observed, []Action{ActionFileChange, ActionWebSearch}) {
		t.Fatal("out-of-order actions were accepted")
	}
}

func TestCodexConfigurationDigestChangesWithConfig(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("model = \"first\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := codexConfigurationDigest(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("model = \"second\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := codexConfigurationDigest(path)
	if err != nil {
		t.Fatal(err)
	}
	if first == second || first == "" || second == "" {
		t.Fatalf("config digests = %q, %q", first, second)
	}
}

func TestCodexRunBuildsIsolatedNonInteractiveInvocation(t *testing.T) {
	capture := t.TempDir()
	script := filepath.Join(t.TempDir(), "fake-codex")
	contents := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$@" > %q/args
env > %q/env
printf '%%s\n' '{"type":"item.completed","item":{"id":"1","type":"agent_message","text":"done"}}'
printf '%%s\n' '{"type":"turn.completed","usage":{"input_tokens":1,"cached_input_tokens":0,"output_tokens":1,"reasoning_output_tokens":0}}'
`, capture, capture)
	if err := os.WriteFile(script, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
	sourceHome := t.TempDir()
	if err := os.WriteFile(filepath.Join(sourceHome, "config.toml"), []byte("model = \"configured\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", sourceHome)
	t.Setenv("HERDR_FAKE", "must-not-leak")
	t.Setenv("GIT_DIR", "must-not-leak")
	t.Setenv("GITHUB_TOKEN", "must-not-leak")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "must-not-leak")

	agent := newCodex(Config{Executable: script})
	result, err := agent.Run(context.Background(), Request{
		WorkDir:         t.TempDir(),
		Prompt:          "test",
		Model:           "model-a",
		ReasoningEffort: "high",
		Sandbox:         "workspace-write",
		Network:         true,
		Timeout:         time.Second,
		OutputSchema:    []byte(`{"type":"object"}`),
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Response != "done" {
		t.Fatalf("response = %q", result.Response)
	}
	arguments, err := os.ReadFile(filepath.Join(capture, "args"))
	if err != nil {
		t.Fatal(err)
	}
	joined := string(arguments)
	for _, expected := range []string{"--disable\nplugins\nexec\n", "--model\nmodel-a\n", "model_reasoning_effort=\"high\"", "sandbox_workspace_write.network_access=true", "--ephemeral\n--json\n--sandbox\nworkspace-write\n", "--output-schema\n", "-\n"} {
		if !strings.Contains(joined, expected) {
			t.Errorf("arguments do not contain %q:\n%s", expected, joined)
		}
	}
	environment, err := os.ReadFile(filepath.Join(capture, "env"))
	if err != nil {
		t.Fatal(err)
	}
	envText := string(environment)
	for _, secret := range []string{"HERDR_FAKE=", "GIT_DIR=", "GITHUB_TOKEN=", "AWS_SECRET_ACCESS_KEY="} {
		if strings.Contains(envText, secret) {
			t.Fatalf("isolated environment leaked %s:\n%s", secret, envText)
		}
	}
	if !strings.Contains(envText, "PATH=") {
		t.Fatalf("isolated environment leaked caller state:\n%s", envText)
	}
	if strings.Contains(envText, "CODEX_HOME="+sourceHome) || !strings.Contains(envText, "CODEX_HOME=") {
		t.Fatalf("CODEX_HOME was not isolated:\n%s", envText)
	}
}

func TestCodexRetryRestoresPristineWorkDirectory(t *testing.T) {
	t.Parallel()

	capture := t.TempDir()
	script := filepath.Join(t.TempDir(), "fake-codex")
	contents := fmt.Sprintf(`#!/bin/sh
count_file=%q/count
count=0
if test -f "$count_file"; then count=$(tr -d '\n' < "$count_file"); fi
count=$((count + 1))
printf '%%s\n' "$count" > "$count_file"
previous=
workdir=
for argument in "$@"; do
  if test "$previous" = "--cd"; then workdir="$argument"; fi
  previous="$argument"
done
if test "$count" = 1; then
  printf 'dirty\n' > "$workdir/contaminated.txt"
  printf '%%s\n' '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1,"reasoning_output_tokens":0}}'
  exit 0
fi
response=clean
if test -e "$workdir/contaminated.txt"; then response=dirty; fi
printf '%%s\n' "{\"type\":\"item.completed\",\"item\":{\"id\":\"answer\",\"type\":\"agent_message\",\"text\":\"$response\"}}"
printf '%%s\n' '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1,"reasoning_output_tokens":0}}'
`, capture)
	if err := os.WriteFile(script, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, "fixture.txt"), []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	agent := newCodex(Config{Executable: script})
	result, err := agent.Run(context.Background(), Request{WorkDir: workDir, Prompt: "test", Timeout: time.Second})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Response != "clean" {
		t.Fatalf("response = %q, want clean", result.Response)
	}
	if _, err := os.Stat(filepath.Join(workDir, "contaminated.txt")); !os.IsNotExist(err) {
		t.Fatalf("contaminated retry artifact survived: %v", err)
	}
}

func TestEffectiveSandboxResolvesEnvironmentOverride(t *testing.T) {
	t.Setenv("SHUHARI_SANDBOX", "danger-full-access")
	if got := EffectiveSandbox("workspace-write"); got != "danger-full-access" {
		t.Fatalf("EffectiveSandbox() = %q", got)
	}
}
