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

type agentJudgeInput struct {
	ID         string   `json:"id"`
	Trial      int      `json:"trial"`
	Side       string   `json:"side"`
	Assertions []string `json:"assertions"`
}

type agentJudgeEntry struct {
	ID               string            `json:"id"`
	Trial            int               `json:"trial"`
	Side             string            `json:"side"`
	AssertionResults []AssertionResult `json:"assertion_results"`
}

type agentJudgeOutput struct {
	Cases []agentJudgeEntry `json:"cases"`
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
	ID                string
	Trial             int
	ForbiddenPatterns map[string][]string
	AOutputPath       string
	BOutputPath       string
	Grader            judgeInput
	Comparator        comparatorInput
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
			outputPaths := map[string]string{withVariant: with.OutputPath, withoutVariant: without.OutputPath}
			responses := map[string]string{withVariant: with.Agent.Response, withoutVariant: without.Agent.Response}
			trialInputs = append(trialInputs, trialJudgeInputs{
				ID:                item.ID,
				Trial:             trial,
				ForbiddenPatterns: item.ForbiddenPatterns,
				AOutputPath:       outputPaths[mapping.A],
				BOutputPath:       outputPaths[mapping.B],
				Grader:            judgeInput{ID: item.ID, Trial: trial, Assertions: item.effectiveAssertions(), A: outputs[mapping.A], B: outputs[mapping.B], AResponse: responses[mapping.A], BResponse: responses[mapping.B]},
				Comparator:        comparatorInput{ID: item.ID, Trial: trial, Prompt: item.Prompt, ExpectedOutput: item.ExpectedOutput, Assertions: item.effectiveAssertions(), A: outputs[mapping.A], B: outputs[mapping.B], AResponse: responses[mapping.A], BResponse: responses[mapping.B]},
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
				var grading Grading
				var buildErr error
				artifactRoot, cleanup, rootErr := prepareJudgeArtifactRoot(run.OutputPath, run.Artifact)
				if rootErr != nil {
					return nil, 0, 0, nil, string(rawEvidence), rootErr
				}
				grading, buildErr = buildAgentGradingWithForbiddenPatterns(item.effectiveAssertions(), item.ForbiddenPatterns, grades[variant], artifactRoot, run.Artifact)
				cleanup()
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
		combined := judgeEntry{ID: item.ID, Trial: item.Trial}
		for _, side := range []struct {
			label       string
			artifactDir string
			artifact    string
		}{
			{label: "A", artifactDir: item.AOutputPath, artifact: item.Grader.A},
			{label: "B", artifactDir: item.BOutputPath, artifact: item.Grader.B},
		} {
			artifactRoot, cleanup, err := prepareJudgeArtifactRoot(side.artifactDir, side.artifact)
			if err != nil {
				return nil, "", retries, err
			}
			input := []agentJudgeInput{{ID: item.ID, Trial: item.Trial, Side: side.label, Assertions: item.Grader.Assertions}}
			var output agentJudgeOutput
			response, attempts, err := runValidatedStructuredAgentJudgeWithRetry(ctx, agent, graderPrompt, input, graderSchema(), config, security, artifactRoot, func(response string) error {
				output = agentJudgeOutput{}
				if err := json.Unmarshal([]byte(response), &output); err != nil {
					return fmt.Errorf("decode response: %w", err)
				}
				trialEntries, err := validateAgentGraderEntries(output, input)
				if err != nil {
					return err
				}
				entry := trialEntries[caseTrialKey(item.ID, item.Trial)]
				if _, err := buildAgentGradingWithForbiddenPatterns(item.Grader.Assertions, item.ForbiddenPatterns, entry.AssertionResults, artifactRoot, side.artifact); err != nil {
					return fmt.Errorf("validate %s evidence: %w", side.label, err)
				}
				return nil
			})
			retries = append(retries, decorateJudgeAttempts("grader", item.ID+"/"+side.label, item.Trial, attempts)...)
			if err != nil {
				cleanup()
				return nil, response, retries, fmt.Errorf("grader case %q trial %d side %s: %w", item.ID, item.Trial, side.label, err)
			}
			trialEntries, err := validateAgentGraderEntries(output, input)
			if err != nil {
				cleanup()
				return nil, response, retries, err
			}
			entry := trialEntries[caseTrialKey(item.ID, item.Trial)]
			if side.label == "A" {
				combined.AAssertionResults = entry.AssertionResults
			} else {
				combined.BAssertionResults = entry.AssertionResults
			}
			cleanup()
		}
		entries[caseTrialKey(item.ID, item.Trial)] = combined
		merged.Cases = append(merged.Cases, combined)
	}
	encoded, err := json.Marshal(merged)
	if err != nil {
		return nil, "", retries, fmt.Errorf("encode grader response: %w", err)
	}
	return entries, string(encoded), retries, nil
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
	return runValidatedStructuredJudgeWithRetryAtRoot(ctx, agent, instructions, input, schema, config, security, "", validate, retryBuilder)
}

func runValidatedStructuredAgentJudgeWithRetry(ctx context.Context, agent harness.Harness, instructions string, input any, schema []byte, config Config, security harness.SecurityResolution, artifactRoot string, validate func(string) error) (string, []judgeCallAttempts, error) {
	return runValidatedStructuredJudgeWithRetryAtRoot(ctx, agent, instructions, input, schema, config, security, artifactRoot, validate, nil)
}

func runValidatedStructuredJudgeWithRetryAtRoot(ctx context.Context, agent harness.Harness, instructions string, input any, schema []byte, config Config, security harness.SecurityResolution, artifactRoot string, validate func(string) error, retryBuilder judgeRetryBuilder) (string, []judgeCallAttempts, error) {
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
					attemptInstructions = validationRetryInstructions(instructions, validationErr)
				}
			} else {
				attemptInstructions = validationRetryInstructions(instructions, validationErr)
			}
		}
		var err error
		var attempts harness.AttemptEvidence
		if artifactRoot == "" {
			response, attempts, err = runStructuredJudge(ctx, agent, attemptInstructions, attemptInput, schema, config, security)
		} else {
			response, attempts, err = runAgentStructuredJudge(ctx, agent, attemptInstructions, attemptInput, schema, config, security, artifactRoot)
		}
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

func validationRetryInstructions(instructions string, validationErr error) string {
	return strings.TrimSpace(instructions) + "\n\nThe previous response failed validation. Return a corrected response for the original input. Read the current workspace artifact again.\nValidation feedback: " + validationErr.Error() + "\nFor every passing present or positive claim, cite the exact relative file path and inclusive line span in evidence_references, then set evidence to the exact cited line text. Do not paraphrase, rename variables, substitute literal paths or values, normalize whitespace or quotes, or cite a nonexistent span. For every fallback absence claim, copy negated_clause verbatim from the assertion; do not paraphrase it.\n"
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

func runAgentStructuredJudge(ctx context.Context, agent harness.Harness, instructions string, input any, schema []byte, config Config, security harness.SecurityResolution, artifactRoot string) (string, harness.AttemptEvidence, error) {
	workDir, err := os.MkdirTemp("", "shuhari-judge-agent-")
	if err != nil {
		return "", harness.AttemptEvidence{}, fmt.Errorf("create agent judge work directory: %w", err)
	}
	defer os.RemoveAll(workDir)
	if err := copyJudgeArtifactTree(artifactRoot, workDir); err != nil {
		return "", harness.AttemptEvidence{}, err
	}
	prompt, err := structuredJudgePrompt(instructions, input)
	if err != nil {
		return "", harness.AttemptEvidence{}, err
	}
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
		return judged.Response, harness.AttemptsFromError(err), fmt.Errorf("run agent judge; prompt is %d bytes: %w", len(prompt), err)
	}
	return judged.Response, judged.Attempts, nil
}

func prepareJudgeArtifactRoot(root, artifact string) (string, func(), error) {
	if root != "" {
		return root, func() {}, nil
	}
	temporary, err := os.MkdirTemp("", "shuhari-judge-fixture-")
	if err != nil {
		return "", func() {}, fmt.Errorf("create synthetic judge artifact: %w", err)
	}
	contents := artifact
	if !strings.HasSuffix(contents, "\n") {
		contents += "\n"
	}
	if err := os.WriteFile(filepath.Join(temporary, "response.md"), []byte(contents), 0o644); err != nil {
		_ = os.RemoveAll(temporary)
		return "", func() {}, fmt.Errorf("write synthetic judge artifact: %w", err)
	}
	return temporary, func() { _ = os.RemoveAll(temporary) }, nil
}

func copyJudgeArtifactTree(source, destination string) error {
	if strings.TrimSpace(source) == "" {
		return errors.New("agent judge artifact directory is required")
	}
	info, err := os.Lstat(source)
	if err != nil {
		return fmt.Errorf("inspect agent judge artifact directory: %w", err)
	}
	if !info.IsDir() {
		return errors.New("agent judge artifact path is not a directory")
	}
	return copyOutputs(source, destination)
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

func validateAgentGraderEntries(output agentJudgeOutput, inputs []agentJudgeInput) (map[string]agentJudgeEntry, error) {
	if len(output.Cases) != len(inputs) {
		return nil, fmt.Errorf("agent grader returned %d cases, want %d", len(output.Cases), len(inputs))
	}
	entries := map[string]agentJudgeEntry{}
	for _, entry := range output.Cases {
		key := caseTrialKey(entry.ID, entry.Trial)
		if _, exists := entries[key]; exists {
			return nil, fmt.Errorf("agent grader returned duplicate case %s", key)
		}
		if entry.Side != "A" && entry.Side != "B" {
			return nil, fmt.Errorf("agent grader returned invalid blind side %q", entry.Side)
		}
		entries[key] = entry
	}
	for _, input := range inputs {
		entry, ok := entries[caseTrialKey(input.ID, input.Trial)]
		if !ok {
			return nil, fmt.Errorf("agent grader omitted case %s", caseTrialKey(input.ID, input.Trial))
		}
		if entry.Side != input.Side {
			return nil, fmt.Errorf("agent grader returned side %q, want %q", entry.Side, input.Side)
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

func buildAgentGrading(expected []string, actual []AssertionResult, artifactRoot, artifact string) (Grading, error) {
	return buildAgentGradingWithForbiddenPatterns(expected, nil, actual, artifactRoot, artifact)
}

func buildAgentGradingWithForbiddenPatterns(expected []string, forbiddenPatterns map[string][]string, actual []AssertionResult, artifactRoot, artifact string) (Grading, error) {
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
			var grounding evidenceGrounding
			if patterns := forbiddenPatterns[assertion]; len(patterns) > 0 {
				result.Absence = &AbsenceClaim{ForbiddenPatterns: append([]string(nil), patterns...)}
				grounding = groundDeclaredAbsence(patterns, artifact)
				if grounding.Kind == evidenceGroundingContradiction {
					result.Passed = false
				}
			} else if result.Absence != nil {
				if err := validateFallbackAbsenceClaim(assertion, result.Absence); err != nil {
					return Grading{}, err
				}
				if requiresVerbatimPositiveEvidence(assertion, result.Absence.NegatedClause) {
					if _, err := groundAgentEvidence(result, artifactRoot); err != nil {
						return Grading{}, err
					}
				}
				grounding = groundAbsence(result.Absence, artifact)
				if grounding.Kind == evidenceGroundingContradiction {
					result.Passed = false
				}
			} else if absenceAssertionCuePattern.MatchString(assertion) {
				return Grading{}, fmt.Errorf("%w: negative assertion %q requires an absence declaration", errInvalidGrading, assertion)
			} else {
				var err error
				grounding, err = groundAgentEvidence(result, artifactRoot)
				if err != nil {
					return Grading{}, err
				}
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

func validateFallbackAbsenceClaim(assertion string, claim *AbsenceClaim) error {
	if claim == nil {
		return fmt.Errorf("%w: absence claim is required", errInvalidGrading)
	}
	if !absenceAssertionCuePattern.MatchString(assertion) {
		return fmt.Errorf("%w: absence claim requires a negated assertion %q", errInvalidGrading, assertion)
	}
	if strings.TrimSpace(claim.NegatedClause) == "" {
		return fmt.Errorf("%w: absence claim negated_clause must be nonblank", errInvalidGrading)
	}
	if !strings.Contains(assertion, claim.NegatedClause) {
		return fmt.Errorf("%w: absence claim negated_clause %q is not a verbatim substring of assertion %q (quote-not-found: copy the negated clause exactly)", errInvalidGrading, claim.NegatedClause, assertion)
	}
	if strings.TrimSpace(claim.Query) == "" {
		return fmt.Errorf("%w: absence claim query must be nonblank", errInvalidGrading)
	}
	if strings.TrimSpace(claim.Rationale) == "" {
		return fmt.Errorf("%w: absence claim rationale must be nonblank", errInvalidGrading)
	}
	return nil
}

func requiresVerbatimPositiveEvidence(assertion, negatedClause string) bool {
	normalizedClause := strings.ToLower(normalizeEvidenceText(negatedClause))
	for _, clause := range absenceClauseBoundaryPattern.Split(strings.ToLower(normalizeEvidenceText(assertion)), -1) {
		if strings.TrimSpace(clause) == "" {
			continue
		}
		if absenceAssertionCuePattern.MatchString(clause) && strings.Contains(clause, normalizedClause) {
			continue
		}
		return true
	}
	return false
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
          "side": {"type": "string", "enum": ["A", "B"]},
          "assertion_results": {"$ref": "#/$defs/assertion_results"}
        },
        "required": ["id", "trial", "side", "assertion_results"],
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
          "text": {"type": "string", "minLength": 1},
          "passed": {"type": "boolean"},
          "evidence": {"type": "string", "minLength": 1},
          "evidence_references": {
            "type": "array",
            "items": {
              "type": "object",
              "properties": {
                "path": {"type": "string", "minLength": 1},
                "start_line": {"type": "integer", "minimum": 1},
                "end_line": {"type": "integer", "minimum": 1}
              },
              "required": ["path", "start_line", "end_line"],
              "additionalProperties": false
            }
          },
          "absence": {
            "type": ["object", "null"],
            "properties": {
              "negated_clause": {"type": "string", "minLength": 1},
              "query": {"type": "string", "minLength": 1},
              "rationale": {"type": "string", "minLength": 1}
            },
            "required": ["negated_clause", "query", "rationale"],
            "additionalProperties": false
          }
        },
	        "required": ["text", "passed", "evidence", "evidence_references", "absence"],
        "additionalProperties": false
      }
    }
  }
}`)
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
