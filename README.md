# Taskrunner
A configurable taskrunner written in Go. Taskrunner can streamline your build system by creating reusable task definitions with names, descriptions, dependencies, and a run function written in Go. Taskrunner comes with a shell interpreter, so it can run arbitrary functions on the command line.

## Setup
Add taskrunner as a dependency `go get -v github.com/pylon-labs/taskrunner`, and create the following `main.go` file:
```go
package main

import (
	"github.com/pylon-labs/taskrunner"
	"github.com/pylon-labs/taskrunner/clireporter"
)

func main() {
	taskrunner.Run(clireporter.StdoutOption)
}
```

## Run a task
Taskrunner can be started by running `go run .`, it is possible to limit log output with `PRINT_LOG_LEVEL=error`. To run an individual task specify the task name `go run . my/task`. For a full list of tasks use `go run . -describe`, or for a list of names `go run . -list`. It may be a good idea to alias `taskrunner` to run taskrunner.

## Example tasks
Add a `tasks.go` file, this contains all task descriptions:
```go
package main

import (
	"context"

	"github.com/pylon-labs/taskrunner"
	"github.com/pylon-labs/taskrunner/goextensions"
	"github.com/pylon-labs/taskrunner/shell"
)

// Run a simple task.
var myTask = taskrunner.Add(&taskrunner.Task{
	Name:         "my/task",
	RunWithFlags: func(ctx context.Context, shellRun shell.ShellRun, flags map[string]taskrunner.FlagArg) error {
		return shellRun(ctx, `echo Hello World`)
	},
})

// Run a task which performs different behaviour based on
// the flags passed to it via CLI.
var myTaskWithFlags = taskrunner.Add(&taskrunner.Task{
	Name:         "my/go/task",
	Description:  "Run a go file and re-run when it changes",
	RunWithFlags: func(ctx context.Context, shellRun shell.ShellRun, flags map[string]taskrunner.FlagArg) error {
		// Note: You can also check the ShortName string i.e. flags["b"].
		if flag, ok := flags["boolFlag"]; ok {
			// Note: FlagArgs include helper fns: BoolVal, StringVal, IntVal, Float64Val
			// and DurationVal. They cast the arg passed via CLI into the appropriate
			// type based on the flag ValueType.
			// The raw value passed via CLI is also avaialble via the FlagArg helper fn Value. 
			if flag.BoolVal() {
				return ShellRun(ctx, `echo Hello World: 1`)
			}
		}
			
		return shellRun(ctx, `echo Hello World: 2`)
	},
	Flags: []taskrunner.TaskFlag{
		{
			Description: "Passing the `--boolFlag/-b` flag has X effect on the task.",
			LongName: "boolFlag",
			ShortName: []rune("b")[0],
			Default: "false",
			// Note: BoolTypeFlag, StringTypeFlag, Float64TypeFlag and DurationFlag.
			ValueType: taskrunner.BoolTypeFlag,
		}
	},
})

// Run a task depending on another task.
var myDependentTask = taskrunner.Add(&taskrunner.Task{
	Name:         "my/dependent/task",
	Dependencies: []*taskrunner.Task{myTask},
	RunWithFlags: func(ctx context.Context, shellRun shell.ShellRun flags map[string]taskrunner.FlagArg) error {
		return shellRun(ctx, `echo Hello Again`)
	},
})


// Run a task which monitors a file and reruns on changes
// a change will also invalidate dependencies.
var myGoTask = taskrunner.Add(&taskrunner.Task{
	Name:         "my/go/task",
	Description:  "Run a go file and re-run when it changes",
	RunWithFlags: func(ctx context.Context, shellRun shell.ShellRun, flags map[string]taskrunner.FlagArg) error {
		return shellRun(ctx, `cd src/example && go run .`)
	},
	Sources: []string{"src/example/**/*.go"},
})

// Run a task using WrapWithGoBuild to automatically setup
// invalidation for the task according to the import graph
// of the specified package.
var builder = goextensions.NewGoBuilder()

var myWrappedTask = taskrunner.Add(&taskrunner.Task{
	Name: "my/wrapped/task",
        RunWithFlags: func(ctx context.Context, shellRun shell.ShellRun, flags map[string]taskrunner.FlagArg) error {
		return shellRun(ctx, `example`)
	},
}, builder.WrapWithGoBuild("example"))
```

## Default tasks to run
It is possible to add a `workspace.taskrunner.json` file, this contains the default tasks to run when taskrunner is run without any arguments.
```json
{
  "path": "./",
  "desiredTasks": [
    "my/task"
  ]
}
```

### Graceful shell cancellation

Shell commands use the interpreter's default cancellation behavior unless the
caller opts in to `shell.GracefulCancellation(grace)`. Supply a positive duration
through `ShellRunOptions` (or directly to `shell.Run`). For example:

```go
taskrunner.Run(taskrunner.ExecutorOptions(
    taskrunner.ShellRunOptions(shell.GracefulCancellation(5 * time.Second)),
))
```

On Unix, the option sends SIGTERM to the immediate external command when its
context is cancelled. pnpm 11.27.1 forwards SIGTERM, whereas its terminal-attached
SIGINT handling assumes the terminal already signalled its children. That
assumption does not hold for taskrunner's PID-only signal. Windows and Plan 9
terminate the immediate process instead. Existing consumers keep the default
handler unless they opt in; this option preserves process groups, sessions, and
terminal ownership.

`os/exec.Cmd.WaitDelay` bounds waiting after cancellation or immediate-process
exit. When the delay expires, it kills the immediate process if necessary and
closes inherited I/O pipes that are still open. It also bounds pipe draining
after normal process exit: if a descendant still holds output open when the
delay expires, the command prints `<name>: output still open <grace> after exit;
closed it` to stderr and exits with status 1, so `||`, `&&`, and `set -e` apply.
Cancellation is returned as a context error, including when graceful cleanup
exits successfully. Normal shell exit codes and redirects are preserved.

#### Compatibility scope and known limitations

This is a narrow signal-compatibility and bounded-wait option, not a guarantee
that every descendant completes cleanup. Tested macOS pnpm 11.27.1 lifecycle
and exec commands, Linux headless commands, and Linux terminal exec commands
complete cooperative cleanup. Ubuntu CI's terminal-attached `pnpm run` cases
return within the deadline but fail descendant-cleanup checks. A retained,
non-forwarding shell reproduces the signal-delivery gap; the exact difference
between that CI environment and passing local Linux fixtures remains unresolved.

A shell or older pnpm launcher can receive SIGTERM without forwarding it to the
application. Descendants may then keep running, hold ports, or duplicate watchers
after a restart. This risk is not exclusive to Linux. Install the actual pinned
pnpm launcher: an older global launcher can synchronously delegate to 11.27.1
without forwarding signals. Check its version outside a project that auto-selects
a package-manager version; checking inside the project can hide the old launcher.

Only the immediate process is forcibly killed. Descendants that ignore signals,
move to another process group/session, or sit behind non-forwarding wrappers may
survive. Closing inherited pipes does not prove that descendants have exited.
The post-exit bound applies only to output that `os/exec` copies through its own
pipe. Output written directly to an OS file, such as the default `os.Stderr`, is
not waited on, so a descendant can keep writing after the command returns. In a
shell pipeline, the next stage reads until every writer closes the pipe, so the
pipeline waits for descendants that hold it, with no bound.
Arbitrary blocking readers/writers and Go task functions must still cooperate
with cancellation. The executor waits for task functions to return; it does not
independently verify that their ports or other resources have been released.

Future work may add explicit process-group ownership for supervised services,
with a separate policy for interactive terminal input and group-wide escalation.
Moving every command into a background group can stop terminal reads; a new
session loses its controlling terminal. Neither approach alone contains children
that detach. These changes need dedicated lifecycle/terminal tests and are
separate from this option. Tracked as follow-up scope under
[PLA-1764](https://linear.app/usepylon/issue/PLA-1764).

#### Regression coverage

The shell regression tests use self-contained process fixtures. Set
`TASKRUNNER_TEST_PNPM=1` with Node and pnpm 11.27.1 on PATH to additionally test
lifecycle and exec cancellation. CI runs headless race checks on Linux/macOS and
runs the pnpm tests with a controlling terminal on both systems.

Only the terminal step sets `TASKRUNNER_TEST_PNPM_TERMINAL=1`. On Linux, this
explicitly skips the two `pnpm run` descendant-cleanup subtests, reporting the
observed cleanup marker and process state. Their startup, cancellation error,
and cancellation deadline are still required. All headless, macOS, and terminal
`pnpm exec` cleanup assertions remain required. Omit the terminal marker to run
the original strict cleanup assertions when investigating the known limitation.
