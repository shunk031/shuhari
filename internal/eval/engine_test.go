package eval

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

type fakeHarness struct {
	mu              sync.Mutex
	runs            int
	judgeResponse   string
	compareResponse string
	requests        []harness.Request
}

type limitedHarness struct {
	fakeHarness
	capabilities harness.Capabilities
}

type failingHarness struct{ fakeHarness }

func (h *failingHarness) Run(_ context.Context, request harness.Request) (harness.Result, error) {
	if len(request.OutputSchema) == 0 {
		return harness.Result{}, errors.New("deliberate execution failure")
	}
	return h.fakeHarness.Run(context.Background(), request)
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
		if strings.Contains(string(request.OutputSchema), `"preferred"`) {
			return harness.Result{Response: h.compareResponse, Duration: time.Millisecond, Usage: harness.Usage{InputTokens: 1}}, nil
		}
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
		variantWithSkill:    {{Text: "The result is correct", Passed: true, Evidence: `The response says "candidate output".`}},
		variantWithoutSkill: {{Text: "The result is correct", Passed: false, Evidence: "baseline"}},
	}
	judge := judgeOutput{Cases: []judgeEntry{{ID: "1", Trial: 1, AAssertionResults: grades[mapping.A], BAssertionResults: grades[mapping.B]}}}
	encoded, _ := json.Marshal(judge)
	comparison, _ := json.Marshal(comparatorOutput{Cases: []comparatorEntry{{ID: "1", Trial: 1, Preferred: preferredLabel(mapping, variantWithSkill), Reason: "candidate is better"}}})
	agent := &fakeHarness{judgeResponse: string(encoded), compareResponse: string(comparison)}
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
		filepath.Join(report.Workspace, "eval-1", "comparison.json"),
		filepath.Join(report.Workspace, "benchmark.json"),
		filepath.Join(report.Workspace, "manifest.json"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("missing artifact %s: %v", path, err)
		}
	}
	for _, variant := range []string{"with_skill", "without_skill"} {
		contents, err := os.ReadFile(filepath.Join(report.Workspace, "eval-1", variant, "grading.json"))
		if err != nil {
			t.Fatal(err)
		}
		var grading Grading
		if err := json.Unmarshal(contents, &grading); err != nil {
			t.Fatal(err)
		}
		for _, result := range grading.AssertionResults {
			if result.EvidenceGrounding == "" {
				t.Fatalf("%s grading artifact omits evidence_grounding: %s", variant, contents)
			}
		}
	}

	cached, err := Run(context.Background(), suite, agent, store, config)
	if err != nil {
		t.Fatalf("cached Run() error = %v", err)
	}
	if !cached.Passed || !cached.Cached {
		t.Fatalf("cached report = %#v", cached)
	}
	if agent.runs != 4 { // candidate, baseline, grader, and comparator.
		t.Fatalf("agent runs = %d, want 4", agent.runs)
	}
	for _, request := range agent.requests {
		if len(request.OutputSchema) == 0 && request.Network {
			t.Fatal("required_actions enabled network without --network")
		}
	}
	var benchmark Benchmark
	contents, err := os.ReadFile(filepath.Join(report.Workspace, "benchmark.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(contents, &benchmark); err != nil {
		t.Fatal(err)
	}
	if benchmark.Security.CredentialBoundary != harness.CredentialBoundaryCodexSandbox {
		t.Fatalf("benchmark credential boundary = %q", benchmark.Security.CredentialBoundary)
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

func TestRunCacheIncludesEffectiveSandboxOverride(t *testing.T) {
	t.Setenv("SHUHARI_SANDBOX", "")

	root := filepath.Join(t.TempDir(), "demo")
	mustWrite(t, filepath.Join(root, "SKILL.md"), "---\nname: demo\ndescription: Demo skill\n---\n")
	mustWrite(t, filepath.Join(root, "evals", "evals.json"), `{"skill_name":"demo","evals":[{"id":"one","prompt":"task","expected_output":"candidate output","assertions":["correct"]}]}`)
	suite, err := LoadSkillSuite(root)
	if err != nil {
		t.Fatal(err)
	}
	mapping := blindLabels("one", 1, variantWithSkill, variantWithoutSkill)
	grades := map[string][]AssertionResult{
		variantWithSkill:    {{Text: "correct", Passed: true, Evidence: `Observed "candidate output".`}},
		variantWithoutSkill: {{Text: "correct", Passed: false, Evidence: "missing"}},
	}
	grader, _ := json.Marshal(judgeOutput{Cases: []judgeEntry{{ID: "one", Trial: 1, AAssertionResults: grades[mapping.A], BAssertionResults: grades[mapping.B]}}})
	compared, _ := json.Marshal(comparatorOutput{Cases: []comparatorEntry{{ID: "one", Trial: 1, Preferred: preferredLabel(mapping, variantWithSkill), Reason: "better"}}})
	agent := &fakeHarness{judgeResponse: string(grader), compareResponse: string(compared)}
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
	if report.Cached || agent.runs != 8 {
		t.Fatalf("sandbox override reused stale cache: cached=%v runs=%d", report.Cached, agent.runs)
	}
	contents, err := os.ReadFile(filepath.Join(report.Workspace, "benchmark.json"))
	if err != nil {
		t.Fatal(err)
	}
	var benchmark Benchmark
	if err := json.Unmarshal(contents, &benchmark); err != nil {
		t.Fatal(err)
	}
	if benchmark.Security.CredentialBoundary != harness.CredentialBoundaryNone || benchmark.Security.SandboxMode != "danger-full-access" {
		t.Fatalf("danger benchmark security = %#v", benchmark.Security)
	}
}

func TestRunPersistsExecutionAndMalformedGraderEvidence(t *testing.T) {
	t.Parallel()

	newSuite := func(t *testing.T) Suite {
		root := filepath.Join(t.TempDir(), "demo")
		mustWrite(t, filepath.Join(root, "SKILL.md"), "---\nname: demo\ndescription: Demo skill\n---\n")
		mustWrite(t, filepath.Join(root, "evals", "evals.json"), `{"skill_name":"demo","evals":[{"id":"one","prompt":"task","expected_output":"output"}]}`)
		suite, err := LoadSkillSuite(root)
		if err != nil {
			t.Fatal(err)
		}
		return suite
	}
	for _, test := range []struct {
		name  string
		agent harness.Harness
		file  string
	}{
		{name: "execution", agent: &failingHarness{}, file: "evidence.json"},
		{name: "grader", agent: &fakeHarness{judgeResponse: `{"cases":[`}, file: "grading-error.json"},
	} {
		t.Run(test.name, func(t *testing.T) {
			report, err := Run(context.Background(), newSuite(t), test.agent, cache.Store{Root: filepath.Join(t.TempDir(), "cache")}, Config{Trials: 1, Jobs: 1, Timeout: time.Second, Workspace: filepath.Join(t.TempDir(), "workspace"), NoCache: true})
			if err == nil {
				t.Fatal("Run() unexpectedly succeeded")
			}
			contents, readErr := os.ReadFile(filepath.Join(report.Workspace, test.file))
			if readErr != nil {
				t.Fatalf("failure evidence missing: %v", readErr)
			}
			if !strings.Contains(string(contents), "error") {
				t.Fatalf("failure evidence lacks error: %s", contents)
			}
		})
	}
}

func preferredLabel(mapping blindMapping, variant string) string {
	if mapping.A == variant {
		return "A"
	}
	return "B"
}

func TestBuildRunPromptDoesNotLeakEvaluatorExpectedOutput(t *testing.T) {
	t.Parallel()

	item := Case{Prompt: "perform the task", ExpectedOutput: "SECRET EXPECTED ANSWER"}
	prompt := buildRunPrompt(item, []string{"input.txt"}, "/workspace/outputs", nil)
	if strings.Contains(prompt, item.ExpectedOutput) || strings.Contains(prompt, "Expected output") {
		t.Fatalf("run prompt leaked evaluator-only expected output: %q", prompt)
	}
	if !strings.Contains(prompt, item.Prompt) {
		t.Fatalf("run prompt omitted task: %q", prompt)
	}
}
