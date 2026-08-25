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
	"github.com/shunk031/shuhari/internal/progress"
	"sync"
)

//go:embed prompts/grader.md
var graderPrompt string

//go:embed prompts/comparator.md
var comparatorPrompt string

type blindMapping struct {
	A string `json:"A"`
	B string `json:"B"`
}

// agentJudgeInput contains only the assertions and blind label. The agent
// judge reads the copied artifact tree from its working directory.
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

type judgeCallAttempts struct {
	harness.AttemptEvidence
}

type judgeTransportRetry struct {
	Stage         string                 `json:"stage"`
	CaseID        string                 `json:"case_id"`
	Trial         int                    `json:"trial"`
	AttemptCount  int                    `json:"attempt_count"`
	AttemptErrors []harness.AttemptError `json:"attempt_errors"`
}

type trialJudgeInputs struct {
	ID          string
	Trial       int
	AOutputPath string
	BOutputPath string
	Assertions  []string
	Comparator  comparatorInput
}

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
			outputs := map[string]runResult{withVariant: with, withoutVariant: without}
			trialInputs = append(trialInputs, trialJudgeInputs{
				ID:          item.ID,
				Trial:       trial,
				AOutputPath: outputs[mapping.A].OutputPath,
				BOutputPath: outputs[mapping.B].OutputPath,
				Assertions:  item.effectiveAssertions(),
				Comparator: comparatorInput{
					ID: item.ID, Trial: trial, Prompt: item.Prompt,
					ExpectedOutput: item.ExpectedOutput, Assertions: item.effectiveAssertions(),
					A: outputs[mapping.A].Artifact, B: outputs[mapping.B].Artifact,
					AResponse: outputs[mapping.A].Agent.Response, BResponse: outputs[mapping.B].Agent.Response,
				},
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
				grading, buildErr := buildGrading(item.effectiveAssertions(), grades[variant])
				if buildErr != nil {
					err = fmt.Errorf("case %s trial %d %s: %w", item.ID, trial, variant, buildErr)
					persistGradingError(iteration, "grader-output", graderResponse, comparatorResponse, mappings, err)
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
	// Judgements are independent per trial, so they run concurrently under the
	// same `--jobs` budget the run phase uses. Grading used to be a plain
	// sequential loop: a six-case suite at three trials meant thirty-six judge
	// invocations end to end, which dominated the wall clock and made the phase
	// look stuck.
	//
	// Results are collected by index rather than appended, so the merged output
	// and the retry log stay in input order regardless of completion order.
	collected := make([]graderTrialResult, len(inputs))

	workers := config.Jobs
	if workers < 1 {
		workers = 1
	}
	semaphore := make(chan struct{}, workers)
	var waitGroup sync.WaitGroup
	for index, item := range inputs {
		waitGroup.Add(1)
		go func(index int, item trialJudgeInputs) {
			defer waitGroup.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()
			collected[index] = gradeOneTrial(ctx, agent, item, config, security)
		}(index, item)
	}
	waitGroup.Wait()

	merged := judgeOutput{}
	entries := map[string]judgeEntry{}
	var retries []judgeTransportRetry
	for _, result := range collected {
		retries = append(retries, result.retries...)
		if result.err != nil {
			return nil, result.response, retries, result.err
		}
		entries[caseTrialKey(result.entry.ID, result.entry.Trial)] = result.entry
		merged.Cases = append(merged.Cases, result.entry)
	}
	encoded, err := json.Marshal(merged)
	if err != nil {
		return nil, "", retries, fmt.Errorf("encode grader response: %w", err)
	}
	return entries, string(encoded), retries, nil
}

// gradeOneTrial judges both blinded sides of one trial.
//
// The two sides stay sequential: they write into one entry, and splitting them
// would double concurrency against the same endpoint for no ordering benefit.
// graderTrialResult carries one trial's judgement plus what the caller needs to
// merge it back in input order.
type graderTrialResult struct {
	entry    judgeEntry
	retries  []judgeTransportRetry
	response string
	err      error
}

// gradeOneTrial judges both blinded sides of one trial.
//
// The two sides stay sequential: they write into one entry, and splitting them
// would double concurrency against the same endpoint for no ordering benefit.
func gradeOneTrial(ctx context.Context, agent harness.Harness, item trialJudgeInputs, config Config, security harness.SecurityResolution) graderTrialResult {
	finish := config.Progress.Started(progress.Event{
		Phase: progress.PhaseGrade,
		Case:  item.ID,
		Trial: item.Trial,
	})
	result := gradeTrialSides(ctx, agent, item, config, security)
	finish(statusOf(result.err), result.err)
	return result
}

func gradeTrialSides(ctx context.Context, agent harness.Harness, item trialJudgeInputs, config Config, security harness.SecurityResolution) graderTrialResult {
	var retries []judgeTransportRetry
	combined := judgeEntry{ID: item.ID, Trial: item.Trial}
	{
		for _, side := range []struct {
			label       string
			artifactDir string
		}{
			{label: "A", artifactDir: item.AOutputPath},
			{label: "B", artifactDir: item.BOutputPath},
		} {
			artifactRoot := side.artifactDir
			input := []agentJudgeInput{{ID: item.ID, Trial: item.Trial, Side: side.label, Assertions: item.Assertions}}
			response, attempts, err := runAgentStructuredJudge(ctx, agent, graderPrompt, input, graderSchema(), config, security, artifactRoot)
			retries = append(retries, decorateJudgeAttempts("grader", item.ID+"/"+side.label, item.Trial, attempts)...)
			if err != nil {
				return graderTrialResult{retries: retries, response: response, err: fmt.Errorf("grader case %q trial %d side %s: %w", item.ID, item.Trial, side.label, err)}
			}
			var output agentJudgeOutput
			if err := json.Unmarshal([]byte(response), &output); err != nil {
				return graderTrialResult{retries: retries, response: response, err: fmt.Errorf("decode grader response: %w", err)}
			}
			trialEntries, err := validateAgentGraderEntries(output, input)
			if err != nil {
				return graderTrialResult{retries: retries, response: response, err: err}
			}
			entry := trialEntries[caseTrialKey(item.ID, item.Trial)]
			if side.label == "A" {
				combined.AAssertionResults = entry.AssertionResults
			} else {
				combined.BAssertionResults = entry.AssertionResults
			}
		}
	}
	return graderTrialResult{entry: combined, retries: retries}
}

func runComparatorsPerTrial(ctx context.Context, agent harness.Harness, inputs []trialJudgeInputs, config Config, security harness.SecurityResolution) (map[string]comparatorEntry, string, []judgeTransportRetry, error) {
	// Comparisons are independent per trial, so they share the run phase's
	// `--jobs` budget rather than running end to end. Collected by index so the
	// merged output stays in input order.
	collected := make([]comparatorTrialResult, len(inputs))

	workers := config.Jobs
	if workers < 1 {
		workers = 1
	}
	semaphore := make(chan struct{}, workers)
	var waitGroup sync.WaitGroup
	for index, item := range inputs {
		waitGroup.Add(1)
		go func(index int, item trialJudgeInputs) {
			defer waitGroup.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()
			finish := config.Progress.Started(progress.Event{
				Phase: progress.PhaseCompare,
				Case:  item.ID,
				Trial: item.Trial,
			})
			collected[index] = compareOneTrial(ctx, agent, item, config, security)
			finish(statusOf(collected[index].err), collected[index].err)
		}(index, item)
	}
	waitGroup.Wait()

	merged := comparatorOutput{}
	entries := map[string]comparatorEntry{}
	var retries []judgeTransportRetry
	for _, result := range collected {
		retries = append(retries, result.retries...)
		if result.err != nil {
			return nil, result.response, retries, result.err
		}
		for key, entry := range result.entries {
			entries[key] = entry
		}
		merged.Cases = append(merged.Cases, result.cases...)
	}
	encoded, err := json.Marshal(merged)
	if err != nil {
		return nil, "", retries, fmt.Errorf("encode comparator response: %w", err)
	}
	return entries, string(encoded), retries, nil
}

// comparatorTrialResult carries one trial's comparison plus what the caller
// needs to merge it back in input order.
type comparatorTrialResult struct {
	entries  map[string]comparatorEntry
	cases    []comparatorEntry
	retries  []judgeTransportRetry
	response string
	err      error
}

func compareOneTrial(ctx context.Context, agent harness.Harness, item trialJudgeInputs, config Config, security harness.SecurityResolution) comparatorTrialResult {
	var retries []judgeTransportRetry
	{
		comparatorInputs := []comparatorInput{item.Comparator}
		response, attempts, err := runStructuredJudge(ctx, agent, comparatorPrompt, comparatorInputs, comparatorSchema(), config, security)
		retries = append(retries, decorateJudgeAttempts("comparator", item.ID, item.Trial, attempts)...)
		if err != nil {
			return comparatorTrialResult{retries: retries, response: response, err: fmt.Errorf("comparator case %q trial %d: %w", item.ID, item.Trial, err)}
		}
		var output comparatorOutput
		if err := json.Unmarshal([]byte(response), &output); err != nil {
			return comparatorTrialResult{retries: retries, response: response, err: fmt.Errorf("decode comparator response: %w", err)}
		}
		trialEntries, err := validateComparatorEntries(output, comparatorInputs)
		if err != nil {
			return comparatorTrialResult{retries: retries, response: response, err: err}
		}
		return comparatorTrialResult{entries: trialEntries, cases: output.Cases, retries: retries}
	}
}

func runStructuredJudge(ctx context.Context, agent harness.Harness, instructions string, input any, schema []byte, config Config, security harness.SecurityResolution) (string, []judgeCallAttempts, error) {
	prompt, err := structuredJudgePrompt(instructions, input)
	if err != nil {
		return "", nil, err
	}
	workDir, err := os.MkdirTemp("", "shuhari-judge-")
	if err != nil {
		return "", nil, fmt.Errorf("create judge work directory: %w", err)
	}
	defer os.RemoveAll(workDir)
	judged, err := runJudgeAgent(ctx, agent, workDir, prompt, schema, config, security)
	if err != nil {
		return judged.Response, []judgeCallAttempts{{AttemptEvidence: harness.AttemptsFromError(err)}}, fmt.Errorf("run judge; prompt is %d bytes: %w", len(prompt), err)
	}
	return judged.Response, []judgeCallAttempts{{AttemptEvidence: judged.Attempts}}, nil
}

func runAgentStructuredJudge(ctx context.Context, agent harness.Harness, instructions string, input any, schema []byte, config Config, security harness.SecurityResolution, artifactRoot string) (string, []judgeCallAttempts, error) {
	workDir, err := os.MkdirTemp("", "shuhari-judge-agent-")
	if err != nil {
		return "", nil, fmt.Errorf("create agent judge work directory: %w", err)
	}
	defer os.RemoveAll(workDir)
	if err := copyJudgeArtifactTree(artifactRoot, workDir); err != nil {
		return "", nil, err
	}
	prompt, err := structuredJudgePrompt(instructions, input)
	if err != nil {
		return "", nil, err
	}
	judged, err := runJudgeAgent(ctx, agent, workDir, prompt, schema, config, security)
	if err != nil {
		return judged.Response, []judgeCallAttempts{{AttemptEvidence: harness.AttemptsFromError(err)}}, fmt.Errorf("run agent judge; prompt is %d bytes: %w", len(prompt), err)
	}
	return judged.Response, []judgeCallAttempts{{AttemptEvidence: judged.Attempts}}, nil
}

func runJudgeAgent(ctx context.Context, agent harness.Harness, workDir, prompt string, schema []byte, config Config, security harness.SecurityResolution) (harness.Result, error) {
	model := config.JudgeModel
	if model == "" {
		model = config.Model
	}
	effort := config.JudgeReasoningEffort
	if effort == "" {
		effort = config.ReasoningEffort
	}
	return agent.Run(ctx, harness.Request{WorkDir: workDir, Prompt: prompt, Model: model, ReasoningEffort: effort, Security: security, Timeout: config.Timeout, OutputSchema: schema})
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
		if item.AttemptCount <= 1 && len(item.AttemptErrors) == 0 {
			continue
		}
		retries = append(retries, judgeTransportRetry{Stage: stage, CaseID: caseID, Trial: trial, AttemptCount: item.AttemptCount, AttemptErrors: item.AttemptErrors})
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

func buildGrading(expected []string, actual []AssertionResult) (Grading, error) {
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
	return []byte(`{
  "type":"object",
  "properties":{"cases":{"type":"array","items":{"type":"object","properties":{
    "id":{"type":"string"},"trial":{"type":"integer"},"side":{"type":"string","enum":["A","B"]},
    "assertion_results":{"type":"array","items":{"type":"object","properties":{
      "text":{"type":"string","minLength":1},"passed":{"type":"boolean"},"evidence":{"type":"string","minLength":1}
    },"required":["text","passed","evidence"],"additionalProperties":false}}
  },"required":["id","trial","side","assertion_results"],"additionalProperties":false}}},
  "required":["cases"],"additionalProperties":false
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
	_ = writeJSON(filepath.Join(iteration, "grading-error.json"), struct {
		SchemaVersion string                  `json:"schema_version"`
		Stage         string                  `json:"stage"`
		Error         string                  `json:"error"`
		Grader        string                  `json:"grader_response,omitempty"`
		Comparator    string                  `json:"comparator_response,omitempty"`
		Mappings      map[string]blindMapping `json:"blind_mappings"`
	}{SchemaVersion: evidenceSchemaVersion, Stage: stage, Error: cause.Error(), Grader: grader, Comparator: comparator, Mappings: mappings})
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
