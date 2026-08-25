package eval

import (
	"time"

	"github.com/shunk031/shuhari/internal/harness"
	"github.com/shunk031/shuhari/internal/progress"
)

type Suite struct {
	Kind       harness.TargetKind
	Name       string
	Root       string
	TargetPath string
	EvalPath   string
	Cases      []Case
}

type Case struct {
	ID             string   `json:"id"`
	Prompt         string   `json:"prompt"`
	ExpectedOutput string   `json:"expected_output"`
	Files          []string `json:"files,omitempty"`
	Assertions     []string `json:"assertions,omitempty"`
}

func (c Case) effectiveAssertions() []string {
	if len(c.Assertions) > 0 {
		return c.Assertions
	}
	return []string{c.ExpectedOutput}
}

type Config struct {
	Trials               int           `json:"trials"`
	Jobs                 int           `json:"jobs"`
	Timeout              time.Duration `json:"timeout"`
	Model                string        `json:"model,omitempty"`
	ReasoningEffort      string        `json:"reasoning_effort,omitempty"`
	JudgeModel           string        `json:"judge_model,omitempty"`
	JudgeReasoningEffort string        `json:"judge_reasoning_effort,omitempty"`
	SandboxLevel         string        `json:"sandbox_level"`
	Network              bool          `json:"network"`
	HostTools            []string      `json:"host_tools,omitempty"`
	// Progress receives phase events as the evaluation runs. A nil reporter
	// discards them, so callers that do not want progress change nothing.
	Progress        *progress.Reporter `json:"-"`
	Workspace       string             `json:"-"`
	StrictAllTrials bool               `json:"strict_all_trials"`
}

type Report struct {
	Passed       bool
	Workspace    string
	FailureCount int
	Reasons      []string
}

type runResult struct {
	Case       Case
	Trial      int
	Variant    string
	RunDir     string
	Agent      harness.Result
	OutputPath string
	Artifact   string
}

type AssertionResult struct {
	Text     string `json:"text"`
	Passed   bool   `json:"passed"`
	Evidence string `json:"evidence"`
}

type GradingSummary struct {
	Passed   int     `json:"passed"`
	Failed   int     `json:"failed"`
	Total    int     `json:"total"`
	PassRate float64 `json:"pass_rate"`
}

type Grading struct {
	AssertionResults []AssertionResult `json:"expectations"`
	Summary          GradingSummary    `json:"summary"`
}

type Comparison struct {
	SchemaVersion    string `json:"schema_version"`
	ID               string `json:"id"`
	Trial            int    `json:"trial"`
	A                string `json:"A"`
	B                string `json:"B"`
	Preferred        string `json:"preferred"`
	PreferredVariant string `json:"preferred_variant"`
	Reason           string `json:"reason"`
}

type gradedRun struct {
	CaseID          string
	Trial           int
	Variant         string
	PassRate        float64
	Passed          bool
	TimeSeconds     float64
	Tokens          float64
	AssertionResult []AssertionResult
}

const (
	workspaceManifestSchemaVersion = "2"
	benchmarkSchemaVersion         = "2"
	comparisonSchemaVersion        = "1"
	evidenceSchemaVersion          = "1"
)

const (
	variantWithSkill           = "with_skill"
	variantWithoutSkill        = "without_skill"
	variantWithInstructions    = "with_instructions"
	variantWithoutInstructions = "without_instructions"
)
