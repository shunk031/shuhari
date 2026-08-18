package eval

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shunk031/shuhari/internal/harness"
)

func TestAgentEvidenceContractCoversPositionalValidationEdges(t *testing.T) {
	t.Parallel()

	if _, err := groundAgentEvidence(AssertionResult{Text: "claim", Passed: true, Evidence: "line"}, ""); err == nil {
		t.Fatal("groundAgentEvidence() accepted a missing artifact root")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "response.md"), []byte("one\ntwo\nthree"), 0o644); err != nil {
		t.Fatal(err)
	}
	grounded, err := groundAgentEvidence(AssertionResult{
		Text: "claim", Passed: true, Evidence: "one\nthree",
		EvidenceReferences: []EvidenceReference{
			{Path: "response.md", StartLine: 1, EndLine: 1},
			{Path: "response.md", StartLine: 3, EndLine: 3},
		},
	}, root)
	if err != nil {
		t.Fatalf("groundAgentEvidence() rejected multiple exact spans: %v", err)
	}
	if grounded.Span != "response.md:1-1, response.md:3-3" || grounded.Observation != "one\nthree" {
		t.Fatalf("grounding = %#v, want exact locations and observation", grounded)
	}
	if _, err := groundAgentEvidence(AssertionResult{Text: "claim", Passed: true, Evidence: "line"}, root); err == nil {
		t.Fatal("groundAgentEvidence() accepted evidence without references")
	}

	if err := os.Mkdir(filepath.Join(root, "directory"), 0o755); err != nil {
		t.Fatal(err)
	}
	absolute := filepath.Join(root, "response.md")
	for _, test := range []struct {
		name string
		ref  EvidenceReference
	}{
		{name: "absolute path", ref: EvidenceReference{Path: absolute, StartLine: 1, EndLine: 1}},
		{name: "parent path", ref: EvidenceReference{Path: "../response.md", StartLine: 1, EndLine: 1}},
		{name: "dot path", ref: EvidenceReference{Path: "./response.md", StartLine: 1, EndLine: 1}},
		{name: "invalid line", ref: EvidenceReference{Path: "response.md", StartLine: 0, EndLine: 1}},
		{name: "missing file", ref: EvidenceReference{Path: "missing.md", StartLine: 1, EndLine: 1}},
		{name: "directory", ref: EvidenceReference{Path: "directory", StartLine: 1, EndLine: 1}},
		{name: "line outside file", ref: EvidenceReference{Path: "response.md", StartLine: 1, EndLine: 4}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := readAgentEvidenceSpan(root, test.ref); err == nil {
				t.Fatalf("readAgentEvidenceSpan() accepted %s", test.name)
			}
		})
	}
	if _, ok := findArtifactQuery(" ", "artifact"); ok {
		t.Fatal("findArtifactQuery() matched an empty query")
	}
	if got := groundAbsence(nil, "artifact"); got.Kind != evidenceGroundingNotApplicable {
		t.Fatalf("nil absence grounding = %#v", got)
	}
	if got := groundAbsence(&AbsenceClaim{Query: "  "}, "artifact"); got.Kind != evidenceGroundingNotApplicable {
		t.Fatalf("blank absence grounding = %#v", got)
	}
	if got := groundDeclaredAbsence(nil, "artifact"); got.Kind != evidenceGroundingNotApplicable {
		t.Fatalf("empty declared absence grounding = %#v", got)
	}
}

func TestStrictJudgeEntryValidationRejectsMalformedResponses(t *testing.T) {
	t.Parallel()

	graderInputs := []judgeInput{{ID: "one", Trial: 1}, {ID: "two", Trial: 1}}
	if _, err := validateGraderEntries(judgeOutput{}, graderInputs); err == nil {
		t.Fatal("validateGraderEntries() accepted a missing response")
	}
	if _, err := validateGraderEntries(judgeOutput{Cases: []judgeEntry{{ID: "one", Trial: 1}, {ID: "one", Trial: 1}}}, graderInputs); err == nil {
		t.Fatal("validateGraderEntries() accepted a duplicate response")
	}
	if _, err := validateGraderEntries(judgeOutput{Cases: []judgeEntry{{ID: "other", Trial: 1}, {ID: "two", Trial: 1}}}, graderInputs); err == nil {
		t.Fatal("validateGraderEntries() accepted an omitted case")
	}
	if entries, err := validateGraderEntries(judgeOutput{Cases: []judgeEntry{{ID: "one", Trial: 1}, {ID: "two", Trial: 1}}}, graderInputs); err != nil || len(entries) != 2 {
		t.Fatalf("validateGraderEntries() valid response = %#v, err=%v", entries, err)
	}

	agentInput := []agentJudgeInput{{ID: "one", Trial: 1, Side: "A"}}
	if _, err := validateAgentGraderEntries(agentJudgeOutput{}, agentInput); err == nil {
		t.Fatal("validateAgentGraderEntries() accepted a missing response")
	}
	if _, err := validateAgentGraderEntries(agentJudgeOutput{Cases: []agentJudgeEntry{{ID: "one", Trial: 1, Side: "A"}, {ID: "one", Trial: 1, Side: "A"}}}, []agentJudgeInput{{ID: "one", Trial: 1, Side: "A"}, {ID: "two", Trial: 1, Side: "A"}}); err == nil {
		t.Fatal("validateAgentGraderEntries() accepted a duplicate response")
	}
	if _, err := validateAgentGraderEntries(agentJudgeOutput{Cases: []agentJudgeEntry{{ID: "one", Trial: 1, Side: "C"}}}, agentInput); err == nil {
		t.Fatal("validateAgentGraderEntries() accepted an invalid side")
	}
	if _, err := validateAgentGraderEntries(agentJudgeOutput{Cases: []agentJudgeEntry{{ID: "other", Trial: 1, Side: "A"}}}, agentInput); err == nil {
		t.Fatal("validateAgentGraderEntries() accepted an omitted case")
	}
	if _, err := validateAgentGraderEntries(agentJudgeOutput{Cases: []agentJudgeEntry{{ID: "one", Trial: 1, Side: "B"}}}, agentInput); err == nil {
		t.Fatal("validateAgentGraderEntries() accepted the wrong blinded side")
	}
	if entries, err := validateAgentGraderEntries(agentJudgeOutput{Cases: []agentJudgeEntry{{ID: "one", Trial: 1, Side: "A"}}}, agentInput); err != nil || len(entries) != 1 {
		t.Fatalf("validateAgentGraderEntries() valid response = %#v, err=%v", entries, err)
	}

	comparisonInputs := []comparatorInput{{ID: "one", Trial: 1}}
	if _, err := validateComparatorEntries(comparatorOutput{}, comparisonInputs); err == nil {
		t.Fatal("validateComparatorEntries() accepted a missing response")
	}
	if _, err := validateComparatorEntries(comparatorOutput{Cases: []comparatorEntry{{ID: "one", Trial: 1, Preferred: "tie", Reason: "reason"}, {ID: "one", Trial: 1, Preferred: "tie", Reason: "reason"}}}, []comparatorInput{{ID: "one", Trial: 1}, {ID: "two", Trial: 1}}); err == nil {
		t.Fatal("validateComparatorEntries() accepted a duplicate response")
	}
	if _, err := validateComparatorEntries(comparatorOutput{Cases: []comparatorEntry{{ID: "one", Trial: 1, Preferred: "C", Reason: "reason"}}}, comparisonInputs); err == nil {
		t.Fatal("validateComparatorEntries() accepted an invalid preference")
	}
	if _, err := validateComparatorEntries(comparatorOutput{Cases: []comparatorEntry{{ID: "one", Trial: 1, Preferred: "A"}}}, comparisonInputs); err == nil {
		t.Fatal("validateComparatorEntries() accepted a blank reason")
	}
	if _, err := validateComparatorEntries(comparatorOutput{Cases: []comparatorEntry{{ID: "other", Trial: 1, Preferred: "A", Reason: "reason"}}}, comparisonInputs); err == nil {
		t.Fatal("validateComparatorEntries() accepted an omitted case")
	}
	if entries, err := validateComparatorEntries(comparatorOutput{Cases: []comparatorEntry{{ID: "one", Trial: 1, Preferred: "tie", Reason: "reason"}}}, comparisonInputs); err != nil || len(entries) != 1 {
		t.Fatalf("validateComparatorEntries() valid response = %#v, err=%v", entries, err)
	}
}

func TestJudgeArtifactPreparationAndRetryErrorPaths(t *testing.T) {
	t.Parallel()

	if !errors.Is((&judgeRetryError{cause: errInvalidGrading}).Unwrap(), errInvalidGrading) {
		t.Fatal("judgeRetryError.Unwrap() did not return its cause")
	}
	if err := copyJudgeArtifactTree("", t.TempDir()); err == nil {
		t.Fatal("copyJudgeArtifactTree() accepted an empty source")
	}
	if err := copyJudgeArtifactTree(filepath.Join(t.TempDir(), "missing"), t.TempDir()); err == nil {
		t.Fatal("copyJudgeArtifactTree() accepted a missing source")
	}
	file := filepath.Join(t.TempDir(), "artifact")
	if err := os.WriteFile(file, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := copyJudgeArtifactTree(file, t.TempDir()); err == nil {
		t.Fatal("copyJudgeArtifactTree() accepted a regular file source")
	}

	if _, err := structuredJudgePrompt("instruction", func() {}); err == nil {
		t.Fatal("structuredJudgePrompt() accepted an unsupported input")
	}
	if _, _, err := runStructuredJudge(context.Background(), &errorJudgeHarness{}, "instruction", map[string]string{"id": "one"}, nil, Config{Timeout: time.Second}, testJudgeSecurity()); err == nil {
		t.Fatal("runStructuredJudge() accepted a judge execution error")
	}
	if _, _, err := runStructuredJudge(context.Background(), &errorJudgeHarness{}, "instruction", func() {}, nil, Config{Timeout: time.Second}, testJudgeSecurity()); err == nil {
		t.Fatal("runStructuredJudge() accepted an unsupported input")
	}
	root := t.TempDir()
	writeAgentArtifact(t, root, "artifact\n")
	if _, _, err := runAgentStructuredJudge(context.Background(), &errorJudgeHarness{}, "instruction", map[string]string{"id": "one"}, nil, Config{Timeout: time.Second}, testJudgeSecurity(), filepath.Join(root, "missing")); err == nil {
		t.Fatal("runAgentStructuredJudge() accepted a missing artifact root")
	}
	if _, _, err := runAgentStructuredJudge(context.Background(), &errorJudgeHarness{}, "instruction", func() {}, nil, Config{Timeout: time.Second}, testJudgeSecurity(), root); err == nil {
		t.Fatal("runAgentStructuredJudge() accepted an unsupported input")
	}
	if _, _, err := runAgentStructuredJudge(context.Background(), &errorJudgeHarness{}, "instruction", map[string]string{"id": "one"}, nil, Config{Timeout: time.Second}, testJudgeSecurity(), root); err == nil {
		t.Fatal("runAgentStructuredJudge() accepted a judge execution error")
	}

	validate := func(response string) error {
		if response == "bad" {
			return errors.New("quote-not-found")
		}
		return nil
	}
	harnessWithRetry := &scriptedJudgeHarness{responses: []string{"bad", "good"}}
	_, _, err := runValidatedStructuredJudgeWithRetryAtRoot(
		context.Background(), harnessWithRetry, "base", map[string]string{"id": "one"}, nil,
		Config{Timeout: time.Second}, testJudgeSecurity(), "", validate,
		func(validationErr error) (judgeRetryRequest, bool) {
			return judgeRetryRequest{Instructions: "retry: " + validationErr.Error()}, true
		},
	)
	if err != nil || len(harnessWithRetry.requests) != 2 || !strings.Contains(harnessWithRetry.requests[1].Prompt, "retry: quote-not-found") {
		t.Fatalf("retry-builder success = err=%v requests=%d prompt=%q", err, len(harnessWithRetry.requests), harnessWithRetry.requests[1].Prompt)
	}
	harnessWithoutRetry := &scriptedJudgeHarness{responses: []string{"bad", "good"}}
	_, _, err = runValidatedStructuredJudgeWithRetryAtRoot(
		context.Background(), harnessWithoutRetry, "base", map[string]string{"id": "one"}, nil,
		Config{Timeout: time.Second}, testJudgeSecurity(), "", validate,
		func(error) (judgeRetryRequest, bool) { return judgeRetryRequest{}, false },
	)
	if err != nil || len(harnessWithoutRetry.requests) != 2 || !strings.Contains(harnessWithoutRetry.requests[1].Prompt, "Validation feedback: quote-not-found") {
		t.Fatalf("retry-builder fallback = err=%v requests=%d prompt=%q", err, len(harnessWithoutRetry.requests), harnessWithoutRetry.requests[1].Prompt)
	}
	harnessTransportFailure := &scriptedJudgeHarness{responses: []string{"bad"}, runErrors: []error{nil, errors.New("transport failed")}}
	_, _, err = runValidatedStructuredJudgeWithRetryAtRoot(
		context.Background(), harnessTransportFailure, "base", map[string]string{"id": "one"}, nil,
		Config{Timeout: time.Second}, testJudgeSecurity(), "", validate, nil,
	)
	if err == nil || !strings.Contains(err.Error(), "transport failed") {
		t.Fatalf("second-attempt transport error = %v", err)
	}
}

type errorJudgeHarness struct{ fakeHarness }

func (*errorJudgeHarness) Run(context.Context, harness.Request) (harness.Result, error) {
	return harness.Result{Response: "partial"}, errors.New("judge failed")
}

type scriptedJudgeHarness struct {
	fakeHarness
	responses []string
	runErrors []error
	requests  []harness.Request
}

func (h *scriptedJudgeHarness) Run(_ context.Context, request harness.Request) (harness.Result, error) {
	h.requests = append(h.requests, request)
	index := len(h.requests) - 1
	if index < len(h.runErrors) && h.runErrors[index] != nil {
		return harness.Result{Response: "partial"}, h.runErrors[index]
	}
	return harness.Result{Response: h.responses[index]}, nil
}

func TestBuildAgentGradingRejectsStructuralContractErrors(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeAgentArtifact(t, root, "evidence\n")
	assertion := "the response is correct"
	for _, test := range []struct {
		name     string
		expected []string
		actual   []AssertionResult
	}{
		{name: "count mismatch", expected: []string{assertion}, actual: nil},
		{name: "blank text", expected: []string{assertion}, actual: []AssertionResult{{Evidence: "evidence"}}},
		{name: "blank evidence", expected: []string{assertion}, actual: []AssertionResult{{Text: assertion}}},
		{name: "duplicate", expected: []string{assertion, "second"}, actual: []AssertionResult{{Text: assertion, Evidence: "evidence"}, {Text: assertion, Evidence: "evidence"}}},
		{name: "missing assertion", expected: []string{assertion}, actual: []AssertionResult{{Text: "other", Evidence: "evidence"}}},
		{name: "failed with absence", expected: []string{assertion}, actual: []AssertionResult{{Text: assertion, Evidence: "evidence", Absence: &AbsenceClaim{Query: "query"}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := buildAgentGrading(test.expected, test.actual, root, "evidence"); err == nil {
				t.Fatalf("buildAgentGrading() accepted %s", test.name)
			}
		})
	}
	negative := "the response does not make the forbidden change"
	if _, err := buildAgentGrading(
		[]string{negative},
		[]AssertionResult{{Text: negative, Passed: true, Evidence: "evidence"}},
		root, "evidence",
	); err == nil {
		t.Fatal("buildAgentGrading() accepted a negative assertion without an absence declaration")
	}
	mixed := "the response performs the required check and does not make the forbidden change"
	if _, err := buildAgentGrading(
		[]string{mixed},
		[]AssertionResult{{Text: mixed, Passed: true, Evidence: "evidence", Absence: &AbsenceClaim{
			NegatedClause: "does not make the forbidden change", Query: "forbidden change", Rationale: "checks the forbidden change",
		}}},
		root, "evidence",
	); err == nil {
		t.Fatal("buildAgentGrading() accepted mixed absence evidence without positional references")
	}
}

func TestFallbackAbsenceValidationRejectsEveryBlankField(t *testing.T) {
	t.Parallel()

	assertion := "the response does not make the forbidden change"
	valid := &AbsenceClaim{NegatedClause: "does not make the forbidden change", Query: "forbidden change", Rationale: "checks the forbidden change"}
	for _, claim := range []*AbsenceClaim{
		nil,
		{},
		{NegatedClause: "does not make another change", Query: "forbidden change", Rationale: "reason"},
		{NegatedClause: valid.NegatedClause, Rationale: "reason"},
		{NegatedClause: valid.NegatedClause, Query: valid.Query},
	} {
		if err := validateFallbackAbsenceClaim(assertion, claim); err == nil {
			t.Fatalf("validateFallbackAbsenceClaim() accepted %#v", claim)
		}
	}
	if err := validateFallbackAbsenceClaim("the response makes the change", valid); err == nil {
		t.Fatal("validateFallbackAbsenceClaim() accepted a positive assertion")
	}
}

func TestGradeRunsCoversMissingRunAndInstructionVariants(t *testing.T) {
	t.Parallel()

	suite := Suite{Kind: harness.TargetInstructions, Cases: []Case{{ID: "one", Assertions: []string{"correct"}}}}
	if _, _, _, _, _, err := gradeRuns(context.Background(), &recordingJudgeHarness{}, suite, nil, Config{Trials: 1, Timeout: time.Second}, testJudgeSecurity(), ""); err == nil {
		t.Fatal("gradeRuns() accepted a missing run")
	}
	if with, without := variantsFor(harness.TargetInstructions); with != variantWithInstructions || without != variantWithoutInstructions {
		t.Fatalf("instruction variants = %q/%q", with, without)
	}
	persistGradingError("", "stage", "grader", "comparator", nil, errors.New("cause"))
}

func TestGradeRunsPersistsRetryFailureBeforeReturningWriteError(t *testing.T) {
	t.Parallel()

	suite, results := oneTrialJudgeSuite(t)
	iteration := filepath.Join(t.TempDir(), "iteration")
	if err := os.WriteFile(iteration, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, _, _, _, err := gradeRuns(context.Background(), &exhaustedJudgeTransportHarness{}, suite, results, Config{Trials: 1, Timeout: time.Second}, testJudgeSecurity(), iteration)
	if err == nil || !strings.Contains(err.Error(), "judge-retries.json") {
		t.Fatalf("gradeRuns() error = %v, want retry artifact write failure", err)
	}
}

func TestGradeRunsReturnsComparatorRetryPersistenceError(t *testing.T) {
	t.Parallel()

	suite, results := oneTrialJudgeSuite(t)
	iteration := filepath.Join(t.TempDir(), "iteration")
	if err := os.WriteFile(iteration, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	attempts := harness.AttemptEvidence{AttemptCount: 2, AttemptErrors: []harness.AttemptError{fakeJudgeAttemptError(1, "comparator transport")}}
	_, _, _, _, _, err := gradeRuns(context.Background(), &comparatorFailureWithAttemptsHarness{attempts: attempts}, suite, results, Config{Trials: 1, Timeout: time.Second}, testJudgeSecurity(), iteration)
	if err == nil || !strings.Contains(err.Error(), "judge-retries.json") {
		t.Fatalf("gradeRuns() error = %v, want comparator retry artifact write failure", err)
	}
}

func TestGradeRunsReturnsFinalRetryPersistenceError(t *testing.T) {
	t.Parallel()

	suite, results := oneTrialJudgeSuite(t)
	iteration := filepath.Join(t.TempDir(), "iteration")
	if err := os.WriteFile(iteration, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	attempts := harness.AttemptEvidence{AttemptCount: 2, AttemptErrors: []harness.AttemptError{fakeJudgeAttemptError(1, "judge transport")}}
	_, _, _, _, _, err := gradeRuns(context.Background(), &recordingJudgeHarness{transportAttempts: attempts}, suite, results, Config{Trials: 1, Timeout: time.Second}, testJudgeSecurity(), iteration)
	if err == nil || !strings.Contains(err.Error(), "judge-retries.json") {
		t.Fatalf("gradeRuns() error = %v, want final retry artifact write failure", err)
	}
}

func TestGradeRunsCoversComparatorBAndComparisonWriteErrors(t *testing.T) {
	t.Parallel()

	suite, results := oneTrialJudgeSuite(t)
	item := &suite.Cases[0]
	mapping := blindLabels(item.ID, 1, variantWithSkill, variantWithoutSkill)
	agent := &recordingJudgeHarness{preferredVariants: map[string]string{item.ID: mapping.B}}
	if _, _, _, _, _, err := gradeRuns(context.Background(), agent, suite, results, Config{Trials: 1, Timeout: time.Second}, testJudgeSecurity(), t.TempDir()); err != nil {
		t.Fatalf("gradeRuns() rejected comparator B preference: %v", err)
	}

	iteration := t.TempDir()
	if err := os.WriteFile(filepath.Join(iteration, "eval-one"), []byte("directory collision"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, _, err := gradeRuns(context.Background(), &recordingJudgeHarness{}, suite, results, Config{Trials: 1, Timeout: time.Second}, testJudgeSecurity(), iteration); err == nil {
		t.Fatal("gradeRuns() accepted a comparison directory collision")
	}

	iteration = t.TempDir()
	if err := os.MkdirAll(filepath.Join(iteration, "eval-one"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(iteration, "eval-one", "comparison.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, _, err := gradeRuns(context.Background(), &recordingJudgeHarness{}, suite, results, Config{Trials: 1, Timeout: time.Second}, testJudgeSecurity(), iteration); err == nil {
		t.Fatal("gradeRuns() accepted a comparison file directory")
	}
}

type comparatorFailureWithAttemptsHarness struct {
	recordingJudgeHarness
	attempts harness.AttemptEvidence
}

func (h *comparatorFailureWithAttemptsHarness) Run(ctx context.Context, request harness.Request) (harness.Result, error) {
	if strings.Contains(string(request.OutputSchema), `"preferred"`) {
		return harness.Result{Response: `{"cases":[]}`, Attempts: h.attempts}, nil
	}
	return h.recordingJudgeHarness.Run(ctx, request)
}

func TestRunComparatorsReportsMalformedJSON(t *testing.T) {
	t.Parallel()

	input := trialJudgeInputs{ID: "one", Trial: 1, Comparator: comparatorInput{ID: "one", Trial: 1}}
	agent := &malformedComparatorHarness{}
	if _, _, _, err := runComparatorsPerTrial(context.Background(), agent, []trialJudgeInputs{input}, Config{Timeout: time.Second}, testJudgeSecurity()); err == nil {
		t.Fatal("runComparatorsPerTrial() accepted malformed comparator JSON")
	}
}

type malformedComparatorHarness struct{ recordingJudgeHarness }

func (h *malformedComparatorHarness) Run(ctx context.Context, request harness.Request) (harness.Result, error) {
	if strings.Contains(string(request.OutputSchema), `"preferred"`) {
		return harness.Result{Response: "{"}, nil
	}
	return h.recordingJudgeHarness.Run(ctx, request)
}

func TestGradeRunsRejectsArtifactChangedAfterJudgeValidation(t *testing.T) {
	t.Parallel()

	suite, results := oneTrialJudgeSuite(t)
	root := t.TempDir()
	writeAgentArtifact(t, root, "before\n")
	for index := range results {
		results[index].OutputPath = root
		results[index].Artifact = "before"
	}
	_, _, _, _, _, err := gradeRuns(context.Background(), &mutatingAgentHarness{sourceRoot: root}, suite, results, Config{Trials: 1, Timeout: time.Second}, testJudgeSecurity(), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "does not equal the cited artifact span") {
		t.Fatalf("gradeRuns() error = %v, want post-judge artifact validation failure", err)
	}
}

type mutatingAgentHarness struct {
	recordingJudgeHarness
	sourceRoot string
}

func (h *mutatingAgentHarness) Run(ctx context.Context, request harness.Request) (harness.Result, error) {
	preferred := strings.Contains(string(request.OutputSchema), `"preferred"`)
	result, err := h.recordingJudgeHarness.Run(ctx, request)
	if err == nil && !preferred && h.runs == 2 {
		if writeErr := os.WriteFile(filepath.Join(h.sourceRoot, "response.md"), []byte("changed\n"), 0o644); writeErr != nil {
			return harness.Result{}, writeErr
		}
	}
	return result, err
}

func TestGradeRunsReturnsGradingWriteError(t *testing.T) {
	t.Parallel()

	suite, results := oneTrialJudgeSuite(t)
	for index := range results {
		path := results[index].RunDir
		if err := os.RemoveAll(path); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("run directory collision"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_, _, _, _, _, err := gradeRuns(context.Background(), &recordingJudgeHarness{}, suite, results, Config{Trials: 1, Timeout: time.Second}, testJudgeSecurity(), "")
	if err == nil || !strings.Contains(err.Error(), "grading.json") {
		t.Fatalf("gradeRuns() error = %v, want grading write failure", err)
	}
}
