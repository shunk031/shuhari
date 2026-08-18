package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shunk031/shuhari/internal/harness"
)

type recordingJudgeHarness struct {
	fakeHarness
	omitGraderKey      string
	omitComparatorKey  string
	invalidGraders     int
	invalidComparators int
	rejectOverBytes    int
	preferredVariants  map[string]string
	transportAttempts  harness.AttemptEvidence
}

func fakeJudgeAttemptError(attempt int, message string) harness.AttemptError {
	return harness.AttemptError{
		Attempt:     attempt,
		Error:       message,
		Timestamp:   time.Date(2026, 8, 17, 12, 0, attempt, 0, time.UTC),
		DurationMS:  int64(attempt),
		StdoutBytes: int64(attempt),
		StderrBytes: int64(attempt),
	}
}

func testJudgeSecurity() harness.SecurityResolution {
	return fakeSecurityResolution(harness.SecurityPolicy{Level: harness.SandboxReadOnly})
}

func (h *recordingJudgeHarness) Run(_ context.Context, request harness.Request) (harness.Result, error) {
	h.mu.Lock()
	h.runs++
	h.requests = append(h.requests, request)
	h.mu.Unlock()
	if h.rejectOverBytes > 0 && len(request.Prompt) > h.rejectOverBytes {
		return harness.Result{}, fmt.Errorf("input_too_large: prompt is %d bytes; limit is %d bytes", len(request.Prompt), h.rejectOverBytes)
	}
	payload := request.Prompt[strings.LastIndex(request.Prompt, "\n\n")+2:]
	if strings.Contains(string(request.OutputSchema), `"preferred"`) {
		var inputs []comparatorInput
		if err := json.Unmarshal([]byte(payload), &inputs); err != nil {
			return harness.Result{}, err
		}
		h.mu.Lock()
		invalid := h.invalidComparators > 0
		if invalid {
			h.invalidComparators--
		}
		h.mu.Unlock()
		if invalid {
			return harness.Result{Response: `{"cases":[]}`}, nil
		}
		output := comparatorOutput{}
		for _, input := range inputs {
			if caseTrialKey(input.ID, input.Trial) == h.omitComparatorKey {
				continue
			}
			preferred := "tie"
			if variant := h.preferredVariants[input.ID]; variant != "" {
				mapping := blindLabels(input.ID, input.Trial, variantWithSkill, variantWithoutSkill)
				preferred = preferredLabel(mapping, variant)
			}
			output.Cases = append(output.Cases, comparatorEntry{ID: input.ID, Trial: input.Trial, Preferred: preferred, Reason: "comparison"})
		}
		encoded, _ := json.Marshal(output)
		return harness.Result{Response: string(encoded), Attempts: h.transportAttempts}, nil
	}
	var inputs []agentJudgeInput
	if err := json.Unmarshal([]byte(payload), &inputs); err != nil {
		return harness.Result{}, err
	}
	h.mu.Lock()
	invalid := h.invalidGraders > 0
	invalidMarker := ""
	if invalid {
		h.invalidGraders--
		invalidMarker = fmt.Sprintf("fabricated-answer-marker-%d", h.invalidGraders)
	}
	h.mu.Unlock()
	output := agentJudgeOutput{}
	for _, input := range inputs {
		if caseTrialKey(input.ID, input.Trial) == h.omitGraderKey {
			continue
		}
		contents, err := os.ReadFile(filepath.Join(request.WorkDir, "response.md"))
		if err != nil {
			return harness.Result{}, err
		}
		observation := strings.TrimSuffix(string(contents), "\n")
		if invalid {
			observation = invalidMarker
		}
		results := make([]AssertionResult, 0, len(input.Assertions))
		for _, assertion := range input.Assertions {
			results = append(results, AssertionResult{
				Text: assertion, Passed: true, Evidence: observation,
				EvidenceReferences: []EvidenceReference{{Path: "response.md", StartLine: 1, EndLine: 1}},
			})
		}
		output.Cases = append(output.Cases, agentJudgeEntry{ID: input.ID, Trial: input.Trial, Side: input.Side, AssertionResults: results})
	}
	encoded, _ := json.Marshal(output)
	return harness.Result{Response: string(encoded), Attempts: h.transportAttempts}, nil
}

func TestGradeRunsPersistsJudgeTransportRetryEvidence(t *testing.T) {
	t.Parallel()

	suite, results := oneTrialJudgeSuite(t)
	iteration := t.TempDir()
	attempts := harness.AttemptEvidence{AttemptCount: 2, AttemptErrors: []harness.AttemptError{fakeJudgeAttemptError(1, "response body decode error")}}
	_, _, _, _, _, err := gradeRuns(context.Background(), &recordingJudgeHarness{transportAttempts: attempts}, suite, results, Config{Trials: 1, Timeout: time.Second}, testJudgeSecurity(), iteration)
	if err != nil {
		t.Fatalf("gradeRuns() error = %v", err)
	}
	contents, err := os.ReadFile(filepath.Join(iteration, "judge-retries.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"stage": "grader"`, `"stage": "comparator"`, `"attempt_count": 2`, "response body decode error"} {
		if !strings.Contains(string(contents), want) {
			t.Fatalf("judge retry artifact lacks %q: %s", want, contents)
		}
	}
}

type exhaustedJudgeTransportHarness struct{ recordingJudgeHarness }

func (h *exhaustedJudgeTransportHarness) Run(_ context.Context, request harness.Request) (harness.Result, error) {
	if len(request.OutputSchema) == 0 {
		return h.recordingJudgeHarness.Run(context.Background(), request)
	}
	attempts := harness.AttemptEvidence{AttemptCount: 3, AttemptErrors: []harness.AttemptError{fakeJudgeAttemptError(1, "disconnect one"), fakeJudgeAttemptError(2, "disconnect two"), fakeJudgeAttemptError(3, "disconnect three")}}
	return harness.Result{}, &harness.RetryError{Cause: fmt.Errorf("%w: disconnect three", harness.ErrTransient), Attempts: attempts}
}

func TestGradeRunsPersistsExhaustedJudgeTransportAttempts(t *testing.T) {
	t.Parallel()

	suite, results := oneTrialJudgeSuite(t)
	iteration := t.TempDir()
	_, _, _, _, _, err := gradeRuns(context.Background(), &exhaustedJudgeTransportHarness{}, suite, results, Config{Trials: 1, Timeout: time.Second}, testJudgeSecurity(), iteration)
	if err == nil || !errors.Is(err, harness.ErrTransient) {
		t.Fatalf("gradeRuns() error = %v, want exhausted transport error", err)
	}
	contents, readErr := os.ReadFile(filepath.Join(iteration, "judge-retries.json"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	for _, want := range []string{`"stage": "grader"`, `"attempt_count": 3`, "disconnect one", "disconnect three"} {
		if !strings.Contains(string(contents), want) {
			t.Fatalf("judge retry artifact lacks %q: %s", want, contents)
		}
	}
}

func TestGradeRunsAllowsMinorityBaselinePreference(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	cases := []Case{{ID: "one", ExpectedOutput: "correct"}, {ID: "two", ExpectedOutput: "correct"}, {ID: "three", ExpectedOutput: "correct"}}
	results := make([]runResult, 0, len(cases)*2)
	for _, item := range cases {
		for _, variant := range []string{variantWithSkill, variantWithoutSkill} {
			runDir := filepath.Join(root, item.ID, variant)
			if err := os.MkdirAll(runDir, 0o755); err != nil {
				t.Fatal(err)
			}
			results = append(results, runResult{Case: item, Trial: 1, Variant: variant, RunDir: runDir, Artifact: variant, Agent: harness.Result{Duration: time.Millisecond}})
		}
	}
	agent := &recordingJudgeHarness{preferredVariants: map[string]string{"one": variantWithSkill, "two": variantWithSkill, "three": variantWithoutSkill}}
	_, candidateWins, baselineWins, reasons, _, err := gradeRuns(context.Background(), agent, Suite{Kind: harness.TargetSkill, Cases: cases}, results, Config{Trials: 1, Timeout: time.Second}, testJudgeSecurity(), root)
	if err != nil {
		t.Fatalf("gradeRuns() error = %v", err)
	}
	if candidateWins != 2 || baselineWins != 1 {
		t.Fatalf("wins = %d/%d, want 2/1", candidateWins, baselineWins)
	}
	if len(reasons) != 0 {
		t.Fatalf("minority baseline preference produced failure reasons: %v", reasons)
	}
}

type rawResponseExclusivityHarness struct{ recordingJudgeHarness }

func (h *rawResponseExclusivityHarness) Run(_ context.Context, request harness.Request) (harness.Result, error) {
	if strings.Contains(string(request.OutputSchema), `"preferred"`) {
		return h.recordingJudgeHarness.Run(context.Background(), request)
	}
	h.mu.Lock()
	h.requests = append(h.requests, request)
	h.mu.Unlock()
	payload := request.Prompt[strings.LastIndex(request.Prompt, "\n\n")+2:]
	var inputs []agentJudgeInput
	if err := json.Unmarshal([]byte(payload), &inputs); err != nil {
		return harness.Result{}, err
	}
	const assertion = "The response returns only the requested three-sentence prose draft."
	output := agentJudgeOutput{Cases: make([]agentJudgeEntry, 0, len(inputs))}
	for _, input := range inputs {
		contents, err := os.ReadFile(filepath.Join(request.WorkDir, "response.md"))
		if err != nil {
			return harness.Result{}, err
		}
		response := strings.TrimSuffix(string(contents), "\n")
		result := AssertionResult{Text: assertion, Passed: response == "one\ntwo\nthree", Evidence: response, EvidenceReferences: []EvidenceReference{{Path: "response.md", StartLine: 1, EndLine: 3}}}
		output.Cases = append(output.Cases, agentJudgeEntry{ID: input.ID, Trial: input.Trial, Side: input.Side, AssertionResults: []AssertionResult{result}})
	}
	encoded, _ := json.Marshal(output)
	return harness.Result{Response: string(encoded)}, nil
}

func TestExclusivityUsesRawResponseNotArtifactFraming(t *testing.T) {
	t.Parallel()

	suite, results := oneTrialJudgeSuite(t)
	const assertion = "The response returns only the requested three-sentence prose draft."
	suite.Cases[0].Assertions = []string{assertion}
	for index := range results {
		results[index].Case = suite.Cases[0]
		if results[index].Variant == variantWithSkill {
			results[index].Agent.Response = "one\ntwo\nthree\n"
		} else {
			results[index].Agent.Response = "one\ntwo\nthree\nextra content\n"
		}
		artifactRoot := filepath.Join(results[index].RunDir, "outputs")
		writeAgentArtifact(t, artifactRoot, results[index].Agent.Response)
		results[index].OutputPath = artifactRoot
		results[index].Artifact = fmt.Sprintf("--- file: response.md (%d bytes) ---\n%s", len(results[index].Agent.Response), results[index].Agent.Response)
	}
	if _, _, _, _, _, err := gradeRuns(context.Background(), &rawResponseExclusivityHarness{}, suite, results, Config{Trials: 1, Timeout: time.Second}, testJudgeSecurity(), t.TempDir()); err != nil {
		t.Fatalf("gradeRuns() error = %v", err)
	}
	for _, result := range results {
		contents, err := os.ReadFile(filepath.Join(result.RunDir, "grading.json"))
		if err != nil {
			t.Fatal(err)
		}
		var grading Grading
		if err := json.Unmarshal(contents, &grading); err != nil {
			t.Fatal(err)
		}
		wantPassed := result.Variant == variantWithSkill
		if got := grading.Summary.Failed == 0; got != wantPassed {
			t.Fatalf("%s grading passed = %v, want %v: %s", result.Variant, got, wantPassed, contents)
		}
	}
}

func TestComparatorInputIncludesOriginalTask(t *testing.T) {
	t.Parallel()

	input := comparatorInput{Prompt: "original task", ExpectedOutput: "expected", Assertions: []string{"check"}}
	encoded, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"original task", "expected", "check"} {
		if !strings.Contains(string(encoded), value) {
			t.Fatalf("comparator input omits %q: %s", value, encoded)
		}
	}
}

func TestJudgeSchemasAreValidJSON(t *testing.T) {
	t.Parallel()

	for name, schema := range map[string][]byte{"grader": graderSchema(), "comparator": comparatorSchema()} {
		if !json.Valid(schema) {
			t.Fatalf("%s schema is not valid JSON: %s", name, schema)
		}
	}
}

func TestGraderSchemaUsesStrictNullableAbsenceField(t *testing.T) {
	t.Parallel()

	var schema struct {
		Definitions map[string]struct {
			Items struct {
				Properties map[string]json.RawMessage `json:"properties"`
				Required   []string                   `json:"required"`
			} `json:"items"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(graderSchema(), &schema); err != nil {
		t.Fatalf("decode grader schema: %v", err)
	}
	assertionResults, ok := schema.Definitions["assertion_results"]
	if !ok {
		t.Fatal("grader schema has no assertion_results definition")
	}
	requiredProperties := make(map[string]bool, len(assertionResults.Items.Required))
	for _, required := range assertionResults.Items.Required {
		requiredProperties[required] = true
	}
	for property := range assertionResults.Items.Properties {
		if !requiredProperties[property] {
			t.Fatalf("grader schema does not require assertion property %q: %v", property, assertionResults.Items.Required)
		}
	}
	var absence struct {
		Type       []string                   `json:"type"`
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(assertionResults.Items.Properties["absence"], &absence); err != nil {
		t.Fatalf("decode absence schema: %v", err)
	}
	if got, want := absence.Type, []string{"object", "null"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("absence type = %v, want %v", got, want)
	}
	for _, property := range []string{"negated_clause", "query", "rationale"} {
		if _, ok := absence.Properties[property]; !ok {
			t.Fatalf("absence schema lacks fallback property %q", property)
		}
	}
}

func TestGradeRunsJudgesEachTrialAndSideSeparately(t *testing.T) {
	t.Parallel()

	suite, results := judgeSuite(t, 2)
	agent := &recordingJudgeHarness{}
	graded, _, _, _, _, err := gradeRuns(context.Background(), agent, suite, results, Config{Trials: 2, Timeout: time.Second}, testJudgeSecurity(), t.TempDir())
	if err != nil {
		t.Fatalf("gradeRuns() error = %v", err)
	}
	if len(graded) != len(results) {
		t.Fatalf("graded runs = %d, want %d", len(graded), len(results))
	}
	seen := map[string]map[string]int{"grader": {}, "comparator": {}}
	for _, request := range agent.requests {
		payload := request.Prompt[strings.LastIndex(request.Prompt, "\n\n")+2:]
		if strings.Contains(string(request.OutputSchema), `"preferred"`) {
			var inputs []comparatorInput
			if err := json.Unmarshal([]byte(payload), &inputs); err != nil {
				t.Fatal(err)
			}
			if len(inputs) != 1 {
				t.Fatalf("comparator prompt trials = %d, want 1", len(inputs))
			}
			seen["comparator"][caseTrialKey(inputs[0].ID, inputs[0].Trial)]++
			continue
		}
		var inputs []agentJudgeInput
		if err := json.Unmarshal([]byte(payload), &inputs); err != nil {
			t.Fatal(err)
		}
		if len(inputs) != 1 || (inputs[0].Side != "A" && inputs[0].Side != "B") {
			t.Fatalf("invalid blinded grader input: %#v", inputs)
		}
		key := caseTrialKey(inputs[0].ID, inputs[0].Trial) + "\x00" + inputs[0].Side
		seen["grader"][key]++
	}
	if len(seen["grader"]) != len(suite.Cases)*2*2 || len(seen["comparator"]) != len(suite.Cases)*2 {
		t.Fatalf("judge coverage = %#v", seen)
	}
	for stage, trials := range seen {
		for key, count := range trials {
			if count != 1 {
				t.Fatalf("%s trial %q judged %d times", stage, key, count)
			}
		}
	}
}

func TestGradeRunsRejectsIncompletePerTrialOutput(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name  string
		agent *recordingJudgeHarness
		stage string
	}{
		{name: "grader", agent: &recordingJudgeHarness{omitGraderKey: caseTrialKey("three", 2)}, stage: `grader case "three" trial 2`},
		{name: "comparator", agent: &recordingJudgeHarness{omitComparatorKey: caseTrialKey("three", 2)}, stage: `comparator case "three" trial 2`},
	} {
		t.Run(test.name, func(t *testing.T) {
			suite, results := judgeSuite(t, 2)
			_, _, _, _, _, err := gradeRuns(context.Background(), test.agent, suite, results, Config{Trials: 2, Timeout: time.Second}, testJudgeSecurity(), t.TempDir())
			if err == nil || !strings.Contains(err.Error(), test.stage) {
				t.Fatalf("gradeRuns() error = %v, want incomplete %s", err, test.stage)
			}
		})
	}
}

func TestGradeRunsRetriesInvalidJudgeResponseOnce(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name  string
		agent *recordingJudgeHarness
	}{
		{name: "grader", agent: &recordingJudgeHarness{invalidGraders: 1}},
		{name: "comparator", agent: &recordingJudgeHarness{invalidComparators: 1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			suite, results := oneTrialJudgeSuite(t)
			graded, _, _, _, _, err := gradeRuns(context.Background(), test.agent, suite, results, Config{Trials: 1, Timeout: time.Second}, testJudgeSecurity(), t.TempDir())
			if err != nil {
				t.Fatalf("gradeRuns() error = %v", err)
			}
			if len(graded) != len(results) {
				t.Fatalf("graded runs = %d, want %d", len(graded), len(results))
			}
			wantCalls := 4
			if len(test.agent.requests) != wantCalls {
				t.Fatalf("judge calls = %d, want %d", len(test.agent.requests), wantCalls)
			}
		})
	}
}

func TestGradeRunsAbortsAfterSecondInvalidJudgeResponse(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name  string
		agent *recordingJudgeHarness
		stage string
	}{
		{name: "grader", agent: &recordingJudgeHarness{invalidGraders: 2}, stage: "grader"},
		{name: "comparator", agent: &recordingJudgeHarness{invalidComparators: 2}, stage: "comparator"},
	} {
		t.Run(test.name, func(t *testing.T) {
			suite, results := oneTrialJudgeSuite(t)
			_, _, _, _, _, err := gradeRuns(context.Background(), test.agent, suite, results, Config{Trials: 1, Timeout: time.Second}, testJudgeSecurity(), t.TempDir())
			if err == nil || !strings.Contains(err.Error(), test.stage) {
				t.Fatalf("gradeRuns() error = %v, want %s validation error", err, test.stage)
			}
			wantCalls := 2
			if test.stage == "comparator" {
				wantCalls = 4
			}
			if len(test.agent.requests) != wantCalls {
				t.Fatalf("judge calls = %d, want %d", len(test.agent.requests), wantCalls)
			}
		})
	}
}

func TestGradingErrorRetainsAllValidationRetryResponses(t *testing.T) {
	t.Parallel()

	suite, results := oneTrialJudgeSuite(t)
	iteration := t.TempDir()
	_, _, _, _, _, err := gradeRuns(context.Background(), &recordingJudgeHarness{invalidGraders: 2}, suite, results, Config{Trials: 1, Timeout: time.Second}, testJudgeSecurity(), iteration)
	if err == nil {
		t.Fatal("gradeRuns() accepted two invalid grader responses")
	}
	contents, err := os.ReadFile(filepath.Join(iteration, "grading-error.json"))
	if err != nil {
		t.Fatal(err)
	}
	var artifact struct {
		JudgeResponses []string `json:"judge_responses"`
	}
	if err := json.Unmarshal(contents, &artifact); err != nil {
		t.Fatal(err)
	}
	if len(artifact.JudgeResponses) != 2 {
		t.Fatalf("judge responses = %d, want both validation attempts; artifact=%s", len(artifact.JudgeResponses), contents)
	}
	for index, marker := range []string{"fabricated-answer-marker-1", "fabricated-answer-marker-0"} {
		if !strings.Contains(artifact.JudgeResponses[index], marker) {
			t.Fatalf("judge response %d lacks %q: %s", index+1, marker, artifact.JudgeResponses[index])
		}
	}
}

func TestJudgeValidationRetryPromptContainsOnlyContractError(t *testing.T) {
	t.Parallel()

	suite, results := oneTrialJudgeSuite(t)
	agent := &recordingJudgeHarness{invalidGraders: 1}
	if _, _, _, _, _, err := gradeRuns(context.Background(), agent, suite, results, Config{Trials: 1, Timeout: time.Second}, testJudgeSecurity(), t.TempDir()); err != nil {
		t.Fatalf("gradeRuns() error = %v", err)
	}
	retryPrompt := agent.requests[1].Prompt
	if !strings.Contains(strings.ToLower(retryPrompt), "validation feedback") || !strings.Contains(retryPrompt, "does not equal the cited artifact span") {
		t.Fatalf("retry prompt lacks contract error: %s", retryPrompt)
	}
	for _, forbidden := range []string{"fabricated-answer-marker", `"passed":true`, `"preferred":"A"`} {
		if strings.Contains(retryPrompt, forbidden) {
			t.Fatalf("retry prompt contains assertion answer %q", forbidden)
		}
	}
}

func oneTrialJudgeSuite(t *testing.T) (Suite, []runResult) {
	t.Helper()
	item := Case{ID: "one", Assertions: []string{"correct"}}
	results := make([]runResult, 0, 2)
	for _, variant := range []string{variantWithSkill, variantWithoutSkill} {
		runDir := filepath.Join(t.TempDir(), variant)
		if err := os.MkdirAll(runDir, 0o755); err != nil {
			t.Fatal(err)
		}
		results = append(results, runResult{Case: item, Trial: 1, Variant: variant, RunDir: runDir, Artifact: variant})
	}
	return Suite{Kind: harness.TargetSkill, Cases: []Case{item}}, results
}

func judgeSuite(t *testing.T, trials int) (Suite, []runResult) {
	t.Helper()
	cases := []Case{{ID: "one", Assertions: []string{"correct"}}, {ID: "two", Assertions: []string{"correct"}}, {ID: "three", Assertions: []string{"correct"}}}
	results := make([]runResult, 0, len(cases)*trials*2)
	for _, item := range cases {
		for trial := 1; trial <= trials; trial++ {
			for _, variant := range []string{variantWithSkill, variantWithoutSkill} {
				runDir := filepath.Join(t.TempDir(), item.ID, variant)
				if err := os.MkdirAll(runDir, 0o755); err != nil {
					t.Fatal(err)
				}
				artifact := fmt.Sprintf("%s-%s-%d", variant, item.ID, trial)
				results = append(results, runResult{Case: item, Trial: trial, Variant: variant, RunDir: runDir, Artifact: artifact})
			}
		}
	}
	return Suite{Kind: harness.TargetSkill, Cases: cases}, results
}
