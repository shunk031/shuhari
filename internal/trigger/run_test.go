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
	mu       sync.Mutex
	runs     int
	attempts harness.AttemptEvidence
	requests []harness.Request
}

func (*triggerHarness) Probe(context.Context) (harness.Identity, error) {
	return harness.Identity{Agent: "fake", Version: "1"}, nil
}

func (*triggerHarness) Capabilities() harness.Capabilities {
	return harness.Capabilities{Skills: true, TriggerEvidence: true}
}

func (*triggerHarness) ResolveSecurity(_ context.Context, policy harness.SecurityPolicy) (harness.SecurityResolution, error) {
	return fakeTriggerSecurityResolution(policy), nil
}

func fakeTriggerSecurityResolution(policy harness.SecurityPolicy) harness.SecurityResolution {
	network := harness.NetworkDenied
	if policy.Network {
		network = harness.NetworkAllowed
	}
	boundary := harness.CredentialBoundaryEnforced
	if policy.Level == harness.SandboxUnsandboxed {
		boundary = harness.CredentialBoundaryNone
	}
	return harness.SecurityResolution{
		SandboxLevel:       policy.Level,
		NetworkAccess:      network,
		CredentialBoundary: boundary,
		Adapter: harness.AdapterSecurity{
			Name:         "fake",
			NativeMode:   "fake-" + string(policy.Level),
			PolicyDigest: "sha256:" + strings.Repeat("b", 64),
		},
	}
}

func (h *triggerHarness) Run(_ context.Context, request harness.Request) (harness.Result, error) {
	h.mu.Lock()
	h.runs++
	h.requests = append(h.requests, request)
	h.mu.Unlock()
	return harness.Result{Response: "done", Transcript: []byte("{}\n"), TargetRead: strings.Contains(request.Prompt, "relevant"), Duration: time.Millisecond, Attempts: h.attempts}, nil
}

func TestRunPersistsTriggerTransportRetryEvidence(t *testing.T) {
	t.Parallel()

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
	attempts := harness.AttemptEvidence{AttemptCount: 2, AttemptErrors: []harness.AttemptError{{Attempt: 1, Error: "stream disconnected before completion"}}}
	report, err := Run(context.Background(), suite, &triggerHarness{attempts: attempts}, cache.Store{Root: t.TempDir()}, Config{Trials: 1, Jobs: 1, Timeout: time.Second, Workspace: filepath.Join(t.TempDir(), "workspace"), NoCache: true})
	if err != nil {
		t.Fatal(err)
	}
	timing, err := os.ReadFile(filepath.Join(report.Workspace, "case-yes", "trial-1", "timing.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(timing), `"attempt_count": 2`) || !strings.Contains(string(timing), "stream disconnected before completion") {
		t.Fatalf("trigger timing lacks retry evidence: %s", timing)
	}
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
	agent := &triggerHarness{}
	report, err := Run(context.Background(), suite, agent, cache.Store{Root: filepath.Join(t.TempDir(), "cache")}, Config{Trials: 3, Jobs: 2, Timeout: time.Second, Workspace: filepath.Join(t.TempDir(), "workspace")})
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
		SchemaVersion string                     `json:"schema_version"`
		DecisionRule  string                     `json:"decision_rule"`
		Security      harness.SecurityResolution `json:"security"`
	}
	if err := json.Unmarshal(summaryContents, &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Security.SandboxLevel != harness.SandboxIsolated || summary.Security.CredentialBoundary != harness.CredentialBoundaryEnforced {
		t.Fatalf("trigger credential boundary = %q", summary.Security.CredentialBoundary)
	}
	if summary.SchemaVersion != "2" || summary.DecisionRule != "majority" {
		t.Fatalf("trigger policy provenance = version %q rule %q, want version 2 majority", summary.SchemaVersion, summary.DecisionRule)
	}
	for _, request := range agent.requests {
		if request.Security != summary.Security {
			t.Fatalf("request security = %#v, want exact artifact value %#v", request.Security, summary.Security)
		}
	}
	manifestContents, err := os.ReadFile(filepath.Join(report.Workspace, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		SchemaVersion string                     `json:"schema_version"`
		Security      harness.SecurityResolution `json:"security"`
	}
	if err := json.Unmarshal(manifestContents, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.SchemaVersion != "2" || manifest.Security != summary.Security {
		t.Fatalf("manifest security = %#v, trigger security = %#v", manifest.Security, summary.Security)
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
	config := Config{Trials: 1, Jobs: 1, Timeout: time.Second, Workspace: filepath.Join(t.TempDir(), "workspace"), SandboxLevel: "isolated"}
	if _, err := Run(context.Background(), suite, agent, store, config); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHUHARI_SANDBOX", "read-only")
	t.Setenv(harness.NoCredentialBoundaryAcknowledgementEnv, "1")
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
		Security harness.SecurityResolution `json:"security"`
	}
	if err := json.Unmarshal(summaryContents, &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Security.CredentialBoundary != harness.CredentialBoundaryEnforced || summary.Security.SandboxLevel != harness.SandboxReadOnly {
		t.Fatalf("read-only trigger security = %#v", summary.Security)
	}
}
