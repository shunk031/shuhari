package harness

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"
)

type TargetKind string

const (
	TargetSkill        TargetKind = "skill"
	TargetInstructions TargetKind = "instructions"
)

type Action string

const (
	ActionWebSearch    Action = "web_search"
	ActionGitHubSearch Action = "github_search"
	ActionFileChange   Action = "file_change"
)

const (
	DangerFullAccessAcknowledgementEnv = "SHUHARI_I_UNDERSTAND_NO_CREDENTIAL_BOUNDARY"
	CredentialBoundaryCodexSandbox     = "codex-sandbox"
	CredentialBoundaryNone             = "none"
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
	Sandbox         string
	Network         bool
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
	TargetRead          bool
	Actions             []Action
	OrderUnknownActions []Action
}

type ExecutionSecurity struct {
	SandboxMode        string `json:"sandbox_mode"`
	CredentialBoundary string `json:"credential_boundary"`
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
	Probe(context.Context) (Identity, error)
	Capabilities() Capabilities
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

func EffectiveSandbox(requested string) string {
	if override := os.Getenv("SHUHARI_SANDBOX"); override != "" {
		return override
	}
	if requested == "" {
		return "workspace-write"
	}
	return requested
}

func ValidateSandbox(sandbox string) error {
	switch sandbox {
	case "read-only", "workspace-write":
		return nil
	case "danger-full-access":
		if os.Getenv(DangerFullAccessAcknowledgementEnv) != "1" {
			return fmt.Errorf("danger-full-access has no credential boundary; set %s=1 only inside an isolated runner or container", DangerFullAccessAcknowledgementEnv)
		}
		return nil
	default:
		return fmt.Errorf("unsupported Codex sandbox %q", sandbox)
	}
}

func ExecutionSecurityForSandbox(sandbox string) ExecutionSecurity {
	boundary := CredentialBoundaryCodexSandbox
	if sandbox == "danger-full-access" {
		boundary = CredentialBoundaryNone
	}
	return ExecutionSecurity{SandboxMode: sandbox, CredentialBoundary: boundary}
}

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
