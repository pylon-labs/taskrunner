package goextensions

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mattn/go-zglob"
	"github.com/pylon-labs/taskrunner"
	"github.com/pylon-labs/taskrunner/clireporter"
	"github.com/pylon-labs/taskrunner/shell"
)

const buildBatchWindow = 50 * time.Millisecond

type buildResult struct {
	dependencies []string
	err          error
}

type request struct {
	pkg      string
	ctx      context.Context
	shellRun shell.ShellRun
	result   chan buildResult
}

// GoBuilder batches compilation and dependency discovery for concurrently requested packages.
type GoBuilder struct {
	requests chan request

	// Options must be set before the first build.
	LogToStdout bool
	ModuleRoot  string
}

func NewGoBuilder() *GoBuilder {
	b := &GoBuilder{requests: make(chan request, 128)}
	go func() {
		for first := range b.requests {
			requests := []request{first}
			timer := time.NewTimer(buildBatchWindow)
		collect:
			for {
				select {
				case r := <-b.requests:
					requests = append(requests, r)
				case <-timer.C:
					break collect
				}
			}
			b.build(requests)
		}
	}()
	return b
}

func (b *GoBuilder) build(requests []request) {
	active := requests[:0]
	for _, r := range requests {
		if err := r.ctx.Err(); err != nil {
			r.result <- buildResult{err: err}
		} else {
			active = append(active, r)
		}
	}
	if len(active) == 0 {
		return
	}

	// Cancel shared work only when every caller has canceled. A restarting task
	// must not cancel compilation for the other services in the same batch.
	ctx, cancel := context.WithCancel(context.WithoutCancel(active[0].ctx))
	defer cancel()
	var canceled atomic.Int32
	for _, r := range active {
		stop := context.AfterFunc(r.ctx, func() {
			if canceled.Add(1) == int32(len(active)) {
				cancel()
			}
		})
		defer stop()
	}

	packages := make(map[string]struct{})
	loggers := make(map[*taskrunner.Logger]struct{})
	for _, r := range active {
		packages[r.pkg] = struct{}{}
		if logger := taskrunner.LoggerFromContext(r.ctx); logger != nil {
			loggers[logger] = struct{}{}
		}
	}
	pkgList := make([]string, 0, len(packages))
	for pkg := range packages {
		pkgList = append(pkgList, fmt.Sprintf("%q", pkg))
	}
	sort.Strings(pkgList)
	args := strings.Join(pkgList, " ")

	var stdout, stderr io.Writer
	if b.LogToStdout {
		stdout = &clireporter.PrefixedWriter{Writer: os.Stdout, Prefix: "go/build/dev", Separator: ">"}
		stderr = &clireporter.PrefixedWriter{Writer: os.Stderr, Prefix: "go/build/dev", Separator: ">"}
	} else {
		var stdouts, stderrs []io.Writer
		for logger := range loggers {
			stdouts = append(stdouts, logger.Stdout)
			stderrs = append(stderrs, logger.Stderr)
		}
		stdout, stderr = io.MultiWriter(stdouts...), io.MultiWriter(stderrs...)
	}
	run := active[0].shellRun
	fmt.Fprintf(stdout, "building packages: %s\n", args)
	err := run(ctx, "go install -v "+args, shell.Stdout(stdout), shell.Stderr(stderr), shell.Dir(b.ModuleRoot))
	dependencies := make(map[string][]string)
	if err == nil {
		var output bytes.Buffer
		// One traversal shares Go's package loading across every binary in the batch.
		err = run(ctx, `go list -f '{{.ImportPath}} {{join .Deps " "}}' `+args,
			shell.Stdout(&output), shell.Stderr(stderr), shell.Dir(b.ModuleRoot))
		if err == nil {
			for _, line := range strings.Split(output.String(), "\n") {
				fields := strings.Fields(line)
				if len(fields) > 0 {
					dependencies[fields[0]] = fields[1:]
				}
			}
		}
	}
	fmt.Fprintln(stdout, "done building packages")
	for _, r := range active {
		result := buildResult{dependencies: dependencies[r.pkg], err: err}
		if result.err == nil {
			if _, ok := dependencies[r.pkg]; !ok {
				result.err = fmt.Errorf("go list returned no dependencies for %s", r.pkg)
			}
		}
		r.result <- result
	}
}

// Build schedules a build and waits for its batch's completion.
func (b *GoBuilder) Build(ctx context.Context, shellRun shell.ShellRun, pkg string) error {
	return b.buildWithDependencies(ctx, shellRun, pkg).err
}

func (b *GoBuilder) buildWithDependencies(ctx context.Context, shellRun shell.ShellRun, pkg string) buildResult {
	result := make(chan buildResult, 1)
	select {
	case b.requests <- request{pkg: pkg, ctx: ctx, shellRun: shellRun, result: result}:
	case <-ctx.Done():
		return buildResult{err: ctx.Err()}
	}
	// Keep task log subscriptions alive until shared work has stopped writing.
	// Returning on cancellation here could let the executor close them mid-build.
	r := <-result
	if err := ctx.Err(); err != nil {
		return buildResult{err: err}
	}
	return r
}

// buildBinder provides hooks for finding the dependencies of a package
// and wiring up a shouldInvalidate implementation that only accepts relevant
// file changes.
type buildBinder struct {
	pkg             string
	pkgDependencies []string
	mu              sync.RWMutex
}

func newBuildBinder(pkg string) *buildBinder {
	binder := &buildBinder{pkg: pkg}
	return binder
}

func (b *buildBinder) saveDependencies(dependencies []string) {
	patterns := make([]string, 0, len(dependencies)+1)
	for _, dep := range append(append([]string{}, dependencies...), b.pkg) {
		patterns = append(patterns, filepath.Join("**/", dep, "/*.go"))
	}
	b.mu.Lock()
	b.pkgDependencies = patterns
	b.mu.Unlock()
}

func (b *buildBinder) shouldInvalidate(event taskrunner.InvalidationEvent) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	switch event := event.(type) {
	case taskrunner.FileChange:
		// Bail out if we have not yet recorded any dependencies, i.e.
		// the task has yet to run, or an error occurred. We don't have enough
		// information to make a good judgment.
		if b.pkgDependencies == nil {
			return true
		}

		// It is possible that the invalidation event is for a non-go file.
		// The builder does not know how to handle this, and is only concerned
		// with invalidating go packages.
		if !strings.HasSuffix(event.File, ".go") {
			return true
		}

		// Ignore test files.
		if strings.HasSuffix(event.File, "_test.go") {
			return false
		}

		for _, dep := range b.pkgDependencies {
			if ok, _ := zglob.Match(dep, event.File); ok {
				return true
			}
		}
		return false
	default:
		return true
	}
}

// WrapWithGoBuild takes an input Task and builds a specified go package
// before running the task. If no task sources are specified, then
// WrapWithGoBuild will automatically setup invalidation for the task according
// to the import graph of the specified package.
func (builder *GoBuilder) WrapWithGoBuild(pkg string) taskrunner.TaskOption {
	return func(task *taskrunner.Task) *taskrunner.Task {
		newTask := *task

		buildBinder := newBuildBinder(pkg)
		// task.Run is deprecated, so default to defining
		// newTask.RunWithFlags if task.Run is not defined.
		// If both Run and RunWithFlags are defined, an error will
		// be thrown in the from the registry when the task is added.
		if task.Run != nil {
			newTask.Run = func(ctx context.Context, shellRun shell.ShellRun) error {
				result := builder.buildWithDependencies(ctx, shellRun, pkg)
				if result.err != nil {
					return result.err
				}
				buildBinder.saveDependencies(result.dependencies)

				if task.Run != nil {
					return task.Run(ctx, shellRun)
				}

				return nil
			}
		} else {
			newTask.RunWithFlags = func(ctx context.Context, shellRun shell.ShellRun, flags map[string]taskrunner.FlagArg) error {
				result := builder.buildWithDependencies(ctx, shellRun, pkg)
				if result.err != nil {
					return result.err
				}
				buildBinder.saveDependencies(result.dependencies)

				if task.RunWithFlags != nil {
					return task.RunWithFlags(ctx, shellRun, flags)
				}

				return nil
			}
		}

		newTask.ShouldInvalidate = func(event taskrunner.InvalidationEvent) bool {
			delegated := true
			if task.ShouldInvalidate != nil {
				delegated = task.ShouldInvalidate(event)
			}
			return buildBinder.shouldInvalidate(event) && delegated
		}

		return &newTask
	}
}
