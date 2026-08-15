package eval

import (
	"time"

	"github.com/shunk031/shuhari/internal/harness"
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
	ID              string           `json:"id"`
	Prompt          string           `json:"prompt"`
	ExpectedOutput  string           `json:"expected_output"`
	Files           []string         `json:"files,omitempty"`
	Assertions      []string         `json:"assertions,omitempty"`
	RequiredActions []harness.Action `json:"required_actions,omitempty"`
}

func (c Case) effectiveAssertions() []string {
	if len(c.Assertions) > 0 {
		return c.Assertions
	}
	return []string{c.ExpectedOutput}
}

type Config struct {
	Trials               int
	Jobs                 int
	Timeout              time.Duration
	Model                string
	ReasoningEffort      string
	JudgeModel           string
	JudgeReasoningEffort string
	Sandbox              string
	Network              bool
	Workspace            string
	StrictAllTrials      bool
	NoCache              bool
}

type Report struct {
	Passed       bool
	Cached       bool
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
	AssertionResults []AssertionResult `json:"assertion_results"`
	Summary          GradingSummary    `json:"summary"`
	Preferred        string            `json:"preferred,omitempty"`
	Reason           string            `json:"reason,omitempty"`
}

type gradedRun struct {
	CaseID      string
	Trial       int
	Variant     string
	PassRate    float64
	Passed      bool
	TimeSeconds float64
	Tokens      float64
}

const (
	variantWithSkill           = "with_skill"
	variantWithoutSkill        = "without_skill"
	variantWithInstructions    = "with_instructions"
	variantWithoutInstructions = "without_instructions"
)
