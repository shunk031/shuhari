package harness

import (
	"context"
	"errors"
	"os/exec"
	"time"
)

const codexCommandWaitDelay = 500 * time.Millisecond

func newCodexCommand(ctx context.Context, executable string, arguments ...string) *exec.Cmd {
	command := exec.CommandContext(ctx, executable, arguments...)
	command.WaitDelay = codexCommandWaitDelay
	configureCodexProcessTree(command)
	return command
}

func runCodexCommand(command *exec.Cmd) error {
	err := command.Run()
	if errors.Is(err, exec.ErrWaitDelay) {
		terminateCodexProcessTree(command)
	}
	return err
}

func runCodexCombinedOutput(command *exec.Cmd) ([]byte, error) {
	output, err := command.CombinedOutput()
	if errors.Is(err, exec.ErrWaitDelay) {
		terminateCodexProcessTree(command)
	}
	return output, err
}
