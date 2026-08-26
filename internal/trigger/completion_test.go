package trigger

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shunk031/shuhari/internal/harness"
)

type completionTriggerHarness struct {
	mu       sync.Mutex
	requests []harness.Request
}

func (*completionTriggerHarness) Probe(_ context.Context, securities ...harness.SecurityResolution) (harness.Identity, error) {
	if len(securities) != 0 {
		return harness.Identity{}, errors.New("completion probe received security")
	}
	return harness.Identity{Agent: "fake", Version: "completion"}, nil
}

func (*completionTriggerHarness) Capabilities() harness.Capabilities {
	return harness.Capabilities{TriggerEvidence: true}
}

func (*completionTriggerHarness) ResolveSecurity(context.Context, harness.SecurityPolicy) (harness.SecurityResolution, error) {
	return harness.SecurityResolution{}, errors.New("completion resolved security")
}

func (h *completionTriggerHarness) Run(_ context.Context, request harness.Request) (harness.Result, error) {
	h.mu.Lock()
	h.requests = append(h.requests, request)
	h.mu.Unlock()
	if request.Mode != harness.ModeCompletion || request.WorkDir != "" || request.Target != nil || !request.Security.Equal(harness.SecurityResolution{}) || len(request.OutputSchema) == 0 {
		return harness.Result{}, errors.New("completion trigger request carried agentic state")
	}
	invoke := strings.Contains(request.Prompt, `"prompt":"relevant"`)
	reason := "the prompt matches the target skill description"
	if !invoke {
		reason = "the prompt does not match the target skill description"
	}
	response := `{"invoke":` + map[bool]string{true: "true", false: "false"}[invoke] + `,"reason":"` + reason + `"}`
	return harness.Result{Response: response, Transcript: []byte("completion transcript\n"), Duration: time.Millisecond, Attempts: harness.AttemptEvidence{AttemptCount: 1}}, nil
}

func mustWrite(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCompletionTriggerRecordsStructuredDecisionWithoutSecurity(t *testing.T) {
	root := filepath.Join(t.TempDir(), "demo")
	mustWrite(t, filepath.Join(root, "SKILL.md"), "---\nname: demo\ndescription: Use for relevant tasks.\n---\n")
	mustWrite(t, filepath.Join(root, "evals", "triggers.json"), `{"skill_name":"demo","cases":[{"id":"yes","prompt":"relevant","should_trigger":true},{"id":"no","prompt":"unrelated","should_trigger":false}]}`)
	suite, err := LoadSuite(root, "")
	if err != nil {
		t.Fatal(err)
	}
	agent := &completionTriggerHarness{}
	report, err := Run(context.Background(), suite, agent, Config{Mode: harness.ModeCompletion, Trials: 1, Jobs: 1, Timeout: time.Second, Workspace: filepath.Join(t.TempDir(), "workspace")})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Passed {
		t.Fatalf("report = %#v", report)
	}
	agent.mu.Lock()
	requests := append([]harness.Request(nil), agent.requests...)
	agent.mu.Unlock()
	if len(requests) != 2 {
		t.Fatalf("completion trigger requests = %d, want two", len(requests))
	}
	contents, err := os.ReadFile(filepath.Join(report.Workspace, "trigger.json"))
	if err != nil {
		t.Fatal(err)
	}
	var artifact struct {
		Mode          harness.Mode                `json:"mode"`
		Security      *harness.SecurityResolution `json:"security"`
		TargetRead    map[string][]bool           `json:"target_read"`
		TargetApplied map[string][]bool           `json:"target_applied"`
		TargetInvoked map[string][]bool           `json:"target_invoked"`
		Decisions     map[string][]Decision       `json:"decisions"`
	}
	if err := json.Unmarshal(contents, &artifact); err != nil {
		t.Fatal(err)
	}
	if artifact.Mode != harness.ModeCompletion || artifact.Security != nil || artifact.TargetRead != nil || artifact.TargetApplied != nil || !artifact.TargetInvoked["yes"][0] || artifact.TargetInvoked["no"][0] || !artifact.Decisions["yes"][0].Invoke || artifact.Decisions["no"][0].Invoke {
		t.Fatalf("trigger completion provenance = %#v", artifact)
	}
	for _, item := range []struct {
		caseID string
		invoke bool
	}{
		{caseID: "yes", invoke: true},
		{caseID: "no", invoke: false},
	} {
		contents, err := os.ReadFile(filepath.Join(report.Workspace, "case-"+item.caseID, "trial-1", "application.json"))
		if err != nil {
			t.Fatal(err)
		}
		var application struct {
			Mode          harness.Mode `json:"mode"`
			TargetRead    *bool        `json:"target_read"`
			Applied       *bool        `json:"applied"`
			TargetInvoked *bool        `json:"target_invoked"`
			Verdict       string       `json:"verdict"`
			Decision      *Decision    `json:"decision"`
		}
		if err := json.Unmarshal(contents, &application); err != nil {
			t.Fatal(err)
		}
		if application.Mode != harness.ModeCompletion || application.TargetRead != nil || application.Applied != nil || application.TargetInvoked == nil || *application.TargetInvoked != item.invoke || application.Decision == nil || application.Decision.Invoke != item.invoke {
			t.Fatalf("completion application provenance for %s = %#v", item.caseID, application)
		}
		wantVerdict := "not_invoked"
		if item.invoke {
			wantVerdict = "invoked"
		}
		if application.Verdict != wantVerdict {
			t.Fatalf("completion application verdict for %s = %q, want %q", item.caseID, application.Verdict, wantVerdict)
		}
	}
}

func TestDecodeCompletionDecisionRequiresInvoke(t *testing.T) {
	if _, err := decodeCompletionDecision(`{"reason":"missing boolean"}`); err == nil || !strings.Contains(err.Error(), "omitted invoke") {
		t.Fatalf("decodeCompletionDecision() error = %v, want missing invoke error", err)
	}
}
