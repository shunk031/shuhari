package harness

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var transientPattern = regexp.MustCompile(`(?i)(?:429|too many requests|timed? ?out|timeout|connection|network|tls|temporar|unavailable|rate.?limit|reset by peer|empty response)`)

type codexHarness struct {
	executable string
}

func newCodex(config Config) *codexHarness {
	executable := config.Executable
	if executable == "" {
		executable = "codex"
	}
	return &codexHarness{executable: executable}
}

func (h *codexHarness) Probe(ctx context.Context) (Identity, error) {
	output, err := exec.CommandContext(ctx, h.executable, "--version").CombinedOutput()
	if err != nil {
		return Identity{}, fmt.Errorf("probe codex: %w: %s", err, strings.TrimSpace(string(output)))
	}
	configPath, err := codexConfigPath()
	if err != nil {
		return Identity{}, err
	}
	configDigest, err := codexConfigurationDigest(configPath)
	if err != nil {
		return Identity{}, err
	}
	executableDigest, err := executableDigest(h.executable)
	if err != nil {
		return Identity{}, err
	}
	return Identity{
		Agent:             "codex",
		Version:           strings.TrimSpace(string(output)),
		ConfigDigest:      configDigest,
		ExecutableDigest:  executableDigest,
		EnvironmentDigest: environmentDigest(cleanEnvironment("")),
	}, nil
}

func executableDigest(executable string) (string, error) {
	path, err := exec.LookPath(executable)
	if err != nil {
		return "", fmt.Errorf("find Codex executable: %w", err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read Codex executable: %w", err)
	}
	digest := sha256.Sum256(contents)
	return hex.EncodeToString(digest[:]), nil
}

func environmentDigest(environment []string) string {
	entries := append([]string(nil), environment...)
	sort.Strings(entries)
	digest := sha256.New()
	for _, entry := range entries {
		_, _ = digest.Write([]byte(entry))
		_, _ = digest.Write([]byte{0})
	}
	return hex.EncodeToString(digest.Sum(nil))
}

func codexConfigPath() (string, error) {
	root := os.Getenv("CODEX_HOME")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("find home directory: %w", err)
		}
		root = filepath.Join(home, ".codex")
	}
	return filepath.Join(root, "config.toml"), nil
}

func codexConfigurationDigest(path string) (string, error) {
	contents, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read Codex config: %w", err)
	}
	digest := sha256.Sum256(contents)
	return hex.EncodeToString(digest[:]), nil
}

func (*codexHarness) Capabilities() Capabilities {
	return Capabilities{Skills: true, Instructions: true, TriggerEvidence: true}
}

func (h *codexHarness) Run(ctx context.Context, request Request) (Result, error) {
	if request.WorkDir == "" {
		return Result{}, errors.New("codex work directory is required")
	}
	if request.Timeout <= 0 {
		return Result{}, errors.New("codex timeout must be positive")
	}
	if err := os.MkdirAll(request.WorkDir, 0o755); err != nil {
		return Result{}, fmt.Errorf("create work directory: %w", err)
	}
	snapshotRoot, err := os.MkdirTemp("", "shuhari-codex-snapshot-")
	if err != nil {
		return Result{}, fmt.Errorf("create work directory snapshot: %w", err)
	}
	defer os.RemoveAll(snapshotRoot)
	snapshot := filepath.Join(snapshotRoot, "workdir")
	if err := copyTree(request.WorkDir, snapshot); err != nil {
		return Result{}, fmt.Errorf("snapshot work directory: %w", err)
	}
	request.Sandbox = EffectiveSandbox(request.Sandbox)
	var last error
	for attempt := 0; attempt < 2; attempt++ {
		if err := restoreDirectory(snapshot, request.WorkDir); err != nil {
			return Result{}, err
		}
		result, err := h.runOnce(ctx, request)
		if err == nil {
			if strings.TrimSpace(result.Response) == "" {
				last = fmt.Errorf("%w: codex returned an empty response", ErrTransient)
				continue
			}
			return result, nil
		}
		last = err
		if !errors.Is(err, ErrTransient) {
			return Result{}, err
		}
	}
	return Result{}, last
}

func (h *codexHarness) runOnce(parent context.Context, request Request) (Result, error) {
	if err := ensureGitRepository(request.WorkDir); err != nil {
		return Result{}, err
	}
	if request.Target != nil {
		if err := installCodexTarget(request.WorkDir, *request.Target); err != nil {
			return Result{}, err
		}
	}

	temporary, err := os.MkdirTemp("", "shuhari-codex-")
	if err != nil {
		return Result{}, fmt.Errorf("create codex temporary directory: %w", err)
	}
	defer os.RemoveAll(temporary)
	codexHome := filepath.Join(temporary, "codex-home")
	if err := initializeCodexHome(codexHome); err != nil {
		return Result{}, err
	}

	args := []string{"--disable", "plugins", "exec"}
	if request.Model != "" {
		args = append(args, "--model", request.Model)
	}
	if request.ReasoningEffort != "" {
		value, _ := json.Marshal(request.ReasoningEffort)
		args = append(args, "-c", "model_reasoning_effort="+string(value))
	}
	if request.Network {
		args = append(args, "-c", "sandbox_workspace_write.network_access=true")
	}
	if override := disabledSkillsOverride(); override != "" {
		args = append(args, "-c", override)
	}
	args = append(args, "--ephemeral", "--json", "--sandbox", EffectiveSandbox(request.Sandbox), "--cd", request.WorkDir)
	if len(request.OutputSchema) > 0 {
		schemaPath := filepath.Join(temporary, "output-schema.json")
		if err := os.WriteFile(schemaPath, request.OutputSchema, 0o600); err != nil {
			return Result{}, fmt.Errorf("write output schema: %w", err)
		}
		args = append(args, "--output-schema", schemaPath)
	}
	args = append(args, "-")

	ctx, cancel := context.WithTimeout(parent, request.Timeout)
	defer cancel()
	command := exec.CommandContext(ctx, h.executable, args...)
	command.Stdin = strings.NewReader(request.Prompt)
	command.Env = codexEnvironment(codexHome)
	started := time.Now()
	stdout, stderr := bytes.Buffer{}, bytes.Buffer{}
	command.Stdout = &stdout
	command.Stderr = &stderr
	err = command.Run()
	duration := time.Since(started)
	if ctx.Err() == context.DeadlineExceeded {
		return Result{}, fmt.Errorf("%w: codex timed out after %s", ErrTransient, request.Timeout)
	}
	if err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = strings.TrimSpace(stdout.String())
		}
		if transientPattern.MatchString(message) {
			return Result{}, fmt.Errorf("%w: %s", ErrTransient, message)
		}
		return Result{}, fmt.Errorf("codex failed: %w: %s", err, message)
	}
	result, err := parseCodexTrace(stdout.Bytes(), targetOrEmpty(request.Target))
	if err != nil {
		return Result{}, err
	}
	result.Transcript = append([]byte(nil), stdout.Bytes()...)
	result.Duration = duration
	return result, nil
}

func targetOrEmpty(target *Target) Target {
	if target == nil {
		return Target{}
	}
	return *target
}

func parseCodexTrace(trace []byte, target Target) (Result, error) {
	result := Result{TargetRead: target.Kind == TargetInstructions}
	type actionObservation struct {
		Index  int
		Action Action
	}
	var targetReadEvents []int
	var actionEvents []actionObservation
	lastMessageEvent := -1
	turnCompleted := false
	eventIndex := 0
	scanner := bufio.NewScanner(bytes.NewReader(trace))
	buffer := make([]byte, 64*1024)
	scanner.Buffer(buffer, 4*1024*1024)
	for scanner.Scan() {
		var event struct {
			Type    string          `json:"type"`
			Item    json.RawMessage `json:"item"`
			Usage   Usage           `json:"usage"`
			Message string          `json:"message"`
			Error   struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return Result{}, fmt.Errorf("decode Codex JSONL event %d: %w", eventIndex+1, err)
		}
		eventIndex++
		switch event.Type {
		case "turn.completed":
			result.Usage = event.Usage
			turnCompleted = true
		case "turn.failed", "error":
			message := event.Error.Message
			if message == "" {
				message = event.Message
			}
			if message == "" {
				message = "Codex turn failed without an error message"
			}
			return Result{}, errors.New(message)
		case "item.started", "item.updated", "item.completed":
			var item struct {
				Type     string `json:"type"`
				Text     string `json:"text"`
				Command  string `json:"command"`
				Query    string `json:"query"`
				Status   string `json:"status"`
				ExitCode *int   `json:"exit_code"`
			}
			if err := json.Unmarshal(event.Item, &item); err != nil {
				return Result{}, fmt.Errorf("decode Codex item event %d: %w", eventIndex, err)
			}
			switch item.Type {
			case "agent_message":
				if event.Type == "item.completed" && item.Text != "" {
					result.Response = item.Text
					lastMessageEvent = eventIndex
				}
			case "file_change":
				if event.Type == "item.completed" && item.Status == "completed" {
					actionEvents = append(actionEvents, actionObservation{Index: eventIndex, Action: ActionFileChange})
				}
			case "web_search":
				if event.Type == "item.completed" && (item.Status == "" || item.Status == "completed") {
					if strings.Contains(strings.ToLower(item.Query), "github") {
						actionEvents = append(actionEvents, actionObservation{Index: eventIndex, Action: ActionGitHubSearch})
					} else {
						actionEvents = append(actionEvents, actionObservation{Index: eventIndex, Action: ActionWebSearch})
					}
				}
			case "command_execution":
				if event.Type != "item.completed" || item.Status != "completed" || (item.ExitCode != nil && *item.ExitCode != 0) {
					continue
				}
				if target.Kind == TargetSkill && commandReadsSkill(item.Command, target.Name) {
					targetReadEvents = append(targetReadEvents, eventIndex)
				}
				for _, action := range classifyCommand(item.Command) {
					actionEvents = append(actionEvents, actionObservation{Index: eventIndex, Action: action})
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return Result{}, fmt.Errorf("scan codex trace: %w", err)
	}
	if !turnCompleted {
		return Result{}, errors.New("Codex trace ended without turn.completed")
	}
	if target.Kind == TargetSkill && lastMessageEvent >= 0 {
		for _, index := range targetReadEvents {
			if index < lastMessageEvent {
				result.TargetRead = true
				break
			}
		}
	}
	for _, observation := range actionEvents {
		if lastMessageEvent < 0 || observation.Index < lastMessageEvent {
			result.Actions = append(result.Actions, observation.Action)
		}
	}
	return result, nil
}

var (
	githubCommand = regexp.MustCompile(`(?i)\b(?:gh\s+(?:api|browse|repo|search)|git\s+(?:clone|fetch)|curl|wget)\b.*(?:github\.com|githubusercontent\.com)`)
	webCommand    = regexp.MustCompile(`(?i)\b(?:curl|wget)\b.*https?://`)
	readCommand   = regexp.MustCompile(`(?i)(?:\b(?:cat|sed|awk|grep|rg|head|tail|less|more|bat|nl|wc|cut|sort|uniq|sha1sum|sha256sum|sha512sum)\b|(?:open|read_text|readFile|readFileSync)\s*\()`)
)

func commandReadsSkill(command, skillName string) bool {
	normalized := filepath.ToSlash(command)
	directory := filepath.ToSlash(filepath.Join("skills", skillName))
	marker := directory + "/SKILL.md"
	mentionsTarget := strings.Contains(normalized, marker) ||
		(strings.Contains(normalized, directory) && strings.Contains(normalized, "SKILL.md"))
	return mentionsTarget && readCommand.MatchString(command)
}

func classifyCommand(command string) []Action {
	if githubCommand.MatchString(command) {
		return []Action{ActionGitHubSearch}
	}
	if webCommand.MatchString(command) {
		return []Action{ActionWebSearch}
	}
	return nil
}

func installCodexTarget(workDir string, target Target) error {
	switch target.Kind {
	case TargetSkill:
		destination := filepath.Join(workDir, ".agents", "skills", target.Name)
		if err := copyTree(target.SourcePath, destination); err != nil {
			return fmt.Errorf("install skill: %w", err)
		}
	case TargetInstructions:
		contents, err := os.ReadFile(target.SourcePath)
		if err != nil {
			return fmt.Errorf("read instructions: %w", err)
		}
		if err := os.WriteFile(filepath.Join(workDir, "AGENTS.md"), contents, 0o644); err != nil {
			return fmt.Errorf("install instructions: %w", err)
		}
	default:
		return fmt.Errorf("unsupported target kind %q", target.Kind)
	}
	return nil
}

func copyTree(source, destination string) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink is not supported: %s", path)
		}
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, contents, info.Mode().Perm())
	})
}

func restoreDirectory(snapshot, destination string) error {
	entries, err := os.ReadDir(destination)
	if err != nil {
		return fmt.Errorf("read work directory before retry: %w", err)
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(destination, entry.Name())); err != nil {
			return fmt.Errorf("reset work directory before retry: %w", err)
		}
	}
	if err := copyTree(snapshot, destination); err != nil {
		return fmt.Errorf("restore work directory before retry: %w", err)
	}
	return nil
}

func ensureGitRepository(path string) error {
	if _, err := os.Stat(filepath.Join(path, ".git")); err == nil {
		return nil
	}
	command := exec.Command("git", "init", "-q", path)
	command.Env = cleanEnvironment("")
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("initialize temporary git repository: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func initializeCodexHome(destination string) error {
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return fmt.Errorf("create codex home: %w", err)
	}
	source := os.Getenv("CODEX_HOME")
	if source == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("find home directory: %w", err)
		}
		source = filepath.Join(home, ".codex")
	}
	for _, name := range []string{"config.toml", "auth.json"} {
		from := filepath.Join(source, name)
		info, err := os.Stat(from)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect codex %s: %w", name, err)
		}
		contents, err := os.ReadFile(from)
		if err != nil {
			return fmt.Errorf("read codex %s: %w", name, err)
		}
		if err := os.WriteFile(filepath.Join(destination, name), contents, info.Mode().Perm()); err != nil {
			return fmt.Errorf("copy codex %s: %w", name, err)
		}
	}
	return nil
}

func disabledSkillsOverride() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	var paths []string
	for _, root := range []string{filepath.Join(home, ".agents", "skills"), filepath.Join(home, ".codex", "skills")} {
		matches, _ := filepath.Glob(filepath.Join(root, "*", "SKILL.md"))
		paths = append(paths, matches...)
	}
	if len(paths) == 0 {
		return ""
	}
	sort.Strings(paths)
	entries := make([]string, 0, len(paths))
	for _, path := range paths {
		absolute, err := filepath.Abs(path)
		if err != nil {
			continue
		}
		encoded, _ := json.Marshal(absolute)
		entries = append(entries, "{path="+string(encoded)+",enabled=false}")
	}
	return "skills.config=[" + strings.Join(entries, ",") + "]"
}

func codexEnvironment(codexHome string) []string {
	return cleanEnvironment(codexHome)
}

func cleanEnvironment(codexHome string) []string {
	result := make([]string, 0, 32)
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if !allowedEnvironmentVariable(name) {
			continue
		}
		result = append(result, entry)
	}
	if codexHome != "" {
		result = append(result, "CODEX_HOME="+codexHome)
	}
	sort.Strings(result)
	return result
}

func allowedEnvironmentVariable(name string) bool {
	if strings.HasPrefix(name, "LC_") {
		return true
	}
	switch name {
	case "PATH", "HOME", "USER", "LOGNAME", "SHELL", "TMPDIR", "TEMP", "TMP", "LANG", "LANGUAGE", "TERM", "COLORTERM", "TZ",
		"SSL_CERT_FILE", "SSL_CERT_DIR", "CURL_CA_BUNDLE", "REQUESTS_CA_BUNDLE",
		"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "all_proxy", "no_proxy":
		return true
	default:
		return false
	}
}
