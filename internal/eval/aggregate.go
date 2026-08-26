package eval

import (
	"math"
	"sort"

	"github.com/shunk031/shuhari/internal/harness"
)

type Statistic struct {
	Mean   float64 `json:"mean"`
	Stddev float64 `json:"stddev"`
}

type BenchmarkConfiguration struct {
	PassRate    Statistic `json:"pass_rate"`
	TimeSeconds Statistic `json:"time_seconds"`
	Tokens      Statistic `json:"tokens"`
}

type BenchmarkDelta struct {
	PassRate    float64 `json:"pass_rate"`
	TimeSeconds float64 `json:"time_seconds"`
	Tokens      float64 `json:"tokens"`
}

type BenchmarkSummary struct {
	WithSkill           *BenchmarkConfiguration `json:"with_skill,omitempty"`
	WithoutSkill        *BenchmarkConfiguration `json:"without_skill,omitempty"`
	WithInstructions    *BenchmarkConfiguration `json:"with_instructions,omitempty"`
	WithoutInstructions *BenchmarkConfiguration `json:"without_instructions,omitempty"`
	Delta               BenchmarkDelta          `json:"delta"`
}

type Benchmark struct {
	SchemaVersion     string                      `json:"schema_version"`
	Mode              harness.Mode                `json:"mode"`
	Security          *harness.SecurityResolution `json:"security"`
	RunSummary        BenchmarkSummary            `json:"run_summary"`
	AssertionAnalysis []AssertionAnalysis         `json:"assertion_analysis"`
}

type AssertionAnalysis struct {
	CaseID          string  `json:"case_id"`
	Assertion       string  `json:"assertion"`
	WithPassRate    float64 `json:"with_pass_rate"`
	WithoutPassRate float64 `json:"without_pass_rate"`
	Category        string  `json:"category"`
}

func caseAssertionsPass(trials []bool, strict bool) bool {
	if len(trials) == 0 {
		return false
	}
	if strict {
		for _, passed := range trials {
			if !passed {
				return false
			}
		}
		return true
	}
	passed := 0
	for _, value := range trials {
		if value {
			passed++
		}
	}
	return passed >= len(trials)/2+1
}

func buildBenchmark(runs []gradedRun) Benchmark {
	byVariant := map[string][]gradedRun{}
	for _, run := range runs {
		byVariant[run.Variant] = append(byVariant[run.Variant], run)
	}
	withName, withoutName := variantWithSkill, variantWithoutSkill
	if len(byVariant[variantWithInstructions]) > 0 {
		withName, withoutName = variantWithInstructions, variantWithoutInstructions
	}
	with := configurationStatistics(byVariant[withName])
	without := configurationStatistics(byVariant[withoutName])
	summary := BenchmarkSummary{
		Delta: BenchmarkDelta{
			PassRate:    with.PassRate.Mean - without.PassRate.Mean,
			TimeSeconds: with.TimeSeconds.Mean - without.TimeSeconds.Mean,
			Tokens:      with.Tokens.Mean - without.Tokens.Mean,
		},
	}
	if withName == variantWithSkill {
		summary.WithSkill, summary.WithoutSkill = &with, &without
	} else {
		summary.WithInstructions, summary.WithoutInstructions = &with, &without
	}
	return Benchmark{SchemaVersion: benchmarkSchemaVersion, RunSummary: summary, AssertionAnalysis: analyzeAssertions(runs, withName, withoutName)}
}

type assertionCounts struct {
	passed int
	total  int
}

func analyzeAssertions(runs []gradedRun, withVariant, withoutVariant string) []AssertionAnalysis {
	byKey := map[string]map[string]*assertionCounts{}
	labels := map[string]struct{ caseID, assertion string }{}
	for _, run := range runs {
		for _, result := range run.AssertionResult {
			key := run.CaseID + "\x00" + result.Text
			labels[key] = struct{ caseID, assertion string }{run.CaseID, result.Text}
			if byKey[key] == nil {
				byKey[key] = map[string]*assertionCounts{}
			}
			if byKey[key][run.Variant] == nil {
				byKey[key][run.Variant] = &assertionCounts{}
			}
			value := byKey[key][run.Variant]
			value.total++
			if result.Passed {
				value.passed++
			}
		}
	}
	keys := make([]string, 0, len(byKey))
	for key := range byKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	analysis := make([]AssertionAnalysis, 0, len(keys))
	for _, key := range keys {
		withRate := countRate(byKey[key][withVariant])
		withoutRate := countRate(byKey[key][withoutVariant])
		category := "mixed"
		switch {
		case withRate == 1 && withoutRate == 1:
			category = "always_pass_both"
		case withRate == 0 && withoutRate == 0:
			category = "always_fail_both"
		case withRate == 1 && withoutRate == 0:
			category = "pass_with_fail_without"
		}
		label := labels[key]
		analysis = append(analysis, AssertionAnalysis{CaseID: label.caseID, Assertion: label.assertion, WithPassRate: withRate, WithoutPassRate: withoutRate, Category: category})
	}
	return analysis
}

func countRate(value *assertionCounts) float64 {
	if value == nil || value.total == 0 {
		return 0
	}
	return float64(value.passed) / float64(value.total)
}

func configurationStatistics(runs []gradedRun) BenchmarkConfiguration {
	passRates := make([]float64, 0, len(runs))
	durations := make([]float64, 0, len(runs))
	tokens := make([]float64, 0, len(runs))
	for _, run := range runs {
		passRates = append(passRates, run.PassRate)
		durations = append(durations, run.TimeSeconds)
		tokens = append(tokens, run.Tokens)
	}
	return BenchmarkConfiguration{PassRate: statistic(passRates), TimeSeconds: statistic(durations), Tokens: statistic(tokens)}
}

func statistic(values []float64) Statistic {
	if len(values) == 0 {
		return Statistic{}
	}
	var total float64
	for _, value := range values {
		total += value
	}
	mean := total / float64(len(values))
	var variance float64
	for _, value := range values {
		difference := value - mean
		variance += difference * difference
	}
	return Statistic{Mean: mean, Stddev: math.Sqrt(variance / float64(len(values)))}
}
