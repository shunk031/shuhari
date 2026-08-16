package eval

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shunk031/shuhari/internal/harness"
)

func TestGradeRunsAllowsMinorityBaselinePreference(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	cases := []Case{
		{ID: "one", ExpectedOutput: "correct"},
		{ID: "two", ExpectedOutput: "correct"},
		{ID: "three", ExpectedOutput: "correct"},
	}
	entries := make([]judgeEntry, 0, len(cases))
	comparisons := make([]comparatorEntry, 0, len(cases))
	results := make([]runResult, 0, len(cases)*2)
	for index, item := range cases {
		mapping := blindLabels(item.ID, 1, variantWithSkill, variantWithoutSkill)
		preferredVariant := variantWithSkill
		if index == len(cases)-1 {
			preferredVariant = variantWithoutSkill
		}
		grades := map[string][]AssertionResult{
			variantWithSkill:    {{Text: "correct", Passed: true, Evidence: `Observed "with_skill".`}},
			variantWithoutSkill: {{Text: "correct", Passed: true, Evidence: `Observed "without_skill".`}},
		}
		entries = append(entries, judgeEntry{
			ID: item.ID, Trial: 1,
			AAssertionResults: grades[mapping.A], BAssertionResults: grades[mapping.B],
		})
		comparisons = append(comparisons, comparatorEntry{ID: item.ID, Trial: 1, Preferred: preferredLabel(mapping, preferredVariant), Reason: "comparison"})
		for _, variant := range []string{variantWithSkill, variantWithoutSkill} {
			runDir := filepath.Join(root, item.ID, variant)
			if err := os.MkdirAll(runDir, 0o755); err != nil {
				t.Fatal(err)
			}
			results = append(results, runResult{Case: item, Trial: 1, Variant: variant, RunDir: runDir, Artifact: variant, Agent: harness.Result{Duration: time.Millisecond}})
		}
	}
	encoded, err := json.Marshal(judgeOutput{Cases: entries})
	if err != nil {
		t.Fatal(err)
	}
	compared, err := json.Marshal(comparatorOutput{Cases: comparisons})
	if err != nil {
		t.Fatal(err)
	}
	agent := &fakeHarness{judgeResponse: string(encoded), compareResponse: string(compared)}
	_, candidateWins, baselineWins, reasons, _, err := gradeRuns(context.Background(), agent, Suite{Kind: harness.TargetSkill, Cases: cases}, results, Config{Trials: 1, Timeout: time.Second}, root)
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
	if !evidenceQuotesArtifact(`Excerpt: "first\nsecond"`, "first\nsecond") {
		t.Fatal("escaped newline in quoted evidence was not matched to the artifact")
	}
	if !evidenceQuotesArtifact("Observed `actual output`.", "actual output") {
		t.Fatal("backtick-quoted evidence was not matched to the artifact")
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
