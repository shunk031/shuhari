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

	"github.com/shunk031/shuhari/internal/cache"
	"github.com/shunk031/shuhari/internal/harness"
)

type applicationVerdictHarness struct {
	mu             sync.Mutex
	judgePrompts   []string
	judgeVerdicts  map[string]string
	judgeEvidence  map[string]string
	transcriptRoot string
}

func (*applicationVerdictHarness) Probe(context.Context, ...harness.SecurityResolution) (harness.Identity, error) {
	return harness.Identity{Agent: "fake", Version: "1"}, nil
}

func (*applicationVerdictHarness) Capabilities() harness.Capabilities {
	return harness.Capabilities{Skills: true, TriggerEvidence: true}
}

func (*applicationVerdictHarness) ResolveSecurity(_ context.Context, policy harness.SecurityPolicy) (harness.SecurityResolution, error) {
	return fakeTriggerSecurityResolution(policy), nil
}

func (h *applicationVerdictHarness) Run(_ context.Context, request harness.Request) (harness.Result, error) {
	if request.OutputSchema != nil {
		h.mu.Lock()
		h.judgePrompts = append(h.judgePrompts, request.Prompt)
		h.mu.Unlock()
		for marker, verdict := range h.judgeVerdicts {
			if strings.Contains(request.Prompt, marker) {
				encoded, _ := json.Marshal(map[string]string{"verdict": verdict, "evidence": h.judgeEvidence[marker]})
				return harness.Result{Response: string(encoded), Duration: time.Millisecond}, nil
			}
		}
		return harness.Result{}, errors.New("judge prompt did not match a fixture")
	}
	name := request.Prompt
	contents, err := os.ReadFile(filepath.Join(h.transcriptRoot, name+".jsonl"))
	if err != nil {
		return harness.Result{}, err
	}
	return harness.Result{
		Response:   "done",
		Transcript: contents,
		TargetRead: name != "no-read",
		Duration:   time.Millisecond,
	}, nil
}

func TestRunClassifiesApplicationInsteadOfConsultation(t *testing.T) {
	t.Parallel()

	suite := newApplicationSuite(t, []Case{
		{ID: "decline-a", Prompt: "read-then-decline-a", ShouldTrigger: false},
		{ID: "decline-b", Prompt: "read-then-decline-b", ShouldTrigger: false},
		{ID: "no-read", Prompt: "no-read", ShouldTrigger: false},
		{ID: "apply", Prompt: "read-then-apply", ShouldTrigger: true},
	})
	agent := &applicationVerdictHarness{
		transcriptRoot: "testdata",
		judgeVerdicts: map[string]string{
			"outside the demo service scope":  "declined",
			"intentionally out of scope":      "declined",
			"required service-check workflow": "applied",
		},
		judgeEvidence: map[string]string{
			"outside the demo service scope":  "The agent explicitly declined the skill and used only the local fixture.",
			"intentionally out of scope":      "The agent explicitly declined the skill and kept the operation local.",
			"required service-check workflow": "The agent followed the skill-specific service-check workflow.",
		},
	}
	report, err := Run(context.Background(), suite, agent, cache.Store{Root: t.TempDir()}, Config{
		Trials: 1, Jobs: 1, Timeout: time.Second, Workspace: filepath.Join(t.TempDir(), "workspace"), NoCache: true,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !report.Passed {
		t.Fatalf("report = %#v, want pass", report)
	}
	if len(agent.judgePrompts) != 3 {
		t.Fatalf("judge calls = %d, want 3 consulted trials", len(agent.judgePrompts))
	}

	contents, err := os.ReadFile(filepath.Join(report.Workspace, "trigger.json"))
	if err != nil {
		t.Fatal(err)
	}
	var artifact struct {
		SchemaVersion string            `json:"schema_version"`
		TargetRead    map[string][]bool `json:"target_read"`
		TargetApplied map[string][]bool `json:"target_applied"`
	}
	if err := json.Unmarshal(contents, &artifact); err != nil {
		t.Fatal(err)
	}
	if artifact.SchemaVersion != "3" {
		t.Fatalf("schema_version = %q, want 3", artifact.SchemaVersion)
	}
	if !artifact.TargetRead["decline-a"][0] || artifact.TargetApplied["decline-a"][0] {
		t.Fatalf("decline-a evidence = read %v applied %v, want true false", artifact.TargetRead["decline-a"], artifact.TargetApplied["decline-a"])
	}
	if artifact.TargetRead["no-read"][0] || artifact.TargetApplied["no-read"][0] {
		t.Fatalf("no-read evidence = read %v applied %v, want false false", artifact.TargetRead["no-read"], artifact.TargetApplied["no-read"])
	}
	if !artifact.TargetApplied["apply"][0] {
		t.Fatalf("apply evidence = %v, want true", artifact.TargetApplied["apply"])
	}
	assertApplicationArtifact(t, report.Workspace, "decline-a", "declined", false)
	assertApplicationArtifact(t, report.Workspace, "no-read", "not_consulted", false)
	assertApplicationArtifact(t, report.Workspace, "apply", "applied", true)
}

func TestRunFailsClosedOnAmbiguousApplication(t *testing.T) {
	t.Parallel()

	suite := newApplicationSuite(t, []Case{
		{ID: "apply", Prompt: "read-then-apply", ShouldTrigger: true},
		{ID: "ambiguous", Prompt: "read-then-decline-a", ShouldTrigger: false},
	})
	agent := &applicationVerdictHarness{
		transcriptRoot: "testdata",
		judgeVerdicts: map[string]string{
			"required service-check workflow": "applied",
			"outside the demo service scope":  "ambiguous",
		},
		judgeEvidence: map[string]string{
			"required service-check workflow": "The agent followed the skill-specific workflow.",
			"outside the demo service scope":  "The transcript contains conflicting signals.",
		},
	}
	report, err := Run(context.Background(), suite, agent, cache.Store{Root: t.TempDir()}, Config{
		Trials: 1, Jobs: 1, Timeout: time.Second, Workspace: filepath.Join(t.TempDir(), "workspace"), NoCache: true,
	})
	if err == nil || !strings.Contains(err.Error(), "application verdict is ambiguous") {
		t.Fatalf("Run() error = %v, want ambiguous application refusal", err)
	}
	assertApplicationArtifact(t, report.Workspace, "ambiguous", "ambiguous", false)
}

func newApplicationSuite(t *testing.T, cases []Case) Suite {
	t.Helper()
	root := filepath.Join(t.TempDir(), "demo-skill")
	if err := os.MkdirAll(filepath.Join(root, "evals"), 0o755); err != nil {
		t.Fatal(err)
	}
	skill := "---\nname: demo-skill\ndescription: Check the demo service\n---\n\nWhen the demo service is in scope, run `demo-service-check --required`.\n"
	if err := os.WriteFile(filepath.Join(root, "SKILL.md"), []byte(skill), 0o644); err != nil {
		t.Fatal(err)
	}
	casesPath := filepath.Join(root, "evals", "triggers.json")
	encoded, err := json.Marshal(map[string]any{"skill_name": "demo-skill", "cases": cases})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(casesPath, encoded, 0o644); err != nil {
		t.Fatal(err)
	}
	return Suite{SkillName: "demo-skill", SkillPath: root, CasesPath: casesPath, Cases: cases}
}

func assertApplicationArtifact(t *testing.T, workspace, caseID, verdict string, applied bool) {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join(workspace, "case-"+caseID, "trial-1", "application.json"))
	if err != nil {
		t.Fatal(err)
	}
	var artifact struct {
		Verdict string `json:"verdict"`
		Applied bool   `json:"applied"`
	}
	if err := json.Unmarshal(contents, &artifact); err != nil {
		t.Fatal(err)
	}
	if artifact.Verdict != verdict || artifact.Applied != applied {
		t.Fatalf("application artifact = %#v, want verdict %q applied %v", artifact, verdict, applied)
	}
}
