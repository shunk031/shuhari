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
	"runtime"
	"sort"
	"strings"
	"time"
)

var transientPattern = regexp.MustCompile(`(?i)(?:429|too many requests|timed? ?out|timeout|connection|network|tls|temporar|unavailable|rate.?limit|reset by peer|empty response|stream disconnected before completion|error decoding response body)`)
var inputTooLargePattern = regexp.MustCompile(`(?i)(?:input_too_large|input exceeds the maximum length)`)

const maxCodexAttempts = 3

const (
	firstTokenTimeoutEnv     = "SHUHARI_FIRST_TOKEN_TIMEOUT"
	defaultFirstTokenTimeout = 90 * time.Second
)

type codexHarness struct {
	executable      string
	waitBeforeRetry func(context.Context, int) error
}

func newCodex(config Config) *codexHarness {
	executable := config.Executable
	if executable == "" {
		executable = "codex"
	}
	return &codexHarness{executable: executable, waitBeforeRetry: waitForCodexRetry}
}

func waitForCodexRetry(ctx context.Context, retry int) error {
	delay := 250 * time.Millisecond * time.Duration(1<<(retry-1))
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (h *codexHarness) Probe(ctx context.Context, securities ...SecurityResolution) (Identity, error) {
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
	seen := map[string]bool{}
	for _, security := range securities {
		if err := validateCodexSecurityResolution(security); err != nil {
			return Identity{}, err
		}
		if security.SandboxLevel == SandboxUnsandboxed || seen[security.Adapter.PolicyDigest] {
			continue
		}
		seen[security.Adapter.PolicyDigest] = true
		if err := h.probeSandbox(ctx, security); err != nil {
			return Identity{}, err
		}
	}
	return Identity{
		Agent:             "codex",
		Version:           strings.TrimSpace(string(output)),
		ConfigDigest:      configDigest,
		ExecutableDigest:  executableDigest,
		EnvironmentDigest: environmentDigest(cleanEnvironment("")),
	}, nil
}

// sandboxProbeCommand returns a command that exits zero and does nothing else.
// The preflight only needs to learn whether the native sandbox starts, so the
// command must succeed anywhere the sandbox itself can run.
//
// It deliberately avoids `/bin/true`: that path does not exist on every
// platform, and recent macOS ships the binary only at `/usr/bin/true`, which
// made the preflight fail there for a reason unrelated to the sandbox. POSIX
// requires `/bin/sh`, and `:` is a shell builtin, so this depends on nothing
// beyond the shell.
func sandboxProbeCommand() []string {
	return sandboxProbeCommandFor(runtime.GOOS)
}

// sandboxProbeCommandFor takes the operating system as an argument so both
// branches are reachable from a test on a single host.
func sandboxProbeCommandFor(goos string) []string {
	if goos == "windows" {
		return []string{"cmd.exe", "/c", "exit", "0"}
	}
	return []string{"/bin/sh", "-c", ":"}
}

func (h *codexHarness) probeSandbox(ctx context.Context, security SecurityResolution) error {
	temporary, err := secureTemporaryDirectory("shuhari-codex-probe-")
	if err != nil {
		return fmt.Errorf("create Codex sandbox preflight directory: %w", err)
	}
	defer os.RemoveAll(temporary)
	workDir := filepath.Join(temporary, "work")
	codexHome := filepath.Join(temporary, "codex-home")
	for _, directory := range []string{workDir, codexHome} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return fmt.Errorf("create Codex sandbox preflight directory: %w", err)
		}
	}
	if err := writeCodexProfile(codexHome, Request{WorkDir: workDir, Security: security}); err != nil {
		return fmt.Errorf("write Codex sandbox preflight profile: %w", err)
	}
	args := []string{"sandbox", "--profile", "shuhari", "--permission-profile", "shuhari-eval", "--cd", workDir}
	args = append(args, sandboxProbeCommand()...)
	command := exec.CommandContext(ctx, h.executable, args...)
	command.Env = cleanEnvironment(codexHome)
	output, err := command.CombinedOutput()
	if err == nil {
		return nil
	}
	reason := strings.TrimSpace(string(output))
	if reason == "" {
		reason = err.Error()
	}
	return &UnsupportedSecurityPolicyError{
		Adapter: "codex",
		Policy:  security.Policy(),
		Reason:  fmt.Sprintf("native sandbox preflight failed: %s; use unsandboxed with --network=true only inside an isolated runner or container", reason),
	}
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

const (
	codexIsolatedNativeMode    = "workspace-write+shuhari-permission-profile"
	codexReadOnlyNativeMode    = "read-only+shuhari-permission-profile"
	codexUnsandboxedNativeMode = "danger-full-access"
	codexSecurityPolicyVersion = "codex-security-v2"
)

var proxyEnvironmentVariables = [...]string{
	"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
	"http_proxy", "https_proxy", "all_proxy", "no_proxy",
}

func (*codexHarness) ResolveSecurity(_ context.Context, policy SecurityPolicy) (SecurityResolution, error) {
	if err := ValidateSecurityPolicy(policy); err != nil {
		var unsupported *UnsupportedSecurityPolicyError
		if errors.As(err, &unsupported) {
			return SecurityResolution{}, &UnsupportedSecurityPolicyError{Adapter: "codex", Policy: policy, Reason: unsupported.Reason}
		}
		return SecurityResolution{}, err
	}
	nativeMode := ""
	boundary := CredentialBoundaryEnforced
	switch policy.Level {
	case SandboxIsolated:
		nativeMode = codexIsolatedNativeMode
	case SandboxReadOnly:
		nativeMode = codexReadOnlyNativeMode
	case SandboxUnsandboxed:
		nativeMode = codexUnsandboxedNativeMode
		boundary = CredentialBoundaryNone
	default:
		return SecurityResolution{}, &UnsupportedSecurityPolicyError{Adapter: "codex", Policy: policy, Reason: "unknown neutral sandbox level"}
	}
	network := NetworkDenied
	if policy.Network {
		network = NetworkAllowed
	}
	if _, err := resolveHostTools(policy.HostTools); err != nil {
		return SecurityResolution{}, &UnsupportedSecurityPolicyError{Adapter: "codex", Policy: policy, Reason: err.Error()}
	}
	resolution := SecurityResolution{
		SandboxLevel:       policy.Level,
		NetworkAccess:      network,
		CredentialBoundary: boundary,
		HostTools:          policy.HostTools,
		Adapter: AdapterSecurity{
			Name:         "codex",
			NativeMode:   nativeMode,
			PolicyDigest: codexSecurityPolicyDigest(policy, nativeMode),
		},
	}
	if err := ValidateSecurityResolution(policy, resolution); err != nil {
		return SecurityResolution{}, err
	}
	return resolution, nil
}

func codexSecurityPolicyDigest(policy SecurityPolicy, nativeMode string) string {
	encoded, _ := json.Marshal(struct {
		Version    string         `json:"version"`
		Policy     SecurityPolicy `json:"policy"`
		NativeMode string         `json:"native_mode"`
	}{Version: codexSecurityPolicyVersion, Policy: policy, NativeMode: nativeMode})
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func (h *codexHarness) Run(ctx context.Context, request Request) (Result, error) {
	if request.WorkDir == "" {
		return Result{}, errors.New("codex work directory is required")
	}
	if request.Timeout <= 0 {
		return Result{}, errors.New("codex timeout must be positive")
	}
	if err := validateCodexSecurityResolution(request.Security); err != nil {
		return Result{}, err
	}
	firstTokenTimeout, err := configuredFirstTokenTimeout()
	if err != nil {
		return Result{}, err
	}
	if err := os.MkdirAll(request.WorkDir, 0o755); err != nil {
		return Result{}, fmt.Errorf("create work directory: %w", err)
	}
	snapshotRoot, err := secureTemporaryDirectory("shuhari-codex-snapshot-")
	if err != nil {
		return Result{}, fmt.Errorf("create work directory snapshot: %w", err)
	}
	defer os.RemoveAll(snapshotRoot)
	snapshot := filepath.Join(snapshotRoot, "workdir")
	if err := copyTree(request.WorkDir, snapshot); err != nil {
		return Result{}, fmt.Errorf("snapshot work directory: %w", err)
	}
	evidence := AttemptEvidence{}
	for attempt := 1; attempt <= maxCodexAttempts; attempt++ {
		if err := restoreDirectory(snapshot, request.WorkDir); err != nil {
			return Result{}, err
		}
		result, observation, err := h.runOnce(ctx, request, firstTokenTimeout)
		if err == nil && strings.TrimSpace(result.Response) != "" {
			evidence.AttemptCount = attempt
			result.Attempts = evidence
			return result, nil
		}
		if err == nil {
			err = fmt.Errorf("%w: codex returned an empty response", ErrTransient)
		}
		evidence.AttemptCount = attempt
		durationMS := observation.Duration.Milliseconds()
		if durationMS < 1 && !observation.Timestamp.IsZero() {
			durationMS = 1
		}
		evidence.AttemptErrors = append(evidence.AttemptErrors, AttemptError{
			Attempt:     attempt,
			Error:       err.Error(),
			Timestamp:   observation.Timestamp,
			DurationMS:  durationMS,
			StdoutBytes: observation.StdoutBytes,
			StderrBytes: observation.StderrBytes,
		})
		if !errors.Is(err, ErrTransient) {
			if attempt > 1 {
				return Result{}, &RetryError{Cause: err, Attempts: evidence}
			}
			return Result{}, err
		}
		if attempt == maxCodexAttempts {
			return Result{}, &RetryError{Cause: err, Attempts: evidence}
		}
		if err := h.waitBeforeRetry(ctx, attempt); err != nil {
			return Result{}, &RetryError{Cause: err, Attempts: evidence}
		}
	}
	panic("unreachable")
}

func configuredFirstTokenTimeout() (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(firstTokenTimeoutEnv))
	if value == "" {
		return defaultFirstTokenTimeout, nil
	}
	timeout, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("parse %s=%q as a positive duration: %w", firstTokenTimeoutEnv, value, err)
	}
	if timeout <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration, got %q", firstTokenTimeoutEnv, value)
	}
	return timeout, nil
}

func validateCodexSecurityResolution(resolution SecurityResolution) error {
	policy := resolution.Policy()
	if err := ValidateSecurityResolution(policy, resolution); err != nil {
		return err
	}
	if resolution.Adapter.Name != "codex" {
		return fmt.Errorf("invalid Codex security resolution: adapter is %q", resolution.Adapter.Name)
	}
	wantMode := ""
	switch resolution.SandboxLevel {
	case SandboxIsolated:
		wantMode = codexIsolatedNativeMode
	case SandboxReadOnly:
		wantMode = codexReadOnlyNativeMode
	case SandboxUnsandboxed:
		wantMode = codexUnsandboxedNativeMode
	}
	if resolution.Adapter.NativeMode != wantMode || resolution.Adapter.PolicyDigest != codexSecurityPolicyDigest(policy, wantMode) {
		return errors.New("invalid Codex security resolution: native mode or policy digest does not match the neutral policy")
	}
	return nil
}

type attemptObservation struct {
	Timestamp   time.Time
	Duration    time.Duration
	StdoutBytes int64
	StderrBytes int64
}

func (h *codexHarness) runOnce(parent context.Context, request Request, firstTokenTimeout time.Duration) (Result, attemptObservation, error) {
	if err := ensureGitRepository(request.WorkDir); err != nil {
		return Result{}, attemptObservation{}, err
	}
	if request.Target != nil {
		if err := installCodexTarget(request.WorkDir, *request.Target); err != nil {
			return Result{}, attemptObservation{}, err
		}
	}

	temporary, err := secureTemporaryDirectory("shuhari-codex-")
	if err != nil {
		return Result{}, attemptObservation{}, fmt.Errorf("create codex temporary directory: %w", err)
	}
	defer os.RemoveAll(temporary)
	codexHome := filepath.Join(temporary, "codex-home")
	if err := initializeCodexHome(codexHome); err != nil {
		return Result{}, attemptObservation{}, err
	}
	if err := writeCodexProfile(codexHome, request); err != nil {
		return Result{}, attemptObservation{}, err
	}
	before, err := workspaceState(request.WorkDir)
	if err != nil {
		return Result{}, attemptObservation{}, err
	}
	ctx, cancel := context.WithTimeout(parent, request.Timeout)
	defer cancel()

	args := []string{"--disable", "plugins", "exec"}
	if request.Model != "" {
		args = append(args, "--model", request.Model)
	}
	// Codex 0.146.0 resolves an unset model by refreshing /models. Its pinned
	// decoder expects a Codex-only `models` field, while OpenAI-compatible
	// gateways return the standard `data` field. The bundled catalog keeps both
	// explicit and default model resolution independent of that incompatible
	// refresh without changing provider or credential settings.
	modelCatalogPath := filepath.Join(temporary, "bundled-models.json")
	if err := writeBundledModelCatalog(ctx, h.executable, codexHome, modelCatalogPath); err != nil {
		return Result{}, attemptObservation{}, err
	}
	args = append(args, "-c", "model_catalog_json="+tomlString(modelCatalogPath))
	if request.ReasoningEffort != "" {
		value, _ := json.Marshal(request.ReasoningEffort)
		args = append(args, "-c", "model_reasoning_effort="+string(value))
	}
	if override := disabledSkillsOverride(); override != "" {
		args = append(args, "-c", override)
	}
	args = append(args, "--profile", "shuhari", "--ephemeral", "--json")
	if request.Security.SandboxLevel == SandboxUnsandboxed {
		args = append(args, "--sandbox", "danger-full-access")
	}
	args = append(args, "--cd", request.WorkDir)
	if len(request.OutputSchema) > 0 {
		schemaPath := filepath.Join(temporary, "output-schema.json")
		if err := os.WriteFile(schemaPath, request.OutputSchema, 0o600); err != nil {
			return Result{}, attemptObservation{}, fmt.Errorf("write output schema: %w", err)
		}
		args = append(args, "--output-schema", schemaPath)
	}
	args = append(args, "-")

	command := exec.CommandContext(ctx, h.executable, args...)
	command.Stdin = strings.NewReader(request.Prompt)
	command.Env = codexEnvironment(codexHome)
	started := time.Now()
	stdout, stderr := newCodexTraceObserver(), bytes.Buffer{}
	command.Stdout = stdout
	command.Stderr = &stderr
	commandDone := make(chan struct{})
	watchdogExpired := make(chan struct{}, 1)
	watchdogFinished := make(chan struct{})
	go func() {
		defer close(watchdogFinished)
		timer := time.NewTimer(firstTokenTimeout)
		defer timer.Stop()
		select {
		case <-stdout.FirstModelItem():
		case <-commandDone:
		case <-ctx.Done():
		case <-timer.C:
			select {
			case <-stdout.FirstModelItem():
				return
			default:
			}
			watchdogExpired <- struct{}{}
			cancel()
		}
	}()
	err = command.Run()
	close(commandDone)
	<-watchdogFinished
	duration := time.Since(started)
	observation := attemptObservation{
		Timestamp:   started.UTC(),
		Duration:    duration,
		StdoutBytes: int64(stdout.Len()),
		StderrBytes: int64(stderr.Len()),
	}
	completed := codexTraceCompleted(stdout.Bytes())
	select {
	case <-watchdogExpired:
		if completed {
			return Result{}, observation, errors.New("codex first-token watchdog expired after a completed response")
		}
		return Result{}, observation, fmt.Errorf("%w: codex produced no model output within %s", ErrTransient, firstTokenTimeout)
	default:
	}
	if ctx.Err() == context.DeadlineExceeded {
		if completed {
			return Result{}, observation, fmt.Errorf("codex timed out after a completed response")
		}
		return Result{}, observation, fmt.Errorf("%w: codex timed out after %s", ErrTransient, request.Timeout)
	}
	if err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = strings.TrimSpace(stdout.String())
		}
		if inputTooLargePattern.MatchString(message) {
			return Result{}, observation, fmt.Errorf("codex failed: %w: %s", err, message)
		}
		if !completed && transientPattern.MatchString(message) {
			return Result{}, observation, fmt.Errorf("%w: %s", ErrTransient, message)
		}
		return Result{}, observation, fmt.Errorf("codex failed: %w: %s", err, message)
	}
	after, err := workspaceState(request.WorkDir)
	if err != nil {
		return Result{}, observation, err
	}
	result, err := parseCodexTrace(stdout.Bytes(), targetOrEmpty(request.Target))
	if err != nil {
		return Result{}, observation, err
	}
	result.Transcript = append([]byte(nil), stdout.Bytes()...)
	result.Duration = duration
	if statesDiffer(before, after) && !containsAction(result.Actions, ActionFileChange) {
		result.OrderUnknownActions = append(result.OrderUnknownActions, ActionFileChange)
	}
	return result, observation, nil
}

type codexTraceObserver struct {
	trace          bytes.Buffer
	pending        []byte
	firstModelItem chan struct{}
	seenModelItem  bool
}

func newCodexTraceObserver() *codexTraceObserver {
	return &codexTraceObserver{firstModelItem: make(chan struct{})}
}

func (observer *codexTraceObserver) Write(data []byte) (int, error) {
	written, err := observer.trace.Write(data)
	observer.pending = append(observer.pending, data...)
	for {
		newline := bytes.IndexByte(observer.pending, '\n')
		if newline < 0 {
			break
		}
		line := observer.pending[:newline]
		observer.pending = observer.pending[newline+1:]
		if !observer.seenModelItem && codexTraceLineStartsModelItem(line) {
			observer.seenModelItem = true
			close(observer.firstModelItem)
		}
	}
	return written, err
}

func (observer *codexTraceObserver) Bytes() []byte {
	return observer.trace.Bytes()
}

func (observer *codexTraceObserver) Len() int {
	return observer.trace.Len()
}

func (observer *codexTraceObserver) String() string {
	return observer.trace.String()
}

func (observer *codexTraceObserver) FirstModelItem() <-chan struct{} {
	return observer.firstModelItem
}

func codexTraceLineStartsModelItem(line []byte) bool {
	var event struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(line, &event) != nil {
		return false
	}
	return event.Type == "item.started" || event.Type == "item.updated" || event.Type == "item.completed"
}

func codexTraceCompleted(trace []byte) bool {
	scanner := bufio.NewScanner(bytes.NewReader(trace))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var event struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			continue
		}
		if event.Type == "turn.completed" {
			return true
		}
	}
	return false
}

func targetOrEmpty(target *Target) Target {
	if target == nil {
		return Target{}
	}
	return *target
}

func parseCodexTrace(trace []byte, target Target) (Result, error) {
	result := Result{TargetRead: target.Kind == TargetInstructions}
	skillContents, err := targetSkillContents(target)
	if err != nil {
		return Result{}, err
	}
	type actionObservation struct {
		Index  int
		Action Action
	}
	type skillReadObservation struct {
		Index  int
		Output string
	}
	var skillReadEvents []skillReadObservation
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
			if !turnCompleted && transientPattern.MatchString(message) {
				return Result{}, fmt.Errorf("%w: %s", ErrTransient, message)
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
				Output   string `json:"aggregated_output"`
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
				if target.Kind == TargetSkill && skillContents != "" && commandReferencesSkill(item.Command, target.Name) {
					skillReadEvents = append(skillReadEvents, skillReadObservation{Index: eventIndex, Output: item.Output})
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
		coverage := newSkillCoverage(skillContents)
		for _, observation := range skillReadEvents {
			if observation.Index < lastMessageEvent {
				coverage.observe(observation.Output)
			}
		}
		result.TargetRead = coverage.ratio() >= skillReadCoverageThreshold
	}
	for _, observation := range actionEvents {
		if lastMessageEvent < 0 || observation.Index < lastMessageEvent {
			result.Actions = append(result.Actions, observation.Action)
		}
	}
	return result, nil
}

const (
	skillReadCoverageThreshold = 0.90
	skillEvidenceChunkBytes    = 128
)

type skillCoverage struct {
	chunks  []string
	covered []bool
	total   int
	read    int
}

func newSkillCoverage(contents string) *skillCoverage {
	coverage := &skillCoverage{}
	for _, line := range strings.SplitAfter(contents, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		for len(line) > skillEvidenceChunkBytes {
			coverage.chunks = append(coverage.chunks, line[:skillEvidenceChunkBytes])
			coverage.total += skillEvidenceChunkBytes
			line = line[skillEvidenceChunkBytes:]
		}
		if line != "" {
			coverage.chunks = append(coverage.chunks, line)
			coverage.total += len(line)
		}
	}
	coverage.covered = make([]bool, len(coverage.chunks))
	return coverage
}

func (coverage *skillCoverage) observe(output string) {
	for index, chunk := range coverage.chunks {
		if !coverage.covered[index] && strings.Contains(output, chunk) {
			coverage.covered[index] = true
			coverage.read += len(chunk)
		}
	}
}

func (coverage *skillCoverage) ratio() float64 {
	if coverage.total == 0 {
		return 0
	}
	return float64(coverage.read) / float64(coverage.total)
}

func commandReferencesSkill(command, skillName string) bool {
	normalized := filepath.ToSlash(command)
	directory := filepath.ToSlash(filepath.Join(".agents", "skills", skillName))
	return strings.Contains(normalized, directory+"/SKILL.md") ||
		(strings.Contains(normalized, directory) && strings.Contains(normalized, "SKILL.md"))
}

var (
	githubCLICommand = regexp.MustCompile(`(?i)\bgh\s+(?:api|browse|repo|search)\b`)
	githubURLCommand = regexp.MustCompile(`(?i)\b(?:git\s+(?:clone|fetch)|curl|wget)\b.*(?:github\.com|githubusercontent\.com)`)
	webCommand       = regexp.MustCompile(`(?i)\b(?:curl|wget)\b.*https?://`)
)

func targetSkillContents(target Target) (string, error) {
	if target.Kind != TargetSkill {
		return "", nil
	}
	contents, err := os.ReadFile(filepath.Join(target.SourcePath, "SKILL.md"))
	if err != nil {
		return "", fmt.Errorf("read target skill evidence: %w", err)
	}
	return string(contents), nil
}

func classifyCommand(command string) []Action {
	if githubCLICommand.MatchString(command) || githubURLCommand.MatchString(command) {
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
		if err := copyTreeExcluding(target.SourcePath, destination, isEvalDefinitionDir); err != nil {
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

// isEvalDefinitionDir reports whether a skill-relative path is the directory
// holding that skill's eval and trigger definitions.
func isEvalDefinitionDir(relative string, entry fs.DirEntry) bool {
	return entry.IsDir() && relative == EvalDefinitionDir
}

func copyTree(source, destination string) error {
	return copyTreeExcluding(source, destination, nil)
}

// copyTreeExcluding copies source into destination. When exclude reports true
// for a directory, that directory and everything beneath it is left out; when it
// reports true for a file, that file is left out.
func copyTreeExcluding(source, destination string, exclude func(relative string, entry fs.DirEntry) bool) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if exclude != nil && exclude(relative, entry) {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
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
	source, err := sourceCodexHome()
	if err != nil {
		return err
	}
	for _, name := range []string{"config.toml", "auth.json"} {
		from := filepath.Join(source, name)
		_, err := os.Stat(from)
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
		if err := os.WriteFile(filepath.Join(destination, name), contents, 0o600); err != nil {
			return fmt.Errorf("copy codex %s: %w", name, err)
		}
	}
	return nil
}

func writeBundledModelCatalog(ctx context.Context, executable, codexHome, destination string) error {
	command := exec.CommandContext(ctx, executable, "--disable", "plugins", "debug", "models", "--bundled")
	command.Env = codexEnvironment(codexHome)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = strings.TrimSpace(stdout.String())
		}
		return fmt.Errorf("prepare bundled Codex model catalog: %w: %s", err, message)
	}
	var catalog struct {
		Models json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &catalog); err != nil {
		return fmt.Errorf("decode bundled Codex model catalog: %w", err)
	}
	var models []json.RawMessage
	if err := json.Unmarshal(catalog.Models, &models); err != nil {
		return fmt.Errorf("decode bundled Codex model catalog models: %w", err)
	}
	if len(models) == 0 {
		return errors.New("decode bundled Codex model catalog: response has no models list")
	}
	if err := os.WriteFile(destination, stdout.Bytes(), 0o600); err != nil {
		return fmt.Errorf("write bundled Codex model catalog: %w", err)
	}
	return nil
}

func sourceCodexHome() (string, error) {
	if source := os.Getenv("CODEX_HOME"); source != "" {
		return source, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find home directory: %w", err)
	}
	return filepath.Join(home, ".codex"), nil
}

func secureTemporaryDirectory(pattern string) (string, error) {
	directory, err := os.MkdirTemp("", pattern)
	if err != nil {
		return "", err
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		_ = os.RemoveAll(directory)
		return "", err
	}
	return directory, nil
}

func writeCodexProfile(codexHome string, request Request) error {
	commandHome := filepath.Join(request.WorkDir, ".shuhari", "home")
	commandTmp := filepath.Join(request.WorkDir, ".shuhari", "tmp")
	for _, path := range []string{commandHome, commandTmp} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return fmt.Errorf("create isolated command directory: %w", err)
		}
	}
	var builder strings.Builder
	if request.Security.SandboxLevel != SandboxUnsandboxed {
		builder.WriteString("default_permissions = \"shuhari-eval\"\n\n")
	}
	builder.WriteString("[shell_environment_policy]\ninherit = \"none\"\nignore_default_excludes = false\nexclude = [\"GH_*\", \"GITHUB_*\"]\n\n")
	builder.WriteString("[shell_environment_policy.set]\n")
	builder.WriteString("CODEX_HOME = \"\"\n")
	fmt.Fprintf(&builder, "HOME = %s\n", tomlString(commandHome))
	commandPath, commandTools := isolatedCommandPath(request.Security.HostTools)
	fmt.Fprintf(&builder, "PATH = %s\n", tomlString(commandPath))
	fmt.Fprintf(&builder, "TMPDIR = %s\n", tomlString(commandTmp))
	builder.WriteString("LANG = \"C.UTF-8\"\n")
	for _, name := range proxyEnvironmentVariables {
		if value, ok := os.LookupEnv(name); ok {
			fmt.Fprintf(&builder, "%s = %s\n", name, tomlString(value))
		}
	}
	if request.Security.SandboxLevel != SandboxUnsandboxed {
		access := "write"
		if request.Security.SandboxLevel == SandboxReadOnly {
			access = "read"
		} else if request.Security.SandboxLevel != SandboxIsolated {
			return fmt.Errorf("unsupported Shuhari sandbox level %q", request.Security.SandboxLevel)
		}
		builder.WriteString("\n[permissions.shuhari-eval]\ndescription = \"Shuhari isolated evaluation\"\n\n")
		builder.WriteString("[permissions.shuhari-eval.filesystem]\n\":minimal\" = \"read\"\n")
		fmt.Fprintf(&builder, "%s = \"deny\"\n", tomlString(codexHome))
		if source, err := sourceCodexHome(); err == nil {
			fmt.Fprintf(&builder, "%s = \"deny\"\n", tomlString(source))
		}
		for _, tool := range commandTools {
			fmt.Fprintf(&builder, "%s = \"read\"\n", tomlString(tool))
		}
		builder.WriteString("\n[permissions.shuhari-eval.filesystem.\":workspace_roots\"]\n")
		fmt.Fprintf(&builder, "\".\" = %s\n", tomlString(access))
		builder.WriteString("\n[permissions.shuhari-eval.network]\n")
		fmt.Fprintf(&builder, "enabled = %t\n", request.Security.NetworkAccess == NetworkAllowed)
	}
	if err := os.WriteFile(filepath.Join(codexHome, "shuhari.config.toml"), []byte(builder.String()), 0o600); err != nil {
		return fmt.Errorf("write isolated Codex profile: %w", err)
	}
	return nil
}

func tomlString(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

// resolveHostTools locates each declared tool on the host.
//
// A tool that cannot be found is refused rather than skipped. Running without
// it would produce an agent that reports the tool as unavailable, which grades
// as a skill failure and hides the real cause.
func resolveHostTools(names []string) ([]string, error) {
	paths := make([]string, 0, len(names))
	for _, name := range names {
		if strings.TrimSpace(name) == "" {
			return nil, errors.New("declared host tool name is empty")
		}
		path, err := exec.LookPath(name)
		if err != nil {
			return nil, fmt.Errorf("declared host tool %q was not found on this host", name)
		}
		paths = append(paths, path)
	}
	return paths, nil
}

// isolatedCommandPath builds the PATH the evaluated agent sees.
//
// The base is a fixed set of system directories, so a run does not inherit
// whatever happens to be installed on the machine. `gh` has long been an
// implicit exception; declared tools are the explicit, recorded form of the
// same idea.
//
// The returned tool paths are granted read permission in the sandbox profile,
// which is what makes them executable there.
func isolatedCommandPath(hostTools []string) (string, []string) {
	if runtime.GOOS == "windows" {
		return `C:\Windows\System32;C:\Windows`, nil
	}
	directories := []string{"/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin"}
	var tools []string
	add := func(path string) {
		directory := filepath.Dir(path)
		for _, existing := range directories {
			if existing == directory {
				return
			}
		}
		directories = append(directories, directory)
		tools = append(tools, path)
	}
	if path, err := exec.LookPath("gh"); err == nil {
		add(path)
	}
	// A declared tool that cannot be resolved was already refused by
	// ResolveSecurity, so anything missing here is ignored rather than
	// silently changing the boundary.
	resolved, err := resolveHostTools(hostTools)
	if err == nil {
		for _, path := range resolved {
			add(path)
		}
	}
	return strings.Join(directories, string(os.PathListSeparator)), tools
}

type workspaceFileState struct {
	Mode   fs.FileMode
	Digest string
}

func workspaceState(root string) (map[string]workspaceFileState, error) {
	state := map[string]workspaceFileState{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if entry.IsDir() && (relative == ".git" || relative == ".shuhari") {
			return filepath.SkipDir
		}
		if entry.IsDir() || relative == "." {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		var contents []byte
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			contents = []byte(target)
		} else if info.Mode().IsRegular() {
			contents, err = os.ReadFile(path)
			if err != nil {
				return err
			}
		}
		digest := sha256.Sum256(contents)
		state[filepath.ToSlash(relative)] = workspaceFileState{Mode: info.Mode(), Digest: hex.EncodeToString(digest[:])}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("snapshot evaluated workspace state: %w", err)
	}
	return state, nil
}

func statesDiffer(before, after map[string]workspaceFileState) bool {
	if len(before) != len(after) {
		return true
	}
	for path, state := range before {
		if after[path] != state {
			return true
		}
	}
	return false
}

func containsAction(actions []Action, expected Action) bool {
	for _, action := range actions {
		if action == expected {
			return true
		}
	}
	return false
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
	if strings.HasPrefix(name, "GH_") || strings.HasPrefix(name, "GITHUB_") {
		return false
	}
	if strings.HasPrefix(name, "LC_") {
		return true
	}
	for _, proxyName := range proxyEnvironmentVariables {
		if name == proxyName {
			return true
		}
	}
	switch name {
	case "PATH", "HOME", "USER", "LOGNAME", "SHELL", "TMPDIR", "TEMP", "TMP", "LANG", "LANGUAGE", "TERM", "COLORTERM", "TZ",
		"SSL_CERT_FILE", "SSL_CERT_DIR", "CURL_CA_BUNDLE", "REQUESTS_CA_BUNDLE":
		return true
	default:
		return false
	}
}
