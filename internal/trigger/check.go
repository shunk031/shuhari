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

	"github.com/shunk031/shuhari/internal/cache"
	"github.com/shunk031/shuhari/internal/harness"
	"github.com/shunk031/shuhari/internal/skill"
)

type rawCase struct {
	ID            json.RawMessage `json:"id"`
	Prompt        string          `json:"prompt"`
	ShouldTrigger *bool           `json:"should_trigger"`
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

func Run(ctx context.Context, suite Suite, agent harness.Harness, store cache.Store, config Config) (Report, error) {
	if config.Trials < 1 || config.Jobs < 1 || config.Timeout <= 0 {
		return Report{}, errors.New("trials, jobs, and timeout must be positive")
	}
	if !agent.Capabilities().TriggerEvidence {
		return Report{}, errors.New("selected agent does not expose trigger evidence")
	}
	identity, err := agent.Probe(ctx)
	if err != nil {
		return Report{}, err
	}
	digest, err := digestSuite(suite)
	if err != nil {
		return Report{}, err
	}
	runnerDigest, err := cache.RunnerDigest()
	if err != nil {
		return Report{}, err
	}
	options, _ := json.Marshal(struct {
		Digest       string
		RunnerDigest string
		Identity     harness.Identity
		Config       Config
	}{Digest: digest, RunnerDigest: runnerDigest, Identity: identity, Config: config})
	key := cache.Key(options)
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
		Case  Case
		Trial int
		Read  bool
		Err   error
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
				read, err := executeCase(ctx, suite, agent, config, iteration, item.Case, item.Trial)
				outcomes <- outcome{Case: item.Case, Trial: item.Trial, Read: read, Err: err}
			}
		}()
	}
	group.Wait()
	close(outcomes)
	reads := map[string][]bool{}
	var firstError error
	for item := range outcomes {
		if item.Err != nil && firstError == nil {
			firstError = item.Err
		}
		if item.Err == nil {
			reads[item.Case.ID] = append(reads[item.Case.ID], item.Read)
		}
	}
	if firstError != nil {
		return Report{Workspace: iteration}, firstError
	}
	var reasons []string
	for _, item := range suite.Cases {
		if !casePass(reads[item.ID], item.ShouldTrigger, config.StrictAllTrials) {
			reasons = append(reasons, fmt.Sprintf("%s: trigger outcomes did not satisfy policy", item.ID))
		}
	}
	report := Report{Passed: len(reasons) == 0, Workspace: iteration, Reasons: reasons}
	summary := struct {
		Passed  bool              `json:"passed"`
		Results map[string][]bool `json:"target_read"`
		Reasons []string          `json:"reasons,omitempty"`
	}{Passed: report.Passed, Results: reads, Reasons: reasons}
	if err := writeJSON(filepath.Join(iteration, "trigger.json"), summary); err != nil {
		return report, err
	}
	if report.Passed && !config.NoCache {
		if err := store.PutSuccess(key, cache.Record{Passed: true, CreatedAt: time.Now().UTC(), Workspace: iteration}); err != nil {
			return report, err
		}
	}
	return report, nil
}

func executeCase(ctx context.Context, suite Suite, agent harness.Harness, config Config, iteration string, item Case, trial int) (bool, error) {
	runDir := filepath.Join(iteration, "case-"+safeName(item.ID), fmt.Sprintf("trial-%d", trial))
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return false, fmt.Errorf("create trigger run directory: %w", err)
	}
	workDir, err := os.MkdirTemp("", "shuhari-trigger-")
	if err != nil {
		return false, fmt.Errorf("create trigger work directory: %w", err)
	}
	defer os.RemoveAll(workDir)
	result, err := agent.Run(ctx, harness.Request{WorkDir: workDir, Prompt: item.Prompt, Target: &harness.Target{Kind: harness.TargetSkill, Name: suite.SkillName, SourcePath: suite.SkillPath}, Model: config.Model, ReasoningEffort: config.ReasoningEffort, Sandbox: config.Sandbox, Network: config.Network, Timeout: config.Timeout})
	if err != nil {
		return false, fmt.Errorf("%s trial %d: %w", item.ID, trial, err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "transcript.jsonl"), result.Transcript, 0o644); err != nil {
		return false, fmt.Errorf("write trigger transcript: %w", err)
	}
	timing := struct {
		TotalTokens int64 `json:"total_tokens"`
		DurationMS  int64 `json:"duration_ms"`
	}{TotalTokens: result.Usage.TotalTokens(), DurationMS: result.Duration.Milliseconds()}
	if err := writeJSON(filepath.Join(runDir, "timing.json"), timing); err != nil {
		return false, err
	}
	return result.TargetRead, nil
}

func casePass(reads []bool, shouldTrigger, strict bool) bool {
	if len(reads) == 0 {
		return false
	}
	if !shouldTrigger || strict {
		for _, read := range reads {
			if read != shouldTrigger {
				return false
			}
		}
		return true
	}
	count := 0
	for _, read := range reads {
		if read {
			count++
		}
	}
	return count >= len(reads)/2+1
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
