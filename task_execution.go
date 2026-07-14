package taskrunner

import (
	"context"
	"sync"
	"time"
)

type taskExecutionState int

const (
	// taskExecutionState_invalid tasks are not running and are queued to be started
	// on the next invalidation plan.
	taskExecutionState_invalid = iota
	// taskExecutionState_running tasks are currently executing. They may transition to
	// error on failure or done on succesful completion. Tasks may also be canceled.
	taskExecutionState_running
	// taskExecutionState_error tasks were running and failed. They may transition to invalid.
	taskExecutionState_error
	// taskExecutionState_done tasks were running and completed without errors. They may transition to invalid.
	taskExecutionState_done
	// taskExecutionState_canceled tasks were running and were stopped by the
	// user. Tasks cannot leave canceled state.
	taskExecutionState_canceled
)

// taskExecution is a node in the Executor's DAG. It holds the state
// for a single task's executions and is reused across task executions.
type taskExecution struct {
	mu         sync.Mutex
	definition *Task

	ctx          context.Context
	cancel       func()
	state        taskExecutionState
	invalidating bool
	terminalCh   chan struct{}

	invalidationDone       chan struct{}
	invalidationTerminalCh chan struct{}

	dependencies []*taskExecution
	dependents   []*taskExecution

	pendingInvalidations map[InvalidationEvent]struct{}
}

func (e *taskExecution) simpleEvent() *simpleEvent {
	return &simpleEvent{
		taskHandler: NewTaskHandler(e),
		timestamp:   time.Now(),
	}
}

// start transitions an executable task to running and returns its execution context.
func (e *taskExecution) start() (context.Context, bool) {
	for _, dep := range e.dependencies {
		if !dep.isDone() {
			return nil, false
		}
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.state != taskExecutionState_invalid || e.invalidating {
		return nil, false
	}

	e.state = taskExecutionState_running
	return e.ctx, true
}

func (e *taskExecution) getState() taskExecutionState {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state
}

func (e *taskExecution) isDone() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state == taskExecutionState_done && !e.invalidating
}

// beginInvalidation atomically claims a task and returns the cancellation and
// completion signals needed to finish its invalidation without holding a mutex.
func (e *taskExecution) beginInvalidation() ([]InvalidationEvent, chan struct{}, chan struct{}, func(), bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.pendingInvalidations) == 0 ||
		e.state == taskExecutionState_invalid ||
		e.state == taskExecutionState_canceled ||
		e.invalidating {
		return nil, nil, nil, nil, false
	}

	reasons := make([]InvalidationEvent, 0, len(e.pendingInvalidations))
	for reason := range e.pendingInvalidations {
		reasons = append(reasons, reason)
	}

	e.invalidating = true
	e.invalidationDone = make(chan struct{})
	e.invalidationTerminalCh = e.terminalCh
	return reasons, e.terminalCh, e.invalidationDone, e.cancel, true
}

func (e *taskExecution) finishInvalidation(executionCtx context.Context, terminalCh <-chan struct{}) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.invalidating {
		return
	}

	e.ctx, e.cancel = context.WithCancel(executionCtx)
	if executionCtx.Err() != nil {
		e.state = taskExecutionState_canceled
	} else {
		e.state = taskExecutionState_invalid
	}
	e.invalidating = false
	if e.terminalCh == terminalCh {
		e.terminalCh = make(chan struct{}, 1)
	}
	e.pendingInvalidations = make(map[InvalidationEvent]struct{})
}

func (e *taskExecution) completeInvalidation(invalidationDone chan struct{}) {
	e.mu.Lock()
	if e.invalidationDone == invalidationDone {
		e.invalidationDone = nil
		e.invalidationTerminalCh = nil
	}
	close(invalidationDone)
	e.mu.Unlock()
}

func (e *taskExecution) waitForInvalidation(terminalCh chan struct{}) {
	e.mu.Lock()
	var invalidationDone chan struct{}
	if e.invalidationTerminalCh == terminalCh {
		invalidationDone = e.invalidationDone
	}
	e.mu.Unlock()
	if invalidationDone != nil {
		<-invalidationDone
	}
}

// Invalidate marks a taskExecution as invalid. It does not produce
// side effects by itself.
func (e *taskExecution) Invalidate(event InvalidationEvent) bool {
	if e.definition.ShouldInvalidate != nil && !e.definition.ShouldInvalidate(event) {
		return false
	}

	e.mu.Lock()
	if e.state == taskExecutionState_invalid {
		e.mu.Unlock()
		return false
	}
	e.pendingInvalidations[event] = struct{}{}
	e.mu.Unlock()

	for _, dep := range e.dependents {
		dep.Invalidate(DependencyChange{
			Source: e.definition,
		})
	}

	return true
}

type taskSet map[*Task]*taskExecution

func (s taskSet) add(executionCtx context.Context, task *Task) (*taskExecution, []*taskExecution) {
	if s[task] != nil {
		return s[task], s[task].dependencies
	}

	ctx, cancel := context.WithCancel(executionCtx)
	self := &taskExecution{
		definition:           task,
		ctx:                  ctx,
		cancel:               cancel,
		state:                taskExecutionState_invalid,
		terminalCh:           make(chan struct{}, 1),
		pendingInvalidations: make(map[InvalidationEvent]struct{}),
	}

	var dependencies []*taskExecution
	for _, dep := range task.Dependencies {
		depExec, depExecDeps := s.add(ctx, dep)
		depExec.dependents = append(depExec.dependents, self)
		dependencies = append(dependencies, depExecDeps...)
		dependencies = append(dependencies, depExec)
	}

	s[task] = self
	self.dependencies = dependencies

	return self, dependencies
}
