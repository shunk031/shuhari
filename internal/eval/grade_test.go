package eval

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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
	results := make([]runResult, 0, len(cases)*2)
	for index, item := range cases {
		mapping := blindLabels(item.ID, 1, variantWithSkill, variantWithoutSkill)
		preferredVariant := variantWithSkill
		if index == len(cases)-1 {
			preferredVariant = variantWithoutSkill
		}
		passing := []AssertionResult{{Text: "correct", Passed: true, Evidence: "present"}}
		entries = append(entries, judgeEntry{
			ID: item.ID, Trial: 1,
			AAssertionResults: passing, BAssertionResults: passing,
			Preferred: preferredLabel(mapping, preferredVariant), Reason: "comparison",
		})
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
	agent := &fakeHarness{judgeResponse: string(encoded)}
	_, candidateWins, baselineWins, reasons, _, err := gradeRuns(context.Background(), agent, Suite{Kind: harness.TargetSkill, Cases: cases}, results, Config{Trials: 1, Timeout: time.Second})
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
