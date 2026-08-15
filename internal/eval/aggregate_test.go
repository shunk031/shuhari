package eval

import (
	"encoding/json"
	"testing"
)

func TestCaseAssertionsPass(t *testing.T) {
	t.Parallel()

	if !caseAssertionsPass([]bool{true, false, true}, false) {
		t.Fatal("2/3 majority did not pass")
	}
	if caseAssertionsPass([]bool{true, false, true}, true) {
		t.Fatal("strict policy accepted a failed trial")
	}
	if caseAssertionsPass(nil, false) {
		t.Fatal("empty trials passed")
	}
}

func TestBenchmarkDelta(t *testing.T) {
	t.Parallel()

	benchmark := buildBenchmark([]gradedRun{
		{Variant: variantWithSkill, PassRate: 1, TimeSeconds: 1, Tokens: 100},
		{Variant: variantWithoutSkill, PassRate: 0.5, TimeSeconds: 0.6, Tokens: 60},
	})
	if benchmark.RunSummary.Delta.PassRate != 0.5 {
		t.Fatalf("pass-rate delta = %v, want 0.5", benchmark.RunSummary.Delta.PassRate)
	}
	if benchmark.RunSummary.Delta.TimeSeconds != 0.4 {
		t.Fatalf("time delta = %v, want 0.4", benchmark.RunSummary.Delta.TimeSeconds)
	}
}

func TestInstructionsBenchmarkOmitsSkillConfigurations(t *testing.T) {
	t.Parallel()

	benchmark := buildBenchmark([]gradedRun{
		{Variant: variantWithInstructions, PassRate: 1},
		{Variant: variantWithoutInstructions, PassRate: 0},
	})
	encoded, err := json.Marshal(benchmark)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if err := json.Unmarshal(encoded, &object); err != nil {
		t.Fatal(err)
	}
	summary := object["run_summary"].(map[string]any)
	if _, exists := summary["with_skill"]; exists {
		t.Fatalf("instructions benchmark contains with_skill: %s", encoded)
	}
	if _, exists := summary["without_skill"]; exists {
		t.Fatalf("instructions benchmark contains without_skill: %s", encoded)
	}
}
