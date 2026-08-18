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

func TestAgentGradingRequiresPositionalVerbatimEvidence(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	const assertion = "The response verifies push permission before writing."
	const artifactLine = "gh api repos/creative-graphic-design/design-generators --jq .permissions.push"
	writeAgentArtifact(t, root, artifactLine+"\n")

	for _, test := range []struct {
		name     string
		evidence string
		wantErr  bool
	}{
		{
			name:     "iteration 29 renamed variable is rejected",
			evidence: "gh api repos/$REPO_SLUG --jq .permissions.push",
			wantErr:  true,
		},
		{
			name:     "verbatim artifact line passes",
			evidence: artifactLine,
		},
		{
			name:     "iteration 31 generic evidence is rejected",
			evidence: "the requested permission check was performed",
			wantErr:  true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			grading, err := buildAgentGrading(
				[]string{assertion},
				[]AssertionResult{{
					Text:     assertion,
					Passed:   true,
					Evidence: test.evidence,
					EvidenceReferences: []EvidenceReference{{
						Path: "response.md", StartLine: 1, EndLine: 1,
					}},
				}},
				root,
				"response.md",
			)
			if test.wantErr {
				if err == nil {
					t.Fatal("buildAgentGrading() accepted non-verbatim evidence")
				}
				return
			}
			if err != nil {
				t.Fatalf("buildAgentGrading() rejected positional evidence: %v", err)
			}
			if got := grading.AssertionResults[0].EvidenceGrounding; got != evidenceGroundingStrong {
				t.Fatalf("grounding = %q, want %q", got, evidenceGroundingStrong)
			}
		})
	}
}

func TestAgentGradingRejectsNonexistentEvidenceSpan(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeAgentArtifact(t, root, "actual evidence\n")
	const assertion = "the artifact contains the required evidence"
	_, err := buildAgentGrading(
		[]string{assertion},
		[]AssertionResult{{
			Text:     assertion,
			Passed:   true,
			Evidence: "actual evidence",
			EvidenceReferences: []EvidenceReference{{
				Path: "response.md", StartLine: 99, EndLine: 99,
			}},
		}},
		root,
		"response.md",
	)
	if err == nil {
		t.Fatal("buildAgentGrading() accepted a nonexistent evidence span")
	}
}

func TestAgentJudgeRetryPromptCarriesPositionalValidationFeedback(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	const (
		assertion = "The response verifies push permission before writing."
		artifact  = "gh api repos/creative-graphic-design/design-generators --jq .permissions.push"
	)
	writeAgentArtifact(t, root, artifact+"\n")
	agent := &retryAgentJudgeHarness{artifact: artifact}
	input := trialJudgeInputs{
		ID: "case-a", Trial: 1, AOutputPath: root, BOutputPath: root,
		Grader: judgeInput{ID: "case-a", Trial: 1, Assertions: []string{assertion}, A: "secret-A", B: "secret-B"},
	}
	if _, _, _, err := runGradersPerTrial(context.Background(), agent, []trialJudgeInputs{input}, Config{Timeout: time.Second}, testJudgeSecurity()); err != nil {
		t.Fatalf("runGradersPerTrial() rejected corrected agent retry: %v", err)
	}
	if len(agent.requests) != 3 {
		t.Fatalf("judge calls = %d, want A initial/retry plus B", len(agent.requests))
	}
	retryPrompt := agent.requests[1].Prompt
	for _, want := range []string{"Validation feedback", "quote-not-found", "copy the cited lines exactly", "read the files yourself"} {
		if !strings.Contains(strings.ToLower(retryPrompt), strings.ToLower(want)) {
			t.Fatalf("retry prompt lacks %q: %s", want, retryPrompt)
		}
	}
	if strings.Contains(retryPrompt, "secret-A") || strings.Contains(retryPrompt, "secret-B") || strings.Contains(retryPrompt, artifact) {
		t.Fatalf("retry prompt rendered an artifact or side marker: %s", retryPrompt)
	}
}

func TestAgentJudgeKeepsBlindedArtifactsOutOfPrompt(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	item := Case{ID: "blind", Assertions: []string{"the response contains its side marker"}}
	results := make([]runResult, 0, 2)
	markers := map[string]string{variantWithSkill: "candidate-only-marker", variantWithoutSkill: "baseline-only-marker"}
	for _, variant := range []string{variantWithSkill, variantWithoutSkill} {
		artifactRoot := filepath.Join(root, variant, "outputs")
		writeAgentArtifact(t, artifactRoot, markers[variant]+"\n")
		runDir := filepath.Dir(artifactRoot)
		results = append(results, runResult{
			Case: item, Trial: 1, Variant: variant, RunDir: runDir,
			OutputPath: artifactRoot, Artifact: markers[variant],
		})
	}

	agent := &blindAgentJudgeHarness{}
	judgeSecurity := fakeSecurityResolution(harness.SecurityPolicy{Level: harness.SandboxUnsandboxed, Network: true})
	_, _, _, _, _, err := gradeRuns(
		context.Background(), agent,
		Suite{Kind: harness.TargetSkill, Cases: []Case{item}},
		results,
		Config{Trials: 1, Timeout: time.Second},
		judgeSecurity,
		filepath.Join(root, "iteration-1"),
	)
	if err != nil {
		t.Fatalf("gradeRuns() error = %v", err)
	}

	agent.mu.Lock()
	requests := append([]harness.Request(nil), agent.requests...)
	snapshots := append([]judgeWorkspaceSnapshot(nil), agent.snapshots...)
	agent.mu.Unlock()
	var graderRequests int
	var snapshotIndex int
	for _, request := range requests {
		if !strings.Contains(string(request.OutputSchema), "evidence_references") {
			continue
		}
		if request.Security != judgeSecurity {
			t.Fatalf("grader request security = %#v, want unsandboxed resolution %#v", request.Security, judgeSecurity)
		}
		graderRequests++
		prompt := request.Prompt
		if strings.Contains(prompt, markers[variantWithSkill]) || strings.Contains(prompt, markers[variantWithoutSkill]) {
			t.Fatalf("grader prompt rendered an artifact marker: %s", prompt)
		}
		if snapshotIndex >= len(snapshots) {
			t.Fatal("judge workspace snapshot missing")
		}
		snapshot := snapshots[snapshotIndex]
		snapshotIndex++
		if len(snapshot.Entries) != 1 || snapshot.Entries[0] != "response.md" {
			t.Fatalf("judge workspace entries = %#v, want only response.md", snapshot.Entries)
		}
		if snapshot.Files["response.md"] != markers[variantWithSkill]+"\n" && snapshot.Files["response.md"] != markers[variantWithoutSkill]+"\n" {
			t.Fatalf("judge workspace exposed unexpected artifact: %q", snapshot.Files["response.md"])
		}
	}
	if graderRequests != 2 {
		t.Fatalf("grader requests = %d, want one blinded request per side", graderRequests)
	}
}

type blindAgentJudgeHarness struct {
	mu        sync.Mutex
	requests  []harness.Request
	snapshots []judgeWorkspaceSnapshot
}

type retryAgentJudgeHarness struct {
	mu       sync.Mutex
	requests []harness.Request
	artifact string
	calls    int
}

func (*retryAgentJudgeHarness) Probe(context.Context, ...harness.SecurityResolution) (harness.Identity, error) {
	return harness.Identity{Agent: "test", Version: "test"}, nil
}

func (*retryAgentJudgeHarness) Capabilities() harness.Capabilities {
	return harness.Capabilities{Skills: true, Instructions: true}
}

func (*retryAgentJudgeHarness) ResolveSecurity(_ context.Context, policy harness.SecurityPolicy) (harness.SecurityResolution, error) {
	return fakeSecurityResolution(policy), nil
}

func (h *retryAgentJudgeHarness) Run(_ context.Context, request harness.Request) (harness.Result, error) {
	h.mu.Lock()
	h.requests = append(h.requests, request)
	h.calls++
	call := h.calls
	h.mu.Unlock()
	var inputs []agentJudgeInput
	payload := request.Prompt[strings.LastIndex(request.Prompt, "\n\n")+2:]
	if err := json.Unmarshal([]byte(payload), &inputs); err != nil {
		return harness.Result{}, err
	}
	if len(inputs) != 1 {
		return harness.Result{}, &harness.RetryError{Cause: harness.ErrTransient, Attempts: harness.AttemptEvidence{AttemptCount: 1}}
	}
	evidence := h.artifact
	if call == 1 {
		evidence = "gh api repos/$REPO_SLUG --jq .permissions.push"
	}
	output, err := json.Marshal(agentJudgeOutput{Cases: []agentJudgeEntry{{
		ID: inputs[0].ID, Trial: inputs[0].Trial, Side: inputs[0].Side,
		AssertionResults: []AssertionResult{{Text: inputs[0].Assertions[0], Passed: true, Evidence: evidence, EvidenceReferences: []EvidenceReference{{Path: "response.md", StartLine: 1, EndLine: 1}}}},
	}}})
	if err != nil {
		return harness.Result{}, err
	}
	return harness.Result{Response: string(output)}, nil
}

type judgeWorkspaceSnapshot struct {
	Entries []string
	Files   map[string]string
}

func (h *blindAgentJudgeHarness) Probe(context.Context, ...harness.SecurityResolution) (harness.Identity, error) {
	return harness.Identity{Agent: "test", Version: "test"}, nil
}

func (*blindAgentJudgeHarness) Capabilities() harness.Capabilities {
	return harness.Capabilities{Skills: true, Instructions: true}
}

func (*blindAgentJudgeHarness) ResolveSecurity(_ context.Context, policy harness.SecurityPolicy) (harness.SecurityResolution, error) {
	return fakeSecurityResolution(policy), nil
}

func (h *blindAgentJudgeHarness) Run(_ context.Context, request harness.Request) (harness.Result, error) {
	h.mu.Lock()
	h.requests = append(h.requests, request)
	if !strings.Contains(string(request.OutputSchema), "preferred") {
		entries, err := os.ReadDir(request.WorkDir)
		if err != nil {
			h.mu.Unlock()
			return harness.Result{}, err
		}
		snapshot := judgeWorkspaceSnapshot{Files: map[string]string{}}
		for _, entry := range entries {
			snapshot.Entries = append(snapshot.Entries, entry.Name())
			if entry.IsDir() {
				continue
			}
			contents, err := os.ReadFile(filepath.Join(request.WorkDir, entry.Name()))
			if err != nil {
				h.mu.Unlock()
				return harness.Result{}, err
			}
			snapshot.Files[entry.Name()] = string(contents)
		}
		h.snapshots = append(h.snapshots, snapshot)
	}
	h.mu.Unlock()
	if strings.Contains(string(request.OutputSchema), "preferred") {
		return harness.Result{Response: `{"cases":[{"id":"blind","trial":1,"preferred":"tie","reason":"comparison"}]}`}, nil
	}
	var inputs []agentJudgeInput
	payload := request.Prompt[strings.LastIndex(request.Prompt, "\n\n")+2:]
	if err := json.Unmarshal([]byte(payload), &inputs); err != nil {
		return harness.Result{}, err
	}
	if len(inputs) != 1 {
		return harness.Result{}, &harness.RetryError{Cause: harness.ErrTransient, Attempts: harness.AttemptEvidence{AttemptCount: 1}}
	}
	contents, err := os.ReadFile(filepath.Join(request.WorkDir, "response.md"))
	if err != nil {
		return harness.Result{}, err
	}
	assertion := inputs[0].Assertions[0]
	output, err := json.Marshal(agentJudgeOutput{Cases: []agentJudgeEntry{{
		ID: inputs[0].ID, Trial: inputs[0].Trial, Side: inputs[0].Side,
		AssertionResults: []AssertionResult{{
			Text: assertion, Passed: true, Evidence: strings.TrimSuffix(string(contents), "\n"),
			EvidenceReferences: []EvidenceReference{{Path: "response.md", StartLine: 1, EndLine: 1}},
		}},
	}}})
	if err != nil {
		return harness.Result{}, err
	}
	return harness.Result{Response: string(output)}, nil
}

func writeAgentArtifact(t *testing.T, root, contents string) {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "response.md"), []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
