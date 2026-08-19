package cli

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"

	"github.com/shunk031/shuhari/internal/harness"
)

type failingHarness struct{}

func (failingHarness) Probe(context.Context, ...harness.SecurityResolution) (harness.Identity, error) {
	return harness.Identity{}, errors.New("probe should not run")
}

func (failingHarness) Capabilities() harness.Capabilities {
	return harness.Capabilities{Skills: true, Instructions: true, TriggerEvidence: true}
}

func (failingHarness) ResolveSecurity(context.Context, harness.SecurityPolicy) (harness.SecurityResolution, error) {
	return harness.SecurityResolution{}, errors.New("security resolution failed")
}

func (failingHarness) Run(context.Context, harness.Request) (harness.Result, error) {
	return harness.Result{}, errors.New("run should not start")
}

func TestCommandRunPathsReachEvaluatorAfterCacheRemoval(t *testing.T) {
	t.Parallel()
	root := writeValidationFixtures(t)
	factory := func(string, harness.Config) (harness.Harness, error) { return failingHarness{}, nil }
	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "skill", args: []string{"eval", "skill", root}},
		{name: "instructions", args: []string{"eval", "instructions", filepath.Join(root, "AGENTS.md")}},
		{name: "trigger", args: []string{"check", "trigger", root}},
	} {
		t.Run(test.name, func(t *testing.T) {
			command := NewRoot(Options{Version: "test", HarnessFactory: factory})
			command.SetArgs(test.args)
			command.SetOut(io.Discard)
			command.SetErr(io.Discard)
			if err := command.Execute(); err == nil || err.Error() != "security resolution failed" {
				t.Fatalf("Execute(%v) error = %v, want security resolution failure", test.args, err)
			}
		})
	}
}

func TestEvalFlagsConfigAndPrintReport(t *testing.T) {
	config := (evalFlags{
		model: "model", reasoningEffort: "high", judgeModel: "judge", judgeReasoningEffort: "low",
		sandbox: "read-only", network: true, workspace: "/tmp/workspace", trials: 3, jobs: 2,
		timeoutSeconds: 7, strict: true,
	}).config()
	if config.Model != "model" || config.JudgeModel != "judge" || config.Trials != 3 || config.Timeout.Seconds() != 7 || !config.StrictAllTrials {
		t.Fatalf("config = %#v", config)
	}
	command := NewRoot(Options{Version: "test"})
	if err := printReport(command, "demo", true, "", nil); err != nil {
		t.Fatalf("printReport() error = %v", err)
	}
}
