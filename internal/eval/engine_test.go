package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	runAttempts     harness.AttemptEvidence
	resolvePolicies []harness.SecurityPolicy
}

type limitedHarness struct {
	fakeHarness
	capabilities harness.Capabilities
}

type failingHarness struct{ fakeHarness }

type unavailableHarness struct{ fakeHarness }

func fakeAttemptError(attempt int, message string) harness.AttemptError {
	return harness.AttemptError{
		Attempt:     attempt,
		Error:       message,
		Timestamp:   time.Date(2026, 8, 17, 12, 0, attempt, 0, time.UTC),
		DurationMS:  int64(attempt),
		StdoutBytes: int64(attempt),
		StderrBytes: int64(attempt),
	}
}

func (*unavailableHarness) Probe(_ context.Context, securities ...harness.SecurityResolution) (harness.Identity, error) {
	return harness.Identity{}, &harness.UnsupportedSecurityPolicyError{
		Adapter: "fake",
		Policy:  securities[0].Policy(),
		Reason:  "native sandbox preflight failed",
	}
}

func (h *failingHarness) Run(_ context.Context, request harness.Request) (harness.Result, error) {
	if len(request.OutputSchema) == 0 {
		return harness.Result{}, errors.New("deliberate execution failure")
	}
	return h.fakeHarness.Run(context.Background(), request)
}

func (h *limitedHarness) Capabilities() harness.Capabilities { return h.capabilities }

func (*fakeHarness) Probe(context.Context, ...harness.SecurityResolution) (harness.Identity, error) {
	return harness.Identity{Agent: "fake", Version: "1"}, nil
}

func (*fakeHarness) Capabilities() harness.Capabilities {
	return harness.Capabilities{Skills: true, Instructions: true, TriggerEvidence: true}
}

func (h *fakeHarness) ResolveSecurity(_ context.Context, policy harness.SecurityPolicy) (harness.SecurityResolution, error) {
	h.mu.Lock()
	h.resolvePolicies = append(h.resolvePolicies, policy)
	h.mu.Unlock()
	return fakeSecurityResolution(policy), nil
}

func fakeSecurityResolution(policy harness.SecurityPolicy) harness.SecurityResolution {
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
			PolicyDigest: "sha256:" + strings.Repeat("a", 64),
		},
	}
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
	return harness.Result{Response: response, Transcript: []byte("{}\n"), Duration: 10 * time.Millisecond, Usage: harness.Usage{InputTokens: 10, OutputTokens: 5}, Attempts: h.runAttempts, Actions: []harness.Action{harness.ActionWebSearch, harness.ActionFileChange}}, nil
}

func TestExecuteTaskPersistsTransportRetryEvidence(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "SKILL.md"), "---\nname: demo\ndescription: Demo skill\n---\n")
	suite := Suite{Kind: harness.TargetSkill, Name: "demo", Root: root, TargetPath: root}
	iteration := t.TempDir()
	attempts := harness.AttemptEvidence{AttemptCount: 2, AttemptErrors: []harness.AttemptError{fakeAttemptError(1, "stream disconnected before completion")}}
	security := fakeSecurityResolution(harness.SecurityPolicy{Level: harness.SandboxIsolated})
	_, err := executeTask(context.Background(), suite, &fakeHarness{runAttempts: attempts}, Config{Timeout: time.Second}, security, iteration, runTask{Case: Case{ID: "one", Prompt: "task"}, Trial: 1, Variant: variantWithSkill})
	if err != nil {
		t.Fatalf("executeTask() error = %v", err)
	}
	contents, err := os.ReadFile(filepath.Join(iteration, "eval-one", variantWithSkill, "timing.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), `"attempt_count": 2`) || !strings.Contains(string(contents), "stream disconnected before completion") {
		t.Fatalf("timing artifact lacks retry evidence: %s", contents)
	}
}

func TestExecuteTaskRetriesProductionTransportAfterInterimMessage(t *testing.T) {
	t.Parallel()

	capture := t.TempDir()
	script := filepath.Join(t.TempDir(), "fake-codex")
	contents := fmt.Sprintf(`#!/bin/sh
count_file=%q/count
count=0
if test -f "$count_file"; then count=$(tr -d '\n' < "$count_file"); fi
count=$((count + 1))
printf '%%s\n' "$count" > "$count_file"
if test "$count" = 1; then
	printf '%%s\n' '{"type":"item.completed","item":{"id":"progress","type":"agent_message","text":"Working on the task."}}'
	printf '%%s\n' '{"type":"error","message":"Reconnecting... 1/5 (stream disconnected before completion: Transport error: network error: error decoding response body)"}'
	exit 0
fi
printf '%%s\n' '{"type":"item.completed","item":{"id":"answer","type":"agent_message","text":"recovered"}}'
printf '%%s\n' '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'
`, capture)
	if err := os.WriteFile(script, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
	agent, err := harness.New("codex", harness.Config{Executable: script})
	if err != nil {
		t.Fatal(err)
	}
	security, err := agent.ResolveSecurity(context.Background(), harness.SecurityPolicy{Level: harness.SandboxIsolated})
	if err != nil {
		t.Fatal(err)
	}
	iteration := t.TempDir()
	_, err = executeTask(context.Background(), Suite{Kind: harness.TargetSkill, Name: "demo"}, agent, Config{Timeout: 2 * time.Second}, security, iteration, runTask{Case: Case{ID: "one", Prompt: "task"}, Trial: 1, Variant: variantWithoutSkill})
	if err != nil {
		t.Fatalf("executeTask() error = %v", err)
	}
	contentsJSON, err := os.ReadFile(filepath.Join(iteration, "eval-one", variantWithoutSkill, "timing.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"attempt_count": 2`, "stream disconnected before completion", "error decoding response body"} {
		if !strings.Contains(string(contentsJSON), want) {
			t.Fatalf("timing artifact lacks %q: %s", want, contentsJSON)
		}
	}
	count, err := os.ReadFile(filepath.Join(capture, "count"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(count)); got != "2" {
		t.Fatalf("Codex invocations = %q, want 2", got)
	}
}

type exhaustedRetryHarness struct{ fakeHarness }

func (h *exhaustedRetryHarness) Run(_ context.Context, request harness.Request) (harness.Result, error) {
	if len(request.OutputSchema) > 0 {
		return h.fakeHarness.Run(context.Background(), request)
	}
	attempts := harness.AttemptEvidence{AttemptCount: 3, AttemptErrors: []harness.AttemptError{fakeAttemptError(1, "disconnect one"), fakeAttemptError(2, "disconnect two"), fakeAttemptError(3, "disconnect three")}}
	return harness.Result{}, &harness.RetryError{Cause: fmt.Errorf("%w: disconnect three", harness.ErrTransient), Attempts: attempts}
}

func TestExecuteTaskPersistsExhaustedTransportAttempts(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "SKILL.md"), "---\nname: demo\ndescription: Demo skill\n---\n")
	iteration := t.TempDir()
	security := fakeSecurityResolution(harness.SecurityPolicy{Level: harness.SandboxIsolated})
	_, err := executeTask(context.Background(), Suite{Kind: harness.TargetSkill, Name: "demo", Root: root, TargetPath: root}, &exhaustedRetryHarness{}, Config{Timeout: time.Second}, security, iteration, runTask{Case: Case{ID: "one", Prompt: "task"}, Trial: 1, Variant: variantWithSkill})
	if err == nil || !errors.Is(err, harness.ErrTransient) {
		t.Fatalf("executeTask() error = %v, want transport failure", err)
	}
	contents, readErr := os.ReadFile(filepath.Join(iteration, "eval-one", variantWithSkill, "timing.json"))
	if readErr != nil {
		t.Fatalf("retry evidence missing: %v", readErr)
	}
	if !strings.Contains(string(contents), `"duration_ms": 6`) || !strings.Contains(string(contents), `"attempt_count": 3`) || !strings.Contains(string(contents), "disconnect one") || !strings.Contains(string(contents), "disconnect three") {
		t.Fatalf("timing artifact lacks exhausted attempts: %s", contents)
	}
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
	if len(agent.resolvePolicies) != 2 {
		t.Fatalf("ResolveSecurity calls = %d, want one run and one judge resolution", len(agent.resolvePolicies))
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
			if result.Passed && result.EvidenceGroundingObservation == "" {
				t.Fatalf("%s grading artifact omits evidence_grounding_observation: %s", variant, contents)
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
	if len(agent.resolvePolicies) != 4 {
		t.Fatalf("ResolveSecurity calls after cache lookup = %d, want two per Run call", len(agent.resolvePolicies))
	}
	for _, request := range agent.requests {
		if len(request.OutputSchema) == 0 && request.Security.NetworkAccess == harness.NetworkAllowed {
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
	if benchmark.Security.SandboxLevel != harness.SandboxIsolated || benchmark.Security.CredentialBoundary != harness.CredentialBoundaryEnforced {
		t.Fatalf("benchmark credential boundary = %q", benchmark.Security.CredentialBoundary)
	}
	if benchmark.SchemaVersion != "2" {
		t.Fatalf("benchmark schema version = %q, want 2", benchmark.SchemaVersion)
	}
	for _, request := range agent.requests {
		want := benchmark.Security
		if len(request.OutputSchema) > 0 {
			want = fakeSecurityResolution(harness.SecurityPolicy{Level: harness.SandboxReadOnly})
		}
		if request.Security != want {
			t.Fatalf("request security = %#v, want exact resolved value %#v", request.Security, want)
		}
	}
	manifestContents, err := os.ReadFile(filepath.Join(report.Workspace, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		SchemaVersion string                     `json:"schema_version"`
		Security      harness.SecurityResolution `json:"security"`
		JudgeSecurity harness.SecurityResolution `json:"judge_security"`
	}
	if err := json.Unmarshal(manifestContents, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.SchemaVersion != "2" || manifest.Security != benchmark.Security {
		t.Fatalf("manifest security = %#v, benchmark security = %#v", manifest.Security, benchmark.Security)
	}
	if manifest.JudgeSecurity != fakeSecurityResolution(harness.SecurityPolicy{Level: harness.SandboxReadOnly}) {
		t.Fatalf("manifest judge security = %#v", manifest.JudgeSecurity)
	}
}

func TestRunRejectsUnavailableSandboxBeforeWritingWorkspace(t *testing.T) {
	t.Parallel()

	agent := &unavailableHarness{}
	report, err := Run(context.Background(), Suite{Kind: harness.TargetSkill}, agent, cache.Store{Root: t.TempDir()}, Config{Trials: 1, Jobs: 1, Timeout: time.Second})
	if err == nil || !errors.Is(err, harness.ErrUnsupportedSecurityPolicy) {
		t.Fatalf("Run() error = %v, want ErrUnsupportedSecurityPolicy", err)
	}
	if report.Workspace != "" {
		t.Fatalf("sandbox preflight failure wrote workspace %q", report.Workspace)
	}
}

func TestRunDoesNotRetryCompletedAssertionFailure(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "demo")
	mustWrite(t, filepath.Join(root, "SKILL.md"), "---\nname: demo\ndescription: Demo skill\n---\n")
	mustWrite(t, filepath.Join(root, "evals", "evals.json"), `{"skill_name":"demo","evals":[{"id":"one","prompt":"task","expected_output":"correct","assertions":["correct"]}]}`)
	suite, err := LoadSkillSuite(root)
	if err != nil {
		t.Fatal(err)
	}
	mapping := blindLabels("one", 1, variantWithSkill, variantWithoutSkill)
	failed := []AssertionResult{{Text: "correct", Passed: false, Evidence: "The expected result is absent."}}
	grades := map[string][]AssertionResult{variantWithSkill: failed, variantWithoutSkill: failed}
	grader, _ := json.Marshal(judgeOutput{Cases: []judgeEntry{{ID: "one", Trial: 1, AAssertionResults: grades[mapping.A], BAssertionResults: grades[mapping.B]}}})
	compared, _ := json.Marshal(comparatorOutput{Cases: []comparatorEntry{{ID: "one", Trial: 1, Preferred: "tie", Reason: "both fail"}}})
	agent := &fakeHarness{judgeResponse: string(grader), compareResponse: string(compared)}
	report, err := Run(context.Background(), suite, agent, cache.Store{Root: t.TempDir()}, Config{Trials: 1, Jobs: 1, Timeout: time.Second, Workspace: filepath.Join(t.TempDir(), "workspace"), NoCache: true})
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed {
		t.Fatal("assertion-failing completed runs passed")
	}
	executionCalls, judgeCalls := 0, 0
	for _, request := range agent.requests {
		if len(request.OutputSchema) == 0 {
			executionCalls++
		} else {
			judgeCalls++
		}
	}
	if executionCalls != 2 {
		t.Fatalf("completed candidate/baseline execution calls = %d, want exactly two", executionCalls)
	}
	if judgeCalls != 2 {
		t.Fatalf("completed grader/comparator calls = %d, want exactly two", judgeCalls)
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

func TestRunCacheIncludesSandboxLevel(t *testing.T) {
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
	config := Config{Trials: 1, Jobs: 1, Timeout: time.Second, Workspace: filepath.Join(t.TempDir(), "workspace"), SandboxLevel: "isolated"}
	if _, err := Run(context.Background(), suite, agent, store, config); err != nil {
		t.Fatal(err)
	}
	config.SandboxLevel = "read-only"
	report, err := Run(context.Background(), suite, agent, store, config)
	if err != nil {
		t.Fatal(err)
	}
	if report.Cached || agent.runs != 8 {
		t.Fatalf("sandbox change reused stale cache: cached=%v runs=%d", report.Cached, agent.runs)
	}
	contents, err := os.ReadFile(filepath.Join(report.Workspace, "benchmark.json"))
	if err != nil {
		t.Fatal(err)
	}
	var benchmark Benchmark
	if err := json.Unmarshal(contents, &benchmark); err != nil {
		t.Fatal(err)
	}
	if benchmark.Security.CredentialBoundary != harness.CredentialBoundaryEnforced || benchmark.Security.SandboxLevel != harness.SandboxReadOnly {
		t.Fatalf("read-only benchmark security = %#v", benchmark.Security)
	}
}

type invalidSecurityHarness struct{ fakeHarness }

func (*invalidSecurityHarness) ResolveSecurity(_ context.Context, policy harness.SecurityPolicy) (harness.SecurityResolution, error) {
	resolution := fakeSecurityResolution(policy)
	resolution.Adapter.PolicyDigest = "x"
	return resolution, nil
}

func TestRunRejectsInvalidResolvedSecurityBeforeWritingArtifacts(t *testing.T) {
	t.Parallel()

	agent := &invalidSecurityHarness{}
	report, err := Run(context.Background(), Suite{Kind: harness.TargetSkill}, agent, cache.Store{Root: t.TempDir()}, Config{Trials: 1, Jobs: 1, Timeout: time.Second})
	if err == nil || !strings.Contains(err.Error(), "security resolution") {
		t.Fatalf("Run() error = %v, want invalid security resolution", err)
	}
	if report.Workspace != "" {
		t.Fatalf("invalid security resolution wrote artifact workspace %q", report.Workspace)
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

func TestRunUsesUnsandboxedSecurityForJudgesWithoutResolvingAgain(t *testing.T) {
	t.Setenv(harness.NoCredentialBoundaryAcknowledgementEnv, "1")

	root := filepath.Join(t.TempDir(), "demo")
	mustWrite(t, filepath.Join(root, "SKILL.md"), "---\nname: demo\ndescription: Demo skill\n---\n")
	mustWrite(t, filepath.Join(root, "evals", "evals.json"), `{"skill_name":"demo","evals":[{"id":"one","prompt":"task","expected_output":"output"}]}`)
	suite, err := LoadSkillSuite(root)
	if err != nil {
		t.Fatal(err)
	}
	mapping := blindLabels("one", 1, variantWithSkill, variantWithoutSkill)
	grades := []AssertionResult{{Text: "output", Passed: true, Evidence: `Observed "output".`}}
	grader, _ := json.Marshal(judgeOutput{Cases: []judgeEntry{{ID: "one", Trial: 1, AAssertionResults: grades, BAssertionResults: grades}}})
	compared, _ := json.Marshal(comparatorOutput{Cases: []comparatorEntry{{ID: "one", Trial: 1, Preferred: preferredLabel(mapping, variantWithSkill), Reason: "better"}}})
	agent := &fakeHarness{judgeResponse: string(grader), compareResponse: string(compared)}
	_, err = Run(context.Background(), suite, agent, cache.Store{Root: t.TempDir()}, Config{
		Trials: 1, Jobs: 1, Timeout: time.Second, Workspace: filepath.Join(t.TempDir(), "workspace"),
		SandboxLevel: string(harness.SandboxUnsandboxed), Network: true, NoCache: true,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	want := []harness.SecurityPolicy{
		{Level: harness.SandboxUnsandboxed, Network: true},
	}
	if !equalSecurityPolicies(agent.resolvePolicies, want) {
		t.Fatalf("ResolveSecurity policies = %#v, want %#v", agent.resolvePolicies, want)
	}
	for _, request := range agent.requests {
		if request.Security.Policy() != want[0] {
			t.Fatalf("request security policy = %#v, want %#v", request.Security.Policy(), want[0])
		}
	}
}

func equalSecurityPolicies(left, right []harness.SecurityPolicy) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
