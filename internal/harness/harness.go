package harness

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type TargetKind string

const (
	TargetSkill        TargetKind = "skill"
	TargetInstructions TargetKind = "instructions"
)

// EvalDefinitionDir is the skill-relative directory that holds a skill's eval
// and trigger definitions.
//
// Those definitions state the expected output, the assertions, and whether the
// skill should trigger for the exact case the agent is answering, so the
// directory is never installed into an evaluated workspace. Fixtures are staged
// separately from the suite root, so withholding it costs a run nothing.
const EvalDefinitionDir = "evals"

type Action string

const (
	ActionWebSearch    Action = "web_search"
	ActionGitHubSearch Action = "github_search"
	ActionFileChange   Action = "file_change"
)

type Target struct {
	Kind       TargetKind
	Name       string
	SourcePath string
}

type Request struct {
	WorkDir         string
	Prompt          string
	Target          *Target
	Model           string
	ReasoningEffort string
	Security        SecurityResolution
	Timeout         time.Duration
	OutputSchema    []byte
}

type Usage struct {
	InputTokens           int64 `json:"input_tokens"`
	CachedInputTokens     int64 `json:"cached_input_tokens"`
	CacheWriteInputTokens int64 `json:"cache_write_input_tokens,omitempty"`
	OutputTokens          int64 `json:"output_tokens"`
	ReasoningOutputTokens int64 `json:"reasoning_output_tokens"`
}

func (u Usage) TotalTokens() int64 {
	return u.InputTokens + u.OutputTokens + u.ReasoningOutputTokens
}

type Result struct {
	Response            string
	Transcript          []byte
	Usage               Usage
	Duration            time.Duration
	Attempts            AttemptEvidence
	TargetRead          bool
	Actions             []Action
	OrderUnknownActions []Action
}

type AttemptError struct {
	Attempt     int       `json:"attempt"`
	Error       string    `json:"error"`
	Timestamp   time.Time `json:"timestamp"`
	DurationMS  int64     `json:"duration_ms"`
	StdoutBytes int64     `json:"stdout_bytes"`
	StderrBytes int64     `json:"stderr_bytes"`
}

type AttemptEvidence struct {
	AttemptCount  int            `json:"attempt_count"`
	AttemptErrors []AttemptError `json:"attempt_errors,omitempty"`
}

type RetryError struct {
	Cause    error
	Attempts AttemptEvidence
}

func (e *RetryError) Error() string { return e.Cause.Error() }

func (e *RetryError) Unwrap() error { return e.Cause }

func AttemptsFromError(err error) AttemptEvidence {
	var retryError *RetryError
	if errors.As(err, &retryError) {
		return retryError.Attempts
	}
	return AttemptEvidence{}
}

type Identity struct {
	Agent             string `json:"agent"`
	Version           string `json:"version"`
	ConfigDigest      string `json:"config_digest,omitempty"`
	ExecutableDigest  string `json:"executable_digest,omitempty"`
	EnvironmentDigest string `json:"environment_digest,omitempty"`
}

type Capabilities struct {
	Skills          bool
	Instructions    bool
	TriggerEvidence bool
}

type Harness interface {
	Probe(context.Context, ...SecurityResolution) (Identity, error)
	Capabilities() Capabilities
	ResolveSecurity(context.Context, SecurityPolicy) (SecurityResolution, error)
	Run(context.Context, Request) (Result, error)
}

type Config struct {
	Executable string
}

func New(name string, config Config) (Harness, error) {
	switch name {
	case "codex":
		return newCodex(config), nil
	default:
		return nil, fmt.Errorf("unsupported agent %q", name)
	}
}

var ErrTransient = errors.New("transient agent failure")

func ContainsOrderedActions(observed, orderUnknown, required []Action) bool {
	unknown := map[Action]int{}
	for _, action := range orderUnknown {
		unknown[action]++
	}
	type state struct {
		Required int
		Observed int
		Web      int
		GitHub   int
		File     int
	}
	seen := map[state]bool{}
	var match func(int, int, map[Action]int) bool
	match = func(requiredIndex, observedIndex int, remaining map[Action]int) bool {
		if requiredIndex == len(required) {
			return true
		}
		current := state{Required: requiredIndex, Observed: observedIndex, Web: remaining[ActionWebSearch], GitHub: remaining[ActionGitHubSearch], File: remaining[ActionFileChange]}
		if seen[current] {
			return false
		}
		seen[current] = true
		wanted := required[requiredIndex]
		if remaining[wanted] > 0 {
			remaining[wanted]--
			if match(requiredIndex+1, observedIndex, remaining) {
				return true
			}
			remaining[wanted]++
		}
		for index := observedIndex; index < len(observed); index++ {
			if observed[index] == wanted && match(requiredIndex+1, index+1, remaining) {
				return true
			}
		}
		return false
	}
	return match(0, 0, unknown)
}
