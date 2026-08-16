package eval

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
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

//go:embed prompts/comparator.md
var comparatorPrompt string

type blindMapping struct {
	A string `json:"A"`
	B string `json:"B"`
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
}

type judgeOutput struct {
	Cases []judgeEntry `json:"cases"`
}

type comparatorInput struct {
	ID             string   `json:"id"`
	Trial          int      `json:"trial"`
	Prompt         string   `json:"prompt"`
	ExpectedOutput string   `json:"expected_output"`
	Assertions     []string `json:"assertions"`
	A              string   `json:"A"`
	B              string   `json:"B"`
}

type comparatorEntry struct {
	ID        string `json:"id"`
	Trial     int    `json:"trial"`
	Preferred string `json:"preferred"`
	Reason    string `json:"reason"`
}

type comparatorOutput struct {
	Cases []comparatorEntry `json:"cases"`
}

type rawJudgeEvidence struct {
	Grader     string `json:"grader"`
	Comparator string `json:"comparator"`
}

type judgeRetryError struct {
	cause     error
	responses []string
}

func (e *judgeRetryError) Error() string { return e.cause.Error() }

func (e *judgeRetryError) Unwrap() error { return e.cause }

type trialJudgeInputs struct {
	ID         string
	Trial      int
	Grader     judgeInput
	Comparator comparatorInput
}

func gradeRuns(ctx context.Context, agent harness.Harness, suite Suite, results []runResult, config Config, iteration string) ([]gradedRun, int, int, []string, string, error) {
	byKey := map[string]runResult{}
	for _, result := range results {
		byKey[runKey(result.Case.ID, result.Trial, result.Variant)] = result
	}
	trialInputs := make([]trialJudgeInputs, 0, len(suite.Cases)*config.Trials)
	mappings := map[string]blindMapping{}
	withVariant, withoutVariant := variantsFor(suite.Kind)
	for _, item := range suite.Cases {
		for trial := 1; trial <= config.Trials; trial++ {
			with, withOK := byKey[runKey(item.ID, trial, withVariant)]
			without, withoutOK := byKey[runKey(item.ID, trial, withoutVariant)]
			if !withOK || !withoutOK {
				err := fmt.Errorf("missing run for case %s trial %d", item.ID, trial)
				persistGradingError(iteration, "prepare", "", "", mappings, err)
				return nil, 0, 0, nil, "", err
			}
			mapping := blindLabels(item.ID, trial, withVariant, withoutVariant)
			mappings[caseTrialKey(item.ID, trial)] = mapping
			outputs := map[string]string{withVariant: with.Artifact, withoutVariant: without.Artifact}
			trialInputs = append(trialInputs, trialJudgeInputs{
				ID:         item.ID,
				Trial:      trial,
				Grader:     judgeInput{ID: item.ID, Trial: trial, Assertions: item.effectiveAssertions(), A: outputs[mapping.A], B: outputs[mapping.B]},
				Comparator: comparatorInput{ID: item.ID, Trial: trial, Prompt: item.Prompt, ExpectedOutput: item.ExpectedOutput, Assertions: item.effectiveAssertions(), A: outputs[mapping.A], B: outputs[mapping.B]},
			})
		}
	}

	graderEntries, graderResponse, err := runGradersPerTrial(ctx, agent, trialInputs, config)
	if err != nil {
		persistGradingError(iteration, "grader", graderResponse, "", mappings, err)
		return nil, 0, 0, nil, graderResponse, err
	}

	comparatorEntries, comparatorResponse, err := runComparatorsPerTrial(ctx, agent, trialInputs, config)
	rawEvidence, _ := json.Marshal(rawJudgeEvidence{Grader: graderResponse, Comparator: comparatorResponse})
	if err != nil {
		persistGradingError(iteration, "comparator", graderResponse, comparatorResponse, mappings, err)
		return nil, 0, 0, nil, string(rawEvidence), err
	}

	graded := make([]gradedRun, 0, len(results))
	candidateWins, baselineWins := 0, 0
	for _, item := range suite.Cases {
		for trial := 1; trial <= config.Trials; trial++ {
			key := caseTrialKey(item.ID, trial)
			gradeEntry := graderEntries[key]
			comparisonEntry := comparatorEntries[key]
			mapping := mappings[key]
			grades := map[string][]AssertionResult{mapping.A: gradeEntry.AAssertionResults, mapping.B: gradeEntry.BAssertionResults}
			for _, variant := range []string{withVariant, withoutVariant} {
				run := byKey[runKey(item.ID, trial, variant)]
				grading, buildErr := buildGrading(item.effectiveAssertions(), grades[variant], run.Artifact)
				if buildErr != nil {
					err = fmt.Errorf("case %s trial %d %s: %w", item.ID, trial, variant, buildErr)
					persistGradingError(iteration, "grader-validation", graderResponse, comparatorResponse, mappings, err)
					return nil, 0, 0, nil, string(rawEvidence), err
				}
				if err := writeJSON(filepath.Join(run.RunDir, "grading.json"), grading); err != nil {
					return nil, 0, 0, nil, string(rawEvidence), err
				}
				graded = append(graded, gradedRun{CaseID: item.ID, Trial: trial, Variant: variant, PassRate: grading.Summary.PassRate, Passed: grading.Summary.Failed == 0, TimeSeconds: run.Agent.Duration.Seconds(), Tokens: float64(run.Agent.Usage.TotalTokens()), AssertionResult: grading.AssertionResults})
			}
			winner := "tie"
			if comparisonEntry.Preferred == "A" {
				winner = mapping.A
			} else if comparisonEntry.Preferred == "B" {
				winner = mapping.B
			}
			comparison := Comparison{SchemaVersion: workspaceSchemaVersion, ID: item.ID, Trial: trial, A: mapping.A, B: mapping.B, Preferred: comparisonEntry.Preferred, PreferredVariant: winner, Reason: comparisonEntry.Reason}
			path := comparisonPath(iteration, item.ID, trial)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return nil, 0, 0, nil, string(rawEvidence), fmt.Errorf("create comparison directory: %w", err)
			}
			if err := writeJSON(path, comparison); err != nil {
				return nil, 0, 0, nil, string(rawEvidence), err
			}
			switch winner {
			case withVariant:
				candidateWins++
			case withoutVariant:
				baselineWins++
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
	return graded, candidateWins, baselineWins, nil, string(rawEvidence), nil
}

func runGradersPerTrial(ctx context.Context, agent harness.Harness, inputs []trialJudgeInputs, config Config) (map[string]judgeEntry, string, error) {
	merged := judgeOutput{}
	entries := map[string]judgeEntry{}
	for _, item := range inputs {
		graderInputs := []judgeInput{item.Grader}
		var output judgeOutput
		var trialEntries map[string]judgeEntry
		response, err := runValidatedStructuredJudge(ctx, agent, graderPrompt, graderInputs, graderSchema(), config, func(response string) error {
			output = judgeOutput{}
			if err := json.Unmarshal([]byte(response), &output); err != nil {
				return fmt.Errorf("decode response: %w", err)
			}
			var err error
			trialEntries, err = validateGraderEntries(output, graderInputs)
			if err != nil {
				return err
			}
			entry := trialEntries[caseTrialKey(item.ID, item.Trial)]
			if _, err := buildGrading(item.Grader.Assertions, entry.AAssertionResults, item.Grader.A); err != nil {
				return fmt.Errorf("validate A evidence: %w", err)
			}
			if _, err := buildGrading(item.Grader.Assertions, entry.BAssertionResults, item.Grader.B); err != nil {
				return fmt.Errorf("validate B evidence: %w", err)
			}
			return nil
		})
		if err != nil {
			return nil, response, fmt.Errorf("grader case %q trial %d: %w", item.ID, item.Trial, err)
		}
		for key, entry := range trialEntries {
			entries[key] = entry
		}
		merged.Cases = append(merged.Cases, output.Cases...)
	}
	encoded, err := json.Marshal(merged)
	if err != nil {
		return nil, "", fmt.Errorf("encode grader response: %w", err)
	}
	return entries, string(encoded), nil
}

func runComparatorsPerTrial(ctx context.Context, agent harness.Harness, inputs []trialJudgeInputs, config Config) (map[string]comparatorEntry, string, error) {
	merged := comparatorOutput{}
	entries := map[string]comparatorEntry{}
	for _, item := range inputs {
		comparatorInputs := []comparatorInput{item.Comparator}
		var output comparatorOutput
		var trialEntries map[string]comparatorEntry
		response, err := runValidatedStructuredJudge(ctx, agent, comparatorPrompt, comparatorInputs, comparatorSchema(), config, func(response string) error {
			output = comparatorOutput{}
			if err := json.Unmarshal([]byte(response), &output); err != nil {
				return fmt.Errorf("decode response: %w", err)
			}
			var err error
			trialEntries, err = validateComparatorEntries(output, comparatorInputs)
			return err
		})
		if err != nil {
			return nil, response, fmt.Errorf("comparator case %q trial %d: %w", item.ID, item.Trial, err)
		}
		for key, entry := range trialEntries {
			entries[key] = entry
		}
		merged.Cases = append(merged.Cases, output.Cases...)
	}
	encoded, err := json.Marshal(merged)
	if err != nil {
		return nil, "", fmt.Errorf("encode comparator response: %w", err)
	}
	return entries, string(encoded), nil
}

func runValidatedStructuredJudge(ctx context.Context, agent harness.Harness, instructions string, input any, schema []byte, config Config, validate func(string) error) (string, error) {
	var response string
	responses := make([]string, 0, 2)
	var validationErr error
	for attempt := 0; attempt < 2; attempt++ {
		attemptInstructions := instructions
		if validationErr != nil {
			attemptInstructions = strings.TrimSpace(instructions) + "\n\nThe previous response failed validation. Return a corrected response for the original input.\nValidation error: " + validationErr.Error()
		}
		var err error
		response, err = runStructuredJudge(ctx, agent, attemptInstructions, input, schema, config)
		responses = append(responses, response)
		if err != nil {
			if len(responses) > 1 {
				return response, &judgeRetryError{cause: err, responses: responses}
			}
			return response, err
		}
		validationErr = validate(response)
		if validationErr == nil {
			return response, nil
		}
	}
	return response, &judgeRetryError{cause: validationErr, responses: responses}
}

func runStructuredJudge(ctx context.Context, agent harness.Harness, instructions string, input any, schema []byte, config Config) (string, error) {
	prompt, err := structuredJudgePrompt(instructions, input)
	if err != nil {
		return "", err
	}
	workDir, err := os.MkdirTemp("", "shuhari-judge-")
	if err != nil {
		return "", fmt.Errorf("create judge work directory: %w", err)
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
	judged, err := agent.Run(ctx, harness.Request{WorkDir: workDir, Prompt: prompt, Model: model, ReasoningEffort: effort, Sandbox: "read-only", Timeout: config.Timeout, OutputSchema: schema})
	if err != nil {
		return judged.Response, fmt.Errorf("run judge; prompt is %d bytes: %w", len(prompt), err)
	}
	return judged.Response, nil
}

func structuredJudgePrompt(instructions string, input any) (string, error) {
	encoded, err := json.Marshal(input)
	if err != nil {
		return "", fmt.Errorf("encode judge input: %w", err)
	}
	return strings.TrimSpace(instructions) + "\n\n" + string(encoded), nil
}

func validateGraderEntries(output judgeOutput, inputs []judgeInput) (map[string]judgeEntry, error) {
	if len(output.Cases) != len(inputs) {
		return nil, fmt.Errorf("grader returned %d cases, want %d", len(output.Cases), len(inputs))
	}
	entries := map[string]judgeEntry{}
	for _, entry := range output.Cases {
		key := caseTrialKey(entry.ID, entry.Trial)
		if _, exists := entries[key]; exists {
			return nil, fmt.Errorf("grader returned duplicate case %s", key)
		}
		entries[key] = entry
	}
	for _, input := range inputs {
		if _, ok := entries[caseTrialKey(input.ID, input.Trial)]; !ok {
			return nil, fmt.Errorf("grader omitted case %s", caseTrialKey(input.ID, input.Trial))
		}
	}
	return entries, nil
}

func validateComparatorEntries(output comparatorOutput, inputs []comparatorInput) (map[string]comparatorEntry, error) {
	if len(output.Cases) != len(inputs) {
		return nil, fmt.Errorf("comparator returned %d cases, want %d", len(output.Cases), len(inputs))
	}
	entries := map[string]comparatorEntry{}
	for _, entry := range output.Cases {
		key := caseTrialKey(entry.ID, entry.Trial)
		if _, exists := entries[key]; exists {
			return nil, fmt.Errorf("comparator returned duplicate case %s", key)
		}
		if entry.Preferred != "A" && entry.Preferred != "B" && entry.Preferred != "tie" {
			return nil, fmt.Errorf("comparator returned invalid preferred value %q", entry.Preferred)
		}
		if strings.TrimSpace(entry.Reason) == "" {
			return nil, fmt.Errorf("comparator returned a blank reason for %s", key)
		}
		entries[key] = entry
	}
	for _, input := range inputs {
		if _, ok := entries[caseTrialKey(input.ID, input.Trial)]; !ok {
			return nil, fmt.Errorf("comparator omitted case %s", caseTrialKey(input.ID, input.Trial))
		}
	}
	return entries, nil
}

func buildGrading(expected []string, actual []AssertionResult, artifact string) (Grading, error) {
	if len(expected) != len(actual) {
		return Grading{}, fmt.Errorf("%w: grader returned %d assertions, want %d", errInvalidGrading, len(actual), len(expected))
	}
	byText := map[string]AssertionResult{}
	for _, result := range actual {
		if strings.TrimSpace(result.Text) == "" || strings.TrimSpace(result.Evidence) == "" {
			return Grading{}, fmt.Errorf("%w: assertion text and evidence must be nonblank", errInvalidGrading)
		}
		if _, exists := byText[result.Text]; exists {
			return Grading{}, fmt.Errorf("%w: duplicate assertion %q", errInvalidGrading, result.Text)
		}
		byText[result.Text] = result
	}
	ordered := make([]AssertionResult, 0, len(expected))
	summary := GradingSummary{Total: len(expected)}
	for _, assertion := range expected {
		result, ok := byText[assertion]
		if !ok {
			return Grading{}, fmt.Errorf("%w: missing assertion %q", errInvalidGrading, assertion)
		}
		if result.Passed && !evidenceQuotesArtifact(result.Evidence, artifact) {
			return Grading{}, fmt.Errorf("%w: passing assertion %q lacks a quoted observation from the artifact", errInvalidGrading, assertion)
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
	return Grading{AssertionResults: ordered, Summary: summary}, nil
}

func evidenceQuotesArtifact(evidence, artifact string) bool {
	normalizedArtifact := normalizeEvidenceText(artifact)
	for _, observation := range quotedObservations(evidence) {
		quoted := normalizeEvidenceText(decodeQuotedObservation(observation))
		if quoted != "" && strings.Contains(normalizedArtifact, quoted) {
			return true
		}
	}
	return false
}

func decodeQuotedObservation(value string) string {
	var decoded strings.Builder
	for index := 0; index < len(value); index++ {
		if value[index] != '\\' || index+1 == len(value) {
			decoded.WriteByte(value[index])
			continue
		}
		if strings.HasPrefix(value[index:], `\\\n`) {
			decoded.WriteByte('\\')
			decoded.WriteByte('\n')
			index += 3
			continue
		}
		switch value[index+1] {
		case 'n':
			decoded.WriteByte('\n')
		case 't':
			decoded.WriteByte('\t')
		case '"':
			decoded.WriteByte('"')
		default:
			decoded.WriteByte(value[index])
			continue
		}
		index++
	}
	return decoded.String()
}

func normalizeEvidenceText(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\\\n", " ")
	value = strings.ReplaceAll(value, "`", "")
	return strings.Join(strings.Fields(value), " ")
}

func quotedObservations(evidence string) []string {
	runes := []rune(evidence)
	observations := make([]string, 0, 1)
	for index := 0; index < len(runes); index++ {
		opening := runes[index]
		closing := rune(0)
		switch opening {
		case '"':
			closing = '"'
		case '“':
			closing = '”'
		default:
			continue
		}
		for end := index + 1; end < len(runes); end++ {
			if runes[end] != closing || (closing == '"' && escapedQuote(runes, end)) {
				continue
			}
			observations = append(observations, string(runes[index+1:end]))
			index = end
			break
		}
	}
	return observations
}

func escapedQuote(value []rune, index int) bool {
	backslashes := 0
	for index--; index >= 0 && value[index] == '\\'; index-- {
		backslashes++
	}
	return backslashes%2 == 1
}

func blindLabels(id string, trial int, with, without string) blindMapping {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", id, trial)))
	if digest[0]&1 == 0 {
		return blindMapping{A: with, B: without}
	}
	return blindMapping{A: without, B: with}
}

func promptDigest(prompt string) string {
	digest := sha256.Sum256([]byte(prompt))
	return hex.EncodeToString(digest[:])
}

func graderSchema() []byte {
	return []byte(`{"type":"object","properties":{"cases":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"},"trial":{"type":"integer"},"A_assertion_results":{"$ref":"#/$defs/assertion_results"},"B_assertion_results":{"$ref":"#/$defs/assertion_results"}},"required":["id","trial","A_assertion_results","B_assertion_results"],"additionalProperties":false}}},"required":["cases"],"additionalProperties":false,"$defs":{"assertion_results":{"type":"array","items":{"type":"object","properties":{"text":{"type":"string","minLength":1},"passed":{"type":"boolean"},"evidence":{"type":"string","minLength":1,"pattern":".*[\"“”].*"}},"required":["text","passed","evidence"],"additionalProperties":false}}}}`)
}

func comparatorSchema() []byte {
	return []byte(`{"type":"object","properties":{"cases":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"},"trial":{"type":"integer"},"preferred":{"type":"string","enum":["A","B","tie"]},"reason":{"type":"string","minLength":1}},"required":["id","trial","preferred","reason"],"additionalProperties":false}}},"required":["cases"],"additionalProperties":false}`)
}

func comparisonPath(iteration, caseID string, trial int) string {
	caseDir := filepath.Join(iteration, "eval-"+safeName(caseID))
	if trial == 1 {
		return filepath.Join(caseDir, "comparison.json")
	}
	return filepath.Join(caseDir, "comparisons", fmt.Sprintf("%d.json", trial))
}

func persistGradingError(iteration, stage, grader, comparator string, mappings map[string]blindMapping, cause error) {
	if iteration == "" {
		return
	}
	var responses []string
	var retryError *judgeRetryError
	if errors.As(cause, &retryError) {
		responses = retryError.responses
	}
	_ = writeJSON(filepath.Join(iteration, "grading-error.json"), struct {
		SchemaVersion string                  `json:"schema_version"`
		Stage         string                  `json:"stage"`
		Error         string                  `json:"error"`
		Grader        string                  `json:"grader_response,omitempty"`
		Comparator    string                  `json:"comparator_response,omitempty"`
		Responses     []string                `json:"judge_responses,omitempty"`
		Mappings      map[string]blindMapping `json:"blind_mappings"`
	}{SchemaVersion: workspaceSchemaVersion, Stage: stage, Error: cause.Error(), Grader: grader, Comparator: comparator, Responses: responses, Mappings: mappings})
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
