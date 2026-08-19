package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/shunk031/shuhari/internal/harness"
	"github.com/spf13/cobra"
)

type HarnessFactory func(string, harness.Config) (harness.Harness, error)

type Options struct {
	Version        string
	HarnessFactory HarnessFactory
}

type gateError struct {
	message string
}

func (e gateError) Error() string { return e.message }

func NewRoot(options Options) *cobra.Command {
	if options.HarnessFactory == nil {
		options.HarnessFactory = harness.New
	}
	root := &cobra.Command{
		Use:           "shuhari",
		Short:         "Evaluate agent skills and instructions",
		Version:       options.Version,
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	root.AddCommand(newEvalCommand(options), newCheckCommand(options))
	return root
}

func Execute(ctx context.Context, options Options, stdout, stderr io.Writer) error {
	root := NewRoot(options)
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetContext(ctx)
	return root.Execute()
}

func newHarness(options Options, agentName, executable string) (harness.Harness, error) {
	return options.HarnessFactory(agentName, harness.Config{Executable: executable})
}

func resolveSecurityFlags(command *cobra.Command, sandbox *string, network bool) error {
	requested := *sandbox
	if !command.Flags().Changed("sandbox") {
		if override := os.Getenv("SHUHARI_SANDBOX"); override != "" {
			requested = override
		}
	}
	level, err := harness.EffectiveSandboxLevel(requested)
	if err != nil {
		return err
	}
	policy := harness.SecurityPolicy{Level: level, Network: network}
	if err := harness.ValidateSecurityPolicy(policy); err != nil {
		return err
	}
	*sandbox = string(level)
	return nil
}

func printReport(command *cobra.Command, name string, passed bool, workspace string, reasons []string) error {
	status := "passed"
	if !passed {
		status = "failed"
	}
	fmt.Fprintf(command.OutOrStdout(), "%s: %s", name, status)
	if workspace != "" {
		fmt.Fprintf(command.OutOrStdout(), " workspace=%s", workspace)
	}
	fprintln(command.OutOrStdout())
	if passed {
		return nil
	}
	for _, reason := range reasons {
		fmt.Fprintf(command.ErrOrStderr(), "  - %s\n", reason)
	}
	return gateError{message: name + " evaluation failed"}
}

func fprintln(writer io.Writer) {
	_, _ = fmt.Fprintln(writer)
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var gate gateError
	if errors.As(err, &gate) {
		return 1
	}
	return 2
}

func Main(version string) {
	err := Execute(context.Background(), Options{Version: version}, os.Stdout, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	os.Exit(exitCode(err))
}
