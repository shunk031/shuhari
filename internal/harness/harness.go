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
	Response   string
	Transcript []byte
	Usage      Usage
	Duration   time.Duration
	TargetRead bool
	Actions    []Action
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

func ContainsOrderedActions(observed, required []Action) bool {
	position := 0
	for _, action := range observed {
		if position < len(required) && action == required[position] {
			position++
		}
	}
	return position == len(required)
}
