package trigger

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shunk031/shuhari/internal/cache"
	"github.com/shunk031/shuhari/internal/harness"
)

type triggerHarness struct {
	mu   sync.Mutex
	runs int
}

func (*triggerHarness) Probe(context.Context) (harness.Identity, error) {
	return harness.Identity{Agent: "fake", Version: "1"}, nil
}

func (*triggerHarness) Capabilities() harness.Capabilities {
	return harness.Capabilities{Skills: true, TriggerEvidence: true}
}

func (h *triggerHarness) Run(_ context.Context, request harness.Request) (harness.Result, error) {
	h.mu.Lock()
	h.runs++
	h.mu.Unlock()
	return harness.Result{Response: "done", Transcript: []byte("{}\n"), TargetRead: strings.Contains(request.Prompt, "relevant"), Duration: time.Millisecond}, nil
}

func TestRunChecksPositiveAndNegativeCases(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "demo")
	if err := os.MkdirAll(filepath.Join(root, "evals"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "SKILL.md"), []byte("---\nname: demo\ndescription: Demo\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := map[string]any{"skill_name": "demo", "cases": []map[string]any{{"id": 1, "prompt": "relevant task", "should_trigger": true}, {"id": 2, "prompt": "near miss", "should_trigger": false}}}
	encoded, _ := json.Marshal(cases)
	if err := os.WriteFile(filepath.Join(root, "evals", "triggers.json"), encoded, 0o644); err != nil {
		t.Fatal(err)
	}
	suite, err := LoadSuite(root, "")
	if err != nil {
		t.Fatal(err)
	}
	report, err := Run(context.Background(), suite, &triggerHarness{}, cache.Store{Root: filepath.Join(t.TempDir(), "cache")}, Config{Trials: 3, Jobs: 2, Timeout: time.Second, Workspace: filepath.Join(t.TempDir(), "workspace")})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !report.Passed {
		t.Fatalf("report = %#v", report)
	}
	if _, err := os.Stat(filepath.Join(report.Workspace, "trigger.json")); err != nil {
		t.Fatalf("missing trigger summary: %v", err)
	}
	summaryContents, err := os.ReadFile(filepath.Join(report.Workspace, "trigger.json"))
	if err != nil {
		t.Fatal(err)
	}
	var summary struct {
		Security harness.ExecutionSecurity `json:"security"`
	}
	if err := json.Unmarshal(summaryContents, &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Security.CredentialBoundary != harness.CredentialBoundaryCodexSandbox {
		t.Fatalf("trigger credential boundary = %q", summary.Security.CredentialBoundary)
	}
}

func TestRunCacheIncludesEffectiveSandboxOverride(t *testing.T) {
	t.Setenv("SHUHARI_SANDBOX", "")

	root := filepath.Join(t.TempDir(), "demo")
	if err := os.MkdirAll(filepath.Join(root, "evals"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "SKILL.md"), []byte("---\nname: demo\ndescription: Demo\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	contents := `{"skill_name":"demo","cases":[{"id":"yes","prompt":"relevant","should_trigger":true},{"id":"no","prompt":"near miss","should_trigger":false}]}`
	if err := os.WriteFile(filepath.Join(root, "evals", "triggers.json"), []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	suite, err := LoadSuite(root, "")
	if err != nil {
		t.Fatal(err)
	}
	agent := &triggerHarness{}
	store := cache.Store{Root: filepath.Join(t.TempDir(), "cache")}
	config := Config{Trials: 1, Jobs: 1, Timeout: time.Second, Workspace: filepath.Join(t.TempDir(), "workspace"), Sandbox: "workspace-write"}
	if _, err := Run(context.Background(), suite, agent, store, config); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHUHARI_SANDBOX", "danger-full-access")
	t.Setenv(harness.DangerFullAccessAcknowledgementEnv, "1")
	report, err := Run(context.Background(), suite, agent, store, config)
	if err != nil {
		t.Fatal(err)
	}
	if report.Cached || agent.runs != 4 {
		t.Fatalf("sandbox override reused stale cache: cached=%v runs=%d", report.Cached, agent.runs)
	}
	summaryContents, err := os.ReadFile(filepath.Join(report.Workspace, "trigger.json"))
	if err != nil {
		t.Fatal(err)
	}
	var summary struct {
		Security harness.ExecutionSecurity `json:"security"`
	}
	if err := json.Unmarshal(summaryContents, &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Security.CredentialBoundary != harness.CredentialBoundaryNone || summary.Security.SandboxMode != "danger-full-access" {
		t.Fatalf("danger trigger security = %#v", summary.Security)
	}
}
