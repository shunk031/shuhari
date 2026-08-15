package eval

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/shunk031/shuhari/internal/harness"
)

//go:embed prompts/grader.md
var graderPrompt string

type blindMapping struct {
	A string
	B string
}

type judgeInput struct {
	ID         string   `json:"id"`
	Trial      int      `json:"trial"`
	Assertions []string `json:"assertions"`
	A          string   `json:"A"`
	B          string   `json:"B"`
}

type judgeEntry struct {
	ID                string            `json:"id"`
	Trial             int               `json:"trial"`
	AAssertionResults []AssertionResult `json:"A_assertion_results"`
	BAssertionResults []AssertionResult `json:"B_assertion_results"`
	Preferred         string            `json:"preferred"`
	Reason            string            `json:"reason"`
}

type judgeOutput struct {
	Cases []judgeEntry `json:"cases"`
}

func gradeRuns(ctx context.Context, agent harness.Harness, suite Suite, results []runResult, config Config) ([]gradedRun, int, int, []string, string, error) {
	byKey := map[string]runResult{}
	for _, result := range results {
		byKey[runKey(result.Case.ID, result.Trial, result.Variant)] = result
	}
	inputs := make([]judgeInput, 0, len(suite.Cases)*config.Trials)
	mappings := map[string]blindMapping{}
	withVariant, withoutVariant := variantsFor(suite.Kind)
	for _, item := range suite.Cases {
		for trial := 1; trial <= config.Trials; trial++ {
			with, withOK := byKey[runKey(item.ID, trial, withVariant)]
			without, withoutOK := byKey[runKey(item.ID, trial, withoutVariant)]
			if !withOK || !withoutOK {
				return nil, 0, 0, nil, "", fmt.Errorf("missing run for case %s trial %d", item.ID, trial)
			}
			mapping := blindLabels(item.ID, trial, withVariant, withoutVariant)
			mappings[caseTrialKey(item.ID, trial)] = mapping
			outputs := map[string]string{withVariant: with.Artifact, withoutVariant: without.Artifact}
			inputs = append(inputs, judgeInput{ID: item.ID, Trial: trial, Assertions: item.effectiveAssertions(), A: outputs[mapping.A], B: outputs[mapping.B]})
		}
	}
	encoded, err := json.Marshal(inputs)
	if err != nil {
		return nil, 0, 0, nil, "", fmt.Errorf("encode grader input: %w", err)
	}
	prompt := strings.TrimSpace(graderPrompt) + "\n\n" + string(encoded)
	workDir, err := os.MkdirTemp("", "shuhari-grader-")
	if err != nil {
		return nil, 0, 0, nil, "", fmt.Errorf("create grader work directory: %w", err)
	}
	defer os.RemoveAll(workDir)
	model := config.JudgeModel
	if model == "" {
		model = config.Model
	}
	effort := config.JudgeReasoningEffort
	if effort == "" {
		effort = config.ReasoningEffort
	}
	judged, err := agent.Run(ctx, harness.Request{WorkDir: workDir, Prompt: prompt, Model: model, ReasoningEffort: effort, Sandbox: "read-only", Timeout: config.Timeout, OutputSchema: judgeSchema()})
	if err != nil {
		return nil, 0, 0, nil, "", fmt.Errorf("run grader: %w", err)
	}
	var output judgeOutput
	if err := json.Unmarshal([]byte(judged.Response), &output); err != nil {
		return nil, 0, 0, nil, judged.Response, fmt.Errorf("decode grader response: %w", err)
	}
	if len(output.Cases) != len(inputs) {
		return nil, 0, 0, nil, judged.Response, fmt.Errorf("grader returned %d cases, want %d", len(output.Cases), len(inputs))
	}
	entries := map[string]judgeEntry{}
	for _, entry := range output.Cases {
		key := caseTrialKey(entry.ID, entry.Trial)
		if _, exists := entries[key]; exists {
			return nil, 0, 0, nil, judged.Response, fmt.Errorf("grader returned duplicate case %s", key)
		}
		entries[key] = entry
	}
	graded := make([]gradedRun, 0, len(results))
	candidateWins, baselineWins := 0, 0
	var reasons []string
	for _, item := range suite.Cases {
		for trial := 1; trial <= config.Trials; trial++ {
			key := caseTrialKey(item.ID, trial)
			entry, ok := entries[key]
			if !ok {
				return nil, 0, 0, nil, judged.Response, fmt.Errorf("grader omitted case %s", key)
			}
			mapping := mappings[key]
			grades := map[string][]AssertionResult{mapping.A: entry.AAssertionResults, mapping.B: entry.BAssertionResults}
			for _, variant := range []string{withVariant, withoutVariant} {
				run := byKey[runKey(item.ID, trial, variant)]
				assertions := grades[variant]
				grading, valid := buildGrading(item.effectiveAssertions(), assertions)
				grading.Preferred = entry.Preferred
				grading.Reason = entry.Reason
				if err := writeJSON(filepath.Join(run.RunDir, "grading.json"), grading); err != nil {
					return nil, 0, 0, nil, judged.Response, err
				}
				graded = append(graded, gradedRun{CaseID: item.ID, Trial: trial, Variant: variant, PassRate: grading.Summary.PassRate, Passed: valid && grading.Summary.Failed == 0, TimeSeconds: run.Agent.Duration.Seconds(), Tokens: float64(run.Agent.Usage.TotalTokens())})
			}
			preferred := entry.Preferred
			if preferred == "A" || preferred == "B" {
				winner := mapping.A
				if preferred == "B" {
					winner = mapping.B
				}
				if winner == withVariant {
					candidateWins++
				} else {
					baselineWins++
				}
			} else if preferred != "tie" {
				return nil, 0, 0, nil, judged.Response, fmt.Errorf("grader returned invalid preferred value %q", preferred)
			}
		}
	}
	sort.Slice(graded, func(i, j int) bool {
		if graded[i].CaseID != graded[j].CaseID {
			return graded[i].CaseID < graded[j].CaseID
		}
		if graded[i].Trial != graded[j].Trial {
			return graded[i].Trial < graded[j].Trial
		}
		return graded[i].Variant < graded[j].Variant
	})
	return graded, candidateWins, baselineWins, reasons, judged.Response, nil
}

func buildGrading(expected []string, actual []AssertionResult) (Grading, bool) {
	valid := len(expected) == len(actual)
	byText := map[string]AssertionResult{}
	for _, result := range actual {
		if _, exists := byText[result.Text]; exists {
			valid = false
		}
		byText[result.Text] = result
	}
	ordered := make([]AssertionResult, 0, len(expected))
	summary := GradingSummary{Total: len(expected)}
	for _, assertion := range expected {
		result, ok := byText[assertion]
		if !ok {
			valid = false
			result = AssertionResult{Text: assertion, Passed: false, Evidence: "grader omitted this assertion"}
		}
		ordered = append(ordered, result)
		if result.Passed {
			summary.Passed++
		} else {
			summary.Failed++
		}
	}
	if summary.Total > 0 {
		summary.PassRate = float64(summary.Passed) / float64(summary.Total)
	}
	return Grading{AssertionResults: ordered, Summary: summary}, valid
}

func blindLabels(id string, trial int, with, without string) blindMapping {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", id, trial)))
	if digest[0]&1 == 0 {
		return blindMapping{A: with, B: without}
	}
	return blindMapping{A: without, B: with}
}

func judgeSchema() []byte {
	return []byte(`{
  "type": "object",
  "properties": {
    "cases": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "id": {"type": "string"},
          "trial": {"type": "integer"},
          "A_assertion_results": {"$ref": "#/$defs/assertion_results"},
          "B_assertion_results": {"$ref": "#/$defs/assertion_results"},
          "preferred": {"type": "string", "enum": ["A", "B", "tie"]},
          "reason": {"type": "string"}
        },
        "required": ["id", "trial", "A_assertion_results", "B_assertion_results", "preferred", "reason"],
        "additionalProperties": false
      }
    }
  },
  "required": ["cases"],
  "additionalProperties": false,
  "$defs": {
    "assertion_results": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "text": {"type": "string"},
          "passed": {"type": "boolean"},
          "evidence": {"type": "string"}
        },
        "required": ["text", "passed", "evidence"],
        "additionalProperties": false
      }
    }
  }
}`)
}

func runKey(id string, trial int, variant string) string {
	return fmt.Sprintf("%s\x00%d\x00%s", id, trial, variant)
}

func caseTrialKey(id string, trial int) string {
	return fmt.Sprintf("%s\x00%d", id, trial)
}

func variantsFor(kind harness.TargetKind) (string, string) {
	if kind == harness.TargetInstructions {
		return variantWithInstructions, variantWithoutInstructions
	}
	return variantWithSkill, variantWithoutSkill
}

var errInvalidGrading = errors.New("invalid grading")
