package eval

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shunk031/shuhari/internal/harness"
)

type evalHarness struct {
	mu       sync.Mutex
	requests []harness.Request
}

func (h *evalHarness) Probe(context.Context, ...harness.SecurityResolution) (harness.Identity, error) {
	return harness.Identity{Agent: "fake", Version: "1"}, nil
}

func (*evalHarness) Capabilities() harness.Capabilities {
	return harness.Capabilities{Skills: true, Instructions: true, TriggerEvidence: true}
}

func (*evalHarness) ResolveSecurity(_ context.Context, policy harness.SecurityPolicy) (harness.SecurityResolution, error) {
	network := harness.NetworkDenied
	boundary := harness.CredentialBoundaryEnforced
	if policy.Network {
		network = harness.NetworkAllowed
	}
	if policy.Level == harness.SandboxUnsandboxed {
		boundary = harness.CredentialBoundaryNone
	}
	return harness.SecurityResolution{
		SandboxLevel: policy.Level, NetworkAccess: network, CredentialBoundary: boundary,
		Adapter: harness.AdapterSecurity{Name: "fake", NativeMode: "fake-" + string(policy.Level), PolicyDigest: "sha256:" + strings.Repeat("a", 64)},
	}, nil
}

func (h *evalHarness) Run(_ context.Context, request harness.Request) (harness.Result, error) {
	h.mu.Lock()
	h.requests = append(h.requests, request)
	h.mu.Unlock()
	if len(request.OutputSchema) == 0 {
		response := "baseline output"
		if request.Target != nil {
			response = "candidate output"
		}
		return harness.Result{Response: response, Transcript: []byte("{}\n"), Duration: 10 * time.Millisecond, Usage: harness.Usage{InputTokens: 10, OutputTokens: 5}, Attempts: harness.AttemptEvidence{AttemptCount: 1}}, nil
	}
	if strings.Contains(string(request.OutputSchema), `"preferred"`) {
		payload := request.Prompt[strings.LastIndex(request.Prompt, "\n\n")+2:]
		var inputs []comparatorInput
		if err := json.Unmarshal([]byte(payload), &inputs); err != nil {
			return harness.Result{}, err
		}
		input := inputs[0]
		preferred := "B"
		if strings.Contains(input.A, "candidate output") {
			preferred = "A"
		}
		return harness.Result{Response: `{"cases":[{"id":"` + input.ID + `","trial":` + formatInt(input.Trial) + `,"preferred":"` + preferred + `","reason":"candidate output is more useful"}]}`, Duration: time.Millisecond, Attempts: judgeAttempts()}, nil
	}
	payload := request.Prompt[strings.LastIndex(request.Prompt, "\n\n")+2:]
	var inputs []agentJudgeInput
	if err := json.Unmarshal([]byte(payload), &inputs); err != nil {
		return harness.Result{}, err
	}
	input := inputs[0]
	contents, err := os.ReadFile(filepath.Join(request.WorkDir, "response.md"))
	if err != nil {
		return harness.Result{}, err
	}
	passed := strings.Contains(string(contents), "candidate output")
	evidence := "response.md records the judged output"
	result, _ := json.Marshal(AssertionResult{Text: input.Assertions[0], Passed: passed, Evidence: evidence})
	return harness.Result{Response: `{"cases":[{"id":"` + input.ID + `","trial":` + formatInt(input.Trial) + `,"side":"` + input.Side + `","assertion_results":[` + string(result) + `]}]}`, Duration: time.Millisecond, Attempts: judgeAttempts()}, nil
}

func judgeAttempts() harness.AttemptEvidence {
	return harness.AttemptEvidence{AttemptCount: 2, AttemptErrors: []harness.AttemptError{{Attempt: 1, Error: "temporary disconnect", Timestamp: time.Now().UTC(), DurationMS: 1, StdoutBytes: 1, StderrBytes: 1}}}
}

func formatInt(value int) string {
	if value == 1 {
		return "1"
	}
	return "2"
}

func TestRunWritesReferenceShapedArtifactsWithBlindedAgentJudges(t *testing.T) {
	root := filepath.Join(t.TempDir(), "demo")
	mustWrite(t, filepath.Join(root, "SKILL.md"), "---\nname: demo\ndescription: Demo skill\n---\n")
	mustWrite(t, filepath.Join(root, "evals", "evals.json"), `{"skill_name":"demo","evals":[{"id":1,"prompt":"do the task","expected_output":"a correct result","assertions":["The result is correct"]}]}`)
	suite, err := LoadSkillSuite(root)
	if err != nil {
		t.Fatal(err)
	}
	agent := &evalHarness{}
	report, err := Run(context.Background(), suite, agent, Config{Trials: 1, Jobs: 1, Timeout: time.Second, Workspace: filepath.Join(t.TempDir(), "workspace")})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Passed {
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
		filepath.Join(report.Workspace, "judge-retries.json"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("missing artifact %s: %v", path, err)
		}
	}
	contents, err := os.ReadFile(filepath.Join(report.Workspace, "eval-1", "with_skill", "grading.json"))
	if err != nil {
		t.Fatal(err)
	}
	var grading Grading
	if err := json.Unmarshal(contents, &grading); err != nil {
		t.Fatal(err)
	}
	if len(grading.AssertionResults) != 1 || !grading.AssertionResults[0].Passed || grading.AssertionResults[0].Evidence != "response.md records the judged output" {
		t.Fatalf("grading = %#v", grading)
	}
	if strings.Contains(string(contents), "grounding") || strings.Contains(string(contents), "evidence_references") {
		t.Fatalf("grading retained removed evidence fields: %s", contents)
	}
}

func TestAllTrialsPassRequiresAtLeastOneSuccessfulTrial(t *testing.T) {
	if allTrialsPass(nil) || !allTrialsPass([]bool{true, true}) || allTrialsPass([]bool{true, false}) {
		t.Fatal("allTrialsPass() did not enforce non-empty all-true semantics")
	}
}
