package shell

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"time"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
)

// GracefulCancellation opts external commands into SIGTERM cancellation on Unix
// (immediate termination on Windows and Plan 9). After grace, the immediate
// process is killed and inherited I/O pipes are closed rather than waited on
// indefinitely. The same bound applies to pipes held open after normal exit.
// grace must be positive. Descendants that ignore termination may survive;
// this option does not provide process-tree containment. Custom blocking I/O
// implementations must still cooperate with cancellation.
func GracefulCancellation(grace time.Duration) RunOption {
	return func(c *runConfig) {
		c.gracefulCancellation = true
		c.cancellationGrace = grace
	}
}

func cancellationHandler(grace time.Duration) interp.ExecHandlerFunc {
	return func(ctx context.Context, args []string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		hc := interp.HandlerCtx(ctx)
		path, err := interp.LookPathDir(hc.Dir, hc.Env, args[0])
		if err != nil {
			fmt.Fprintln(hc.Stderr, err)
			return interp.NewExitStatus(127)
		}
		cmd := exec.CommandContext(ctx, path, args[1:]...)
		cmd.Args = args
		cmd.Dir = hc.Dir
		cmd.Env = commandEnvironment(hc.Env)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = hc.Stdin, hc.Stdout, hc.Stderr
		cmd.Cancel = func() error { return terminateProcess(cmd.Process) }
		cmd.WaitDelay = grace
		err = cmd.Run()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return interp.NewExitStatus(commandExitStatus(exitErr))
		}
		var execErr *exec.Error
		if errors.As(err, &execErr) {
			fmt.Fprintln(hc.Stderr, err)
			return interp.NewExitStatus(127)
		}
		return err
	}
}

func commandEnvironment(env expand.Environ) []string {
	// Interpreter overlays may enumerate a name more than once, including an
	// unset or unexported value after its inherited, exported value.
	values := make(map[string]string)
	env.Each(func(name string, vr expand.Variable) bool {
		delete(values, name)
		if vr.IsSet() && vr.Exported && vr.Kind == expand.String {
			values[name] = vr.String()
		}
		return true
	})
	list := make([]string, 0, len(values))
	for name, value := range values {
		list = append(list, name+"="+value)
	}
	sort.Strings(list)
	return list
}
