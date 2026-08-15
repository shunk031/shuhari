package eval

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/shunk031/shuhari/internal/cache"
	"github.com/shunk031/shuhari/internal/harness"
)

type fakeHarness struct {
	mu            sync.Mutex
	runs          int
	judgeResponse string
	requests      []harness.Request
}

type limitedHarness struct {
	fakeHarness
	capabilities harness.Capabilities
}

func (h *limitedHarness) Capabilities() harness.Capabilities { return h.capabilities }

func (*fakeHarness) Probe(context.Context) (harness.Identity, error) {
	return harness.Identity{Agent: "fake", Version: "1"}, nil
}

func (*fakeHarness) Capabilities() harness.Capabilities {
	return harness.Capabilities{Skills: true, Instructions: true, TriggerEvidence: true}
}

func (h *fakeHarness) Run(_ context.Context, request harness.Request) (harness.Result, error) {
	h.mu.Lock()
	h.runs++
	h.requests = append(h.requests, request)
	h.mu.Unlock()
	if len(request.OutputSchema) > 0 {
		return harness.Result{Response: h.judgeResponse, Duration: time.Millisecond, Usage: harness.Usage{InputTokens: 1}}, nil
	}
	response := "baseline output"
	if request.Target != nil {
		response = "candidate output"
	}
	return harness.Result{Response: response, Transcript: []byte("{}\n"), Duration: 10 * time.Millisecond, Usage: harness.Usage{InputTokens: 10, OutputTokens: 5}, Actions: []harness.Action{harness.ActionWebSearch, harness.ActionFileChange}}, nil
}

func TestRunWritesAgentSkillsWorkspaceAndCachesSuccess(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "demo")
	mustWrite(t, filepath.Join(root, "SKILL.md"), "---\nname: demo\ndescription: Demo skill\n---\n")
	mustWrite(t, filepath.Join(root, "evals", "evals.json"), `{
  "skill_name": "demo",
  "evals": [{
    "id": 1,
    "prompt": "do the task",
    "expected_output": "a correct result",
    "assertions": ["The result is correct"],
    "required_actions": ["web_search", "file_change"]
  }]
}`)
	suite, err := LoadSkillSuite(root)
	if err != nil {
		t.Fatal(err)
	}
	mapping := blindLabels("1", 1, variantWithSkill, variantWithoutSkill)
	grades := map[string][]AssertionResult{
		variantWithSkill:    {{Text: "The result is correct", Passed: true, Evidence: "candidate"}},
		variantWithoutSkill: {{Text: "The result is correct", Passed: false, Evidence: "baseline"}},
	}
	judge := judgeOutput{Cases: []judgeEntry{{ID: "1", Trial: 1, AAssertionResults: grades[mapping.A], BAssertionResults: grades[mapping.B], Preferred: preferredLabel(mapping, variantWithSkill), Reason: "candidate is better"}}}
	encoded, _ := json.Marshal(judge)
	agent := &fakeHarness{judgeResponse: string(encoded)}
	store := cache.Store{Root: filepath.Join(t.TempDir(), "cache")}
	workspace := filepath.Join(t.TempDir(), "workspace")
	config := Config{Trials: 1, Jobs: 2, Timeout: time.Second, Workspace: workspace}

	report, err := Run(context.Background(), suite, agent, store, config)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !report.Passed || report.Cached {
		t.Fatalf("report = %#v", report)
	}
	for _, path := range []string{
		filepath.Join(report.Workspace, "eval-1", "with_skill", "outputs", "response.md"),
		filepath.Join(report.Workspace, "eval-1", "with_skill", "timing.json"),
		filepath.Join(report.Workspace, "eval-1", "with_skill", "grading.json"),
		filepath.Join(report.Workspace, "eval-1", "without_skill", "grading.json"),
		filepath.Join(report.Workspace, "benchmark.json"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("missing artifact %s: %v", path, err)
		}
	}

	cached, err := Run(context.Background(), suite, agent, store, config)
	if err != nil {
		t.Fatalf("cached Run() error = %v", err)
	}
	if !cached.Passed || !cached.Cached {
		t.Fatalf("cached report = %#v", cached)
	}
	if agent.runs != 3 { // candidate, baseline, and one grader invocation.
		t.Fatalf("agent runs = %d, want 3", agent.runs)
	}
	for _, request := range agent.requests {
		if len(request.OutputSchema) == 0 && request.Network {
			t.Fatal("required_actions enabled network without --network")
		}
	}
}

func TestRunRejectsHarnessWithoutTargetCapability(t *testing.T) {
	t.Parallel()

	agent := &limitedHarness{capabilities: harness.Capabilities{Instructions: true}}
	_, err := Run(context.Background(), Suite{Kind: harness.TargetSkill}, agent, cache.Store{Root: t.TempDir()}, Config{Trials: 1, Jobs: 1, Timeout: time.Second})
	if err == nil {
		t.Fatal("Run() accepted a harness without skill support")
	}
}

func preferredLabel(mapping blindMapping, variant string) string {
	if mapping.A == variant {
		return "A"
	}
	return "B"
}
