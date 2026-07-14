package taskrunner

import (
	"context"
	"log"

	zglob "github.com/mattn/go-zglob"
	"github.com/pylon-labs/taskrunner/watcher"
)

// IsTaskSource checks if a provided path matches a source glob for the task.
func IsTaskSource(task *Task, path string) (matches bool) {
	for _, source := range task.Sources {
		if ok, err := zglob.Match(source, path); err != nil {
			log.Fatalf("invalid glob (%s):\n%v\n", path, err)
		} else if ok {
			matches = true
		}
	}
	for _, ignore := range task.Ignore {
		if ok, err := zglob.Match(ignore, path); err != nil {
			log.Fatalf("invalid glob (%s):\n%v\n", path, err)
		} else if ok {
			matches = false
		}
	}
	return matches
}

func (e *Executor) runWatch(ctx context.Context, cancel context.CancelFunc) {
	watcher := watcher.NewWatcher(e.config.WorkingDir)
	for _, enhancer := range e.watcherEnhancers {
		watcher = enhancer(watcher)
	}

	go func() {
		for event := range watcher.Events() {
			for task := range e.tasks {
				if IsTaskSource(task, event.RelativeFilename) {
					e.Invalidate(task, FileChange{
						File: event.RelativeFilename,
					})
				}
			}
		}
	}()

	e.wg.Go(func() error {
		err := watcher.Run(ctx)
		wasCanceled := ctx.Err() == context.Canceled
		cancel()
		e.mu.Lock()
		e.mu.Unlock()
		if err != nil {
			if !wasCanceled {
				return err
			}
		}
		return nil
	})
}
