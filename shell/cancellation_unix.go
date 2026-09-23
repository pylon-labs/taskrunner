//go:build !windows && !plan9

package shell

import (
	"os"
	"os/exec"
	"syscall"
)

func terminateProcess(process *os.Process) error {
	return process.Signal(syscall.SIGTERM)
}

func commandExitStatus(err *exec.ExitError) uint8 {
	if status, ok := err.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return uint8(128 + status.Signal())
	}
	return uint8(err.ExitCode())
}
