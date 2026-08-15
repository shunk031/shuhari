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
	target, skillContents := testSkillTarget(t)

	trace := []byte(fmt.Sprintf(`{"type":"thread.started","thread_id":"thread-1"}
{"type":"item.completed","item":{"id":"item-1","type":"command_execution","command":"sed -n '1,80p' .agents/skills/demo/SKILL.md","aggregated_output":%q,"exit_code":0,"status":"completed"}}
{"type":"item.completed","item":{"id":"item-2","type":"web_search","query":"official Go documentation"}}
{"type":"item.completed","item":{"id":"item-3","type":"agent_message","text":"finished"}}
{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":2,"output_tokens":4,"reasoning_output_tokens":1}}
`, skillContents))

	result, err := parseCodexTrace(trace, target)
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
	target, skillContents := testSkillTarget(t)

	tests := []struct {
		name    string
		command string
		output  string
		status  string
		exit    int
		before  bool
		want    bool
	}{
		{name: "failed read", command: "cat .agents/skills/demo/SKILL.md", output: skillContents, status: "failed", exit: 1, before: true, want: false},
		{name: "path mention", command: "printf '%s' 'cat .agents/skills/demo/SKILL.md'", output: "cat .agents/skills/demo/SKILL.md", status: "completed", exit: 0, before: true, want: false},
		{name: "metadata only", command: "wc -c .agents/skills/demo/SKILL.md", output: "128 .agents/skills/demo/SKILL.md", status: "completed", exit: 0, before: true, want: false},
		{name: "read after response", command: "cat .agents/skills/demo/SKILL.md", output: skillContents, status: "completed", exit: 0, before: false, want: false},
		{name: "read with dd", command: "dd if=.agents/skills/demo/SKILL.md", output: skillContents, status: "completed", exit: 0, before: true, want: true},
		{name: "read with perl", command: `perl -0777 -ne 'print' .agents/skills/demo/SKILL.md`, output: skillContents, status: "completed", exit: 0, before: true, want: true},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			command := fmt.Sprintf(`{"type":"item.completed","item":{"id":"read","type":"command_execution","command":%q,"aggregated_output":%q,"exit_code":%d,"status":%q}}`, test.command, test.output, test.exit, test.status)
			message := `{"type":"item.completed","item":{"id":"answer","type":"agent_message","text":"done"}}`
			lines := []string{message, command}
			if test.before {
				lines = []string{command, message}
			}
			lines = append(lines, `{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1,"reasoning_output_tokens":0}}`)
			result, err := parseCodexTrace([]byte(strings.Join(lines, "\n")+"\n"), target)
			if err != nil {
				t.Fatalf("parseCodexTrace() error = %v", err)
			}
			if result.TargetRead != test.want {
				t.Fatalf("TargetRead = %v, want %v", result.TargetRead, test.want)
			}
		})
	}
}

func testSkillTarget(t *testing.T) (Target, string) {
	t.Helper()
	root := t.TempDir()
	contents := "---\nname: demo\ndescription: Demonstrate semantic read evidence.\n---\n\n# Demo\n\nFollow the demo workflow.\n"
	if err := os.WriteFile(filepath.Join(root, "SKILL.md"), []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	return Target{Kind: TargetSkill, Name: "demo", SourcePath: root}, contents
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

func TestClassifyCommandRecognizesStandardGitHubCLIForms(t *testing.T) {
	t.Parallel()

	for _, command := range []string{
		"gh api repos/owner/repo",
		"gh search code 'needle' --repo owner/repo",
		"gh repo view owner/repo",
	} {
		actions := classifyCommand(command)
		if len(actions) != 1 || actions[0] != ActionGitHubSearch {
			t.Fatalf("classifyCommand(%q) = %#v, want github_search", command, actions)
		}
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
if test -f "$CODEX_HOME/shuhari.config.toml"; then
  cp "$CODEX_HOME/shuhari.config.toml" %q/profile
fi
if test -f "$CODEX_HOME/auth.json"; then
  stat -c '%%a' "$CODEX_HOME/auth.json" > %q/auth-mode
fi
printf '%%s\n' '{"type":"item.completed","item":{"id":"1","type":"agent_message","text":"done"}}'
printf '%%s\n' '{"type":"turn.completed","usage":{"input_tokens":1,"cached_input_tokens":0,"output_tokens":1,"reasoning_output_tokens":0}}'
`, capture, capture, capture, capture)
	if err := os.WriteFile(script, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
	sourceHome := t.TempDir()
	if err := os.WriteFile(filepath.Join(sourceHome, "config.toml"), []byte("model = \"configured\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceHome, "auth.json"), []byte(`{"token":"secret"}`), 0o644); err != nil {
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
	for _, expected := range []string{"--disable\nplugins\nexec\n", "--model\nmodel-a\n", "model_reasoning_effort=\"high\"", "--profile\nshuhari\n", "--ephemeral\n--json\n", "--output-schema\n", "-\n"} {
		if !strings.Contains(joined, expected) {
			t.Errorf("arguments do not contain %q:\n%s", expected, joined)
		}
	}
	if strings.Contains(joined, "--sandbox\nworkspace-write") {
		t.Fatalf("workspace-write used legacy sandbox instead of restrictive profile:\n%s", joined)
	}
	profile, err := os.ReadFile(filepath.Join(capture, "profile"))
	if err != nil {
		t.Fatalf("read generated Codex profile: %v", err)
	}
	profileText := string(profile)
	for _, expected := range []string{`inherit = "none"`, `CODEX_HOME = ""`, `default_permissions = "shuhari-eval"`, `":minimal" = "read"`, `"." = "write"`, `network`, `enabled = true`, `codex-home`, `= "deny"`} {
		if !strings.Contains(profileText, expected) {
			t.Errorf("profile does not contain %q:\n%s", expected, profileText)
		}
	}
	authMode, err := os.ReadFile(filepath.Join(capture, "auth-mode"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(authMode)) != "600" {
		t.Fatalf("copied auth mode = %q, want 600", strings.TrimSpace(string(authMode)))
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

func TestCodexRunDetectsShellProducedFiles(t *testing.T) {
	t.Parallel()

	script := filepath.Join(t.TempDir(), "fake-codex")
	contents := `#!/bin/sh
previous=
workdir=
for argument in "$@"; do
  if test "$previous" = "--cd"; then workdir="$argument"; fi
  previous="$argument"
done
printf 'created\n' > "$workdir/outputs/result.txt"
printf '%s\n' '{"type":"item.completed","item":{"id":"command","type":"command_execution","command":"cp input.txt outputs/result.txt","aggregated_output":"","exit_code":0,"status":"completed"}}'
printf '%s\n' '{"type":"item.completed","item":{"id":"answer","type":"agent_message","text":"done"}}'
printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1,"reasoning_output_tokens":0}}'
`
	if err := os.WriteFile(script, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
	workDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workDir, "outputs"), 0o755); err != nil {
		t.Fatal(err)
	}
	agent := newCodex(Config{Executable: script})
	result, err := agent.Run(context.Background(), Request{WorkDir: workDir, Prompt: "test", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if !ContainsOrderedActions(result.Actions, []Action{ActionFileChange}) {
		t.Fatalf("shell-produced file was not observed: %#v", result.Actions)
	}
}

func TestWriteCodexProfileDangerFullAccessOnlyHardensCommandEnvironment(t *testing.T) {
	t.Parallel()

	codexHome := t.TempDir()
	request := Request{WorkDir: t.TempDir(), Sandbox: "danger-full-access"}
	if err := writeCodexProfile(codexHome, request); err != nil {
		t.Fatal(err)
	}
	profile, err := os.ReadFile(filepath.Join(codexHome, "shuhari.config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	profileText := string(profile)
	for _, expected := range []string{`inherit = "none"`, `CODEX_HOME = ""`} {
		if !strings.Contains(profileText, expected) {
			t.Errorf("danger-full-access profile does not contain %q:\n%s", expected, profileText)
		}
	}
	for _, unsupportedClaim := range []string{"default_permissions", "[permissions.shuhari-eval]"} {
		if strings.Contains(profileText, unsupportedClaim) {
			t.Errorf("danger-full-access profile claims unavailable isolation through %q:\n%s", unsupportedClaim, profileText)
		}
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
