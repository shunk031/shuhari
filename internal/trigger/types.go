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
	Trials          int
	Jobs            int
	Timeout         time.Duration
	Model           string
	ReasoningEffort string
	Sandbox         string
	Network         bool
	Workspace       string
	StrictAllTrials bool
	NoCache         bool
}

type Report struct {
	Passed    bool
	Cached    bool
	Workspace string
	Reasons   []string
}
