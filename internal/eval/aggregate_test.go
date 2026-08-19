package eval

import "testing"

func TestCaseAssertionsPassesMajorityAndStrictPolicies(t *testing.T) {
	for _, test := range []struct {
		name   string
		values []bool
		strict bool
		want   bool
	}{
		{name: "empty", values: nil, want: false},
		{name: "majority pass", values: []bool{true, false, true}, want: true},
		{name: "majority fail", values: []bool{true, false, false}, want: false},
		{name: "strict pass", values: []bool{true, true}, strict: true, want: true},
		{name: "strict fail", values: []bool{true, false}, strict: true, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := caseAssertionsPass(test.values, test.strict); got != test.want {
				t.Fatalf("caseAssertionsPass(%v, %v) = %v, want %v", test.values, test.strict, got, test.want)
			}
		})
	}
}

func TestBuildBenchmarkClassifiesAssertionsAndStatistics(t *testing.T) {
	runs := []gradedRun{
		{CaseID: "one", Variant: variantWithSkill, PassRate: 1, TimeSeconds: 2, Tokens: 10, AssertionResult: []AssertionResult{{Text: "claim", Passed: true}}},
		{CaseID: "one", Variant: variantWithoutSkill, PassRate: 0, TimeSeconds: 4, Tokens: 20, AssertionResult: []AssertionResult{{Text: "claim", Passed: false}}},
		{CaseID: "two", Variant: variantWithSkill, PassRate: 0, TimeSeconds: 3, Tokens: 11, AssertionResult: []AssertionResult{{Text: "claim", Passed: false}}},
		{CaseID: "two", Variant: variantWithoutSkill, PassRate: 0, TimeSeconds: 5, Tokens: 21, AssertionResult: []AssertionResult{{Text: "claim", Passed: false}}},
	}
	benchmark := buildBenchmark(runs)
	if benchmark.RunSummary.WithSkill == nil || benchmark.RunSummary.WithoutSkill == nil {
		t.Fatalf("skill benchmark summary = %#v", benchmark.RunSummary)
	}
	if len(benchmark.AssertionAnalysis) != 2 {
		t.Fatalf("assertion analysis = %#v", benchmark.AssertionAnalysis)
	}
	if benchmark.AssertionAnalysis[0].Category == "mixed" {
		t.Fatalf("assertion analysis did not classify pass/fail categories: %#v", benchmark.AssertionAnalysis)
	}
	if statistic([]float64{1, 3}).Mean != 2 || statistic([]float64{1, 3}).Stddev != 1 {
		t.Fatalf("statistic = %#v", statistic([]float64{1, 3}))
	}
	if got := countRate(nil); got != 0 {
		t.Fatalf("countRate(nil) = %v", got)
	}
}

func TestBuildBenchmarkSelectsInstructionVariants(t *testing.T) {
	runs := []gradedRun{
		{CaseID: "one", Variant: variantWithInstructions, PassRate: 1, AssertionResult: []AssertionResult{{Text: "claim", Passed: true}}},
		{CaseID: "one", Variant: variantWithoutInstructions, PassRate: 0, AssertionResult: []AssertionResult{{Text: "claim", Passed: false}}},
	}
	benchmark := buildBenchmark(runs)
	if benchmark.RunSummary.WithInstructions == nil || benchmark.RunSummary.WithoutInstructions == nil || benchmark.RunSummary.WithSkill != nil {
		t.Fatalf("instruction benchmark summary = %#v", benchmark.RunSummary)
	}
}
