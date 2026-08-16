package cli

import (
	"fmt"
	"time"

	"github.com/shunk031/shuhari/internal/trigger"
	"github.com/spf13/cobra"
)

type triggerFlags struct {
	agent           string
	executable      string
	model           string
	reasoningEffort string
	sandbox         string
	network         bool
	workspace       string
	casesPath       string
	trials          int
	jobs            int
	timeoutSeconds  int
	strict          bool
	noCache         bool
	validateOnly    bool
}

func newCheckCommand(options Options) *cobra.Command {
	command := &cobra.Command{Use: "check", Short: "Run gate-oriented guidance checks"}
	command.AddCommand(newCheckTriggerCommand(options))
	return command
}

func newCheckTriggerCommand(options Options) *cobra.Command {
	flags := triggerFlags{}
	command := &cobra.Command{
		Use:   "trigger <skill-path>",
		Short: "Check positive and negative trigger cases",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			suite, err := trigger.LoadSuite(args[0], flags.casesPath)
			if err != nil {
				return err
			}
			if flags.validateOnly {
				fmt.Fprintf(command.OutOrStdout(), "%s: valid\n", suite.SkillName)
				return nil
			}
			store, err := cacheStore(options)
			if err != nil {
				return err
			}
			agent, err := newHarness(options, flags.agent, flags.executable)
			if err != nil {
				return err
			}
			report, err := trigger.Run(command.Context(), suite, agent, store, trigger.Config{Trials: flags.trials, Jobs: flags.jobs, Timeout: time.Duration(flags.timeoutSeconds) * time.Second, Model: flags.model, ReasoningEffort: flags.reasoningEffort, Sandbox: flags.sandbox, Network: flags.network, Workspace: flags.workspace, StrictAllTrials: flags.strict, NoCache: flags.noCache})
			if err != nil {
				return err
			}
			return printReport(command, suite.SkillName, report.Passed, report.Cached, report.Workspace, report.Reasons)
		},
	}
	command.Flags().StringVar(&flags.agent, "agent", "codex", "agent harness to use")
	command.Flags().StringVar(&flags.executable, "agent-executable", "", "override the agent executable")
	command.Flags().StringVar(&flags.model, "model", "", "model used for trigger runs")
	command.Flags().StringVar(&flags.reasoningEffort, "reasoning-effort", "", "reasoning effort used for trigger runs")
	command.Flags().StringVar(&flags.sandbox, "sandbox", "workspace-write", "agent sandbox mode")
	command.Flags().BoolVar(&flags.network, "network", false, "allow network access during trigger runs")
	command.Flags().StringVar(&flags.workspace, "workspace", "", "workspace root (defaults beside the skill)")
	command.Flags().StringVar(&flags.casesPath, "cases", "", "trigger cases JSON (defaults to evals/triggers.json)")
	command.Flags().IntVar(&flags.trials, "trials", 1, "trials per trigger case")
	command.Flags().IntVar(&flags.jobs, "jobs", 2, "maximum concurrent trigger runs")
	command.Flags().IntVar(&flags.timeoutSeconds, "timeout", 180, "timeout per agent invocation in seconds")
	command.Flags().BoolVar(&flags.strict, "strict-all-trials", false, "require every trial to match should_trigger")
	command.Flags().BoolVar(&flags.noCache, "no-cache", false, "ignore and do not write the success cache")
	command.Flags().BoolVar(&flags.validateOnly, "validate-only", false, "validate inputs without running an agent")
	return command
}
