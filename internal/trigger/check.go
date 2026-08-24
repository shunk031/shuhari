package trigger

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shunk031/shuhari/internal/harness"
	"github.com/shunk031/shuhari/internal/receipt"
	"github.com/shunk031/shuhari/internal/skill"
	contracts "github.com/shunk031/shuhari/schemas"
)

type rawCase struct {
	ID            json.RawMessage `json:"id"`
	Prompt        string          `json:"prompt"`
	ShouldTrigger *bool           `json:"should_trigger"`
}

type triggerVerdict struct {
	SchemaVersion string                     `json:"schema_version"`
	Security      harness.SecurityResolution `json:"security"`
	DecisionRule  string                     `json:"decision_rule"`
	Passed        bool                       `json:"passed"`
	Reads         map[string][]bool          `json:"target_read"`
	Applications  map[string][]bool          `json:"target_applied"`
	Reasons       []string                   `json:"reasons,omitempty"`
	Error         string                     `json:"error,omitempty"`
}

func LoadSuite(skillPath, casesPath string) (Suite, error) {
	absolute, err := filepath.Abs(skillPath)
	if err != nil {
		return Suite{}, fmt.Errorf("resolve skill path: %w", err)
	}
	info, err := os.Stat(absolute)
	if err != nil || !info.IsDir() {
		if err != nil {
			return Suite{}, fmt.Errorf("inspect skill path: %w", err)
		}
		return Suite{}, fmt.Errorf("skill path is not a directory: %s", absolute)
	}
	metadata, err := skill.Load(absolute)
	if err != nil {
		return Suite{}, err
	}
	if casesPath == "" {
		casesPath = filepath.Join(absolute, "evals", "triggers.json")
	} else {
		casesPath, err = filepath.Abs(casesPath)
		if err != nil {
			return Suite{}, fmt.Errorf("resolve trigger cases: %w", err)
		}
	}
	contents, err := os.ReadFile(casesPath)
	if err != nil {
		return Suite{}, fmt.Errorf("read trigger cases: %w", err)
	}
	var raw struct {
		SkillName string    `json:"skill_name"`
		Cases     []rawCase `json:"cases"`
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil {
		return Suite{}, fmt.Errorf("decode trigger cases: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return Suite{}, errors.New("decode trigger cases: trailing JSON value")
	} else if !errors.Is(err, io.EOF) {
		return Suite{}, fmt.Errorf("decode trigger cases: %w", err)
	}
	if raw.SkillName != metadata.Name {
		return Suite{}, fmt.Errorf("trigger skill_name %q does not match SKILL.md name %q", raw.SkillName, metadata.Name)
	}
	if len(raw.Cases) == 0 {
		return Suite{}, errors.New("trigger cases must not be empty")
	}
	seen := map[string]bool{}
	seenPaths := map[string]string{}
	positive, negative := false, false
	cases := make([]Case, 0, len(raw.Cases))
	for index, item := range raw.Cases {
		id, err := decodeID(item.ID)
		if err != nil {
			return Suite{}, fmt.Errorf("cases[%d].id: %w", index, err)
		}
		if seen[id] {
			return Suite{}, fmt.Errorf("duplicate trigger case id %q", id)
		}
		seen[id] = true
		pathName := safeName(id)
		if previous, exists := seenPaths[pathName]; exists {
			return Suite{}, fmt.Errorf("trigger case ids %q and %q map to the same workspace path %q", previous, id, pathName)
		}
		seenPaths[pathName] = id
		if strings.TrimSpace(item.Prompt) == "" || item.ShouldTrigger == nil {
			return Suite{}, fmt.Errorf("cases[%d] requires prompt and should_trigger", index)
		}
		positive = positive || *item.ShouldTrigger
		negative = negative || !*item.ShouldTrigger
		cases = append(cases, Case{ID: id, Prompt: item.Prompt, ShouldTrigger: *item.ShouldTrigger})
	}
	if !positive || !negative {
		return Suite{}, errors.New("trigger cases require at least one positive and one negative control")
	}
	return Suite{SkillName: metadata.Name, Cases: cases, SkillPath: absolute, CasesPath: casesPath}, nil
}

func Run(ctx context.Context, suite Suite, agent harness.Harness, config Config) (Report, error) {
	if config.Trials < 1 || config.Jobs < 1 || config.Timeout <= 0 {
		return Report{}, errors.New("trials, jobs, and timeout must be positive")
	}
	level, err := harness.EffectiveSandboxLevel(config.SandboxLevel)
	if err != nil {
		return Report{}, err
	}
	config.SandboxLevel = string(level)
	policy := harness.SecurityPolicy{Level: level, Network: config.Network}
	security, err := agent.ResolveSecurity(ctx, policy)
	if err != nil {
		return Report{}, err
	}
	if err := harness.ValidateSecurityResolution(policy, security); err != nil {
		return Report{}, fmt.Errorf("security resolution: %w", err)
	}
	if !agent.Capabilities().TriggerEvidence {
		return Report{}, errors.New("selected agent does not expose trigger evidence")
	}
	identity, err := agent.Probe(ctx, security)
	if err != nil {
		return Report{}, err
	}
	digest, err := digestSuite(suite)
	if err != nil {
		return Report{}, err
	}
	iteration, err := createIteration(suite, config.Workspace)
	if err != nil {
		return Report{}, err
	}
	if err := writeTriggerManifest(iteration, suite, digest, identity, config, security); err != nil {
		return Report{Workspace: iteration}, err
	}
	measurement, runErr := measure(ctx, suite, agent, config, security, iteration)
	if runErr != nil {
		summary := triggerVerdict{SchemaVersion: triggerArtifactSchemaVersion, Security: security, DecisionRule: decisionRule(config.StrictAllTrials), Passed: false, Reads: measurement.Reads, Applications: measurement.Applications, Error: runErr.Error()}
		if err := contracts.Validate("trigger", summary); err != nil {
			return Report{Workspace: iteration}, err
		}
		_ = writeJSON(filepath.Join(iteration, "trigger.json"), summary)
		return Report{Workspace: iteration}, runErr
	}
	reasons := ApplyPolicy(suite, measurement, Policy{Trials: config.Trials, StrictAllTrials: config.StrictAllTrials})
	report := Report{Passed: len(reasons) == 0, Workspace: iteration, Reasons: reasons}
	summary := triggerVerdict{SchemaVersion: triggerArtifactSchemaVersion, Security: security, DecisionRule: decisionRule(config.StrictAllTrials), Passed: report.Passed, Reads: measurement.Reads, Applications: measurement.Applications, Reasons: reasons}
	if err := contracts.Validate("trigger", summary); err != nil {
		return report, err
	}
	if err := writeJSON(filepath.Join(iteration, "trigger.json"), summary); err != nil {
		return report, err
	}
	return report, nil
}

func measure(ctx context.Context, suite Suite, agent harness.Harness, config Config, security harness.SecurityResolution, iteration string) (Measurement, error) {
	type task struct {
		Case  Case
		Trial int
	}
	tasks := make(chan task, len(suite.Cases)*config.Trials)
	for _, item := range suite.Cases {
		for trial := 1; trial <= config.Trials; trial++ {
			tasks <- task{Case: item, Trial: trial}
		}
	}
	close(tasks)
	type outcome struct {
		Case    Case
		Trial   int
		Read    bool
		Applied bool
		Err     error
	}
	outcomes := make(chan outcome, len(suite.Cases)*config.Trials)
	workers := config.Jobs
	if limit := len(suite.Cases) * config.Trials; workers > limit {
		workers = limit
	}
	var group sync.WaitGroup
	group.Add(workers)
	for range workers {
		go func() {
			defer group.Done()
			for item := range tasks {
				result, err := executeCase(ctx, suite, agent, config, security, iteration, item.Case, item.Trial)
				outcomes <- outcome{Case: item.Case, Trial: item.Trial, Read: result.TargetRead, Applied: result.Applied, Err: err}
			}
		}()
	}
	group.Wait()
	close(outcomes)
	readsByTrial := map[string]map[int]bool{}
	applicationsByTrial := map[string]map[int]bool{}
	var firstError error
	for item := range outcomes {
		if item.Err != nil && firstError == nil {
			firstError = item.Err
		}
		if item.Err == nil {
			if readsByTrial[item.Case.ID] == nil {
				readsByTrial[item.Case.ID] = map[int]bool{}
			}
			readsByTrial[item.Case.ID][item.Trial] = item.Read
			if applicationsByTrial[item.Case.ID] == nil {
				applicationsByTrial[item.Case.ID] = map[int]bool{}
			}
			applicationsByTrial[item.Case.ID][item.Trial] = item.Applied
		}
	}
	reads := map[string][]bool{}
	applications := map[string][]bool{}
	for _, item := range suite.Cases {
		for trial := 1; trial <= config.Trials; trial++ {
			if value, ok := readsByTrial[item.ID][trial]; ok {
				reads[item.ID] = append(reads[item.ID], value)
			}
			if value, ok := applicationsByTrial[item.ID][trial]; ok {
				applications[item.ID] = append(applications[item.ID], value)
			}
		}
	}
	if firstError != nil {
		return Measurement{Reads: reads, Applications: applications}, firstError
	}
	return Measurement{Reads: reads, Applications: applications}, nil
}

func ApplyPolicy(suite Suite, measurement Measurement, policy Policy) []string {
	var reasons []string
	for _, item := range suite.Cases {
		applications := measurement.Applications[item.ID]
		if len(applications) != policy.Trials || !casePass(applications, item.ShouldTrigger, policy.StrictAllTrials) {
			reasons = append(reasons, fmt.Sprintf("%s: trigger outcomes did not satisfy policy", item.ID))
		}
	}
	return reasons
}

func executeCase(ctx context.Context, suite Suite, agent harness.Harness, config Config, security harness.SecurityResolution, iteration string, item Case, trial int) (applicationArtifact, error) {
	runDir := filepath.Join(iteration, "case-"+safeName(item.ID), fmt.Sprintf("trial-%d", trial))
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return applicationArtifact{}, fmt.Errorf("create trigger run directory: %w", err)
	}
	workDir, err := os.MkdirTemp("", "shuhari-trigger-")
	if err != nil {
		return applicationArtifact{}, fmt.Errorf("create trigger work directory: %w", err)
	}
	defer os.RemoveAll(workDir)
	result, err := agent.Run(ctx, harness.Request{WorkDir: workDir, Prompt: item.Prompt, Target: &harness.Target{Kind: harness.TargetSkill, Name: suite.SkillName, SourcePath: suite.SkillPath}, Model: config.Model, ReasoningEffort: config.ReasoningEffort, Security: security, Timeout: config.Timeout})
	if err != nil {
		runErr := fmt.Errorf("%s trial %d: %w", item.ID, trial, err)
		if attempts := harness.AttemptsFromError(err); attempts.AttemptCount > 0 {
			if writeErr := receipt.WriteTiming(filepath.Join(runDir, "timing.json"), harness.Usage{}, 0, attempts); writeErr != nil {
				return applicationArtifact{}, errors.Join(runErr, writeErr)
			}
		}
		return applicationArtifact{}, runErr
	}
	if err := os.WriteFile(filepath.Join(runDir, "transcript.jsonl"), result.Transcript, 0o644); err != nil {
		return applicationArtifact{}, fmt.Errorf("write trigger transcript: %w", err)
	}
	if err := receipt.WriteTiming(filepath.Join(runDir, "timing.json"), result.Usage, result.Duration, result.Attempts); err != nil {
		return applicationArtifact{}, err
	}
	application, err := classifyApplication(ctx, suite, agent, config, security, runDir, item, result)
	if err != nil {
		return application, fmt.Errorf("%s trial %d: %w", item.ID, trial, err)
	}
	return application, nil
}

func casePass(reads []bool, shouldTrigger, strict bool) bool {
	if len(reads) == 0 {
		return false
	}
	required := len(reads)/2 + 1
	if strict {
		required = len(reads)
	}
	matches := 0
	for _, read := range reads {
		if read == shouldTrigger {
			matches++
		}
	}
	return matches >= required
}

func decisionRule(strict bool) string {
	if strict {
		return "strict"
	}
	return "majority"
}

func decodeID(raw json.RawMessage) (string, error) {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil && strings.TrimSpace(text) != "" {
		return text, nil
	}
	var number json.Number
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&number); err != nil {
		return "", errors.New("must be a non-empty string or integer")
	}
	if _, err := strconv.ParseInt(number.String(), 10, 64); err != nil {
		return "", errors.New("numeric id must be an integer")
	}
	return number.String(), nil
}

func createIteration(suite Suite, configured string) (string, error) {
	root := configured
	if root == "" {
		root = filepath.Join(filepath.Dir(suite.SkillPath), suite.SkillName+"-workspace")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", fmt.Errorf("create trigger workspace: %w", err)
	}
	next := 1
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", fmt.Errorf("read trigger workspace: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "trigger-iteration-") {
			continue
		}
		value, err := strconv.Atoi(strings.TrimPrefix(entry.Name(), "trigger-iteration-"))
		if err == nil && value >= next {
			next = value + 1
		}
	}
	iteration := filepath.Join(root, fmt.Sprintf("trigger-iteration-%d", next))
	if err := os.Mkdir(iteration, 0o755); err != nil {
		return "", fmt.Errorf("create trigger iteration: %w", err)
	}
	return iteration, nil
}

func digestSuite(suite Suite) (string, error) {
	hash := sha256.New()
	var paths []string
	if err := filepath.WalkDir(suite.SkillPath, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() {
			paths = append(paths, path)
		}
		return nil
	}); err != nil {
		return "", fmt.Errorf("walk skill: %w", err)
	}
	sort.Strings(paths)
	for _, path := range paths {
		info, err := os.Lstat(path)
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("skill contains a symlink: %s", path)
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		relative, err := filepath.Rel(suite.SkillPath, path)
		if err != nil {
			return "", err
		}
		_, _ = hash.Write([]byte(filepath.ToSlash(relative)))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write(contents)
		_, _ = hash.Write([]byte{0})
	}
	if !pathWithin(suite.CasesPath, suite.SkillPath) {
		info, err := os.Lstat(suite.CasesPath)
		if err != nil {
			return "", fmt.Errorf("inspect trigger cases: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("trigger cases file is a symlink: %s", suite.CasesPath)
		}
		contents, err := os.ReadFile(suite.CasesPath)
		if err != nil {
			return "", fmt.Errorf("read trigger cases: %w", err)
		}
		_, _ = hash.Write([]byte(filepath.Clean(suite.CasesPath)))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write(contents)
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func writeTriggerManifest(iteration string, suite Suite, suiteDigest string, identity harness.Identity, config Config, security harness.SecurityResolution) error {
	policy := harness.SecurityPolicy{Level: harness.SandboxLevel(config.SandboxLevel), Network: config.Network}
	if err := harness.ValidateSecurityResolution(policy, security); err != nil {
		return fmt.Errorf("validate manifest security: %w", err)
	}
	manifest := struct {
		SchemaVersion string                     `json:"schema_version"`
		CreatedAt     time.Time                  `json:"created_at"`
		TargetKind    harness.TargetKind         `json:"target_kind"`
		TargetName    string                     `json:"target_name"`
		SuiteDigest   string                     `json:"suite_digest"`
		AgentIdentity harness.Identity           `json:"agent_identity"`
		Config        Config                     `json:"config"`
		Security      harness.SecurityResolution `json:"security"`
	}{SchemaVersion: triggerManifestSchemaVersion, CreatedAt: time.Now().UTC(), TargetKind: harness.TargetSkill, TargetName: suite.SkillName, SuiteDigest: suiteDigest, AgentIdentity: identity, Config: config, Security: security}
	if err := contracts.Validate("workspace", manifest); err != nil {
		return err
	}
	return writeJSON(filepath.Join(iteration, "manifest.json"), manifest)
}

func pathWithin(path, root string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func safeName(value string) string {
	replacer := strings.NewReplacer("/", "-", "\\", "-", " ", "-")
	return strings.Trim(replacer.Replace(value), "-.")
}

func writeJSON(path string, value any) error {
	contents, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	contents = append(contents, '\n')
	return os.WriteFile(path, contents, 0o644)
}
