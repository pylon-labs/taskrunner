package taskrunner

import (
	"context"
	"testing"
	"time"

	"github.com/pylon-labs/taskrunner/config"
	"github.com/pylon-labs/taskrunner/shell"
	"github.com/stretchr/testify/assert"
)

type testInvalidationEvent struct{}

func (f testInvalidationEvent) Reason() InvalidationReason {
	return InvalidationReason_Invalid
}

func (f testInvalidationEvent) Description() string {
	return "the test case decided to invalidate the task"
}

func assertNextEventKind(t *testing.T, events <-chan ExecutorEvent, expected ExecutorEventKind) {
	t.Helper()
	select {
	case event, ok := <-events:
		if !ok {
			t.Fatalf("events closed before observing %s", expected)
		}
		assert.Equal(t, expected, event.Kind())
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", expected)
	}
}

func TestKeepAliveTaskRestartsAfterInvalidationCancellation(t *testing.T) {
	config := &config.Config{}
	started := make(chan struct{}, 2)

	task := &Task{
		Name:      "keepalive",
		KeepAlive: true,
		Run: func(ctx context.Context, shellRun shell.ShellRun) error {
			started <- struct{}{}
			<-ctx.Done()
			return nil
		},
	}

	executor := NewExecutor(config, []*Task{task})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- executor.Run(ctx, []string{task.Name}, &Runtime{})
	}()

	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for keepalive task to start")
	}
	executor.Invalidate(task, testInvalidationEvent{})

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for keepalive task to restart after invalidation")
	}

	cancel()
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for executor to stop")
	}
}

func TestKeepAliveTaskRestartsAfterTaskContextCancellation(t *testing.T) {
	config := &config.Config{}
	started := make(chan struct{}, 2)

	task := &Task{
		Name:      "keepalive",
		KeepAlive: true,
		Run: func(ctx context.Context, shellRun shell.ShellRun) error {
			started <- struct{}{}
			<-ctx.Done()
			return nil
		},
	}

	executor := NewExecutor(config, []*Task{task})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- executor.Run(ctx, []string{task.Name}, &Runtime{})
	}()

	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for keepalive task to start")
	}

	executor.taskExecution(task).cancel()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for keepalive task to restart after task context cancellation")
	}

	cancel()
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for executor to stop")
	}
}

func TestKeepAliveInvalidationSynchronizesTaskCompletion(t *testing.T) {
	config := &config.Config{}
	started := make(chan struct{}, 2)
	cancellationObserved := make(chan struct{}, 1)
	allowCompletion := make(chan struct{})

	task := &Task{
		Name:      "keepalive",
		KeepAlive: true,
		Run: func(ctx context.Context, shellRun shell.ShellRun) error {
			started <- struct{}{}
			<-ctx.Done()
			select {
			case cancellationObserved <- struct{}{}:
				<-allowCompletion
			default:
			}
			return nil
		},
	}

	executor := NewExecutor(config, []*Task{task})
	events := executor.Subscribe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- executor.Run(ctx, []string{task.Name}, &Runtime{})
	}()

	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for keepalive task to start")
	}
	assertNextEventKind(t, events, ExecutorEventKind_TaskStarted)

	executor.Invalidate(task, testInvalidationEvent{})
	invalidationDone := make(chan struct{})
	go func() {
		executor.evaluateInvalidationPlan(false)
		close(invalidationDone)
	}()

	select {
	case <-cancellationObserved:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for keepalive task to observe invalidation cancellation")
	}
	assertNextEventKind(t, events, ExecutorEventKind_TaskInvalidated)
	close(allowCompletion)
	assertNextEventKind(t, events, ExecutorEventKind_TaskStopped)

	select {
	case <-invalidationDone:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for invalidation to finish after task completion")
	}
	select {
	case err := <-done:
		t.Fatalf("executor stopped before the keepalive task restarted: %v", err)
	default:
	}

	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for keepalive task to restart after invalidation")
	}
	assertNextEventKind(t, events, ExecutorEventKind_TaskStarted)

	cancel()
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for executor to stop")
	}
}

func TestTaskWaitsForInvalidationCompletion(t *testing.T) {
	task := &Task{Name: "task"}
	tasks := make(taskSet)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	execution, _ := tasks.add(ctx, task)
	execution.state = taskExecutionState_running
	execution.pendingInvalidations[testInvalidationEvent{}] = struct{}{}
	executionCtx := execution.ctx

	_, terminalCh, invalidationDone, cancelInvalidation, ok := execution.beginInvalidation()
	assert.True(t, ok)
	select {
	case <-executionCtx.Done():
		t.Fatal("beginInvalidation canceled the task before its event could be published")
	default:
	}
	cancelInvalidation()
	select {
	case <-executionCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("captured invalidation cancellation did not stop the task")
	}

	waitDone := make(chan struct{})
	go func() {
		execution.waitForInvalidation(terminalCh)
		close(waitDone)
	}()

	select {
	case <-waitDone:
		t.Fatal("task stopped waiting before invalidation completed")
	case <-time.After(50 * time.Millisecond):
	}

	execution.finishInvalidation(ctx, terminalCh)
	execution.completeInvalidation(invalidationDone)
	select {
	case <-waitDone:
	case <-time.After(time.Second):
		t.Fatal("task did not stop waiting after invalidation completed")
	}
	assert.Equal(t, taskExecutionState(taskExecutionState_invalid), execution.getState())
}

func TestInvalidatingDependencyIsNotReady(t *testing.T) {
	dependency := &Task{Name: "dependency"}
	dependent := &Task{
		Name:         "dependent",
		Dependencies: []*Task{dependency},
	}
	tasks := make(taskSet)
	dependentExecution, _ := tasks.add(context.Background(), dependent)
	dependencyExecution := tasks[dependency]
	dependencyExecution.state = taskExecutionState_done
	dependencyExecution.invalidating = true

	_, ready := dependentExecution.start()
	assert.False(t, ready)
}

func TestShouldInvalidateCanReadTaskState(t *testing.T) {
	task := &Task{Name: "task"}
	tasks := make(taskSet)
	execution, _ := tasks.add(context.Background(), task)
	execution.state = taskExecutionState_done
	handler := NewTaskHandler(execution)
	observedState := make(chan TaskHandlerExecutionState, 1)
	task.ShouldInvalidate = func(event InvalidationEvent) bool {
		observedState <- handler.State()
		return false
	}

	done := make(chan struct{})
	go func() {
		execution.Invalidate(testInvalidationEvent{})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ShouldInvalidate deadlocked while reading task state")
	}
	assert.Equal(t, TaskHandlerExecutionState(TaskHandlerExecutionState_Done), <-observedState)
}

func TestDependentTaskStartsWithoutInvalidationDelay(t *testing.T) {
	config := &config.Config{}
	dependencyDone := make(chan struct{}, 1)
	dependentStarted := make(chan struct{}, 1)

	dependency := &Task{
		Name: "dependency",
		Run: func(ctx context.Context, shellRun shell.ShellRun) error {
			dependencyDone <- struct{}{}
			return nil
		},
	}
	dependent := &Task{
		Name:         "dependent",
		Dependencies: []*Task{dependency},
		Run: func(ctx context.Context, shellRun shell.ShellRun) error {
			dependentStarted <- struct{}{}
			return nil
		},
	}

	executor := NewExecutor(config, []*Task{dependency, dependent})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- executor.Run(ctx, []string{dependent.Name}, &Runtime{})
	}()

	select {
	case <-dependencyDone:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for dependency task to complete")
	}

	select {
	case <-dependentStarted:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("dependent task start was delayed by invalidation planning")
	}

	cancel()
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for executor to stop")
	}
}

func TestInvalidateCoalescesWakeupsWithoutBlocking(t *testing.T) {
	config := &config.Config{}
	task := &Task{
		Name: "task",
		Run: func(ctx context.Context, shellRun shell.ShellRun) error {
			return nil
		},
	}

	executor := NewExecutor(config, []*Task{task})
	tasks := make(taskSet)
	execution, _ := tasks.add(context.Background(), task)
	execution.state = taskExecutionState_done
	executor.tasks = tasks

	done := make(chan struct{})
	go func() {
		executor.Invalidate(task, testInvalidationEvent{})
		executor.Invalidate(task, testInvalidationEvent{})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for duplicate invalidations to coalesce")
	}
}

func boolPtr(i bool) *bool {
	return &i
}

func intPtr(i int) *int {
	return &i
}

func float64Ptr(i float64) *float64 {
	return &i
}

func stringPtr(i string) *string {
	return &i
}

func durationPtr(i time.Duration) *time.Duration {
	return &i
}

func TestParseTaskOptionsListToMap(t *testing.T) {
	config := &config.Config{}
	mockRunWithFlags := func(ctx context.Context, shellRun shell.ShellRun, flags map[string]FlagArg) error {
		return nil
	}

	taskFlagA := TaskFlag{
		Description: "This is a description of what bool flag A does.",
		LongName:    "flagA",
		ShortName:   []rune("a")[0],
		ValueType:   BoolTypeFlag,
		Default:     "true",
	}
	taskFlagB := TaskFlag{
		Description: "This is a description of what string flag B does.",
		ShortName:   []rune("b")[0],
		ValueType:   StringTypeFlag,
		Default:     "presidential dolphin",
	}
	taskFlagC := TaskFlag{
		Description: "This is a description of what int flag C does.",
		ShortName:   []rune("c")[0],
		ValueType:   IntTypeFlag,
		Default:     "10",
	}
	taskFlagD := TaskFlag{
		Description: "This is a description of what float64 flag D does.",
		ShortName:   []rune("d")[0],
		ValueType:   Float64TypeFlag,
		Default:     "1.72",
	}
	taskFlagE := TaskFlag{
		Description: "This is a description of what duration flag E does.",
		ShortName:   []rune("e")[0],
		ValueType:   DurationTypeFlag,
		Default:     "11h",
	}
	taskFlagF := TaskFlag{
		Description: "This is a description of what duration flag F does.",
		ShortName:   []rune("f")[0],
		ValueType:   BoolTypeFlag,
	}
	mockTaskName := "mock/task/1"
	mockTask1 := &Task{
		Name:         mockTaskName,
		RunWithFlags: mockRunWithFlags,
		Flags:        []TaskFlag{taskFlagA, taskFlagB, taskFlagC, taskFlagD, taskFlagE},
	}
	mockTaskFlagRegistry := map[string]map[string]TaskFlag{
		mockTaskName: {
			"flagA": taskFlagA,
			"a":     taskFlagA,
			"b":     taskFlagB,
			"c":     taskFlagC,
			"d":     taskFlagD,
			"e":     taskFlagE,
			"f":     taskFlagF,
		},
	}

	tasks := []*Task{mockTask1}
	testCases := []struct {
		description          string
		flagArgs             []string
		expectedFlagAVal     *bool
		expectedLongFlagAVal *bool
		expectedFlagBVal     *string
		expectedFlagCVal     *int
		expectedFlagDVal     *float64
		expectedFlagEVal     *time.Duration
		expectedFlagFVal     *bool
		expectFlagFNil       bool
		invalidFlagPanicVal  string
	}{
		{
			description:          "Should parse BoolTypeFlags correctly and include value for both Short and Long Names",
			flagArgs:             []string{"-a"},
			expectedFlagAVal:     boolPtr(true),
			expectedLongFlagAVal: boolPtr(true),
		},
		{
			description:      "Should parse String/Duration/Int/Float64TypeFlags wrapped in \"\"s correctly",
			flagArgs:         []string{"-b=\"test\"", "-c=\"1\"", "-d=\"1.3\"", "-e=\"10h\""},
			expectedFlagBVal: stringPtr("test"),
			expectedFlagCVal: intPtr(1),
			expectedFlagDVal: float64Ptr(1.3),
			expectedFlagEVal: durationPtr(time.Duration(10 * time.Hour)),
		},
		{
			description:      "Should parse String/Duration/Int/Float64TypeFlags not wrapped in \"\"s correctly",
			flagArgs:         []string{"-b=test", "-c=1", "-d=1.3", "-e=10h"},
			expectedFlagBVal: stringPtr("test"),
			expectedFlagCVal: intPtr(1),
			expectedFlagDVal: float64Ptr(1.3),
			expectedFlagEVal: durationPtr(time.Duration(10 * time.Hour)),
		},
		{
			description:      "Should respect Default str value if no arg passed to variable flag",
			flagArgs:         []string{"-b"},
			expectedFlagBVal: stringPtr("presidential dolphin"),
		},
		{
			description:      "Should respect Default int value if no arg passed to variable flag",
			flagArgs:         []string{"-c"},
			expectedFlagCVal: intPtr(10),
		},
		{
			description:      "Should respect Default float64 value if no arg passed to variable flag",
			flagArgs:         []string{"-d"},
			expectedFlagDVal: float64Ptr(1.72),
		},
		{
			description:      "Should respect Default duration value if no arg passed to variable flag",
			flagArgs:         []string{"-e"},
			expectedFlagEVal: durationPtr(time.Duration(11 * time.Hour)),
		},
		{
			description:      "Bare bool flag without =value should be treated as true",
			flagArgs:         []string{"-f"},
			expectedFlagFVal: boolPtr(true),
		},
		{
			description:    "Explicit empty bool flag (--flag=) should not default to true",
			flagArgs:       []string{"-f="},
			expectFlagFNil: true,
		},
		{
			description:          "Should store Default values for all flags and Value nil when no args are passed",
			flagArgs:             []string{},
			expectedFlagAVal:     boolPtr(true),
			expectedLongFlagAVal: boolPtr(true),
			expectedFlagBVal:     stringPtr("presidential dolphin"),
			expectedFlagCVal:     intPtr(10),
			expectedFlagDVal:     float64Ptr(1.72),
			expectFlagFNil:       true,
		},
		{
			description:         "Should panic if there are multiple `=`s in the flag",
			flagArgs:            []string{"-b=\"testing\"=\"testing123\""},
			invalidFlagPanicVal: "Invalid flag syntax for mock/task/1: `-b=\"testing\"=\"testing123\"`",
		},
		{
			description:         "Should panic if an unsupported flag is passed",
			flagArgs:            []string{"--invalidFlag"},
			invalidFlagPanicVal: "Unsupported flag passed to mock/task/1: `--invalidFlag`. See error: Unsupported flag: --invalidFlag",
		},
		{
			description: "Should panic if not possible to cast string to int for IntTypeFlag args",
			flagArgs:    []string{"-c=\"\"00\"10\"b\"\""},
		},
		{
			description: "Should panic if not possible to cast string to float64 for Float64TypeFlag args",
			flagArgs:    []string{"-d=\"\"00\"10.3\"b\"\""},
		},
		{
			description: "Should panic if not possible to cast string to duration for DurationTypeFlag args",
			flagArgs:    []string{"-e=\"\"00\"10.3\"b\"\"09m"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			executorOptions := []ExecutorOption{
				func(e *Executor) {
					e.taskFlagArgs = map[string][]string{
						mockTaskName: tc.flagArgs,
					}
				},
				func(e *Executor) {
					e.taskFlagsRegistry = mockTaskFlagRegistry
				},
			}

			executor := NewExecutor(config, tasks, executorOptions...)
			if tc.invalidFlagPanicVal != "" {
				assert.PanicsWithValue(t, tc.invalidFlagPanicVal, func() { executor.parseTaskFlagsIntoMap(mockTaskName, tc.flagArgs) })
			} else {
				flagsMap := executor.parseTaskFlagsIntoMap(mockTaskName, tc.flagArgs)
				if tc.expectedFlagAVal != nil {
					assert.Equal(t, *tc.expectedFlagAVal, *flagsMap["a"].BoolVal())
					assert.Equal(t, *tc.expectedLongFlagAVal, *flagsMap["flagA"].BoolVal())

					flagArgShort := flagsMap["a"]
					assert.Panics(t, func() { flagArgShort.IntVal() })
					assert.Panics(t, func() { flagArgShort.StringVal() })
					assert.Panics(t, func() { flagArgShort.Float64Val() })
					assert.Panics(t, func() { flagArgShort.DurationVal() })

					flagArgLong := flagsMap["flagA"]
					assert.Panics(t, func() { flagArgLong.IntVal() })
					assert.Panics(t, func() { flagArgLong.StringVal() })
					assert.Panics(t, func() { flagArgLong.Float64Val() })
					assert.Panics(t, func() { flagArgLong.DurationVal() })

					if len(tc.flagArgs) == 0 {
						assert.Nil(t, flagArgShort.Value)
						assert.Nil(t, flagArgLong.Value)
					}
				}

				if tc.expectedFlagBVal != nil {
					assert.Equal(t, *tc.expectedFlagBVal, *flagsMap["b"].StringVal())

					flagArg := flagsMap["b"]
					assert.Panics(t, func() { flagArg.IntVal() })
					assert.Panics(t, func() { flagArg.BoolVal() })
					assert.Panics(t, func() { flagArg.Float64Val() })
					assert.Panics(t, func() { flagArg.DurationVal() })

					if len(tc.flagArgs) == 0 {
						assert.Nil(t, flagArg.Value)
					}
				}

				if tc.expectedFlagCVal != nil {
					assert.Equal(t, *tc.expectedFlagCVal, *flagsMap["c"].IntVal())

					flagArg := flagsMap["c"]
					assert.Panics(t, func() { flagArg.StringVal() })
					assert.Panics(t, func() { flagArg.BoolVal() })
					assert.Panics(t, func() { flagArg.Float64Val() })
					assert.Panics(t, func() { flagArg.DurationVal() })

					if len(tc.flagArgs) == 0 {
						assert.Nil(t, flagArg.Value)
					}
				}

				if tc.expectedFlagDVal != nil {
					assert.Equal(t, *tc.expectedFlagDVal, *flagsMap["d"].Float64Val())

					flagArg := flagsMap["d"]
					assert.Panics(t, func() { flagArg.StringVal() })
					assert.Panics(t, func() { flagArg.BoolVal() })
					assert.Panics(t, func() { flagArg.IntVal() })
					assert.Panics(t, func() { flagArg.DurationVal() })

					if len(tc.flagArgs) == 0 {
						assert.Nil(t, flagArg.Value)
					}
				}

				if tc.expectedFlagEVal != nil {
					assert.Equal(t, *tc.expectedFlagEVal, *flagsMap["e"].DurationVal())

					flagArg := flagsMap["e"]
					assert.Panics(t, func() { flagArg.StringVal() })
					assert.Panics(t, func() { flagArg.BoolVal() })
					assert.Panics(t, func() { flagArg.IntVal() })
					assert.Panics(t, func() { flagArg.Float64Val() })

					if len(tc.flagArgs) == 0 {
						assert.Nil(t, flagArg.Value)
					}
				}

				if tc.expectedFlagFVal != nil {
					assert.Equal(t, *tc.expectedFlagFVal, *flagsMap["f"].BoolVal())
				}
				if tc.expectFlagFNil {
					assert.Nil(t, flagsMap["f"].BoolVal())
				}
			}
		})
	}
}
