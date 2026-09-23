package shell

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"mvdan.cc/sh/v3/interp"
)

func TestRunShell(t *testing.T) {
	var buffer bytes.Buffer
	assert.NoError(t, Run(context.Background(), "echo 1", Stdout(&buffer)))
	assert.Equal(t, "1\n", buffer.String())
}

// TestShellProcess runs in a subprocess so cancellation exercises real OS signals.
func TestShellProcess(t *testing.T) {
	mode := os.Getenv("TASKRUNNER_SHELL_HELPER")
	if mode == "" {
		return
	}
	time.AfterFunc(20*time.Second, func() { os.Exit(90) })
	signal.Ignore(os.Interrupt)
	if mode == "ignore" {
		signal.Ignore(syscall.SIGTERM)
	} else {
		termination := make(chan os.Signal, 1)
		signal.Notify(termination, syscall.SIGTERM)
		go func() {
			<-termination
			time.Sleep(100 * time.Millisecond)
			_ = os.WriteFile(os.Getenv("TASKRUNNER_STOPPED"), []byte("stopped"), 0600)
			os.Exit(0)
		}()
	}
	if mode == "pipe" || mode == "orphan" {
		child := exec.Command(os.Args[0], "-test.run=^TestShellProcess$")
		child.Env = append(os.Environ(), "TASKRUNNER_SHELL_HELPER=ignore", "TASKRUNNER_READY="+os.Getenv("TASKRUNNER_READY")+".child")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if child.Start() != nil {
			os.Exit(91)
		}
	}
	_ = os.WriteFile(os.Getenv("TASKRUNNER_READY"), []byte(strconv.Itoa(os.Getpid())), 0600)
	if mode == "orphan" {
		os.Exit(0)
	}
	for {
		time.Sleep(time.Hour)
	}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	require.Eventually(t, func() bool { _, err := os.Stat(path); return err == nil }, 10*time.Second, 10*time.Millisecond, "waiting for %s", path)
}

func fixtureProcess(t *testing.T, path string) *os.Process {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	pid, err := strconv.Atoi(string(data))
	require.NoError(t, err)
	p, err := os.FindProcess(pid)
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Kill(); _ = p.Release() })
	return p
}

func TestRunGracefulCancellation(t *testing.T) {
	if runtime.GOOS == "windows" || runtime.GOOS == "plan9" {
		t.Skip("POSIX signal fixture")
	}
	executable, err := os.Executable()
	require.NoError(t, err)
	for _, mode := range []string{"graceful", "ignore", "pipe", "orphan"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			ready, stopped := filepath.Join(dir, "ready"), filepath.Join(dir, "stopped")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var stderr bytes.Buffer
			done := make(chan error, 1)
			go func() {
				done <- Run(ctx, `"$TASKRUNNER_HELPER_EXE" -test.run=^TestShellProcess$`, Env(map[string]string{
					"TASKRUNNER_HELPER_EXE": executable, "TASKRUNNER_SHELL_HELPER": mode,
					"TASKRUNNER_READY": ready, "TASKRUNNER_STOPPED": stopped,
				}), Stdout(io.Discard), Stderr(&stderr), GracefulCancellation(500*time.Millisecond))
			}()
			waitForFile(t, ready)
			process := fixtureProcess(t, ready)
			if mode == "pipe" || mode == "orphan" {
				waitForFile(t, ready+".child")
				fixtureProcess(t, ready+".child")
			}
			if mode != "orphan" {
				cancel()
			}
			select {
			case err := <-done:
				if mode == "orphan" {
					code, ok := interp.IsExitStatus(err)
					assert.True(t, ok, "orphaned pipe must be an exit status, got %v", err)
					assert.Equal(t, uint8(1), code)
					assert.Contains(t, stderr.String(), "output still open 500ms after exit")
				} else {
					assert.ErrorIs(t, err, context.Canceled)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("cancellation did not finish within its bound")
			}
			assert.Error(t, process.Signal(syscall.Signal(0)), "immediate process must be reaped")
			if mode == "graceful" || mode == "pipe" {
				_, err := os.Stat(stopped)
				assert.NoError(t, err, "graceful cleanup must finish")
			}
		})
	}
}

func TestRunCancellationShellSemantics(t *testing.T) {
	if runtime.GOOS == "windows" || runtime.GOOS == "plan9" {
		t.Skip("POSIX shell fixtures")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, os.Mkdir(filepath.Join(dir, "bin"), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bin", "fixture"), []byte("#!/bin/sh\nprintf found"), 0700))
	cases := []struct {
		name, command, want string
		status              uint8
	}{
		{name: "pipeline", command: `printf abc | tr a-z A-Z`, want: "ABC"},
		{name: "stdin", command: `cat`, want: "input"},
		{name: "environment overlays", command: `export VALUE=inner; unset DROP; LOCAL=private; sh -c 'printf "%s:%s:%s" "$VALUE" "${DROP-missing}" "${LOCAL-missing}"'`, want: "inner:missing:missing"},
		{name: "temporary environment", command: `VALUE=temporary sh -c 'printf %s "$VALUE"'`, want: "temporary"},
		{name: "directory", command: `sh -c 'printf %s "$PWD"'`, want: dir},
		{name: "relative PATH", command: `PATH=bin fixture`, want: "found"},
		{name: "exit status", command: `sh -c 'exit 23'`, status: 23},
		{name: "signal status", command: `sh -c 'kill -TERM $$'`, status: 143},
		{name: "missing command", command: `taskrunner-command-that-does-not-exist`, status: 127},
		{name: "redirect", command: `sh -c 'printf redirected' > output; cat output`, want: "redirected"},
		{name: "background output holder", command: `sh -c 'sleep 2 &'; printf after`, want: "after"},
		{name: "background output holder after failure", command: `sh -c 'sleep 2 & exit 3'`, status: 3},
	}
	for _, policy := range []string{"legacy", "graceful"} {
		for _, tc := range cases {
			t.Run(policy+"/"+tc.name, func(t *testing.T) {
				var out bytes.Buffer
				opts := []RunOption{Dir(dir), Stdout(&out), Stderr(io.Discard), Stdin(strings.NewReader("input")), Env(map[string]string{"VALUE": "outer", "DROP": "inherited", "PRIVATE": "inherited"})}
				if policy == "graceful" {
					opts = append(opts, GracefulCancellation(time.Second))
				}
				err := Run(context.Background(), tc.command, opts...)
				if tc.status == 0 {
					assert.NoError(t, err)
				} else {
					code, ok := interp.IsExitStatus(err)
					assert.True(t, ok)
					assert.Equal(t, tc.status, code)
				}
				assert.Equal(t, tc.want, out.String())
			})
		}
	}
}

func TestRunCancellationBeforeStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	marker := filepath.Join(t.TempDir(), "unexpected")
	assert.ErrorIs(t, Run(ctx, `echo started > "$MARKER"`, Env(map[string]string{"MARKER": marker}), GracefulCancellation(time.Second)), context.Canceled)
	_, err := os.Stat(marker)
	assert.True(t, os.IsNotExist(err))
	for _, grace := range []time.Duration{0, -time.Second} {
		assert.ErrorContains(t, Run(context.Background(), "echo unexpected", GracefulCancellation(grace)), "must be positive")
	}
}

func TestRunPnpmCancellation(t *testing.T) {
	if os.Getenv("TASKRUNNER_TEST_PNPM") == "" {
		t.Skip("set TASKRUNNER_TEST_PNPM=1 with pnpm 11.27.1 and Node installed")
	}
	pnpm, err := exec.LookPath("pnpm")
	require.NoError(t, err)
	version, err := exec.Command(pnpm, "--version").Output()
	require.NoError(t, err)
	require.Equal(t, "11.27.1", strings.TrimSpace(string(version)))
	for _, command := range []string{`pnpm run dev`, `pnpm run wrapped`, `pnpm exec node child.cjs`} {
		t.Run(command, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"taskrunner-signal-fixture","private":true,"scripts":{"dev":"node child.cjs","wrapped":"sh wrapper.sh"}}`), 0600))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "child.cjs"), []byte(`const fs=require('fs');setTimeout(()=>process.exit(90),20000);process.on('SIGTERM',()=>setTimeout(()=>{fs.writeFileSync('stopped','yes');process.exit(0)},100));setInterval(()=>{},1000);fs.writeFileSync('ready',String(process.pid));`), 0600))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "wrapper.sh"), []byte(`trap 'kill -TERM "$child"; wait "$child"; exit' TERM
node child.cjs &
child=$!
wait "$child"
`), 0600))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				done <- Run(ctx, command, Dir(dir), Stdout(io.Discard), Stderr(io.Discard), GracefulCancellation(2*time.Second))
			}()
			waitForFile(t, filepath.Join(dir, "ready"))
			child := fixtureProcess(t, filepath.Join(dir, "ready"))
			cancel()
			select {
			case err := <-done:
				assert.ErrorIs(t, err, context.Canceled)
			case <-time.After(4 * time.Second):
				t.Fatal("pnpm cancellation hung")
			}
			t.Run("descendant_cleanup", func(t *testing.T) {
				_, err := os.Stat(filepath.Join(dir, "stopped"))
				if runtime.GOOS == "linux" && os.Getenv("TASKRUNNER_TEST_PNPM_TERMINAL") == "1" && strings.HasPrefix(command, "pnpm run ") {
					t.Skipf("known Linux terminal lifecycle limitation: PID-only SIGTERM may not reach descendants; cleanup marker error=%v, child still present=%t; cancellation bound remains required", err, child.Signal(syscall.Signal(0)) == nil)
				}
				assert.NoError(t, err)
				require.Eventually(t, func() bool { return child.Signal(syscall.Signal(0)) != nil }, time.Second, 10*time.Millisecond, "pnpm child still alive")
			})
		})
	}
}
