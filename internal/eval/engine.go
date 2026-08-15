package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/shunk031/shuhari/internal/cache"
	"github.com/shunk031/shuhari/internal/harness"
)

type runTask struct {
	Case    Case
	Trial   int
	Variant string
}

type runOutcome struct {
	Result runResult
	Err    error
}

func Run(ctx context.Context, suite Suite, agent harness.Harness, store cache.Store, config Config) (Report, error) {
	if config.Trials < 1 || config.Jobs < 1 || config.Timeout <= 0 {
		return Report{}, errors.New("trials, jobs, and timeout must be positive")
	}
	config.Sandbox = harness.EffectiveSandbox(config.Sandbox)
	capabilities := agent.Capabilities()
	if suite.Kind == harness.TargetSkill && !capabilities.Skills {
		return Report{}, errors.New("selected agent does not support skill evaluation")
	}
	if suite.Kind == harness.TargetInstructions && !capabilities.Instructions {
		return Report{}, errors.New("selected agent does not support instructions evaluation")
	}
	identity, err := agent.Probe(ctx)
	if err != nil {
		return Report{}, err
	}
	digest, err := suiteDigest(suite)
	if err != nil {
		return Report{}, err
	}
	runnerDigest, err := cache.RunnerDigest()
	if err != nil {
		return Report{}, err
	}
	cacheOptions, _ := json.Marshal(struct {
		Digest           string
		RunnerDigest     string
		Identity         harness.Identity
		Config           Config
		GraderDigest     string
		ComparatorDigest string
	}{Digest: digest, RunnerDigest: runnerDigest, Identity: identity, Config: config, GraderDigest: promptDigest(graderPrompt), ComparatorDigest: promptDigest(comparatorPrompt)})
	key := cache.Key(cacheOptions)
	if !config.NoCache {
		if record, ok, err := store.GetSuccess(key); err != nil {
			return Report{}, err
		} else if ok {
			return Report{Passed: true, Cached: true, Workspace: record.Workspace}, nil
		}
	}
	iteration, err := createIteration(suite, config.Workspace)
	if err != nil {
		return Report{}, err
	}
	if err := writeManifest(iteration, suite, digest, runnerDigest, identity, config); err != nil {
		return Report{Workspace: iteration}, err
	}
	withVariant, withoutVariant := variantsFor(suite.Kind)
	tasks := make([]runTask, 0, len(suite.Cases)*config.Trials*2)
	for _, item := range suite.Cases {
		for trial := 1; trial <= config.Trials; trial++ {
			tasks = append(tasks, runTask{Case: item, Trial: trial, Variant: withVariant}, runTask{Case: item, Trial: trial, Variant: withoutVariant})
		}
	}
	results, err := executeTasks(ctx, suite, agent, config, iteration, tasks)
	if err != nil {
		_ = writeJSON(filepath.Join(iteration, "evidence.json"), struct {
			SchemaVersion string `json:"schema_version"`
			Stage         string `json:"stage"`
			Error         string `json:"error"`
		}{SchemaVersion: workspaceSchemaVersion, Stage: "execution", Error: err.Error()})
		return Report{Workspace: iteration}, err
	}
	graded, candidateWins, baselineWins, reasons, judgeOutput, err := gradeRuns(ctx, agent, suite, results, config, iteration)
	if err != nil {
		_ = writeJSON(filepath.Join(iteration, "evidence.json"), struct {
			SchemaVersion string `json:"schema_version"`
			Stage         string `json:"stage"`
			Error         string `json:"error"`
			JudgeOutput   string `json:"judge_output,omitempty"`
		}{SchemaVersion: workspaceSchemaVersion, Stage: "grading", Error: err.Error(), JudgeOutput: judgeOutput})
		return Report{Workspace: iteration}, err
	}
	benchmark := buildBenchmark(graded)
	if err := writeJSON(filepath.Join(iteration, "benchmark.json"), benchmark); err != nil {
		return Report{Workspace: iteration}, err
	}

	passedByCase := map[string][]bool{}
	actionsByCase := map[string][]bool{}
	gradedByKey := map[string]gradedRun{}
	for _, run := range graded {
		gradedByKey[runKey(run.CaseID, run.Trial, run.Variant)] = run
	}
	for _, item := range suite.Cases {
		for trial := 1; trial <= config.Trials; trial++ {
			grade, ok := gradedByKey[runKey(item.ID, trial, withVariant)]
			passedByCase[item.ID] = append(passedByCase[item.ID], ok && grade.Passed)
			for _, result := range results {
				if result.Case.ID == item.ID && result.Trial == trial && result.Variant == withVariant {
					actionsByCase[item.ID] = append(actionsByCase[item.ID], harness.ContainsOrderedActions(result.Agent.Actions, item.RequiredActions))
					break
				}
			}
		}
	}
	for _, item := range suite.Cases {
		if !caseAssertionsPass(passedByCase[item.ID], config.StrictAllTrials) {
			reasons = append(reasons, fmt.Sprintf("%s: candidate assertions did not satisfy the trial policy", item.ID))
		}
		if len(item.RequiredActions) > 0 && !allTrialsPass(actionsByCase[item.ID]) {
			reasons = append(reasons, fmt.Sprintf("%s: required actions were not observed in order", item.ID))
		}
	}
	if candidateWins < baselineWins {
		reasons = append(reasons, fmt.Sprintf("candidate wins %d are lower than baseline wins %d", candidateWins, baselineWins))
	}
	passed := len(reasons) == 0
	report := Report{Passed: passed, Workspace: iteration, FailureCount: len(reasons), Reasons: reasons}
	if !passed {
		evidence := struct {
			SchemaVersion string   `json:"schema_version"`
			Reasons       []string `json:"reasons"`
			CandidateWins int      `json:"candidate_wins"`
			BaselineWins  int      `json:"baseline_wins"`
			JudgeOutput   string   `json:"judge_output"`
		}{SchemaVersion: workspaceSchemaVersion, Reasons: reasons, CandidateWins: candidateWins, BaselineWins: baselineWins, JudgeOutput: judgeOutput}
		if err := writeJSON(filepath.Join(iteration, "evidence.json"), evidence); err != nil {
			return report, err
		}
		return report, nil
	}
	if !config.NoCache {
		if err := store.PutSuccess(key, cache.Record{Passed: true, CreatedAt: time.Now().UTC(), Workspace: iteration}); err != nil {
			return report, err
		}
	}
	return report, nil
}

func allTrialsPass(values []bool) bool {
	if len(values) == 0 {
		return false
	}
	for _, value := range values {
		if !value {
			return false
		}
	}
	return true
}

func writeManifest(iteration string, suite Suite, suiteDigest, runnerDigest string, identity harness.Identity, config Config) error {
	return writeJSON(filepath.Join(iteration, "manifest.json"), struct {
		SchemaVersion    string             `json:"schema_version"`
		CreatedAt        time.Time          `json:"created_at"`
		TargetKind       harness.TargetKind `json:"target_kind"`
		TargetName       string             `json:"target_name"`
		SuiteDigest      string             `json:"suite_digest"`
		RunnerDigest     string             `json:"runner_digest"`
		AgentIdentity    harness.Identity   `json:"agent_identity"`
		Config           Config             `json:"config"`
		GraderDigest     string             `json:"grader_prompt_digest"`
		ComparatorDigest string             `json:"comparator_prompt_digest"`
	}{SchemaVersion: workspaceSchemaVersion, CreatedAt: time.Now().UTC(), TargetKind: suite.Kind, TargetName: suite.Name, SuiteDigest: suiteDigest, RunnerDigest: runnerDigest, AgentIdentity: identity, Config: config, GraderDigest: promptDigest(graderPrompt), ComparatorDigest: promptDigest(comparatorPrompt)})
}

func executeTasks(ctx context.Context, suite Suite, agent harness.Harness, config Config, iteration string, tasks []runTask) ([]runResult, error) {
	queue := make(chan runTask, len(tasks))
	outcomes := make(chan runOutcome, len(tasks))
	for _, task := range tasks {
		queue <- task
	}
	close(queue)
	workers := config.Jobs
	if workers > len(tasks) {
		workers = len(tasks)
	}
	var group sync.WaitGroup
	group.Add(workers)
	for range workers {
		go func() {
			defer group.Done()
			for task := range queue {
				result, err := executeTask(ctx, suite, agent, config, iteration, task)
				outcomes <- runOutcome{Result: result, Err: err}
			}
		}()
	}
	group.Wait()
	close(outcomes)
	results := make([]runResult, 0, len(tasks))
	var firstError error
	for outcome := range outcomes {
		if outcome.Err != nil && firstError == nil {
			firstError = outcome.Err
		}
		if outcome.Err == nil {
			results = append(results, outcome.Result)
		}
	}
	if firstError != nil {
		return results, firstError
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].Case.ID != results[j].Case.ID {
			return results[i].Case.ID < results[j].Case.ID
		}
		if results[i].Trial != results[j].Trial {
			return results[i].Trial < results[j].Trial
		}
		return results[i].Variant < results[j].Variant
	})
	return results, nil
}

func executeTask(ctx context.Context, suite Suite, agent harness.Harness, config Config, iteration string, task runTask) (runResult, error) {
	runDir := runDirectory(iteration, task.Case.ID, task.Variant, task.Trial)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return runResult{}, fmt.Errorf("create run directory: %w", err)
	}
	workDir, err := os.MkdirTemp("", "shuhari-run-")
	if err != nil {
		return runResult{}, fmt.Errorf("create run work directory: %w", err)
	}
	defer os.RemoveAll(workDir)
	files, err := stageFixtures(suite, task.Case, workDir)
	if err != nil {
		return runResult{}, err
	}
	outputDir := filepath.Join(workDir, "outputs")
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return runResult{}, fmt.Errorf("create agent output directory: %w", err)
	}
	var target *harness.Target
	withVariant, _ := variantsFor(suite.Kind)
	if task.Variant == withVariant {
		target = &harness.Target{Kind: suite.Kind, Name: suite.Name, SourcePath: suite.TargetPath}
	}
	prompt := buildRunPrompt(task.Case, files, outputDir, target)
	result, err := agent.Run(ctx, harness.Request{WorkDir: workDir, Prompt: prompt, Target: target, Model: config.Model, ReasoningEffort: config.ReasoningEffort, Sandbox: config.Sandbox, Network: config.Network, Timeout: config.Timeout})
	if err != nil {
		return runResult{}, fmt.Errorf("%s trial %d %s: %w", task.Case.ID, task.Trial, task.Variant, err)
	}
	artifactDir := filepath.Join(runDir, "outputs")
	if err := copyOutputs(outputDir, artifactDir); err != nil {
		return runResult{}, err
	}
	if err := os.WriteFile(filepath.Join(artifactDir, "response.md"), []byte(strings.TrimSpace(result.Response)+"\n"), 0o644); err != nil {
		return runResult{}, fmt.Errorf("write response artifact: %w", err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "transcript.jsonl"), result.Transcript, 0o644); err != nil {
		return runResult{}, fmt.Errorf("write transcript: %w", err)
	}
	timing := struct {
		TotalTokens int64 `json:"total_tokens"`
		DurationMS  int64 `json:"duration_ms"`
	}{TotalTokens: result.Usage.TotalTokens(), DurationMS: result.Duration.Milliseconds()}
	if err := writeJSON(filepath.Join(runDir, "timing.json"), timing); err != nil {
		return runResult{}, err
	}
	artifact, err := renderArtifact(artifactDir)
	if err != nil {
		return runResult{}, err
	}
	return runResult{Case: task.Case, Trial: task.Trial, Variant: task.Variant, RunDir: runDir, Agent: result, OutputPath: artifactDir, Artifact: artifact}, nil
}

func buildRunPrompt(item Case, files []string, outputDir string, target *harness.Target) string {
	var builder strings.Builder
	builder.WriteString("Execute this task in the current workspace.\n")
	if target != nil {
		fmt.Fprintf(&builder, "- Use the available %s named %q.\n", target.Kind, target.Name)
	}
	fmt.Fprintf(&builder, "- Task: %s\n", item.Prompt)
	fmt.Fprintf(&builder, "- Expected output: %s\n", item.ExpectedOutput)
	if len(files) > 0 {
		fmt.Fprintf(&builder, "- Input files: %s\n", strings.Join(files, ", "))
	}
	fmt.Fprintf(&builder, "- Save all produced files under: %s\n", outputDir)
	return builder.String()
}
