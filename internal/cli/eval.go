package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/shunk031/shuhari/internal/eval"
	"github.com/shunk031/shuhari/internal/harness"
	"github.com/spf13/cobra"
)

type evalFlags struct {
	agent                string
	executable           string
	model                string
	reasoningEffort      string
	judgeModel           string
	judgeReasoningEffort string
	sandbox              string
	network              bool
	hostTools            []string
	progressOn           bool
	workspace            string
	trials               int
	jobs                 int
	timeoutSeconds       int
	strict               bool
	validateOnly         bool
}

func newEvalCommand(options Options) *cobra.Command {
	command := &cobra.Command{Use: "eval", Short: "Evaluate guidance output quality"}
	command.AddCommand(newEvalSkillCommand(options), newEvalInstructionsCommand(options))
	return command
}

func newEvalSkillCommand(options Options) *cobra.Command {
	flags := evalFlags{}
	command := &cobra.Command{
		Use:   "skill <skill-path-or-file>...",
		Short: "Evaluate an Agent Skill with and without the skill",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			if err := resolveSecurityFlags(command, &flags.sandbox, flags.network); err != nil {
				return err
			}
			paths, err := resolveSkillPaths(args)
			if err != nil {
				return err
			}
			if flags.workspace != "" && len(paths) > 1 {
				return fmt.Errorf("--workspace cannot be shared by multiple skills")
			}
			if flags.validateOnly {
				for _, path := range paths {
					suite, err := eval.LoadSkillSuite(path)
					if err != nil {
						return err
					}
					fmt.Fprintf(command.OutOrStdout(), "%s: valid\n", suite.Name)
				}
				return nil
			}
			var agentCreated bool
			var agentInstance harness.Harness
			var gateFailure error
			for _, path := range paths {
				suite, err := eval.LoadSkillSuite(path)
				if err != nil {
					return err
				}
				if !agentCreated {
					agentInstance, err = newHarness(options, flags.agent, flags.executable)
					if err != nil {
						return err
					}
					agentCreated = true
				}
				report, err := eval.Run(command.Context(), suite, agentInstance, flags.config())
				if err != nil {
					return err
				}
				if err := printReport(command, suite.Name, report.Passed, report.Workspace, report.Reasons); err != nil {
					gateFailure = err
				}
			}
			return gateFailure
		},
	}
	addEvalFlags(command, &flags)
	return command
}

func newEvalInstructionsCommand(options Options) *cobra.Command {
	flags := evalFlags{}
	var evalPath string
	command := &cobra.Command{
		Use:   "instructions <instructions-file>",
		Short: "Evaluate repository instructions with and without the instructions",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			if err := resolveSecurityFlags(command, &flags.sandbox, flags.network); err != nil {
				return err
			}
			suite, err := eval.LoadInstructionsSuite(args[0], evalPath)
			if err != nil {
				return err
			}
			if flags.validateOnly {
				fmt.Fprintf(command.OutOrStdout(), "%s: valid\n", suite.Name)
				return nil
			}
			agent, err := newHarness(options, flags.agent, flags.executable)
			if err != nil {
				return err
			}
			report, err := eval.Run(command.Context(), suite, agent, flags.config())
			if err != nil {
				return err
			}
			return printReport(command, suite.Name, report.Passed, report.Workspace, report.Reasons)
		},
	}
	command.Flags().StringVar(&evalPath, "evals", "", "path to the instructions eval JSON")
	addEvalFlags(command, &flags)
	return command
}

func addEvalFlags(command *cobra.Command, flags *evalFlags) {
	command.Flags().StringVar(&flags.agent, "agent", "codex", "agent harness to use")
	command.Flags().StringVar(&flags.executable, "agent-executable", "", "override the agent executable")
	command.Flags().StringVar(&flags.model, "model", "", "model used for evaluated runs")
	command.Flags().StringVar(&flags.reasoningEffort, "reasoning-effort", "", "reasoning effort used for evaluated runs")
	command.Flags().StringVar(&flags.judgeModel, "judge-model", "", "model used for grading (defaults to --model)")
	command.Flags().StringVar(&flags.judgeReasoningEffort, "judge-reasoning-effort", "", "reasoning effort used for grading")
	command.Flags().StringVar(&flags.sandbox, "sandbox", "isolated", "Shuhari sandbox level: isolated, read-only, or unsandboxed")
	command.Flags().BoolVar(&flags.network, "network", false, "allow network access during evaluated runs")
	command.Flags().StringArrayVar(&flags.hostTools, "allow-tool", nil, "expose a host executable to the evaluated agent, repeatable (runs otherwise see a fixed system PATH)")
	command.Flags().BoolVar(&flags.progressOn, "progress", false, "stream phase events as JSON Lines on stderr while the evaluation runs")
	command.Flags().StringVar(&flags.workspace, "workspace", "", "workspace root (defaults beside the target)")
	command.Flags().IntVar(&flags.trials, "trials", 1, "trials per case and configuration")
	command.Flags().IntVar(&flags.jobs, "jobs", 2, "maximum concurrent evaluated runs")
	command.Flags().IntVar(&flags.timeoutSeconds, "timeout", 180, "timeout per agent invocation in seconds")
	command.Flags().BoolVar(&flags.strict, "strict-all-trials", false, "require every trial in every case to pass")
	command.Flags().BoolVar(&flags.validateOnly, "validate-only", false, "validate inputs without running an agent")
}

func (flags evalFlags) config() eval.Config {
	return eval.Config{Trials: flags.trials, Jobs: flags.jobs, Timeout: time.Duration(flags.timeoutSeconds) * time.Second, Model: flags.model, ReasoningEffort: flags.reasoningEffort, JudgeModel: flags.judgeModel, JudgeReasoningEffort: flags.judgeReasoningEffort, SandboxLevel: flags.sandbox, Network: flags.network, HostTools: flags.hostTools, Progress: progressReporter(flags.progressOn), Workspace: flags.workspace, StrictAllTrials: flags.strict}
}

func resolveSkillPaths(arguments []string) ([]string, error) {
	seen := map[string]bool{}
	var result []string
	for _, argument := range arguments {
		path, err := filepath.Abs(argument)
		if err != nil {
			return nil, fmt.Errorf("resolve %q: %w", argument, err)
		}
		info, err := os.Stat(path)
		if err == nil && !info.IsDir() {
			path = filepath.Dir(path)
		} else if err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("inspect %q: %w", argument, err)
		} else if os.IsNotExist(err) {
			path = filepath.Dir(path)
		}
		for {
			if _, err := os.Stat(filepath.Join(path, "SKILL.md")); err == nil {
				break
			}
			parent := filepath.Dir(path)
			if parent == path {
				return nil, fmt.Errorf("no SKILL.md found above %q", argument)
			}
			path = parent
		}
		if !seen[path] {
			seen[path] = true
			result = append(result, path)
		}
	}
	sort.Strings(result)
	return result, nil
}
