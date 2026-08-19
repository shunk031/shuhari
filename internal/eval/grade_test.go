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
