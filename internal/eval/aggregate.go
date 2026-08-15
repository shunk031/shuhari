package eval

import "math"

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
	RunSummary BenchmarkSummary `json:"run_summary"`
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
	return Benchmark{RunSummary: summary}
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
