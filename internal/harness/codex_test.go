package harness

import (
	"context"
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
	contents := `#!/bin/sh
printf '%s\n' "$@" > "$SHUHARI_TEST_CAPTURE/args"
env > "$SHUHARI_TEST_CAPTURE/env"
printf '%s\n' '{"type":"item.completed","item":{"id":"1","type":"agent_message","text":"done"}}'
printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":1,"cached_input_tokens":0,"output_tokens":1,"reasoning_output_tokens":0}}'
`
	if err := os.WriteFile(script, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
	sourceHome := t.TempDir()
	if err := os.WriteFile(filepath.Join(sourceHome, "config.toml"), []byte("model = \"configured\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHUHARI_TEST_CAPTURE", capture)
	t.Setenv("CODEX_HOME", sourceHome)
	t.Setenv("HERDR_FAKE", "must-not-leak")
	t.Setenv("GIT_DIR", "must-not-leak")

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
	if strings.Contains(envText, "HERDR_FAKE=") || strings.Contains(envText, "GIT_DIR=") {
		t.Fatalf("isolated environment leaked caller state:\n%s", envText)
	}
	if strings.Contains(envText, "CODEX_HOME="+sourceHome) || !strings.Contains(envText, "CODEX_HOME=") {
		t.Fatalf("CODEX_HOME was not isolated:\n%s", envText)
	}
}
