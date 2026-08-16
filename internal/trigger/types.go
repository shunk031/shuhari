package trigger

import "time"

type Case struct {
	ID            string `json:"id"`
	Prompt        string `json:"prompt"`
	ShouldTrigger bool   `json:"should_trigger"`
}

type Suite struct {
	SkillName string `json:"skill_name"`
	Cases     []Case `json:"cases"`
	SkillPath string `json:"-"`
	CasesPath string `json:"-"`
}

type Config struct {
	Trials          int           `json:"trials"`
	Jobs            int           `json:"jobs"`
	Timeout         time.Duration `json:"timeout"`
	Model           string        `json:"model,omitempty"`
	ReasoningEffort string        `json:"reasoning_effort,omitempty"`
	Sandbox         string        `json:"sandbox"`
	Network         bool          `json:"network"`
	Workspace       string        `json:"-"`
	StrictAllTrials bool          `json:"strict_all_trials"`
	NoCache         bool          `json:"-"`
}

type Report struct {
	Passed    bool
	Cached    bool
	Workspace string
	Reasons   []string
}

type Measurement struct {
	Results map[string][]bool `json:"target_read"`
}

type Policy struct {
	Trials          int
	StrictAllTrials bool
}
