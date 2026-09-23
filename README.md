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
context is cancelled. This lets wrappers such as pnpm 11.27.1 relay programmatic
shutdown: its terminal-attached SIGINT handling assumes the terminal already
signalled its children, which is not true for taskrunner's PID-only signal.
Windows and Plan 9 terminate the immediate process instead.

`os/exec.Cmd.WaitDelay` gives the command the specified time to exit before
killing it and closing inherited I/O pipes. It also bounds pipe draining after
normal process exit, returning an error if output cannot be fully drained.
Cancellation is returned as a context error, including when graceful cleanup
exits successfully. Normal shell exit codes and redirects are preserved.

This is cooperative cancellation, not process-tree containment. A descendant
that ignores termination or sits behind a non-forwarding wrapper may survive
forced shutdown. Arbitrary blocking readers/writers and Go task functions must
still cooperate with cancellation. The executor waits for task functions to
return; it does not independently verify every descendant has exited.

The shell regression tests use self-contained process fixtures. Set
`TASKRUNNER_TEST_PNPM=1` with Node and pnpm 11.27.1 on PATH to additionally test
lifecycle and exec cancellation. CI runs these tests headlessly and with a
controlling terminal on Linux and macOS.
