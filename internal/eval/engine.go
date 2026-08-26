package eval

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/shunk031/shuhari/internal/harness"
	"github.com/shunk031/shuhari/internal/progress"
	"github.com/shunk031/shuhari/internal/receipt"
	contracts "github.com/shunk031/shuhari/schemas"
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

func Run(ctx context.Context, suite Suite, agent harness.Harness, config Config) (Report, error) {
	if config.Trials < 1 || config.Jobs < 1 || config.Timeout <= 0 {
		return Report{}, errors.New("trials, jobs, and timeout must be positive")
	}
	mode, err := harness.EffectiveMode(config.Mode)
	if err != nil {
		return Report{}, err
	}
	config.Mode = mode
	var level harness.SandboxLevel
	var runSecurity, judgeSecurity harness.SecurityResolution
	var runSecurityArtifact, judgeSecurityArtifact *harness.SecurityResolution
	if mode == harness.ModeCompletion {
		config.SandboxLevel = ""
		capabilities := agent.Capabilities()
		if suite.Kind == harness.TargetSkill && !capabilities.Skills {
			return Report{}, errors.New("selected agent does not support skill evaluation")
		}
		if suite.Kind == harness.TargetInstructions && !capabilities.Instructions {
			return Report{}, errors.New("selected agent does not support instructions evaluation")
		}
	} else {
		level, err = harness.EffectiveSandboxLevel(config.SandboxLevel)
		if err != nil {
			return Report{}, err
		}
		config.SandboxLevel = string(level)
		runPolicy := harness.SecurityPolicy{Level: level, Network: config.Network, HostTools: config.HostTools}
		runSecurity, err = agent.ResolveSecurity(ctx, runPolicy)
		if err != nil {
			return Report{}, err
		}
		if err := harness.ValidateSecurityResolution(runPolicy, runSecurity); err != nil {
			return Report{}, fmt.Errorf("security resolution: %w", err)
		}
		judgePolicy := harness.SecurityPolicy{Level: harness.SandboxReadOnly, Network: false}
		judgeSecurity = runSecurity
		if runPolicy.Level == harness.SandboxUnsandboxed {
			judgePolicy = runPolicy
		}
		if !judgePolicy.Equal(runPolicy) {
			judgeSecurity, err = agent.ResolveSecurity(ctx, judgePolicy)
			if err != nil {
				return Report{}, fmt.Errorf("resolve judge security: %w", err)
			}
		}
		if err := harness.ValidateSecurityResolution(judgePolicy, judgeSecurity); err != nil {
			return Report{}, fmt.Errorf("judge security resolution: %w", err)
		}
		runSecurityArtifact = &runSecurity
		judgeSecurityArtifact = &judgeSecurity
		capabilities := agent.Capabilities()
		if suite.Kind == harness.TargetSkill && !capabilities.Skills {
			return Report{}, errors.New("selected agent does not support skill evaluation")
		}
		if suite.Kind == harness.TargetInstructions && !capabilities.Instructions {
			return Report{}, errors.New("selected agent does not support instructions evaluation")
		}
	}
	var identity harness.Identity
	if mode == harness.ModeCompletion {
		identity, err = agent.Probe(ctx)
	} else {
		identity, err = agent.Probe(ctx, runSecurity, judgeSecurity)
	}
	if err != nil {
		return Report{}, err
	}
	digest, err := suiteDigest(suite)
	if err != nil {
		return Report{}, err
	}
	iteration, err := createIteration(suite, config.Workspace)
	if err != nil {
		return Report{}, err
	}
	if err := writeManifest(iteration, suite, digest, identity, config, runSecurityArtifact, judgeSecurityArtifact); err != nil {
		return Report{Workspace: iteration}, err
	}
	withVariant, withoutVariant := variantsFor(suite.Kind)
	tasks := make([]runTask, 0, len(suite.Cases)*config.Trials*2)
	for _, item := range suite.Cases {
		for trial := 1; trial <= config.Trials; trial++ {
			tasks = append(tasks, runTask{Case: item, Trial: trial, Variant: withVariant}, runTask{Case: item, Trial: trial, Variant: withoutVariant})
		}
	}
	config.Progress.SetTotal(progress.PhaseRun, len(tasks))
	results, err := executeTasks(ctx, suite, agent, config, runSecurity, iteration, tasks)
	if err != nil {
		_ = writeJSON(filepath.Join(iteration, "evidence.json"), struct {
			SchemaVersion string `json:"schema_version"`
			Stage         string `json:"stage"`
			Error         string `json:"error"`
		}{SchemaVersion: evidenceSchemaVersion, Stage: "execution", Error: err.Error()})
		return Report{Workspace: iteration}, err
	}
	graded, candidateWins, baselineWins, reasons, judgeOutput, err := gradeRuns(ctx, agent, suite, results, config, judgeSecurity, iteration)
	if err != nil {
		_ = writeJSON(filepath.Join(iteration, "evidence.json"), struct {
			SchemaVersion string `json:"schema_version"`
			Stage         string `json:"stage"`
			Error         string `json:"error"`
			JudgeOutput   string `json:"judge_output,omitempty"`
		}{SchemaVersion: evidenceSchemaVersion, Stage: "grading", Error: err.Error(), JudgeOutput: judgeOutput})
		return Report{Workspace: iteration}, err
	}
	benchmark := buildBenchmark(graded)
	benchmark.Mode = config.Mode
	benchmark.Security = runSecurityArtifact
	if runSecurityArtifact != nil {
		runPolicy := harness.SecurityPolicy{Level: level, Network: config.Network, HostTools: config.HostTools}
		if err := harness.ValidateSecurityResolution(runPolicy, *benchmark.Security); err != nil {
			return Report{Workspace: iteration}, fmt.Errorf("validate benchmark security: %w", err)
		}
	}
	if err := contracts.Validate("benchmark", benchmark); err != nil {
		return Report{Workspace: iteration}, err
	}
	if err := writeJSON(filepath.Join(iteration, "benchmark.json"), benchmark); err != nil {
		return Report{Workspace: iteration}, err
	}

	passedByCase := map[string][]bool{}
	gradedByKey := map[string]gradedRun{}
	for _, run := range graded {
		gradedByKey[runKey(run.CaseID, run.Trial, run.Variant)] = run
	}
	for _, item := range suite.Cases {
		for trial := 1; trial <= config.Trials; trial++ {
			grade, ok := gradedByKey[runKey(item.ID, trial, withVariant)]
			passedByCase[item.ID] = append(passedByCase[item.ID], ok && grade.Passed)
		}
	}
	for _, item := range suite.Cases {
		if !caseAssertionsPass(passedByCase[item.ID], config.StrictAllTrials) {
			reasons = append(reasons, fmt.Sprintf("%s: candidate assertions did not satisfy the trial policy", item.ID))
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
		}{SchemaVersion: evidenceSchemaVersion, Reasons: reasons, CandidateWins: candidateWins, BaselineWins: baselineWins, JudgeOutput: judgeOutput}
		if err := writeJSON(filepath.Join(iteration, "evidence.json"), evidence); err != nil {
			return report, err
		}
		return report, nil
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

func writeManifest(iteration string, suite Suite, suiteDigest string, identity harness.Identity, config Config, security, judgeSecurity *harness.SecurityResolution) error {
	if security != nil {
		policy := harness.SecurityPolicy{Level: harness.SandboxLevel(config.SandboxLevel), Network: config.Network, HostTools: config.HostTools}
		if err := harness.ValidateSecurityResolution(policy, *security); err != nil {
			return fmt.Errorf("validate manifest security: %w", err)
		}
	}
	manifest := struct {
		SchemaVersion    string                      `json:"schema_version"`
		CreatedAt        time.Time                   `json:"created_at"`
		TargetKind       harness.TargetKind          `json:"target_kind"`
		TargetName       string                      `json:"target_name"`
		SuiteDigest      string                      `json:"suite_digest"`
		AgentIdentity    harness.Identity            `json:"agent_identity"`
		Config           Config                      `json:"config"`
		Mode             harness.Mode                `json:"mode"`
		Security         *harness.SecurityResolution `json:"security"`
		JudgeSecurity    *harness.SecurityResolution `json:"judge_security"`
		GraderDigest     string                      `json:"grader_prompt_digest"`
		ComparatorDigest string                      `json:"comparator_prompt_digest"`
	}{SchemaVersion: workspaceManifestSchemaVersion, CreatedAt: time.Now().UTC(), TargetKind: suite.Kind, TargetName: suite.Name, SuiteDigest: suiteDigest, AgentIdentity: identity, Config: config, Mode: config.Mode, Security: security, JudgeSecurity: judgeSecurity, GraderDigest: promptDigest(graderPrompt), ComparatorDigest: promptDigest(comparatorPrompt)}
	if err := contracts.Validate("workspace", manifest); err != nil {
		return err
	}
	return writeJSON(filepath.Join(iteration, "manifest.json"), manifest)
}

func executeTasks(ctx context.Context, suite Suite, agent harness.Harness, config Config, security harness.SecurityResolution, iteration string, tasks []runTask) ([]runResult, error) {
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
				finish := config.Progress.Started(progress.Event{
					Phase:   progress.PhaseRun,
					Case:    task.Case.ID,
					Trial:   task.Trial,
					Variant: task.Variant,
				})
				result, err := executeTask(ctx, suite, agent, config, security, iteration, task)
				finish(statusOf(err), err)
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

func executeTask(ctx context.Context, suite Suite, agent harness.Harness, config Config, security harness.SecurityResolution, iteration string, task runTask) (runResult, error) {
	if config.Mode == harness.ModeCompletion {
		return executeCompletionTask(ctx, suite, agent, config, iteration, task)
	}
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
	result, err := agent.Run(ctx, harness.Request{WorkDir: workDir, Prompt: prompt, Target: target, Model: config.Model, ReasoningEffort: config.ReasoningEffort, Security: security, Timeout: config.Timeout})
	if err != nil {
		runErr := fmt.Errorf("%s trial %d %s: %w", task.Case.ID, task.Trial, task.Variant, err)
		if attempts := harness.AttemptsFromError(err); attempts.AttemptCount > 0 {
			if writeErr := receipt.WriteTiming(filepath.Join(runDir, "timing.json"), harness.Usage{}, 0, attempts); writeErr != nil {
				return runResult{}, errors.Join(runErr, writeErr)
			}
		}
		return runResult{}, runErr
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
	if err := receipt.WriteTiming(filepath.Join(runDir, "timing.json"), result.Usage, result.Duration, result.Attempts); err != nil {
		return runResult{}, err
	}
	artifact, err := renderArtifact(artifactDir)
	if err != nil {
		return runResult{}, err
	}
	return runResult{Case: task.Case, Trial: task.Trial, Variant: task.Variant, RunDir: runDir, Agent: result, OutputPath: artifactDir, Artifact: artifact}, nil
}

func executeCompletionTask(ctx context.Context, suite Suite, agent harness.Harness, config Config, iteration string, task runTask) (runResult, error) {
	runDir := runDirectory(iteration, task.Case.ID, task.Variant, task.Trial)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return runResult{}, fmt.Errorf("create completion run directory: %w", err)
	}
	withVariant, _ := variantsFor(suite.Kind)
	prompt, err := buildCompletionPrompt(suite, task.Case, task.Variant == withVariant)
	if err != nil {
		return runResult{}, err
	}
	result, err := agent.Run(ctx, harness.Request{Mode: harness.ModeCompletion, Prompt: prompt, Model: config.Model, ReasoningEffort: config.ReasoningEffort, Timeout: config.Timeout})
	if err != nil {
		runErr := fmt.Errorf("%s trial %d %s: %w", task.Case.ID, task.Trial, task.Variant, err)
		if attempts := harness.AttemptsFromError(err); attempts.AttemptCount > 0 {
			if writeErr := receipt.WriteTiming(filepath.Join(runDir, "timing.json"), harness.Usage{}, 0, attempts); writeErr != nil {
				return runResult{}, errors.Join(runErr, writeErr)
			}
		}
		return runResult{}, runErr
	}
	artifactDir := filepath.Join(runDir, "outputs")
	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		return runResult{}, fmt.Errorf("create completion output directory: %w", err)
	}
	if err := os.WriteFile(filepath.Join(artifactDir, "response.md"), []byte(strings.TrimSpace(result.Response)+"\n"), 0o644); err != nil {
		return runResult{}, fmt.Errorf("write response artifact: %w", err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "transcript.jsonl"), result.Transcript, 0o644); err != nil {
		return runResult{}, fmt.Errorf("write transcript: %w", err)
	}
	if err := receipt.WriteTiming(filepath.Join(runDir, "timing.json"), result.Usage, result.Duration, result.Attempts); err != nil {
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
	if len(files) > 0 {
		fmt.Fprintf(&builder, "- Input files: %s\n", strings.Join(files, ", "))
	}
	fmt.Fprintf(&builder, "- Save all produced files under: %s\n", outputDir)
	return builder.String()
}

func buildCompletionPrompt(suite Suite, item Case, withGuidance bool) (string, error) {
	var builder strings.Builder
	builder.WriteString("Answer this evaluation task in one response. Do not use tools or modify files.\n")
	if withGuidance {
		contents, label, err := completionTarget(suite)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&builder, "\n--- begin %s ---\n%s\n--- end %s ---\n", label, strings.TrimRight(contents, "\n"), label)
	}
	fmt.Fprintf(&builder, "\nTask:\n%s\n", item.Prompt)
	for _, relative := range item.Files {
		contents, err := os.ReadFile(filepath.Join(suite.Root, filepath.Clean(relative)))
		if err != nil {
			return "", fmt.Errorf("read fixture %q for completion: %w", relative, err)
		}
		fmt.Fprintf(&builder, "\n--- begin fixture %s ---\n%s\n--- end fixture %s ---\n", filepath.ToSlash(relative), strings.TrimRight(string(contents), "\n"), filepath.ToSlash(relative))
	}
	return builder.String(), nil
}

func completionTarget(suite Suite) (string, string, error) {
	path := suite.TargetPath
	label := filepath.Base(path)
	if suite.Kind == harness.TargetSkill {
		path = filepath.Join(path, "SKILL.md")
		label = "SKILL.md"
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", "", fmt.Errorf("read %s for completion: %w", label, err)
	}
	return string(contents), label, nil
}

// statusOf maps an error to the status field of a finish event, so a consumer
// can filter without inspecting the error string.
func statusOf(err error) string {
	if err != nil {
		return "error"
	}
	return "ok"
}
