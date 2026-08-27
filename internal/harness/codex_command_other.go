//go:build !(darwin || dragonfly || freebsd || linux || netbsd || openbsd)

package harness

import "os/exec"

func configureCodexProcessTree(*exec.Cmd) {}

func terminateCodexProcessTree(command *exec.Cmd) {
	if command.Process != nil {
		_ = command.Process.Kill()
	}
}
