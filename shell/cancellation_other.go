//go:build windows || plan9

package shell

import (
	"os"
	"os/exec"
)

func terminateProcess(process *os.Process) error {
	return process.Kill()
}

func commandExitStatus(err *exec.ExitError) uint8 {
	return uint8(err.ExitCode())
}
