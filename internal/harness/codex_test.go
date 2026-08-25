package harness

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

func TestParseCodexTraceClassifiesIncompleteTransportError(t *testing.T) {
	t.Parallel()

	trace := []byte("{\"type\":\"error\",\"message\":\"stream disconnected before completion: error decoding response body\"}\n")
	_, err := parseCodexTrace(trace, Target{})
	if err == nil || !errors.Is(err, ErrTransient) {
		t.Fatalf("parseCodexTrace() error = %v, want transient transport error", err)
	}
}

func TestParseCodexTraceDoesNotRetryCompletedTurn(t *testing.T) {
	t.Parallel()

	trace := []byte("{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"completed answer\"}}\n{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}\n{\"type\":\"error\",\"message\":\"connection reset by peer\"}\n")
	_, err := parseCodexTrace(trace, Target{})
	if err == nil {
		t.Fatal("parseCodexTrace() accepted an error after a completed message")
	}
	if errors.Is(err, ErrTransient) {
		t.Fatalf("completed turn was classified retryable: %v", err)
	}
}

func TestTransportFailurePatterns(t *testing.T) {
	t.Parallel()

	for _, message := range []string{
		"stream disconnected before completion: stream closed before response.completed",
		"Transport error: network error: error decoding response body",
		"Reconnecting... 1/5 (stream disconnected before completion: Transport error: network error: error decoding response body)",
		"connection reset by peer",
	} {
		if !transientPattern.MatchString(message) {
			t.Errorf("transport error was not classified transient: %q", message)
		}
	}
	if transientPattern.MatchString("input_too_large") {
		t.Fatal("input_too_large was classified transient")
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

func TestParseCodexTraceAccumulatesChunkedSkillReads(t *testing.T) {
	t.Parallel()
	target, skillContents := testLongSkillTarget(t)
	lines := strings.SplitAfter(skillContents, "\n")
	middle := len(lines) / 2
	trace := []byte(fmt.Sprintf(`{"type":"item.completed","item":{"type":"command_execution","command":"sed -n '1,10p' .agents/skills/demo/SKILL.md","aggregated_output":%q,"exit_code":0,"status":"completed"}}
{"type":"item.completed","item":{"type":"command_execution","command":"sed -n '11,40p' .agents/skills/demo/SKILL.md","aggregated_output":%q,"exit_code":0,"status":"completed"}}
{"type":"item.completed","item":{"type":"agent_message","text":"done"}}
{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}
`, strings.Join(lines[:middle], ""), strings.Join(lines[middle:], "")))
	result, err := parseCodexTrace(trace, target)
	if err != nil {
		t.Fatal(err)
	}
	if !result.TargetRead {
		t.Fatal("two successful chunks covering the skill did not count as a read")
	}
}

func TestParseCodexTraceUsesDocumentedSkillCoverageThreshold(t *testing.T) {
	t.Parallel()
	target, skillContents := testLongSkillTarget(t)
	lines := strings.SplitAfter(skillContents, "\n")

	for _, test := range []struct {
		name     string
		fraction float64
		want     bool
	}{
		{name: "minor tail truncation", fraction: 0.95, want: true},
		{name: "material truncation", fraction: 0.70, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			count := int(float64(len(lines)) * test.fraction)
			trace := []byte(fmt.Sprintf(`{"type":"item.completed","item":{"type":"command_execution","command":"sed -n '1,40p' .agents/skills/demo/SKILL.md","aggregated_output":%q,"exit_code":0,"status":"completed"}}
{"type":"item.completed","item":{"type":"agent_message","text":"done"}}
{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}
`, strings.Join(lines[:count], "")))
			result, err := parseCodexTrace(trace, target)
			if err != nil {
				t.Fatal(err)
			}
			if result.TargetRead != test.want {
				t.Fatalf("TargetRead = %v, want %v at fraction %.2f (threshold %.2f)", result.TargetRead, test.want, test.fraction, skillReadCoverageThreshold)
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

func testLongSkillTarget(t *testing.T) (Target, string) {
	t.Helper()
	root := t.TempDir()
	var builder strings.Builder
	builder.WriteString("---\nname: demo\ndescription: Chunked read evidence.\n---\n")
	for index := 1; index <= 40; index++ {
		fmt.Fprintf(&builder, "instruction-%02d: perform the distinct workflow step number %02d carefully.\n", index, index)
	}
	contents := builder.String()
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
	if !ContainsOrderedActions(observed, nil, []Action{ActionWebSearch, ActionFileChange}) {
		t.Fatal("ordered subsequence was not accepted")
	}
	if ContainsOrderedActions(observed, nil, []Action{ActionFileChange, ActionWebSearch}) {
		t.Fatal("out-of-order actions were accepted")
	}
	if !ContainsOrderedActions([]Action{ActionGitHubSearch}, []Action{ActionFileChange}, []Action{ActionFileChange, ActionGitHubSearch}) {
		t.Fatal("order-unknown file change was not placed before a known GitHub action")
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
if test "$3" = debug && test "$4" = models; then
  printf '%%s\n' '{"models":[{"slug":"bundled-model","base_instructions":"bundled"}]}'
  exit 0
fi
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
	security := mustResolveCodexSecurity(t, agent, SecurityPolicy{Level: SandboxIsolated, Network: true})
	result, err := agent.Run(context.Background(), Request{
		WorkDir:         t.TempDir(),
		Prompt:          "test",
		Model:           "model-a",
		ReasoningEffort: "high",
		Security:        security,
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

func TestCodexRunUsesBundledModelCatalogForExplicitModel(t *testing.T) {
	capture := t.TempDir()
	script := filepath.Join(t.TempDir(), "fake-codex")
	contents := fmt.Sprintf(`#!/bin/sh
if test "$3" = debug && test "$4" = models; then
  printf '%%s\n' '{"models":[{"slug":"bundled-model","base_instructions":"bundled"}]}'
  exit 0
fi
printf '%%s\n' "$@" > %q/args
for argument in "$@"; do
  case "$argument" in
    model_catalog_json=*)
      catalog_path=${argument#model_catalog_json=}
      catalog_path=${catalog_path#\"}
      catalog_path=${catalog_path%%\"}
      cp "$catalog_path" %q/catalog.json
      ;;
  esac
done
printf '%%s\n' '{"type":"item.completed","item":{"id":"1","type":"agent_message","text":"done"}}'
printf '%%s\n' '{"type":"turn.completed","usage":{"input_tokens":1,"cached_input_tokens":0,"output_tokens":1,"reasoning_output_tokens":0}}'
`, capture, capture)
	if err := os.WriteFile(script, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", t.TempDir())
	agent := newCodex(Config{Executable: script})
	security := mustResolveCodexSecurity(t, agent, SecurityPolicy{Level: SandboxIsolated, Network: true})
	_, err := agent.Run(context.Background(), Request{
		WorkDir:  t.TempDir(),
		Prompt:   "test",
		Model:    "model-a",
		Security: security,
		Timeout:  time.Second,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	arguments, err := os.ReadFile(filepath.Join(capture, "args"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(arguments), "model_catalog_json=") {
		t.Fatalf("explicit model invocation did not select a static model catalog:\n%s", arguments)
	}
	catalog, err := os.ReadFile(filepath.Join(capture, "catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(catalog), `"bundled-model"`) {
		t.Fatalf("invocation did not pass the bundled model catalog to Codex: %s", catalog)
	}
}

func TestCodexRunUsesBundledModelCatalogForUnsetModelAfterRefreshDecodeFailure(t *testing.T) {
	capture := t.TempDir()
	script := filepath.Join(t.TempDir(), "fake-codex")
	contents := fmt.Sprintf(`#!/bin/sh
if test "$3" = debug && test "$4" = models; then
  printf '%%s\n' '{"models":[{"slug":"bundled-model","base_instructions":"bundled"}]}'
  exit 0
fi
has_catalog=
for argument in "$@"; do
  case "$argument" in
    model_catalog_json=*)
      has_catalog=1
      catalog_path=${argument#model_catalog_json=}
      catalog_path=${catalog_path#\"}
      catalog_path=${catalog_path%%\"}
      cp "$catalog_path" %q/catalog.json
      ;;
  esac
done
if test -z "$has_catalog"; then
  printf '%%s\n' '{"type":"error","message":"error decoding response body: missing field models; gateway body={\"object\":\"list\",\"data\":[{\"id\":\"default-model\"}]}"}'
  exit 1
fi
printf '%%s\n' "$@" > %q/args
printf '%%s\n' '{"type":"item.completed","item":{"id":"1","type":"agent_message","text":"done"}}'
printf '%%s\n' '{"type":"turn.completed","usage":{"input_tokens":1,"cached_input_tokens":0,"output_tokens":1,"reasoning_output_tokens":0}}'
`, capture, capture)
	if err := os.WriteFile(script, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", t.TempDir())
	agent := newCodex(Config{Executable: script})
	security := mustResolveCodexSecurity(t, agent, SecurityPolicy{Level: SandboxIsolated, Network: true})
	result, err := agent.Run(context.Background(), Request{
		WorkDir:  t.TempDir(),
		Prompt:   "test",
		Security: security,
		Timeout:  time.Second,
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
	if strings.Contains(string(arguments), "--model\n") {
		t.Fatalf("unset-model invocation unexpectedly selected an explicit model:\n%s", arguments)
	}
	catalog, err := os.ReadFile(filepath.Join(capture, "catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(catalog), `"bundled-model"`) {
		t.Fatalf("unset-model invocation did not pass the bundled model catalog to Codex: %s", catalog)
	}
}

func TestCodexRunRejectsEmptyBundledModelCatalog(t *testing.T) {
	capture := t.TempDir()
	script := filepath.Join(t.TempDir(), "fake-codex")
	contents := fmt.Sprintf(`#!/bin/sh
if test "$3" = debug && test "$4" = models; then
  printf '%%s\n' '{"models":[]}'
  exit 0
fi
touch %q/exec
`, capture)
	if err := os.WriteFile(script, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", t.TempDir())
	agent := newCodex(Config{Executable: script})
	security := mustResolveCodexSecurity(t, agent, SecurityPolicy{Level: SandboxIsolated})
	_, err := agent.Run(context.Background(), Request{
		WorkDir:  t.TempDir(),
		Prompt:   "test",
		Security: security,
		Timeout:  time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "response has no models list") {
		t.Fatalf("Run() error = %v, want empty bundled catalog rejection", err)
	}
	if _, statErr := os.Stat(filepath.Join(capture, "exec")); !os.IsNotExist(statErr) {
		t.Fatalf("Codex exec ran after empty bundled catalog: %v", statErr)
	}
}

func TestCodexRunMatchesShellWriteBeforeGitHubActionWithoutInventingOrder(t *testing.T) {
	t.Parallel()

	script := filepath.Join(t.TempDir(), "fake-codex")
	contents := `#!/bin/sh
if test "$3" = debug && test "$4" = models; then
  printf '%s\n' '{"models":[{"slug":"bundled-model","base_instructions":"bundled"}]}'
  exit 0
fi
previous=
workdir=
for argument in "$@"; do
  if test "$previous" = "--cd"; then workdir="$argument"; fi
  previous="$argument"
done
printf 'created\n' > "$workdir/outputs/result.txt"
printf '%s\n' '{"type":"item.completed","item":{"id":"command","type":"command_execution","command":"cp input.txt outputs/result.txt","aggregated_output":"","exit_code":0,"status":"completed"}}'
printf '%s\n' '{"type":"item.completed","item":{"id":"github","type":"command_execution","command":"gh api repos/owner/repo","aggregated_output":"{}","exit_code":0,"status":"completed"}}'
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
	security := mustResolveCodexSecurity(t, agent, SecurityPolicy{Level: SandboxIsolated})
	result, err := agent.Run(context.Background(), Request{WorkDir: workDir, Prompt: "test", Security: security, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if !ContainsOrderedActions(result.Actions, result.OrderUnknownActions, []Action{ActionFileChange, ActionGitHubSearch}) {
		t.Fatalf("shell write then GitHub action was not satisfiable: ordered=%#v unknown=%#v", result.Actions, result.OrderUnknownActions)
	}
	if len(result.OrderUnknownActions) != 1 || result.OrderUnknownActions[0] != ActionFileChange {
		t.Fatalf("workspace diff action was assigned a fake order: ordered=%#v unknown=%#v", result.Actions, result.OrderUnknownActions)
	}
}

func TestDangerFullAccessRefusesBeforeCredentialProbeChildCanRun(t *testing.T) {
	capture := t.TempDir()
	script := filepath.Join(t.TempDir(), "fake-codex")
	contents := fmt.Sprintf(`#!/bin/sh
touch %q/invoked
/bin/sh -c 'find /tmp -path "*/shuhari-codex-*/codex-home/auth.json" -exec cat {} \;' > %q/leaked
printf '%%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"done"}}'
printf '%%s\n' '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'
`, capture, capture)
	if err := os.WriteFile(script, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(NoCredentialBoundaryAcknowledgementEnv, "")
	agent := newCodex(Config{Executable: script})
	_, err := agent.ResolveSecurity(context.Background(), SecurityPolicy{Level: SandboxUnsandboxed, Network: true})
	if err == nil || !strings.Contains(err.Error(), NoCredentialBoundaryAcknowledgementEnv) {
		t.Fatalf("unsandboxed was not refused with an actionable error: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(capture, "invoked")); !os.IsNotExist(statErr) {
		t.Fatalf("Codex process ran credential-reading child before refusal: %v", statErr)
	}
}

func TestDangerFullAccessCredentialProbeIsExplicitlyLabeledNoBoundary(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("same-UID /proc credential discovery is Linux-specific")
	}
	capture := t.TempDir()
	script := filepath.Join(t.TempDir(), "fake-codex")
	contents := fmt.Sprintf(`#!/bin/sh
if test "$3" = debug && test "$4" = models; then
  printf '%%s\n' '{"models":[{"slug":"bundled-model","base_instructions":"bundled"}]}'
  exit 0
fi
/bin/sh -c 'client_home=$(tr "\000" "\n" < /proc/$PPID/environ | sed -n "s/^CODEX_HOME=//p"); cat "$client_home/auth.json"' > %q/leaked
printf '%%s\n' '{"type":"item.completed","item":{"type":"command_execution","command":"read parent auth material","aggregated_output":"credential probe completed","exit_code":0,"status":"completed"}}'
printf '%%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"done"}}'
printf '%%s\n' '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'
`, capture)
	if err := os.WriteFile(script, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
	sourceHome := t.TempDir()
	if err := os.WriteFile(filepath.Join(sourceHome, "auth.json"), []byte("test-auth-material"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", sourceHome)
	t.Setenv(NoCredentialBoundaryAcknowledgementEnv, "1")
	agent := newCodex(Config{Executable: script})
	security := mustResolveCodexSecurity(t, agent, SecurityPolicy{Level: SandboxUnsandboxed, Network: true})
	if _, err := agent.Run(context.Background(), Request{WorkDir: t.TempDir(), Prompt: "read auth material", Security: security, Timeout: time.Second}); err != nil {
		t.Fatal(err)
	}
	leaked, err := os.ReadFile(filepath.Join(capture, "leaked"))
	if err != nil {
		t.Fatal(err)
	}
	if string(leaked) != "test-auth-material" {
		t.Fatalf("credential probe did not reproduce danger-mode reachability: %q", leaked)
	}
	if security.CredentialBoundary != CredentialBoundaryNone {
		t.Fatalf("reachable credential mode was not labeled as no boundary: %#v", security)
	}
}

func TestSecureTemporaryDirectoryIsRandomAndPrivate(t *testing.T) {
	t.Parallel()
	first, err := secureTemporaryDirectory("shuhari-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(first)
	second, err := secureTemporaryDirectory("shuhari-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(second)
	if first == second {
		t.Fatal("temporary directory names were reused")
	}
	for _, path := range []string{first, second} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Fatalf("temporary directory mode = %o, want 700", info.Mode().Perm())
		}
	}
}

func TestWriteCodexProfileDangerFullAccessOnlyHardensCommandEnvironment(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv(NoCredentialBoundaryAcknowledgementEnv, "1")
	agent := newCodex(Config{})
	security := mustResolveCodexSecurity(t, agent, SecurityPolicy{Level: SandboxUnsandboxed, Network: true})
	request := Request{WorkDir: t.TempDir(), Security: security}
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

func TestWriteCodexProfilePropagatesParentProxiesWithoutCredentials(t *testing.T) {
	proxyValues := map[string]string{
		"HTTP_PROXY":  "http://upper-http-proxy.invalid:8080",
		"HTTPS_PROXY": "http://upper-https-proxy.invalid:8080",
		"ALL_PROXY":   "http://upper-all-proxy.invalid:8080",
		"NO_PROXY":    "localhost,127.0.0.1",
		"http_proxy":  "http://lower-http-proxy.invalid:8080",
		"https_proxy": "http://lower-https-proxy.invalid:8080",
		"all_proxy":   "http://lower-all-proxy.invalid:8080",
		"no_proxy":    "localhost,::1",
	}
	for name, value := range proxyValues {
		t.Setenv(name, value)
	}
	for name, value := range map[string]string{
		"OPENAI_API_KEY":        "synthetic-openai-key",
		"GEN_AI_GATEWAY_PAT":    "synthetic-gateway-pat",
		"AWS_ACCESS_KEY_ID":     "synthetic-access-key",
		"AWS_SECRET_ACCESS_KEY": "synthetic-secret-key",
		"GH_TOKEN":              "synthetic-gh-token",
		"GITHUB_TOKEN":          "synthetic-github-token",
	} {
		t.Setenv(name, value)
	}

	codexHome := t.TempDir()
	security := mustResolveCodexSecurity(t, newCodex(Config{}), SecurityPolicy{Level: SandboxReadOnly, Network: true})
	if err := writeCodexProfile(codexHome, Request{WorkDir: t.TempDir(), Security: security}); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(filepath.Join(codexHome, "shuhari.config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	profile := string(contents)
	for name, value := range proxyValues {
		expected := fmt.Sprintf("%s = %s", name, tomlString(value))
		if !strings.Contains(profile, expected) {
			t.Errorf("profile omitted parent proxy %s: %q\n%s", name, expected, profile)
		}
	}
	for _, name := range []string{"OPENAI_API_KEY", "GEN_AI_GATEWAY_PAT", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "GH_TOKEN", "GITHUB_TOKEN"} {
		if strings.Contains(profile, name+" =") {
			t.Errorf("profile propagated credential-class variable %s:\n%s", name, profile)
		}
	}
}

func TestCodexRetryRestoresPristineWorkDirectory(t *testing.T) {
	t.Parallel()

	capture := t.TempDir()
	script := filepath.Join(t.TempDir(), "fake-codex")
	contents := fmt.Sprintf(`#!/bin/sh
if test "$3" = debug && test "$4" = models; then
  printf '%%s\n' '{"models":[{"slug":"bundled-model","base_instructions":"bundled"}]}'
  exit 0
fi
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
	agent.waitBeforeRetry = noRetryWait
	security := mustResolveCodexSecurity(t, agent, SecurityPolicy{Level: SandboxIsolated})
	result, err := agent.Run(context.Background(), Request{WorkDir: workDir, Prompt: "test", Security: security, Timeout: time.Second})
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

func TestCodexRetriesDisconnectedStreamThenSucceeds(t *testing.T) {
	t.Parallel()

	capture := t.TempDir()
	script := filepath.Join(t.TempDir(), "fake-codex")
	contents := fmt.Sprintf(`#!/bin/sh
if test "$3" = debug && test "$4" = models; then
  printf '%%s\n' '{"models":[{"slug":"bundled-model","base_instructions":"bundled"}]}'
  exit 0
fi
count_file=%q/count
count=0
if test -f "$count_file"; then count=$(tr -d '\n' < "$count_file"); fi
count=$((count + 1))
printf '%%s\n' "$count" > "$count_file"
if test "$count" = 1; then
  printf '%%s\n' 'Reconnecting... 1/5 (stream disconnected before completion: stream closed before response.completed)' >&2
  exit 2
fi
printf '%%s\n' '{"type":"item.completed","item":{"id":"answer","type":"agent_message","text":"recovered"}}'
printf '%%s\n' '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1,"reasoning_output_tokens":0}}'
`, capture)
	if err := os.WriteFile(script, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
	agent := newCodex(Config{Executable: script})
	agent.waitBeforeRetry = noRetryWait
	security := mustResolveCodexSecurity(t, agent, SecurityPolicy{Level: SandboxIsolated})
	result, err := agent.Run(context.Background(), Request{WorkDir: t.TempDir(), Prompt: "test", Security: security, Timeout: time.Second})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Response != "recovered" {
		t.Fatalf("response = %q, want recovered", result.Response)
	}
	if result.Attempts.AttemptCount != 2 || len(result.Attempts.AttemptErrors) != 1 || !strings.Contains(result.Attempts.AttemptErrors[0].Error, "stream disconnected before completion") {
		t.Fatalf("attempt evidence = %#v, want one failed transport attempt before success", result.Attempts)
	}
	count, err := os.ReadFile(filepath.Join(capture, "count"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(count)) != "2" {
		t.Fatalf("Codex invocations = %q, want two", strings.TrimSpace(string(count)))
	}
}

func TestCodexStopsAfterTwoTransportRetries(t *testing.T) {
	t.Parallel()

	capture := t.TempDir()
	script := filepath.Join(t.TempDir(), "fake-codex")
	contents := fmt.Sprintf(`#!/bin/sh
if test "$3" = debug && test "$4" = models; then
  printf '%%s\n' '{"models":[{"slug":"bundled-model","base_instructions":"bundled"}]}'
  exit 0
fi
count_file=%q/count
count=0
if test -f "$count_file"; then count=$(tr -d '\n' < "$count_file"); fi
printf '%%s\n' "$((count + 1))" > "$count_file"
printf '%%s\n' 'connection reset by peer' >&2
exit 2
`, capture)
	if err := os.WriteFile(script, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
	agent := newCodex(Config{Executable: script})
	security := mustResolveCodexSecurity(t, agent, SecurityPolicy{Level: SandboxIsolated})
	var retries []int
	agent.waitBeforeRetry = func(_ context.Context, retry int) error {
		retries = append(retries, retry)
		return nil
	}
	_, err := agent.Run(context.Background(), Request{WorkDir: t.TempDir(), Prompt: "test", Security: security, Timeout: time.Second})
	if err == nil || !errors.Is(err, ErrTransient) {
		t.Fatalf("Run() error = %v, want exhausted transient error", err)
	}
	count, readErr := os.ReadFile(filepath.Join(capture, "count"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.TrimSpace(string(count)) != "3" {
		t.Fatalf("Codex invocations = %q, want three", strings.TrimSpace(string(count)))
	}
	attempts := AttemptsFromError(err)
	if attempts.AttemptCount != 3 || len(attempts.AttemptErrors) != 3 {
		t.Fatalf("attempt evidence = %#v, want all three failed attempts", attempts)
	}
	if fmt.Sprint(retries) != "[1 2]" {
		t.Fatalf("backoff retries = %v, want [1 2]", retries)
	}
}

func TestCodexDoesNotRetryCompletedResponse(t *testing.T) {
	t.Parallel()

	capture := t.TempDir()
	script := filepath.Join(t.TempDir(), "fake-codex")
	contents := fmt.Sprintf(`#!/bin/sh
if test "$3" = debug && test "$4" = models; then
  printf '%%s\n' '{"models":[{"slug":"bundled-model","base_instructions":"bundled"}]}'
  exit 0
fi
count_file=%q/count
count=0
if test -f "$count_file"; then count=$(tr -d '\n' < "$count_file"); fi
printf '%%s\n' "$((count + 1))" > "$count_file"
printf '%%s\n' '{"type":"item.completed","item":{"id":"answer","type":"agent_message","text":"completed"}}'
printf '%%s\n' '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1,"reasoning_output_tokens":0}}'
printf '%%s\n' 'connection reset by peer after completed response' >&2
exit 2
`, capture)
	if err := os.WriteFile(script, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
	agent := newCodex(Config{Executable: script})
	agent.waitBeforeRetry = noRetryWait
	security := mustResolveCodexSecurity(t, agent, SecurityPolicy{Level: SandboxIsolated})
	_, err := agent.Run(context.Background(), Request{WorkDir: t.TempDir(), Prompt: "test", Security: security, Timeout: time.Second})
	if err == nil {
		t.Fatal("Run() accepted a nonzero completed command")
	}
	if errors.Is(err, ErrTransient) {
		t.Fatalf("completed response was classified retryable: %v", err)
	}
	count, readErr := os.ReadFile(filepath.Join(capture, "count"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.TrimSpace(string(count)) != "1" {
		t.Fatalf("Codex invocations = %q, want one completed attempt", strings.TrimSpace(string(count)))
	}
}

func TestCodexProductionTransportMarkerControls(t *testing.T) {
	t.Parallel()

	const transportMessage = "Reconnecting... 1/5 (stream disconnected before completion: Transport error: network error: error decoding response body)"
	tests := []struct {
		name             string
		body             string
		wantResponse     string
		wantError        bool
		wantTransient    bool
		wantAttemptCount int
		wantInvocations  string
	}{
		{
			name: "retry then success",
			body: `if test "$count" = 1; then
	sleep 0.01
	printf '%s\n' '{"type":"item.completed","item":{"id":"progress","type":"agent_message","text":"Working on the task."}}'
	printf '%s\n' 'Reconnecting... 1/5 (stream disconnected before completion: Transport error: network error: error decoding response body)' >&2
	exit 2
fi
printf '%s\n' '{"type":"item.completed","item":{"id":"answer","type":"agent_message","text":"recovered"}}'
printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'`,
			wantResponse:     "recovered",
			wantAttemptCount: 2,
			wantInvocations:  "2",
		},
		{
			name: "retry exhaustion",
			body: `sleep 0.01
printf '%s\n' '{"type":"item.completed","item":{"id":"progress","type":"agent_message","text":"Working on the task."}}'
printf '%s\n' 'transport diagnostic' >&2
printf '%s\n' '{"type":"error","message":"Reconnecting... 1/5 (stream disconnected before completion: Transport error: network error: error decoding response body)"}'
exit 0`,
			wantError:        true,
			wantTransient:    true,
			wantAttemptCount: 3,
			wantInvocations:  "3",
		},
		{
			name: "completed response is not retried",
			body: `printf '%s\n' '{"type":"item.completed","item":{"id":"answer","type":"agent_message","text":"completed"}}'
printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'
printf '%s\n' '{"type":"error","message":"Reconnecting... 1/5 (stream disconnected before completion: Transport error: network error: error decoding response body)"}'
exit 0`,
			wantError:        true,
			wantAttemptCount: 0,
			wantInvocations:  "1",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			capture := t.TempDir()
			script := filepath.Join(t.TempDir(), "fake-codex")
			contents := fmt.Sprintf(`#!/bin/sh
			if test "$3" = debug && test "$4" = models; then
			  printf '%%s\n' '{"models":[{"slug":"bundled-model","base_instructions":"bundled"}]}'
			  exit 0
			fi
count_file=%q/count
count=0
if test -f "$count_file"; then count=$(tr -d '\n' < "$count_file"); fi
count=$((count + 1))
printf '%%s\n' "$count" > "$count_file"
%s
`, capture, test.body)
			if err := os.WriteFile(script, []byte(contents), 0o755); err != nil {
				t.Fatal(err)
			}
			agent := newCodex(Config{Executable: script})
			agent.waitBeforeRetry = noRetryWait
			security := mustResolveCodexSecurity(t, agent, SecurityPolicy{Level: SandboxIsolated})
			result, err := agent.Run(context.Background(), Request{WorkDir: t.TempDir(), Prompt: "test", Security: security, Timeout: time.Second})
			if test.wantError && err == nil {
				t.Fatal("Run() succeeded, want error")
			}
			if !test.wantError && err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if errors.Is(err, ErrTransient) != test.wantTransient {
				t.Fatalf("Run() transient = %v, want %v; error=%v", errors.Is(err, ErrTransient), test.wantTransient, err)
			}
			attempts := result.Attempts
			if err != nil {
				attempts = AttemptsFromError(err)
			}
			if attempts.AttemptCount != test.wantAttemptCount {
				t.Fatalf("attempt count = %d, want %d; evidence=%#v", attempts.AttemptCount, test.wantAttemptCount, attempts)
			}
			if test.wantAttemptCount > 1 {
				if len(attempts.AttemptErrors) != test.wantAttemptCount-1 && !test.wantError {
					t.Fatalf("attempt errors = %d, want %d failed attempts before success", len(attempts.AttemptErrors), test.wantAttemptCount-1)
				}
				if test.wantError && len(attempts.AttemptErrors) != test.wantAttemptCount {
					t.Fatalf("attempt errors = %d, want %d exhausted attempts", len(attempts.AttemptErrors), test.wantAttemptCount)
				}
				for index, attemptErr := range attempts.AttemptErrors {
					if !strings.Contains(attemptErr.Error, transportMessage) {
						t.Fatalf("attempt error does not contain production marker: %#v", attemptErr)
					}
					if attemptErr.Attempt != index+1 || attemptErr.Timestamp.IsZero() || attemptErr.DurationMS <= 0 {
						t.Fatalf("attempt error lacks timing evidence: %#v", attemptErr)
					}
					if attemptErr.StdoutBytes <= 0 || attemptErr.StderrBytes <= 0 {
						t.Fatalf("attempt error lacks captured byte counts: %#v", attemptErr)
					}
					if index > 0 && !attemptErr.Timestamp.After(attempts.AttemptErrors[index-1].Timestamp) {
						t.Fatalf("attempt timestamps are not distinct and ordered: %#v", attempts.AttemptErrors)
					}
				}
			}
			if result.Response != test.wantResponse {
				t.Fatalf("response = %q, want %q", result.Response, test.wantResponse)
			}
			count, readErr := os.ReadFile(filepath.Join(capture, "count"))
			if readErr != nil {
				t.Fatal(readErr)
			}
			if got := strings.TrimSpace(string(count)); got != test.wantInvocations {
				t.Fatalf("Codex invocations = %q, want %q", got, test.wantInvocations)
			}
		})
	}
}

func TestCodexFirstTokenWatchdogControls(t *testing.T) {
	t.Setenv("SHUHARI_FIRST_TOKEN_TIMEOUT", "250ms")

	tests := []struct {
		name             string
		body             string
		wantResponse     string
		wantError        bool
		wantTransient    bool
		wantAttemptCount int
		wantInvocations  string
		wantWatchdog     bool
		wantElapsed      time.Duration
	}{
		{
			name: "silent attempt retries then succeeds",
			body: `if test "$count" = 1; then
  printf '%s\n' '{"type":"thread.started","thread_id":"silent"}'
  exec sleep 2
fi
printf '%s\n' '{"type":"item.completed","item":{"id":"answer","type":"agent_message","text":"recovered"}}'
printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'`,
			wantResponse:     "recovered",
			wantAttemptCount: 2,
			wantInvocations:  "2",
			wantWatchdog:     true,
		},
		{
			name: "silent attempts exhaust retry bound",
			body: `printf '%s\n' '{"type":"thread.started","thread_id":"silent"}'
exec sleep 2`,
			wantError:        true,
			wantTransient:    true,
			wantAttemptCount: 3,
			wantInvocations:  "3",
			wantWatchdog:     true,
		},
		{
			name: "first model item disables watchdog",
			body: `sleep 0.02
printf '%s\n' '{"type":"item.started","item":{"id":"answer","type":"agent_message","text":""}}'
sleep 0.35
printf '%s\n' '{"type":"item.completed","item":{"id":"answer","type":"agent_message","text":"slow response"}}'
printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'`,
			wantResponse:     "slow response",
			wantAttemptCount: 1,
			wantInvocations:  "1",
			wantElapsed:      300 * time.Millisecond,
		},
		{
			name: "completed response is not retried",
			body: `printf '%s\n' '{"type":"item.completed","item":{"id":"answer","type":"agent_message","text":"completed"}}'
printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'
printf '%s\n' 'connection reset by peer after completed response' >&2
exit 2`,
			wantError:        true,
			wantAttemptCount: 0,
			wantInvocations:  "1",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			capture := t.TempDir()
			script := filepath.Join(t.TempDir(), "fake-codex")
			contents := fmt.Sprintf(`#!/bin/sh
			if test "$3" = debug && test "$4" = models; then
			  printf '%%s\n' '{"models":[{"slug":"bundled-model","base_instructions":"bundled"}]}'
			  exit 0
			fi
count_file=%q/count
count=0
if test -f "$count_file"; then count=$(tr -d '\n' < "$count_file"); fi
count=$((count + 1))
printf '%%s\n' "$count" > "$count_file"
%s
`, capture, test.body)
			if err := os.WriteFile(script, []byte(contents), 0o755); err != nil {
				t.Fatal(err)
			}
			agent := newCodex(Config{Executable: script})
			agent.waitBeforeRetry = noRetryWait
			security := mustResolveCodexSecurity(t, agent, SecurityPolicy{Level: SandboxIsolated})
			started := time.Now()
			result, err := agent.Run(context.Background(), Request{WorkDir: t.TempDir(), Prompt: "test", Security: security, Timeout: 500 * time.Millisecond})
			elapsed := time.Since(started)
			if test.wantError && err == nil {
				t.Fatal("Run() succeeded, want error")
			}
			if !test.wantError && err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if errors.Is(err, ErrTransient) != test.wantTransient {
				t.Fatalf("Run() transient = %v, want %v; error=%v", errors.Is(err, ErrTransient), test.wantTransient, err)
			}
			attempts := result.Attempts
			if err != nil {
				attempts = AttemptsFromError(err)
			}
			if attempts.AttemptCount != test.wantAttemptCount {
				t.Fatalf("attempt count = %d, want %d; evidence=%#v", attempts.AttemptCount, test.wantAttemptCount, attempts)
			}
			if test.wantWatchdog {
				wantErrors := test.wantAttemptCount
				if !test.wantError {
					wantErrors--
				}
				if len(attempts.AttemptErrors) != wantErrors {
					t.Fatalf("attempt errors = %d, want %d", len(attempts.AttemptErrors), wantErrors)
				}
				for index, attemptErr := range attempts.AttemptErrors {
					if !strings.Contains(attemptErr.Error, "no model output within 250ms") {
						t.Fatalf("attempt error is not a first-token watchdog failure: %#v", attemptErr)
					}
					if attemptErr.DurationMS < 150 || attemptErr.DurationMS > 1000 {
						t.Fatalf("attempt duration = %dms, want approximately 250ms", attemptErr.DurationMS)
					}
					if attemptErr.StdoutBytes == 0 || attemptErr.Timestamp.IsZero() {
						t.Fatalf("watchdog attempt lacks receipt evidence: %#v", attemptErr)
					}
					if index > 0 && !attemptErr.Timestamp.After(attempts.AttemptErrors[index-1].Timestamp) {
						t.Fatalf("attempt timestamps are not ordered: %#v", attempts.AttemptErrors)
					}
				}
			}
			if test.wantElapsed > 0 && elapsed < test.wantElapsed {
				t.Fatalf("Run() elapsed %s, want at least %s after first model item", elapsed, test.wantElapsed)
			}
			if result.Response != test.wantResponse {
				t.Fatalf("response = %q, want %q", result.Response, test.wantResponse)
			}
			count, readErr := os.ReadFile(filepath.Join(capture, "count"))
			if readErr != nil {
				t.Fatal(readErr)
			}
			if got := strings.TrimSpace(string(count)); got != test.wantInvocations {
				t.Fatalf("Codex invocations = %q, want %q", got, test.wantInvocations)
			}
		})
	}
}

func TestCodexFirstTokenTimeoutConfiguration(t *testing.T) {
	t.Setenv("SHUHARI_FIRST_TOKEN_TIMEOUT", "")
	if timeout, err := configuredFirstTokenTimeout(); err != nil || timeout != 90*time.Second {
		t.Fatalf("configuredFirstTokenTimeout() = %s, %v; want 90s", timeout, err)
	}

	for _, value := range []string{"invalid", "0s", "-1s"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("SHUHARI_FIRST_TOKEN_TIMEOUT", value)
			capture := t.TempDir()
			script := filepath.Join(t.TempDir(), "fake-codex")
			contents := fmt.Sprintf("#!/bin/sh\nprintf 'called\\n' > %q/called\nprintf '%%s\\n' '{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"done\"}}'\nprintf '%%s\\n' '{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}'\n", capture)
			if err := os.WriteFile(script, []byte(contents), 0o755); err != nil {
				t.Fatal(err)
			}
			agent := newCodex(Config{Executable: script})
			security := mustResolveCodexSecurity(t, agent, SecurityPolicy{Level: SandboxIsolated})
			_, err := agent.Run(context.Background(), Request{WorkDir: t.TempDir(), Prompt: "test", Security: security, Timeout: time.Second})
			if err == nil || !strings.Contains(err.Error(), "SHUHARI_FIRST_TOKEN_TIMEOUT") {
				t.Fatalf("Run() error = %v, want actionable watchdog configuration error", err)
			}
			if _, statErr := os.Stat(filepath.Join(capture, "called")); !os.IsNotExist(statErr) {
				t.Fatalf("Codex started before watchdog configuration refusal: %v", statErr)
			}
		})
	}
}

func noRetryWait(context.Context, int) error { return nil }

func TestCodexDoesNotRetryInputTooLarge(t *testing.T) {
	capture := t.TempDir()
	script := filepath.Join(t.TempDir(), "fake-codex")
	contents := fmt.Sprintf(`#!/bin/sh
if test "$3" = debug && test "$4" = models; then
  printf '%%s\n' '{"models":[{"slug":"bundled-model","base_instructions":"bundled"}]}'
  exit 0
fi
count_file=%q/count
count=0
if test -f "$count_file"; then count=$(tr -d '\n' < "$count_file"); fi
printf '%%s\n' "$((count + 1))" > "$count_file"
printf '%%s\n' 'WARNING: temporary directory is in use' >&2
printf '%%s\n' 'turn/start failed: Input exceeds the maximum length of 1048576 characters. data: {"input_error_code":"input_too_large"}' >&2
exit 1
`, capture)
	if err := os.WriteFile(script, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
	agent := newCodex(Config{Executable: script})
	t.Setenv(NoCredentialBoundaryAcknowledgementEnv, "1")
	security := mustResolveCodexSecurity(t, agent, SecurityPolicy{Level: SandboxUnsandboxed, Network: true})
	_, err := agent.Run(context.Background(), Request{WorkDir: t.TempDir(), Prompt: "oversized", Security: security, Timeout: time.Second})
	if err == nil {
		t.Fatal("Run() accepted input_too_large")
	}
	if errors.Is(err, ErrTransient) {
		t.Fatalf("input_too_large was classified transient: %v", err)
	}
	count, readErr := os.ReadFile(filepath.Join(capture, "count"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.TrimSpace(string(count)) != "1" {
		t.Fatalf("Codex invocations = %q, want one", strings.TrimSpace(string(count)))
	}
}

func TestEffectiveSandboxLevelDoesNotReadEnvironment(t *testing.T) {
	t.Setenv("SHUHARI_SANDBOX", "read-only")
	got, err := EffectiveSandboxLevel("isolated")
	if err != nil || got != SandboxIsolated {
		t.Fatalf("EffectiveSandboxLevel() = %q, %v", got, err)
	}
}

func TestCodexProbeRejectsUnavailableProtectedSandbox(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "codex")
	contents := `#!/bin/sh
if test "$1" = "--version"; then
  printf '%s\n' 'codex-cli test'
  exit 0
fi
printf '%s\n' 'failed to create user namespace' >&2
exit 1
`
	if err := os.WriteFile(script, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
	codexHome := filepath.Join(root, "codex-home")
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", codexHome)
	agent := newCodex(Config{Executable: script})
	security := mustResolveCodexSecurity(t, agent, SecurityPolicy{Level: SandboxIsolated})
	_, err := agent.Probe(context.Background(), security)
	if err == nil || !errors.Is(err, ErrUnsupportedSecurityPolicy) {
		t.Fatalf("Probe() error = %v, want ErrUnsupportedSecurityPolicy", err)
	}
	if !strings.Contains(err.Error(), "unsandboxed") || !strings.Contains(err.Error(), "isolated runner") {
		t.Fatalf("Probe() error is not actionable: %v", err)
	}
}

func mustResolveCodexSecurity(t *testing.T, agent *codexHarness, policy SecurityPolicy) SecurityResolution {
	t.Helper()
	resolution, err := agent.ResolveSecurity(context.Background(), policy)
	if err != nil {
		t.Fatalf("ResolveSecurity() error = %v", err)
	}
	return resolution
}

func TestInstallCodexTargetWithholdsEvalDefinitionsFromSkill(t *testing.T) {
	source := t.TempDir()
	writeSkillFile(t, source, "SKILL.md", "---\nname: demo\ndescription: demo\n---\n")
	writeSkillFile(t, source, filepath.Join("references", "notes.md"), "reference material")
	writeSkillFile(t, source, filepath.Join("scripts", "run.sh"), "#!/bin/sh\n")
	writeSkillFile(t, source, filepath.Join(EvalDefinitionDir, "evals.json"), `{"skill_name":"demo"}`)
	writeSkillFile(t, source, filepath.Join(EvalDefinitionDir, "triggers.json"), `{"skill_name":"demo"}`)
	writeSkillFile(t, source, filepath.Join(EvalDefinitionDir, "files", "sample.csv"), "a,b\n")

	workDir := t.TempDir()
	if err := installCodexTarget(workDir, Target{Kind: TargetSkill, Name: "demo", SourcePath: source}); err != nil {
		t.Fatalf("installCodexTarget() error = %v", err)
	}

	installed := filepath.Join(workDir, ".agents", "skills", "demo")
	for _, relative := range []string{
		"SKILL.md",
		filepath.Join("references", "notes.md"),
		filepath.Join("scripts", "run.sh"),
	} {
		if _, err := os.Stat(filepath.Join(installed, relative)); err != nil {
			t.Fatalf("skill content %q was not installed: %v", relative, err)
		}
	}

	// The evaluated agent must not be able to read the expected output, the
	// assertions, or the trigger expectation for the case it is answering.
	if _, err := os.Stat(filepath.Join(installed, EvalDefinitionDir)); !os.IsNotExist(err) {
		t.Fatalf("eval definitions reached the evaluated workspace: err = %v", err)
	}
}

func TestCopyTreeStillCopiesEverythingForSnapshots(t *testing.T) {
	source := t.TempDir()
	writeSkillFile(t, source, filepath.Join(EvalDefinitionDir, "evals.json"), `{"skill_name":"demo"}`)

	destination := filepath.Join(t.TempDir(), "snapshot")
	if err := copyTree(source, destination); err != nil {
		t.Fatalf("copyTree() error = %v", err)
	}

	// Retry snapshots restore the whole work directory, so this path must not
	// inherit the target-installation exclusion.
	if _, err := os.Stat(filepath.Join(destination, EvalDefinitionDir, "evals.json")); err != nil {
		t.Fatalf("copyTree() skipped a file it must preserve: %v", err)
	}
}

func writeSkillFile(t *testing.T, root, relative, contents string) {
	t.Helper()
	path := filepath.Join(root, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSandboxProbeCommandRunsOnThisHost(t *testing.T) {
	command := sandboxProbeCommand()
	if len(command) == 0 {
		t.Fatal("sandboxProbeCommand() returned no command")
	}

	// The preflight reports a failure here as "the sandbox cannot start", so a
	// probe command that is merely missing would be misdiagnosed. Recent macOS
	// ships no /bin/true, which is exactly how that happened.
	if runtime.GOOS != "windows" {
		if _, err := os.Stat(command[0]); err != nil {
			t.Fatalf("sandbox probe executable %q is not present on this host: %v", command[0], err)
		}
	}

	if err := exec.Command(command[0], command[1:]...).Run(); err != nil {
		t.Fatalf("sandbox probe command %v did not succeed: %v", command, err)
	}
}

func TestSandboxProbeCommandPerOperatingSystem(t *testing.T) {
	for _, testCase := range []struct {
		goos string
		want []string
	}{
		{goos: "darwin", want: []string{"/bin/sh", "-c", ":"}},
		{goos: "linux", want: []string{"/bin/sh", "-c", ":"}},
		{goos: "windows", want: []string{"cmd.exe", "/c", "exit", "0"}},
	} {
		t.Run(testCase.goos, func(t *testing.T) {
			got := sandboxProbeCommandFor(testCase.goos)
			if len(got) != len(testCase.want) {
				t.Fatalf("sandboxProbeCommandFor(%q) = %v, want %v", testCase.goos, got, testCase.want)
			}
			for index := range testCase.want {
				if got[index] != testCase.want[index] {
					t.Fatalf("sandboxProbeCommandFor(%q) = %v, want %v", testCase.goos, got, testCase.want)
				}
			}
		})
	}
}

func TestResolveSecurityRefusesUnknownHostTool(t *testing.T) {
	agent := newCodex(Config{Executable: "codex"})
	policy := SecurityPolicy{Level: SandboxIsolated, HostTools: []string{"definitely-not-a-real-tool-xyz"}}

	_, err := agent.ResolveSecurity(context.Background(), policy)
	if err == nil || !errors.Is(err, ErrUnsupportedSecurityPolicy) {
		t.Fatalf("ResolveSecurity() error = %v, want ErrUnsupportedSecurityPolicy", err)
	}
	// Running without a declared tool would surface as the agent reporting the
	// tool unavailable, which grades as a skill failure and hides the cause.
	if !strings.Contains(err.Error(), "definitely-not-a-real-tool-xyz") {
		t.Fatalf("ResolveSecurity() error does not name the missing tool: %v", err)
	}
}

func TestResolveSecurityRecordsDeclaredHostTools(t *testing.T) {
	agent := newCodex(Config{Executable: "codex"})
	bare := mustResolveCodexSecurity(t, agent, SecurityPolicy{Level: SandboxIsolated})
	withTool := mustResolveCodexSecurity(t, agent, SecurityPolicy{Level: SandboxIsolated, HostTools: []string{"sh"}})

	if len(withTool.HostTools) != 1 || withTool.HostTools[0] != "sh" {
		t.Fatalf("HostTools = %v, want [sh]", withTool.HostTools)
	}
	// Exposing a tool widens the boundary, so it must change the recorded
	// policy digest rather than producing a run indistinguishable from a
	// sealed one.
	if withTool.Adapter.PolicyDigest == bare.Adapter.PolicyDigest {
		t.Fatal("declared host tools did not change the policy digest")
	}
}

func TestIsolatedCommandPathExposesDeclaredTools(t *testing.T) {
	basePath, _ := isolatedCommandPath(nil)
	toolPath, tools := isolatedCommandPath([]string{"sh"})

	if basePath != toolPath {
		// `sh` lives in /bin, already on the base path, so nothing should move.
		t.Fatalf("declaring an already-reachable tool changed PATH:\n base=%s\n with=%s", basePath, toolPath)
	}
	for _, tool := range tools {
		if strings.HasSuffix(tool, "/sh") {
			t.Fatal("an already-reachable tool was granted a redundant permission entry")
		}
	}
}

func TestIsolatedCommandPathAddsToolOutsideTheSystemPath(t *testing.T) {
	// The point of the feature is a tool that the fixed system PATH does not
	// already reach, so the test needs one that genuinely lives elsewhere.
	directory := t.TempDir()
	tool := filepath.Join(directory, "shuhari-fake-tool")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))

	path, tools := isolatedCommandPath([]string{"shuhari-fake-tool"})

	if !strings.Contains(path, directory) {
		t.Fatalf("declared tool directory %q is not on the sandbox PATH: %s", directory, path)
	}
	found := false
	for _, granted := range tools {
		if granted == tool {
			found = true
		}
	}
	// Without the read grant the binary is on PATH but not executable in the
	// sandbox, which fails in a way that looks like the tool is missing.
	if !found {
		t.Fatalf("declared tool %q was not granted read permission: %v", tool, tools)
	}
}

func TestResolveHostToolsRejectsEmptyName(t *testing.T) {
	if _, err := resolveHostTools([]string{"  "}); err == nil {
		t.Fatal("resolveHostTools() accepted an empty tool name")
	}
}
