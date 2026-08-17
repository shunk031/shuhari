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
	AResponse  string   `json:"A_response"`
	BResponse  string   `json:"B_response"`
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
	AResponse      string   `json:"A_response"`
	BResponse      string   `json:"B_response"`
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

type judgeCallAttempts struct {
	ValidationAttempt int
	harness.AttemptEvidence
}

type judgeTransportRetry struct {
	Stage             string                 `json:"stage"`
	CaseID            string                 `json:"case_id"`
	Trial             int                    `json:"trial"`
	ValidationAttempt int                    `json:"validation_attempt"`
	AttemptCount      int                    `json:"attempt_count"`
	AttemptErrors     []harness.AttemptError `json:"attempt_errors"`
}

func (e *judgeRetryError) Error() string { return e.cause.Error() }

func (e *judgeRetryError) Unwrap() error { return e.cause }

type trialJudgeInputs struct {
	ID         string
	Trial      int
	Grader     judgeInput
	Comparator comparatorInput
}

type focusedGraderInput struct {
	ID          string   `json:"id"`
	Trial       int      `json:"trial"`
	Assertions  []string `json:"assertions"`
	A           string   `json:"A"`
	B           string   `json:"B"`
	AResponse   string   `json:"A_response"`
	BResponse   string   `json:"B_response"`
	AAssertions []string `json:"A_assertions"`
	BAssertions []string `json:"B_assertions"`
}

type judgeRetryRequest struct {
	Instructions string
	Input        any
}

type judgeRetryBuilder func(error) (judgeRetryRequest, bool)

func gradeRuns(ctx context.Context, agent harness.Harness, suite Suite, results []runResult, config Config, judgeSecurity harness.SecurityResolution, iteration string) ([]gradedRun, int, int, []string, string, error) {
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
			responses := map[string]string{withVariant: with.Agent.Response, withoutVariant: without.Agent.Response}
			trialInputs = append(trialInputs, trialJudgeInputs{
				ID:         item.ID,
				Trial:      trial,
				Grader:     judgeInput{ID: item.ID, Trial: trial, Assertions: item.effectiveAssertions(), A: outputs[mapping.A], B: outputs[mapping.B], AResponse: responses[mapping.A], BResponse: responses[mapping.B]},
				Comparator: comparatorInput{ID: item.ID, Trial: trial, Prompt: item.Prompt, ExpectedOutput: item.ExpectedOutput, Assertions: item.effectiveAssertions(), A: outputs[mapping.A], B: outputs[mapping.B], AResponse: responses[mapping.A], BResponse: responses[mapping.B]},
			})
		}
	}

	graderEntries, graderResponse, judgeRetries, err := runGradersPerTrial(ctx, agent, trialInputs, config, judgeSecurity)
	if err != nil {
		if writeErr := persistJudgeRetries(iteration, judgeRetries); writeErr != nil {
			return nil, 0, 0, nil, graderResponse, writeErr
		}
		persistGradingError(iteration, "grader", graderResponse, "", mappings, err)
		return nil, 0, 0, nil, graderResponse, err
	}

	comparatorEntries, comparatorResponse, comparatorRetries, err := runComparatorsPerTrial(ctx, agent, trialInputs, config, judgeSecurity)
	judgeRetries = append(judgeRetries, comparatorRetries...)
	rawEvidence, _ := json.Marshal(rawJudgeEvidence{Grader: graderResponse, Comparator: comparatorResponse})
	if err != nil {
		if writeErr := persistJudgeRetries(iteration, judgeRetries); writeErr != nil {
			return nil, 0, 0, nil, string(rawEvidence), writeErr
		}
		persistGradingError(iteration, "comparator", graderResponse, comparatorResponse, mappings, err)
		return nil, 0, 0, nil, string(rawEvidence), err
	}
	if err := persistJudgeRetries(iteration, judgeRetries); err != nil {
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
			comparison := Comparison{SchemaVersion: comparisonSchemaVersion, ID: item.ID, Trial: trial, A: mapping.A, B: mapping.B, Preferred: comparisonEntry.Preferred, PreferredVariant: winner, Reason: comparisonEntry.Reason}
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

func runGradersPerTrial(ctx context.Context, agent harness.Harness, inputs []trialJudgeInputs, config Config, security harness.SecurityResolution) (map[string]judgeEntry, string, []judgeTransportRetry, error) {
	merged := judgeOutput{}
	entries := map[string]judgeEntry{}
	var retries []judgeTransportRetry
	for _, item := range inputs {
		graderInputs := []judgeInput{item.Grader}
		var output judgeOutput
		var trialEntries map[string]judgeEntry
		var previousEntries map[string]judgeEntry
		var focused *focusedGraderInput
		focusedRetry := false
		response, attempts, err := runValidatedStructuredJudgeWithRetry(ctx, agent, graderPrompt, graderInputs, graderSchema(), config, security, func(response string) error {
			output = judgeOutput{}
			if err := json.Unmarshal([]byte(response), &output); err != nil {
				return fmt.Errorf("decode response: %w", err)
			}
			if focusedRetry {
				err := validateFocusedGraderResponse(output, focused, previousEntries, item.Grader, &trialEntries)
				if err == nil {
					output.Cases = []judgeEntry{trialEntries[caseTrialKey(item.ID, item.Trial)]}
				}
				return err
			}
			if previousEntries != nil {
				for index := range output.Cases {
					entry := &output.Cases[index]
					previous, ok := previousEntries[caseTrialKey(entry.ID, entry.Trial)]
					if !ok {
						continue
					}
					entry.AAssertionResults = preserveValidatedAssertionResults(item.Grader.Assertions, previous.AAssertionResults, entry.AAssertionResults, item.Grader.A)
					entry.BAssertionResults = preserveValidatedAssertionResults(item.Grader.Assertions, previous.BAssertionResults, entry.BAssertionResults, item.Grader.B)
				}
			}
			var err error
			trialEntries, err = validateGraderEntries(output, graderInputs)
			if err != nil {
				return err
			}
			previousEntries = trialEntries
			missingA := missingAssertions(item.Grader.Assertions, trialEntries[caseTrialKey(item.ID, item.Trial)].AAssertionResults)
			missingB := missingAssertions(item.Grader.Assertions, trialEntries[caseTrialKey(item.ID, item.Trial)].BAssertionResults)
			if len(missingA) > 0 || len(missingB) > 0 {
				focused = &focusedGraderInput{
					ID: item.Grader.ID, Trial: item.Grader.Trial, Assertions: mergeAssertionsInOrder(item.Grader.Assertions, missingA, missingB),
					A: item.Grader.A, B: item.Grader.B, AResponse: item.Grader.AResponse, BResponse: item.Grader.BResponse,
					AAssertions: missingA, BAssertions: missingB,
				}
				return fmt.Errorf("grader omitted required assertions for A=%v B=%v", missingA, missingB)
			}
			entry := trialEntries[caseTrialKey(item.ID, item.Trial)]
			if _, err := buildGrading(item.Grader.Assertions, entry.AAssertionResults, item.Grader.A); err != nil {
				return fmt.Errorf("validate A evidence: %w", err)
			}
			if _, err := buildGrading(item.Grader.Assertions, entry.BAssertionResults, item.Grader.B); err != nil {
				return fmt.Errorf("validate B evidence: %w", err)
			}
			return nil
		}, func(_ error) (judgeRetryRequest, bool) {
			if focused == nil {
				return judgeRetryRequest{}, false
			}
			focusedRetry = true
			return judgeRetryRequest{
				Instructions: strings.TrimSpace(graderPrompt) + "\n\nThe previous response omitted required assertion results. Return results only for the assertions listed in A_assertions and B_assertions; leave the other side's result array empty. Do not repeat any already validated assertion.\n",
				Input:        []focusedGraderInput{*focused},
			}, true
		})
		retries = append(retries, decorateJudgeAttempts("grader", item.ID, item.Trial, attempts)...)
		if err != nil {
			return nil, response, retries, fmt.Errorf("grader case %q trial %d: %w", item.ID, item.Trial, err)
		}
		for key, entry := range trialEntries {
			entries[key] = entry
		}
		merged.Cases = append(merged.Cases, output.Cases...)
	}
	encoded, err := json.Marshal(merged)
	if err != nil {
		return nil, "", retries, fmt.Errorf("encode grader response: %w", err)
	}
	return entries, string(encoded), retries, nil
}

func preserveValidatedAssertionResults(expected []string, previous, current []AssertionResult, artifact string) []AssertionResult {
	if len(previous) != len(expected) || len(current) != len(expected) {
		return current
	}
	previousByText := make(map[string]AssertionResult, len(previous))
	for _, result := range previous {
		if _, duplicate := previousByText[result.Text]; duplicate {
			return current
		}
		previousByText[result.Text] = result
	}
	currentByText := make(map[string]int, len(current))
	for index, result := range current {
		if _, duplicate := currentByText[result.Text]; duplicate {
			return current
		}
		currentByText[result.Text] = index
	}
	merged := append([]AssertionResult(nil), current...)
	for _, assertion := range expected {
		previousResult, previousOK := previousByText[assertion]
		currentIndex, currentOK := currentByText[assertion]
		if !previousOK || !currentOK {
			return current
		}
		if _, err := buildGrading([]string{assertion}, []AssertionResult{previousResult}, artifact); err == nil {
			merged[currentIndex] = previousResult
		}
	}
	return merged
}

func missingAssertions(expected []string, actual []AssertionResult) []string {
	seen := make(map[string]bool, len(actual))
	for _, result := range actual {
		seen[result.Text] = true
	}
	missing := make([]string, 0)
	for _, assertion := range expected {
		if !seen[assertion] {
			missing = append(missing, assertion)
		}
	}
	return missing
}

func mergeAssertionsInOrder(expected []string, sides ...[]string) []string {
	wanted := map[string]bool{}
	for _, assertions := range sides {
		for _, assertion := range assertions {
			wanted[assertion] = true
		}
	}
	merged := make([]string, 0, len(wanted))
	for _, assertion := range expected {
		if wanted[assertion] {
			merged = append(merged, assertion)
		}
	}
	return merged
}

func validateFocusedGraderResponse(output judgeOutput, focused *focusedGraderInput, previous map[string]judgeEntry, input judgeInput, destination *map[string]judgeEntry) error {
	if focused == nil {
		return errors.New("focused grader retry has no requested assertions")
	}
	entries, err := validateGraderEntries(output, []judgeInput{{ID: focused.ID, Trial: focused.Trial}})
	if err != nil {
		return err
	}
	entry := entries[caseTrialKey(focused.ID, focused.Trial)]
	if len(entry.AAssertionResults) != len(focused.AAssertions) || len(entry.BAssertionResults) != len(focused.BAssertions) {
		return fmt.Errorf("focused grader retry returned A=%d B=%d assertions, want A=%d B=%d", len(entry.AAssertionResults), len(entry.BAssertionResults), len(focused.AAssertions), len(focused.BAssertions))
	}
	if _, err := buildGrading(focused.AAssertions, entry.AAssertionResults, input.A); err != nil {
		return fmt.Errorf("validate focused A evidence: %w", err)
	}
	if _, err := buildGrading(focused.BAssertions, entry.BAssertionResults, input.B); err != nil {
		return fmt.Errorf("validate focused B evidence: %w", err)
	}
	prior, ok := previous[caseTrialKey(focused.ID, focused.Trial)]
	if !ok {
		return fmt.Errorf("focused grader retry has no prior case %s", caseTrialKey(focused.ID, focused.Trial))
	}
	merged := judgeEntry{ID: prior.ID, Trial: prior.Trial, AAssertionResults: append(append([]AssertionResult{}, prior.AAssertionResults...), entry.AAssertionResults...), BAssertionResults: append(append([]AssertionResult{}, prior.BAssertionResults...), entry.BAssertionResults...)}
	if _, err := buildGrading(input.Assertions, merged.AAssertionResults, input.A); err != nil {
		return fmt.Errorf("validate merged A evidence: %w", err)
	}
	if _, err := buildGrading(input.Assertions, merged.BAssertionResults, input.B); err != nil {
		return fmt.Errorf("validate merged B evidence: %w", err)
	}
	*destination = map[string]judgeEntry{caseTrialKey(merged.ID, merged.Trial): merged}
	output.Cases = []judgeEntry{merged}
	return nil
}

func runComparatorsPerTrial(ctx context.Context, agent harness.Harness, inputs []trialJudgeInputs, config Config, security harness.SecurityResolution) (map[string]comparatorEntry, string, []judgeTransportRetry, error) {
	merged := comparatorOutput{}
	entries := map[string]comparatorEntry{}
	var retries []judgeTransportRetry
	for _, item := range inputs {
		comparatorInputs := []comparatorInput{item.Comparator}
		var output comparatorOutput
		var trialEntries map[string]comparatorEntry
		response, attempts, err := runValidatedStructuredJudge(ctx, agent, comparatorPrompt, comparatorInputs, comparatorSchema(), config, security, func(response string) error {
			output = comparatorOutput{}
			if err := json.Unmarshal([]byte(response), &output); err != nil {
				return fmt.Errorf("decode response: %w", err)
			}
			var err error
			trialEntries, err = validateComparatorEntries(output, comparatorInputs)
			return err
		})
		retries = append(retries, decorateJudgeAttempts("comparator", item.ID, item.Trial, attempts)...)
		if err != nil {
			return nil, response, retries, fmt.Errorf("comparator case %q trial %d: %w", item.ID, item.Trial, err)
		}
		for key, entry := range trialEntries {
			entries[key] = entry
		}
		merged.Cases = append(merged.Cases, output.Cases...)
	}
	encoded, err := json.Marshal(merged)
	if err != nil {
		return nil, "", retries, fmt.Errorf("encode comparator response: %w", err)
	}
	return entries, string(encoded), retries, nil
}

func runValidatedStructuredJudge(ctx context.Context, agent harness.Harness, instructions string, input any, schema []byte, config Config, security harness.SecurityResolution, validate func(string) error) (string, []judgeCallAttempts, error) {
	return runValidatedStructuredJudgeWithRetry(ctx, agent, instructions, input, schema, config, security, validate, nil)
}

func runValidatedStructuredJudgeWithRetry(ctx context.Context, agent harness.Harness, instructions string, input any, schema []byte, config Config, security harness.SecurityResolution, validate func(string) error, retryBuilder judgeRetryBuilder) (string, []judgeCallAttempts, error) {
	var response string
	responses := make([]string, 0, 2)
	var callAttempts []judgeCallAttempts
	var validationErr error
	for attempt := 0; attempt < 2; attempt++ {
		attemptInstructions := instructions
		attemptInput := input
		if validationErr != nil {
			if retryBuilder != nil {
				if retry, ok := retryBuilder(validationErr); ok {
					attemptInstructions = retry.Instructions
					attemptInput = retry.Input
				} else {
					attemptInstructions = strings.TrimSpace(instructions) + "\n\nThe previous response failed validation. Return a corrected response for the original input.\nValidation error: " + validationErr.Error()
				}
			} else {
				attemptInstructions = strings.TrimSpace(instructions) + "\n\nThe previous response failed validation. Return a corrected response for the original input.\nValidation error: " + validationErr.Error()
			}
		}
		var err error
		var attempts harness.AttemptEvidence
		response, attempts, err = runStructuredJudge(ctx, agent, attemptInstructions, attemptInput, schema, config, security)
		if attempts.AttemptCount > 1 || len(attempts.AttemptErrors) > 0 {
			callAttempts = append(callAttempts, judgeCallAttempts{ValidationAttempt: attempt + 1, AttemptEvidence: attempts})
		}
		responses = append(responses, response)
		if err != nil {
			if len(responses) > 1 {
				return response, callAttempts, &judgeRetryError{cause: err, responses: responses}
			}
			return response, callAttempts, err
		}
		validationErr = validate(response)
		if validationErr == nil {
			return response, callAttempts, nil
		}
	}
	return response, callAttempts, &judgeRetryError{cause: validationErr, responses: responses}
}

func runStructuredJudge(ctx context.Context, agent harness.Harness, instructions string, input any, schema []byte, config Config, security harness.SecurityResolution) (string, harness.AttemptEvidence, error) {
	prompt, err := structuredJudgePrompt(instructions, input)
	if err != nil {
		return "", harness.AttemptEvidence{}, err
	}
	workDir, err := os.MkdirTemp("", "shuhari-judge-")
	if err != nil {
		return "", harness.AttemptEvidence{}, fmt.Errorf("create judge work directory: %w", err)
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
	judged, err := agent.Run(ctx, harness.Request{WorkDir: workDir, Prompt: prompt, Model: model, ReasoningEffort: effort, Security: security, Timeout: config.Timeout, OutputSchema: schema})
	if err != nil {
		return judged.Response, harness.AttemptsFromError(err), fmt.Errorf("run judge; prompt is %d bytes: %w", len(prompt), err)
	}
	return judged.Response, judged.Attempts, nil
}

func decorateJudgeAttempts(stage, caseID string, trial int, attempts []judgeCallAttempts) []judgeTransportRetry {
	retries := make([]judgeTransportRetry, 0, len(attempts))
	for _, item := range attempts {
		retries = append(retries, judgeTransportRetry{Stage: stage, CaseID: caseID, Trial: trial, ValidationAttempt: item.ValidationAttempt, AttemptCount: item.AttemptCount, AttemptErrors: item.AttemptErrors})
	}
	return retries
}

func persistJudgeRetries(iteration string, retries []judgeTransportRetry) error {
	if iteration == "" || len(retries) == 0 {
		return nil
	}
	return writeJSON(filepath.Join(iteration, "judge-retries.json"), struct {
		SchemaVersion string                `json:"schema_version"`
		Retries       []judgeTransportRetry `json:"retries"`
	}{SchemaVersion: evidenceSchemaVersion, Retries: retries})
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
		if result.Passed {
			grounding := groundEvidence(result.Evidence, artifact)
			if result.Absence != nil {
				if !supportsAbsenceClaim(assertion, result.Absence.Query) {
					return Grading{}, fmt.Errorf("%w: absence claim does not match a negated assertion %q", errInvalidGrading, assertion)
				}
				grounding = groundAbsence(result.Absence, artifact)
			}
			if grounding.Kind == evidenceGroundingHallucination {
				return Grading{}, fmt.Errorf("%w: passing assertion %q lacks grounded evidence in the artifact", errInvalidGrading, assertion)
			}
			result.EvidenceGrounding = grounding.Kind
			result.EvidenceGroundingScore = grounding.Score
			result.EvidenceGroundingSpan = grounding.Span
			result.EvidenceGroundingObservation = grounding.Observation
		} else {
			if result.Absence != nil {
				return Grading{}, fmt.Errorf("%w: failed assertion %q cannot carry an absence claim", errInvalidGrading, assertion)
			}
			result.EvidenceGrounding = evidenceGroundingNotApplicable
			result.EvidenceGroundingScore = 0
			result.EvidenceGroundingSpan = ""
			result.EvidenceGroundingObservation = ""
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
	return []byte(`{"type":"object","properties":{"cases":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"},"trial":{"type":"integer"},"A_assertion_results":{"$ref":"#/$defs/assertion_results"},"B_assertion_results":{"$ref":"#/$defs/assertion_results"}},"required":["id","trial","A_assertion_results","B_assertion_results"],"additionalProperties":false}}},"required":["cases"],"additionalProperties":false,"$defs":{"assertion_results":{"type":"array","items":{"type":"object","properties":{"text":{"type":"string","minLength":1},"passed":{"type":"boolean"},"evidence":{"type":"string","minLength":1,"pattern":".*[\"“”].*"},"absence":{"type":"object","properties":{"query":{"type":"string","minLength":1}},"required":["query"],"additionalProperties":false}},"required":["text","passed","evidence"],"additionalProperties":false}}}}`)
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
	}{SchemaVersion: evidenceSchemaVersion, Stage: stage, Error: cause.Error(), Grader: grader, Comparator: comparator, Responses: responses, Mappings: mappings})
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
