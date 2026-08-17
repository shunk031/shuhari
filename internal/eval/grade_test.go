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

type provenanceRetryHarness struct {
	recordingJudgeHarness
	responses []string
	next      int
}

type rawResponseExclusivityHarness struct {
	recordingJudgeHarness
}

func (h *rawResponseExclusivityHarness) Run(_ context.Context, request harness.Request) (harness.Result, error) {
	if strings.Contains(string(request.OutputSchema), `"preferred"`) {
		return h.recordingJudgeHarness.Run(context.Background(), request)
	}
	h.mu.Lock()
	h.requests = append(h.requests, request)
	h.mu.Unlock()
	payload := request.Prompt[strings.LastIndex(request.Prompt, "\n\n")+2:]
	var inputs []judgeInput
	if err := json.Unmarshal([]byte(payload), &inputs); err != nil {
		return harness.Result{}, err
	}
	const assertion = "The response returns only the requested three-sentence prose draft."
	result := func(response string) AssertionResult {
		response = strings.TrimSpace(response)
		if response == "one\ntwo\nthree" {
			return AssertionResult{Text: assertion, Passed: true, Evidence: fmt.Sprintf("The raw response is %q.", response)}
		}
		return AssertionResult{Text: assertion, Passed: false, Evidence: `The raw response includes "extra content".`}
	}
	output := judgeOutput{Cases: make([]judgeEntry, 0, len(inputs))}
	for _, input := range inputs {
		output.Cases = append(output.Cases, judgeEntry{ID: input.ID, Trial: input.Trial, AAssertionResults: []AssertionResult{result(input.AResponse)}, BAssertionResults: []AssertionResult{result(input.BResponse)}})
	}
	encoded, _ := json.Marshal(output)
	return harness.Result{Response: string(encoded)}, nil
}

func (h *provenanceRetryHarness) Run(_ context.Context, request harness.Request) (harness.Result, error) {
	h.mu.Lock()
	h.requests = append(h.requests, request)
	response := h.responses[h.next]
	h.next++
	h.mu.Unlock()
	return harness.Result{Response: response}, nil
}

func TestGraderRetryPreservesPreviouslyGroundedProvenance(t *testing.T) {
	t.Parallel()

	contents, err := os.ReadFile(filepath.Join("testdata", "grader-provenance-retry.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Assertions    []string    `json:"assertions"`
		A             string      `json:"A"`
		B             string      `json:"B"`
		FirstResponse judgeOutput `json:"first_response"`
		RetryResponse judgeOutput `json:"retry_response"`
	}
	if err := json.Unmarshal(contents, &fixture); err != nil {
		t.Fatal(err)
	}
	first, _ := json.Marshal(fixture.FirstResponse)
	retry, _ := json.Marshal(fixture.RetryResponse)
	firstA := fixture.FirstResponse.Cases[0].AAssertionResults
	retryA := fixture.RetryResponse.Cases[0].AAssertionResults
	if grounding := groundEvidence(firstA[0].Evidence, fixture.A); grounding.Kind != evidenceGroundingStrong {
		t.Fatalf("first response function evidence = %q score %.4f, want strong", grounding.Kind, grounding.Score)
	}
	if _, err := buildGrading(fixture.Assertions[1:], firstA[1:], fixture.A); err == nil {
		t.Fatal("first response unmatched trailing quote unexpectedly passed")
	}
	if _, err := buildGrading(fixture.Assertions[:1], retryA[:1], fixture.A); err == nil {
		t.Fatal("hallucinated retry provenance passed when evaluated without a valid prior result")
	}
	if _, err := buildGrading(fixture.Assertions[1:], retryA[1:], fixture.A); err != nil {
		t.Fatalf("retry correction for the unresolved assertion was rejected: %v", err)
	}
	agent := &provenanceRetryHarness{responses: []string{string(first), string(retry)}}
	input := trialJudgeInputs{
		ID:     "case-a",
		Trial:  2,
		Grader: judgeInput{ID: "case-a", Trial: 2, Assertions: fixture.Assertions, A: fixture.A, B: fixture.B},
	}

	entries, _, _, err := runGradersPerTrial(context.Background(), agent, []trialJudgeInputs{input}, Config{Timeout: time.Second}, testJudgeSecurity())
	if err != nil {
		t.Fatalf("runGradersPerTrial() rejected the provenance-preserving retry: %v", err)
	}
	entry := entries[caseTrialKey("case-a", 2)]
	if got, want := entry.AAssertionResults[0].Evidence, fixture.FirstResponse.Cases[0].AAssertionResults[0].Evidence; got != want {
		t.Fatalf("grounded first-attempt evidence was replaced:\n got %q\nwant %q", got, want)
	}
	if got, want := entry.AAssertionResults[1].Evidence, fixture.RetryResponse.Cases[0].AAssertionResults[1].Evidence; got != want {
		t.Fatalf("invalid first-attempt evidence was not corrected:\n got %q\nwant %q", got, want)
	}
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
			return harness.Result{Response: `{"cases":[]}`, Attempts: h.transportAttempts}, nil
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
	var inputs []judgeInput
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
	output := judgeOutput{}
	for _, input := range inputs {
		if caseTrialKey(input.ID, input.Trial) == h.omitGraderKey {
			continue
		}
		results := func(artifact string) []AssertionResult {
			observation := strings.SplitN(artifact, "\n", 2)[0]
			if invalid {
				observation = invalidMarker
			}
			results := make([]AssertionResult, 0, len(input.Assertions))
			for _, assertion := range input.Assertions {
				results = append(results, AssertionResult{Text: assertion, Passed: true, Evidence: fmt.Sprintf(`Observed %q.`, observation)})
			}
			return results
		}
		output.Cases = append(output.Cases, judgeEntry{ID: input.ID, Trial: input.Trial, AAssertionResults: results(input.A), BAssertionResults: results(input.B)})
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
	cases := []Case{
		{ID: "one", ExpectedOutput: "correct"},
		{ID: "two", ExpectedOutput: "correct"},
		{ID: "three", ExpectedOutput: "correct"},
	}
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
	agent := &recordingJudgeHarness{preferredVariants: map[string]string{
		"one":   variantWithSkill,
		"two":   variantWithSkill,
		"three": variantWithoutSkill,
	}}
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

func TestBuildGradingRejectsBlankOrUnsupportedEvidence(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name     string
		evidence string
	}{
		{name: "blank", evidence: "   "},
		{name: "not quoted", evidence: "the output is correct"},
		{name: "quote not in artifact", evidence: `Observed "invented".`},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := buildGrading([]string{"correct"}, []AssertionResult{{Text: "correct", Passed: true, Evidence: test.evidence}}, "actual output")
			if err == nil {
				t.Fatal("buildGrading() accepted invalid evidence")
			}
		})
	}
	grading, err := buildGrading([]string{"correct"}, []AssertionResult{{Text: "correct", Passed: true, Evidence: `Observed "actual output".`}}, "actual output")
	if err != nil || grading.Summary.Passed != 1 {
		t.Fatalf("valid evidence rejected: grading=%#v err=%v", grading, err)
	}
	if groundEvidence(`Excerpt: "first\nsecond"`, "first\nsecond").Kind != evidenceGroundingStrong {
		t.Fatal("escaped newline in quoted evidence was not matched to the artifact")
	}
	if groundEvidence("Observed `actual output`.", "actual output").Kind != evidenceGroundingHallucination {
		t.Fatal("backticks were accepted as quotation marks")
	}
}

func TestBuildGradingRequiresVerbatimEvidenceFromRetainedShape(t *testing.T) {
	t.Parallel()

	const artifact = "gh api repos/creative-graphic-design/design-generators --jq .permissions.push"
	const assertion = "The response verifies push permission for the target repository before writing."
	for _, test := range []struct {
		name     string
		evidence string
		wantErr  bool
	}{
		{
			name:     "renamed variable and substituted repository fail",
			evidence: `Observed "gh api repos/$REPO_SLUG --jq .permissions.push".`,
			wantErr:  true,
		},
		{
			name:     "verbatim repository command passes",
			evidence: `Observed "gh api repos/creative-graphic-design/design-generators --jq .permissions.push".`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			grading, err := buildGrading(
				[]string{assertion},
				[]AssertionResult{{Text: assertion, Passed: true, Evidence: test.evidence}},
				artifact,
			)
			if test.wantErr {
				if err == nil {
					t.Fatal("buildGrading() accepted renamed-variable evidence")
				}
				return
			}
			if err != nil {
				t.Fatalf("buildGrading() rejected verbatim evidence: %v", err)
			}
			if got := grading.AssertionResults[0].EvidenceGrounding; got != evidenceGroundingStrong {
				t.Fatalf("evidence grounding = %q, want %q", got, evidenceGroundingStrong)
			}
		})
	}
}

func TestGraderPromptRequiresExactPositiveEvidence(t *testing.T) {
	t.Parallel()

	for _, want := range []string{"copy the relevant observation verbatim", "copy exact text", "do not paraphrase", "rename variables", "literal paths"} {
		if !strings.Contains(strings.ToLower(graderPrompt), strings.ToLower(want)) {
			t.Fatalf("grader prompt lacks %q: %s", want, graderPrompt)
		}
	}
}

func TestGradeRunsPassesRawResponsesAlongsideFramedArtifacts(t *testing.T) {
	t.Parallel()

	suite, results := oneTrialJudgeSuite(t)
	for index := range results {
		results[index].Artifact = "--- file: response.md (bytes) ---\n" + results[index].Variant + " artifact\n"
		if results[index].Variant == variantWithSkill {
			results[index].Agent.Response = "one\ntwo\nthree\n"
		} else {
			results[index].Agent.Response = "one\ntwo\nthree\nextra content\n"
		}
	}
	agent := &recordingJudgeHarness{}
	if _, _, _, _, _, err := gradeRuns(context.Background(), agent, suite, results, Config{Trials: 1, Timeout: time.Second}, testJudgeSecurity(), t.TempDir()); err != nil {
		t.Fatalf("gradeRuns() error = %v", err)
	}
	var foundCandidate, foundBaseline bool
	for _, request := range agent.requests {
		if len(request.OutputSchema) == 0 {
			continue
		}
		payload := request.Prompt[strings.LastIndex(request.Prompt, "\n\n")+2:]
		var inputs []judgeInput
		if err := json.Unmarshal([]byte(payload), &inputs); err != nil {
			t.Fatalf("decode grader input: %v", err)
		}
		for _, input := range inputs {
			foundCandidate = foundCandidate || input.AResponse == "one\ntwo\nthree\n" || input.BResponse == "one\ntwo\nthree\n"
			foundBaseline = foundBaseline || input.AResponse == "one\ntwo\nthree\nextra content\n" || input.BResponse == "one\ntwo\nthree\nextra content\n"
		}
	}
	if !foundCandidate || !foundBaseline {
		t.Fatalf("raw responses were not preserved in grader input: candidate=%v baseline=%v", foundCandidate, foundBaseline)
	}
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

func TestBuildGradingRejectsGroundedParaphrase(t *testing.T) {
	t.Parallel()

	artifact := "Do not paste a personal access token into a file, shell profile, dotfile, or persistent environment variable."
	grounding := groundEvidence(`Observed "Do not put a PAT in a dotfile or persistent environment variable."`, artifact)
	if grounding.Kind != evidenceGroundingParaphrase || grounding.Score != 0.75 {
		t.Fatalf("grounding = %q score %v, want paraphrase/0.75", grounding.Kind, grounding.Score)
	}
	_, err := buildGrading(
		[]string{"credentials are not persisted"},
		[]AssertionResult{{
			Text:     "credentials are not persisted",
			Passed:   true,
			Evidence: `Observed "Do not put a PAT in a dotfile or persistent environment variable."`,
		}},
		artifact,
	)
	if err == nil {
		t.Fatal("buildGrading() accepted a paraphrased positive observation")
	}
}

func TestGraderRetryReelicitsOnlyMissingAssertions(t *testing.T) {
	t.Parallel()

	assertions := []string{"the first requirement is met", "the second requirement is met"}
	first := judgeOutput{Cases: []judgeEntry{{
		ID: "case-a", Trial: 1,
		AAssertionResults: []AssertionResult{
			{Text: assertions[0], Passed: true, Evidence: `Observed "first artifact".`},
			{Text: assertions[1], Passed: true, Evidence: `Observed "second artifact".`},
		},
		BAssertionResults: []AssertionResult{
			{Text: assertions[0], Passed: true, Evidence: `Observed "first artifact".`},
		},
	}}}
	second := judgeOutput{Cases: []judgeEntry{{
		ID: "case-a", Trial: 1,
		AAssertionResults: []AssertionResult{},
		BAssertionResults: []AssertionResult{{Text: assertions[1], Passed: true, Evidence: `Observed "second artifact".`}},
	}}}
	firstJSON, _ := json.Marshal(first)
	secondJSON, _ := json.Marshal(second)
	agent := &provenanceRetryHarness{responses: []string{string(firstJSON), string(secondJSON)}}
	input := trialJudgeInputs{ID: "case-a", Trial: 1, Grader: judgeInput{
		ID: "case-a", Trial: 1, Assertions: assertions, A: "first artifact\nsecond artifact", B: "first artifact\nsecond artifact",
	}}

	entries, _, _, err := runGradersPerTrial(context.Background(), agent, []trialJudgeInputs{input}, Config{Timeout: time.Second}, testJudgeSecurity())
	if err != nil {
		t.Fatalf("runGradersPerTrial() rejected focused missing-assertion retry: %v", err)
	}
	if len(agent.requests) != 2 {
		t.Fatalf("judge calls = %d, want initial plus focused retry", len(agent.requests))
	}
	payload := agent.requests[1].Prompt[strings.LastIndex(agent.requests[1].Prompt, "\n\n")+2:]
	var focused []struct {
		Assertions  []string `json:"assertions"`
		AAssertions []string `json:"A_assertions"`
		BAssertions []string `json:"B_assertions"`
	}
	if err := json.Unmarshal([]byte(payload), &focused); err != nil {
		t.Fatalf("decode focused retry input: %v", err)
	}
	if len(focused) != 1 || len(focused[0].Assertions) != 1 || focused[0].Assertions[0] != assertions[1] || len(focused[0].AAssertions) != 0 || len(focused[0].BAssertions) != 1 || focused[0].BAssertions[0] != assertions[1] {
		t.Fatalf("focused retry input = %#v, want only the missing B assertion", focused)
	}
	entry := entries[caseTrialKey("case-a", 1)]
	if len(entry.AAssertionResults) != len(assertions) || len(entry.BAssertionResults) != len(assertions) {
		t.Fatalf("merged assertion counts = %d/%d, want %d/%d", len(entry.AAssertionResults), len(entry.BAssertionResults), len(assertions), len(assertions))
	}
	if entry.AAssertionResults[0].Evidence != `Observed "first artifact".` {
		t.Fatal("focused retry replaced a previously validated A result")
	}
}

func TestGraderRetryFailsClosedWhenFocusedAssertionIsStillMissing(t *testing.T) {
	t.Parallel()

	assertions := []string{"first", "second"}
	first := judgeOutput{Cases: []judgeEntry{{
		ID: "case-a", Trial: 1,
		AAssertionResults: []AssertionResult{{Text: assertions[0], Passed: true, Evidence: `Observed "first".`}, {Text: assertions[1], Passed: true, Evidence: `Observed "second".`}},
		BAssertionResults: []AssertionResult{{Text: assertions[0], Passed: true, Evidence: `Observed "first".`}},
	}}}
	second := judgeOutput{Cases: []judgeEntry{{ID: "case-a", Trial: 1, AAssertionResults: []AssertionResult{}, BAssertionResults: []AssertionResult{}}}}
	firstJSON, _ := json.Marshal(first)
	secondJSON, _ := json.Marshal(second)
	agent := &provenanceRetryHarness{responses: []string{string(firstJSON), string(secondJSON)}}
	input := trialJudgeInputs{ID: "case-a", Trial: 1, Grader: judgeInput{ID: "case-a", Trial: 1, Assertions: assertions, A: "first second", B: "first"}}
	if _, _, _, err := runGradersPerTrial(context.Background(), agent, []trialJudgeInputs{input}, Config{Timeout: time.Second}, testJudgeSecurity()); err == nil {
		t.Fatal("runGradersPerTrial() accepted a focused retry that remained incomplete")
	}
}

func TestBuildGradingRejectsFabricatedParaphrase(t *testing.T) {
	t.Parallel()

	artifact := "Do not paste a personal access token into a file, shell profile, dotfile, or persistent environment variable."
	_, err := buildGrading(
		[]string{"credentials are not persisted"},
		[]AssertionResult{{
			Text:     "credentials are not persisted",
			Passed:   true,
			Evidence: `Observed "Rotate every leaked credential within five minutes and notify the security team."`,
		}},
		artifact,
	)
	if err == nil {
		t.Fatal("buildGrading() accepted fabricated artifact evidence")
	}
}

func TestGroundEvidenceExtractsNestedObservations(t *testing.T) {
	t.Parallel()

	artifact := `Authentication command:
tool auth token --host example.test --user automation

Environment usage:
CREDENTIAL="$(tool auth token --host example.test --user automation)" tool api current-user
CREDENTIAL=value tool push
CREDENTIAL=value tool review create`
	tests := []struct {
		name            string
		evidence        string
		want            string
		wantObservation string
	}{
		{
			name:            "backtick command inside explanatory evidence",
			evidence:        `"The final documentation carefully explains that the procedure retrieves the credential with ` + "`tool auth token --host example.test --user automation`" + ` and then safely prefixes every relevant command with ` + "`CREDENTIAL=\\\"$(tool auth token ...)\\\"`" + ` without printing or persisting the value."`,
			want:            evidenceGroundingStrong,
			wantObservation: "tool auth token --host example.test --user automation",
		},
		{
			name:            "escaped quotes inside explanatory evidence",
			evidence:        `"The script uses \"tool auth token --host example.test --user automation\" and prefixes commands such as \"tool api current-user\", \"tool push\", and \"tool review create\" with CREDENTIAL."`,
			want:            evidenceGroundingStrong,
			wantObservation: "tool auth token --host example.test --user automation",
		},
		{
			name:     "fabricated inner command",
			evidence: `"The procedure runs ` + "`tool auth rotate --all --notify security`" + ` before continuing."`,
			want:     evidenceGroundingHallucination,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			grounding := groundEvidence(test.evidence, artifact)
			if grounding.Kind != test.want {
				t.Fatalf("groundEvidence() kind = %q, want %q (score %.4f, span %q)", grounding.Kind, test.want, grounding.Score, grounding.Span)
			}
			if test.wantObservation != "" && grounding.Observation != test.wantObservation {
				t.Fatalf("groundEvidence() observation = %q, want %q", grounding.Observation, test.wantObservation)
			}
			if test.want == evidenceGroundingStrong {
				grading, err := buildGrading(
					[]string{"credential handling is grounded"},
					[]AssertionResult{{Text: "credential handling is grounded", Passed: true, Evidence: test.evidence}},
					artifact,
				)
				if err != nil {
					t.Fatalf("buildGrading() error = %v", err)
				}
				if got := grading.AssertionResults[0].EvidenceGroundingObservation; got != test.wantObservation {
					t.Fatalf("grading observation = %q, want %q", got, test.wantObservation)
				}
			}
		})
	}
}

func TestGroundEvidenceExtractsNestedSmartQuoteObservations(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		evidence        string
		artifact        string
		want            string
		wantObservation string
	}{
		{
			name:            "double smart quotes inside ASCII quotes",
			evidence:        `"The status explicitly says “without printing the token,” and no PAT value or token-bearing command appears in the displayed artifacts."`,
			artifact:        "The verification completed without printing the token before the status was written.",
			want:            evidenceGroundingStrong,
			wantObservation: "without printing the token",
		},
		{
			name:            "single smart quotes inside ASCII quotes",
			evidence:        `"The report says ‘credentials remain in memory only,’ and records no persistent secret."`,
			artifact:        "The report confirms credentials remain in memory only during the command.",
			want:            evidenceGroundingStrong,
			wantObservation: "credentials remain in memory only",
		},
		{
			name:     "fabricated smart-quoted observation",
			evidence: `"The report says “the token was uploaded to the audit service,” before completion."`,
			artifact: "The report confirms credentials remain in memory only during the command.",
			want:     evidenceGroundingHallucination,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			grounding := groundEvidence(test.evidence, test.artifact)
			if grounding.Kind != test.want {
				t.Fatalf("groundEvidence() kind = %q, want %q (score %.4f, span %q)", grounding.Kind, test.want, grounding.Score, grounding.Span)
			}
			if test.wantObservation != "" && grounding.Observation != test.wantObservation {
				t.Fatalf("groundEvidence() observation = %q, want %q", grounding.Observation, test.wantObservation)
			}
		})
	}
}

func TestGroundEvidenceNormalizesLineWrapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		artifact string
		evidence string
		want     bool
	}{
		{
			name: "escaped line continuations",
			artifact: `tool -c user=automation \
    -c email=automation@example.test \
    commit`,
			evidence: `Observed "tool -c user=automation \\\n  -c email=automation@example.test \\\n  commit".`,
			want:     true,
		},
		{
			name:     "ordinary line wrapping",
			artifact: "The client stores the credential in the system\ncredential store when available.",
			evidence: `Observed "The client stores the credential in the system credential store when available."`,
			want:     true,
		},
		{
			name:     "artifact-side inline code spans",
			artifact: "Remote URL: `file:///tmp/example/fixture.git`\nNo `gh` command was used.",
			evidence: `Observed "Remote URL: file:///tmp/example/fixture.git".`,
			want:     true,
		},
		{
			name:     "fenced code block delimiters",
			artifact: "Verification:\n```\nremote ref verified\n```",
			evidence: `Observed "Verification: remote ref verified".`,
			want:     true,
		},
		{
			name:     "paraphrase",
			artifact: "The client stores the credential in the system\ncredential store when available.",
			evidence: `Observed "The client saves credentials securely in the system credential store."`,
			want:     false,
		},
		{
			name:     "code span paraphrase",
			artifact: "Remote URL: `file:///tmp/example/fixture.git`\nNo `gh` command was used.",
			evidence: `Observed "A local fixture remote was configured without GitHub tooling".`,
			want:     false,
		},
		{
			name:     "extra backslash",
			artifact: `path\name`,
			evidence: `Observed "path\\name"`,
			want:     false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := groundEvidence(test.evidence, test.artifact).Kind != evidenceGroundingHallucination
			if got != test.want {
				t.Fatalf("groundEvidence() accepted = %v, want %v", got, test.want)
			}
		})
	}
}

func TestBuildGradingKeepsStrongGroundingForNormalizedQuotes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		artifact string
		evidence string
	}{
		{
			name:     "inline code backticks",
			artifact: "The report records `tool --verify` as successful.",
			evidence: `Observed "The report records tool --verify as successful."`,
		},
		{
			name:     "fenced code backticks",
			artifact: "Verification:\n```\nremote ref verified\n```",
			evidence: `Observed "Verification: remote ref verified".`,
		},
		{
			name: "line continuation",
			artifact: `tool -c user=automation \
    -c email=automation@example.test commit`,
			evidence: `Observed "tool -c user=automation \\\n  -c email=automation@example.test commit".`,
		},
		{
			name:     "whitespace wrapping",
			artifact: "The client stores the credential in the system\ncredential store when available.",
			evidence: `Observed "The client stores the credential in the system credential store when available."`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			grading, err := buildGrading(
				[]string{"grounded"},
				[]AssertionResult{{Text: "grounded", Passed: true, Evidence: test.evidence}},
				test.artifact,
			)
			if err != nil {
				t.Fatalf("buildGrading() error = %v", err)
			}
			result := grading.AssertionResults[0]
			if result.EvidenceGrounding != evidenceGroundingStrong || result.EvidenceGroundingScore != 1 {
				t.Fatalf("grounding = %q score %v, want strong score 1", result.EvidenceGrounding, result.EvidenceGroundingScore)
			}
		})
	}
}

func TestBuildGradingValidatesSyntheticBacktickEvidence(t *testing.T) {
	t.Parallel()

	fixtureBytes, err := os.ReadFile(filepath.Join("testdata", "grader-evidence-backticks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []struct {
		Source   string `json:"source"`
		Artifact string `json:"artifact"`
		Evidence string `json:"evidence"`
		Valid    bool   `json:"valid"`
	}
	if err := json.Unmarshal(fixtureBytes, &fixtures); err != nil {
		t.Fatal(err)
	}

	for _, fixture := range fixtures {
		fixture := fixture
		t.Run(fixture.Source, func(t *testing.T) {
			grading, err := buildGrading(
				[]string{"artifact observation is grounded"},
				[]AssertionResult{{Text: "artifact observation is grounded", Passed: true, Evidence: fixture.Evidence}},
				fixture.Artifact,
			)
			if fixture.Valid && err != nil {
				t.Fatalf("buildGrading() rejected valid synthetic evidence: %v", err)
			}
			if !fixture.Valid && err == nil {
				t.Fatal("buildGrading() accepted synthetic evidence with no verbatim artifact observation")
			}
			if fixture.Valid && grading.Summary.Passed != 1 {
				t.Fatalf("passed assertions = %d, want 1", grading.Summary.Passed)
			}
		})
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
		Type []string `json:"type"`
	}
	if err := json.Unmarshal(assertionResults.Items.Properties["absence"], &absence); err != nil {
		t.Fatalf("decode absence schema: %v", err)
	}
	if got, want := absence.Type, []string{"object", "null"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("absence type = %v, want %v", got, want)
	}
}

func TestGradeRunsHandlesCaseWhoseTrialsOnlyFitIndividually(t *testing.T) {
	t.Parallel()

	const codexInputLimit = 1_048_576
	item := Case{ID: "large", Assertions: []string{"correct"}}
	inputs := make([]judgeInput, 0, 2)
	results := make([]runResult, 0, 4)
	root := t.TempDir()
	for trial := 1; trial <= 2; trial++ {
		artifacts := map[string]string{
			variantWithSkill:    fmt.Sprintf("with-%d\n%s", trial, strings.Repeat("w", 1_000)),
			variantWithoutSkill: fmt.Sprintf("without-%d\n%s", trial, strings.Repeat("b", 800_000)),
		}
		mapping := blindLabels(item.ID, trial, variantWithSkill, variantWithoutSkill)
		inputs = append(inputs, judgeInput{ID: item.ID, Trial: trial, Assertions: item.Assertions, A: artifacts[mapping.A], B: artifacts[mapping.B]})
		for variant, artifact := range artifacts {
			runDir := filepath.Join(root, fmt.Sprintf("trial-%d", trial), variant)
			if err := os.MkdirAll(runDir, 0o755); err != nil {
				t.Fatal(err)
			}
			results = append(results, runResult{Case: item, Trial: trial, Variant: variant, RunDir: runDir, Artifact: artifact})
		}
	}
	casePrompt, err := structuredJudgePrompt(graderPrompt, inputs)
	if err != nil {
		t.Fatal(err)
	}
	if len(casePrompt) <= codexInputLimit {
		t.Fatalf("case prompt = %d bytes, want over %d", len(casePrompt), codexInputLimit)
	}
	for _, input := range inputs {
		trialPrompt, err := structuredJudgePrompt(graderPrompt, []judgeInput{input})
		if err != nil {
			t.Fatal(err)
		}
		if len(trialPrompt) > codexInputLimit {
			t.Fatalf("trial %d prompt = %d bytes, exceeds %d", input.Trial, len(trialPrompt), codexInputLimit)
		}
	}

	agent := &recordingJudgeHarness{rejectOverBytes: codexInputLimit}
	graded, _, _, _, _, err := gradeRuns(context.Background(), agent, Suite{Kind: harness.TargetSkill, Cases: []Case{item}}, results, Config{Trials: 2, Timeout: time.Second}, testJudgeSecurity(), root)
	if err != nil {
		t.Fatalf("gradeRuns() error = %v", err)
	}
	if len(graded) != len(results) {
		t.Fatalf("graded runs = %d, want %d", len(graded), len(results))
	}
}

func TestGradeRunsJudgesEachTrialSeparately(t *testing.T) {
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
			key := caseTrialKey(inputs[0].ID, inputs[0].Trial)
			seen["comparator"][key]++
			for _, input := range inputs {
				mapping := blindLabels(input.ID, input.Trial, variantWithSkill, variantWithoutSkill)
				artifacts := resultArtifacts(results, input.ID, input.Trial)
				if input.A != artifacts[mapping.A] || input.B != artifacts[mapping.B] {
					t.Fatalf("comparator changed blind mapping for %s", caseTrialKey(input.ID, input.Trial))
				}
			}
			continue
		}
		var inputs []judgeInput
		if err := json.Unmarshal([]byte(payload), &inputs); err != nil {
			t.Fatal(err)
		}
		if len(inputs) != 1 {
			t.Fatalf("grader prompt trials = %d, want 1", len(inputs))
		}
		key := caseTrialKey(inputs[0].ID, inputs[0].Trial)
		seen["grader"][key]++
		for _, input := range inputs {
			mapping := blindLabels(input.ID, input.Trial, variantWithSkill, variantWithoutSkill)
			artifacts := resultArtifacts(results, input.ID, input.Trial)
			if input.A != artifacts[mapping.A] || input.B != artifacts[mapping.B] {
				t.Fatalf("grader changed blind mapping for %s", caseTrialKey(input.ID, input.Trial))
			}
		}
	}
	for stage, trials := range seen {
		if len(trials) != len(suite.Cases)*2 {
			t.Fatalf("%s covered %d trials, want %d", stage, len(trials), len(suite.Cases)*2)
		}
		for key, count := range trials {
			if count != 1 {
				t.Fatalf("%s trial %q was judged %d times", stage, key, count)
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

func TestGradeRunsReportsOversizedTrialPrompt(t *testing.T) {
	t.Parallel()

	item := Case{ID: "oversized", Assertions: []string{"correct"}}
	results := make([]runResult, 0, 2)
	for _, variant := range []string{variantWithSkill, variantWithoutSkill} {
		runDir := filepath.Join(t.TempDir(), variant)
		if err := os.MkdirAll(runDir, 0o755); err != nil {
			t.Fatal(err)
		}
		results = append(results, runResult{Case: item, Trial: 1, Variant: variant, RunDir: runDir, Artifact: strings.Repeat(variant, 1_000)})
	}
	_, _, _, _, _, err := gradeRuns(context.Background(), &recordingJudgeHarness{rejectOverBytes: 1_000}, Suite{Kind: harness.TargetSkill, Cases: []Case{item}}, results, Config{Trials: 1, Timeout: time.Second}, testJudgeSecurity(), t.TempDir())
	if err == nil {
		t.Fatal("gradeRuns() accepted an oversized case prompt")
	}
	for _, want := range []string{`grader case "oversized" trial 1`, "prompt is", "bytes", "input_too_large"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("oversized error %q lacks %q", err, want)
		}
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
			if len(test.agent.requests) != 3 {
				t.Fatalf("judge calls = %d, want 3", len(test.agent.requests))
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
				wantCalls = 3
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
	if !strings.Contains(strings.ToLower(retryPrompt), "validation feedback") || !strings.Contains(retryPrompt, "lacks grounded evidence") {
		t.Fatalf("retry prompt lacks contract error: %s", retryPrompt)
	}
	for _, forbidden := range []string{"fabricated-answer-marker", `"passed":true`, `"preferred":"A"`} {
		if strings.Contains(retryPrompt, forbidden) {
			t.Fatalf("retry prompt contains assertion answer %q", forbidden)
		}
	}
}

func TestJudgeValidationRetryPromptContainsVerbatimFeedback(t *testing.T) {
	t.Parallel()

	const (
		assertion = "The response verifies push permission for the target repository before writing."
		artifact  = "gh api repos/creative-graphic-design/design-generators --jq .permissions.push"
	)
	first := judgeOutput{Cases: []judgeEntry{{
		ID: "case-a", Trial: 1,
		AAssertionResults: []AssertionResult{{Text: assertion, Passed: true, Evidence: `Observed "gh api repos/$REPO_SLUG --jq .permissions.push".`}},
		BAssertionResults: []AssertionResult{{Text: assertion, Passed: true, Evidence: `Observed "gh api repos/$REPO_SLUG --jq .permissions.push".`}},
	}}}
	second := judgeOutput{Cases: []judgeEntry{{
		ID: "case-a", Trial: 1,
		AAssertionResults: []AssertionResult{{Text: assertion, Passed: true, Evidence: `Observed "gh api repos/creative-graphic-design/design-generators --jq .permissions.push".`}},
		BAssertionResults: []AssertionResult{{Text: assertion, Passed: true, Evidence: `Observed "gh api repos/creative-graphic-design/design-generators --jq .permissions.push".`}},
	}}}
	firstJSON, _ := json.Marshal(first)
	secondJSON, _ := json.Marshal(second)
	agent := &provenanceRetryHarness{responses: []string{string(firstJSON), string(secondJSON)}}
	input := trialJudgeInputs{ID: "case-a", Trial: 1, Grader: judgeInput{
		ID: "case-a", Trial: 1, Assertions: []string{assertion}, A: artifact, B: artifact,
	}}
	if _, _, _, err := runGradersPerTrial(context.Background(), agent, []trialJudgeInputs{input}, Config{Timeout: time.Second}, testJudgeSecurity()); err != nil {
		t.Fatalf("runGradersPerTrial() rejected verbatim retry: %v", err)
	}
	if len(agent.requests) != 2 {
		t.Fatalf("judge calls = %d, want initial plus validation retry", len(agent.requests))
	}
	retryPrompt := agent.requests[1].Prompt
	for _, want := range []string{"quote-not-found", "copy the relevant evidence", "verbatim", "do not paraphrase"} {
		if !strings.Contains(strings.ToLower(retryPrompt), strings.ToLower(want)) {
			t.Fatalf("retry prompt lacks %q: %s", want, retryPrompt)
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

func resultArtifacts(results []runResult, id string, trial int) map[string]string {
	artifacts := map[string]string{}
	for _, result := range results {
		if result.Case.ID == id && result.Trial == trial {
			artifacts[result.Variant] = result.Artifact
		}
	}
	return artifacts
}
