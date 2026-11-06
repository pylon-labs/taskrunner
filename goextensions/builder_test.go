package goextensions_test

import (
	"context"
	"errors"
	"fmt"
	"github.com/pylon-labs/taskrunner"
	"github.com/pylon-labs/taskrunner/goextensions"
	"github.com/pylon-labs/taskrunner/shell"
	"github.com/stretchr/testify/assert"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestGoBuilderBatchesDependencyDiscovery(t *testing.T) {
	b := goextensions.NewGoBuilder()
	b.ModuleRoot = t.TempDir()
	var builds, discoveries atomic.Int32
	var generation atomic.Int32
	generation.Store(1)
	run := func(ctx context.Context, command string, options ...shell.RunOption) error {
		if strings.HasPrefix(command, "go install ") {
			builds.Add(1)
			return nil
		}
		discoveries.Add(1)
		dependency := "pylon/old"
		if generation.Load() == 2 {
			dependency = "pylon/new"
		}
		return shell.Run(ctx, fmt.Sprintf("printf 'pylon/a %s\\npylon/b %s\\n'", dependency, dependency), options...)
	}
	tasks := make([]*taskrunner.Task, 2)
	for i, pkg := range []string{"pylon/a", "pylon/b"} {
		tasks[i] = b.WrapWithGoBuild(pkg)(&taskrunner.Task{})
	}
	for pass := range 2 {
		var wg sync.WaitGroup
		results := make(chan error, len(tasks))
		for _, task := range tasks {
			wg.Add(1)
			go func() {
				defer wg.Done()
				results <- task.RunWithFlags(context.Background(), run, nil)
			}()
		}
		wg.Wait()
		close(results)
		for err := range results {
			assert.NoError(t, err)
		}
		assert.Equal(t, int32(pass+1), builds.Load())
		assert.Equal(t, int32(pass+1), discoveries.Load())
		for _, task := range tasks {
			assert.True(t, task.ShouldInvalidate(taskrunner.FileChange{File: "backend/go/src/pylon/a/main.go"}) || task.ShouldInvalidate(taskrunner.FileChange{File: "backend/go/src/pylon/b/main.go"}))
			assert.Equal(t, pass == 0, task.ShouldInvalidate(taskrunner.FileChange{File: "backend/go/src/pylon/old/value.go"}))
			assert.Equal(t, pass == 1, task.ShouldInvalidate(taskrunner.FileChange{File: "backend/go/src/pylon/new/value.go"}))
			assert.False(t, task.ShouldInvalidate(taskrunner.FileChange{File: "backend/go/src/pylon/unrelated/value.go"}))
			assert.False(t, task.ShouldInvalidate(taskrunner.FileChange{File: "backend/go/src/pylon/new/value_test.go"}))
		}
		generation.Store(2)
	}
}

func TestGoBuilderCancellationIsScopedToCaller(t *testing.T) {
	b := goextensions.NewGoBuilder()
	b.ModuleRoot = t.TempDir()
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	defer cancelFirst()
	started := make(chan context.Context, 1)
	release := make(chan struct{})
	run := func(ctx context.Context, command string, options ...shell.RunOption) error {
		if strings.HasPrefix(command, "go install ") {
			started <- ctx
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return shell.Run(ctx, "printf 'pylon/a\\npylon/b\\n'", options...)
	}
	firstResult, secondResult := make(chan error, 1), make(chan error, 1)
	go func() { firstResult <- b.Build(firstCtx, run, "pylon/a") }()
	go func() { secondResult <- b.Build(context.Background(), run, "pylon/b") }()
	var buildContext context.Context
	select {
	case buildContext = <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("build did not start")
	}
	cancelFirst()
	assert.NoError(t, buildContext.Err())
	close(release)
	assert.ErrorIs(t, <-firstResult, context.Canceled)
	select {
	case err := <-secondResult:
		assert.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("remaining caller did not complete")
	}
}

func TestGoBuilderRecoversFromFailedBatch(t *testing.T) {
	b := goextensions.NewGoBuilder()
	b.ModuleRoot = t.TempDir()
	failure := errors.New("compilation failed")
	var attempts atomic.Int32
	run := func(ctx context.Context, command string, options ...shell.RunOption) error {
		if strings.HasPrefix(command, "go install ") {
			if attempts.Add(1) == 1 {
				return failure
			}
			return nil
		}
		return shell.Run(ctx, "printf 'pylon/a\\n'", options...)
	}
	assert.ErrorIs(t, b.Build(context.Background(), run, "pylon/a"), failure)
	assert.NoError(t, b.Build(context.Background(), run, "pylon/a"))
}
