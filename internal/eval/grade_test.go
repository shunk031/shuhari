package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shunk031/shuhari/internal/harness"
)

func TestBuildGradingRecordsFreeFormEvidenceAndFailedVerdicts(t *testing.T) {
	expected := []string{"The result is useful.", "The result is safe."}
	grading, err := buildGrading(expected, []AssertionResult{
		{Text: expected[0], Passed: true, Evidence: "renamed-variable paraphrase"},
		{Text: expected[1], Passed: false, Evidence: "The judge found the safety requirement unmet."},
	})
	if err != nil {
		t.Fatal(err)
	}
	if grading.Summary.Passed != 1 || grading.Summary.Failed != 1 || grading.Summary.PassRate != 0.5 {
		t.Fatalf("summary = %#v", grading.Summary)
	}
	if grading.AssertionResults[0].Evidence != "renamed-variable paraphrase" {
		t.Fatalf("evidence was transformed: %#v", grading.AssertionResults[0])
	}
	encoded, err := json.Marshal(grading)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"expectations"`) || strings.Contains(string(encoded), "grounding") {
		t.Fatalf("grading artifact shape = %s", encoded)
	}
}

func TestBuildGradingRejectsOnlyStructuralProblems(t *testing.T) {
	for name, actual := range map[string][]AssertionResult{
		"missing assertion":   {{Text: "other", Passed: true, Evidence: "e"}},
		"duplicate assertion": {{Text: "claim", Passed: true, Evidence: "a"}, {Text: "claim", Passed: false, Evidence: "b"}},
		"blank evidence":      {{Text: "claim", Passed: true}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := buildGrading([]string{"claim"}, actual); err == nil {
				t.Fatal("buildGrading accepted malformed structure")
			}
		})
	}
}

func TestValidateAgentGraderEntriesPreservesBlindSide(t *testing.T) {
	input := []agentJudgeInput{{ID: "case", Trial: 1, Side: "A", Assertions: []string{"claim"}}}
	output := agentJudgeOutput{Cases: []agentJudgeEntry{{ID: "case", Trial: 1, Side: "A", AssertionResults: []AssertionResult{{Text: "claim", Passed: true, Evidence: "response.md:1"}}}}}
	entries, err := validateAgentGraderEntries(output, input)
	if err != nil {
		t.Fatal(err)
	}
	if entries[caseTrialKey("case", 1)].Side != "A" {
		t.Fatal("grader side was not preserved")
	}
	output.Cases[0].Side = "B"
	if _, err := validateAgentGraderEntries(output, input); err == nil {
		t.Fatal("grader accepted a side identity change")
	}
}

func TestGraderSchemaMatchesFreeFormEvidenceContract(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal(graderSchema(), &schema); err != nil {
		t.Fatalf("grader schema is invalid JSON: %v", err)
	}
	if strings.Contains(string(graderSchema()), "evidence_references") || strings.Contains(string(graderSchema()), "absence") {
		t.Fatal("grader schema retains removed evidence machinery")
	}
}

func TestJudgeShapeValidatorsFailClosedForMalformedCases(t *testing.T) {
	t.Run("grader", func(t *testing.T) {
		input := []agentJudgeInput{{ID: "one", Trial: 1, Side: "A", Assertions: []string{"claim"}}}
		cases := []agentJudgeOutput{
			{},
			{Cases: []agentJudgeEntry{{ID: "one", Trial: 1, Side: "C"}}},
			{Cases: []agentJudgeEntry{{ID: "one", Trial: 1, Side: "A"}, {ID: "one", Trial: 1, Side: "A"}}},
			{Cases: []agentJudgeEntry{{ID: "other", Trial: 1, Side: "A"}}},
		}
		for _, output := range cases {
			if _, err := validateAgentGraderEntries(output, input); err == nil {
				t.Fatalf("validateAgentGraderEntries accepted %#v", output)
			}
		}
	})
	t.Run("comparator", func(t *testing.T) {
		input := []comparatorInput{{ID: "one", Trial: 1}}
		cases := []comparatorOutput{
			{},
			{Cases: []comparatorEntry{{ID: "one", Trial: 1, Preferred: "C", Reason: "reason"}}},
			{Cases: []comparatorEntry{{ID: "one", Trial: 1, Preferred: "A"}}},
			{Cases: []comparatorEntry{{ID: "other", Trial: 1, Preferred: "A", Reason: "reason"}}},
		}
		for _, output := range cases {
			if _, err := validateComparatorEntries(output, input); err == nil {
				t.Fatalf("validateComparatorEntries accepted %#v", output)
			}
		}
	})
}

func TestJudgeHelpersCoverBothBlindMappingsAndReceiptShapes(t *testing.T) {
	first := blindLabels("one", 1, variantWithSkill, variantWithoutSkill)
	var second blindMapping
	for index := 0; index < 100; index++ {
		candidate := blindLabels(fmt.Sprintf("case-%d", index), 1, variantWithSkill, variantWithoutSkill)
		if candidate != first {
			second = candidate
			break
		}
	}
	if second == (blindMapping{}) {
		t.Fatalf("blind mapping did not exercise both assignments: %#v", first)
	}
	with, without := variantsFor(harness.TargetInstructions)
	if with != variantWithInstructions || without != variantWithoutInstructions {
		t.Fatalf("instruction variants = %q, %q", with, without)
	}
	if got := comparisonPath("one", "case", 2); !strings.HasSuffix(got, filepath.Join("eval-case", "comparisons", "2.json")) {
		t.Fatalf("comparison path = %s", got)
	}
	if got := comparisonPath("one", "case", 1); !strings.HasSuffix(got, filepath.Join("eval-case", "comparison.json")) {
		t.Fatalf("comparison path = %s", got)
	}
	if err := persistJudgeRetries("", nil); err != nil {
		t.Fatal(err)
	}
}

func TestCopyJudgeArtifactTreeRejectsInvalidRoots(t *testing.T) {
	if err := copyJudgeArtifactTree("", t.TempDir()); err == nil {
		t.Fatal("copyJudgeArtifactTree accepted an empty root")
	}
	file := filepath.Join(t.TempDir(), "artifact")
	if err := os.WriteFile(file, []byte("artifact"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := copyJudgeArtifactTree(file, t.TempDir()); err == nil {
		t.Fatal("copyJudgeArtifactTree accepted a regular file")
	}
}

type malformedJudgeHarness struct{ *evalHarness }

func (malformedJudgeHarness) Run(context.Context, harness.Request) (harness.Result, error) {
	return harness.Result{Response: "not-json", Attempts: harness.AttemptEvidence{AttemptCount: 1}}, nil
}

func TestMalformedAgentJudgeResponseFailsClosed(t *testing.T) {
	root := t.TempDir()
	input := []trialJudgeInputs{{
		ID: "one", Trial: 1, AOutputPath: root, BOutputPath: root, Assertions: []string{"claim"},
	}}
	_, _, _, err := runGradersPerTrial(context.Background(), malformedJudgeHarness{evalHarness: &evalHarness{}}, input, Config{Timeout: time.Second}, harness.SecurityResolution{})
	if err == nil || !strings.Contains(err.Error(), "decode grader response") {
		t.Fatalf("runGradersPerTrial() error = %v, want decode failure", err)
	}
}

func TestMalformedComparatorResponseFailsClosed(t *testing.T) {
	root := t.TempDir()
	input := []trialJudgeInputs{{
		ID: "one", Trial: 1, AOutputPath: root, BOutputPath: root,
		Comparator: comparatorInput{ID: "one", Trial: 1, A: "A", B: "B"},
	}}
	_, _, _, err := runComparatorsPerTrial(context.Background(), malformedJudgeHarness{evalHarness: &evalHarness{}}, input, Config{Timeout: time.Second}, harness.SecurityResolution{})
	if err == nil || !strings.Contains(err.Error(), "decode comparator response") {
		t.Fatalf("runComparatorsPerTrial() error = %v, want decode failure", err)
	}
}

func TestJudgePipelinesAndGradeRunsAcceptWellFormedResponses(t *testing.T) {
	root := t.TempDir()
	withOutputs := filepath.Join(root, "with", "outputs")
	withoutOutputs := filepath.Join(root, "without", "outputs")
	mustWrite(t, filepath.Join(withOutputs, "response.md"), "candidate output\n")
	mustWrite(t, filepath.Join(withoutOutputs, "response.md"), "baseline output\n")
	input := trialJudgeInputs{
		ID: "one", Trial: 1, AOutputPath: withOutputs, BOutputPath: withoutOutputs,
		Assertions: []string{"The result is useful."},
		Comparator: comparatorInput{ID: "one", Trial: 1, A: "candidate output", B: "baseline output"},
	}
	agent := &evalHarness{}
	graderEntries, _, retries, err := runGradersPerTrial(context.Background(), agent, []trialJudgeInputs{input}, Config{Timeout: time.Second}, harness.SecurityResolution{})
	if err != nil || len(graderEntries) != 1 || len(retries) != 2 {
		t.Fatalf("runGradersPerTrial() = %#v, retries=%#v, err=%v", graderEntries, retries, err)
	}
	comparatorEntries, _, comparatorRetries, err := runComparatorsPerTrial(context.Background(), agent, []trialJudgeInputs{input}, Config{Timeout: time.Second}, harness.SecurityResolution{})
	if err != nil || comparatorEntries[caseTrialKey("one", 1)].Preferred != "A" || len(comparatorRetries) != 1 {
		t.Fatalf("runComparatorsPerTrial() = %#v, retries=%#v, err=%v", comparatorEntries, comparatorRetries, err)
	}

	suite := Suite{Kind: harness.TargetSkill, Cases: []Case{{ID: "one", ExpectedOutput: "The result is useful."}}}
	iteration := filepath.Join(root, "iteration")
	if err := os.MkdirAll(iteration, 0o755); err != nil {
		t.Fatal(err)
	}
	results := []runResult{
		{Case: suite.Cases[0], Trial: 1, Variant: variantWithSkill, RunDir: filepath.Join(root, "with"), OutputPath: withOutputs, Artifact: "candidate output", Agent: harness.Result{Response: "candidate output"}},
		{Case: suite.Cases[0], Trial: 1, Variant: variantWithoutSkill, RunDir: filepath.Join(root, "without"), OutputPath: withoutOutputs, Artifact: "baseline output", Agent: harness.Result{Response: "baseline output"}},
	}
	graded, candidateWins, baselineWins, _, _, err := gradeRuns(context.Background(), agent, suite, results, Config{Trials: 1, Timeout: time.Second}, harness.SecurityResolution{}, iteration)
	if err != nil || len(graded) != 2 || candidateWins != 1 || baselineWins != 0 {
		t.Fatalf("gradeRuns() = graded=%#v candidate=%d baseline=%d err=%v", graded, candidateWins, baselineWins, err)
	}
	for _, path := range []string{
		filepath.Join(root, "with", "grading.json"),
		filepath.Join(root, "without", "grading.json"),
		filepath.Join(root, "iteration", "eval-one", "comparison.json"),
		filepath.Join(root, "iteration", "judge-retries.json"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("missing grade artifact %s: %v", path, err)
		}
	}
}
