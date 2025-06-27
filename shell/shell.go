package shell

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// runConfig collects options prior to constructing the sh Runner.
type runConfig struct {
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
	env    map[string]string
	dir    string
}

// RunOption configures execution of a shell command.
type RunOption func(*runConfig)

type ShellRun func(ctx context.Context, command string, options ...RunOption) error

// Stdout sets the output location for the stdout of a command.
func Stdout(writer io.Writer) RunOption {
	return func(c *runConfig) {
		c.stdout = writer
	}
}

// Stderr sets the output location for the stderr of a command.
func Stderr(writer io.Writer) RunOption {
	return func(c *runConfig) {
		c.stderr = writer
	}
}

// Stdin pipes in the contents provided by the reader to the command.
func Stdin(reader io.Reader) RunOption {
	return func(c *runConfig) {
		c.stdin = reader
	}
}

// Env sets the environment variables for the command.
func Env(vars map[string]string) RunOption {
	return func(c *runConfig) {
		if c.env == nil {
			c.env = make(map[string]string)
		}
		for k, v := range vars {
			c.env[k] = v
		}
	}
}

// Dir sets the working directory for the command.
func Dir(path string) RunOption {
	return func(c *runConfig) {
		c.dir = path
	}
}

// Run executes a shell command using mvdan.cc/sh's interpreter.
func Run(ctx context.Context, command string, opts ...RunOption) error {
	// Parse the program
	p, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		return err
	}

	// Build configuration from provided options
	cfg := &runConfig{}
	for _, opt := range opts {
		opt(cfg)
	}

	// Default std streams
	if cfg.stdin == nil {
		cfg.stdin = os.Stdin
	}
	if cfg.stdout == nil {
		cfg.stdout = os.Stdout
	}
	if cfg.stderr == nil {
		cfg.stderr = os.Stderr
	}

	runnerOpts := []interp.RunnerOption{
		interp.StdIO(cfg.stdin, cfg.stdout, cfg.stderr),
	}

	// Build environment pairs
	envPairs := os.Environ()
	for k, v := range cfg.env {
		envPairs = append(envPairs, fmt.Sprintf("%s=%s", k, v))
	}
	runnerOpts = append(runnerOpts, interp.Env(expand.ListEnviron(envPairs...)))

	if cfg.dir != "" {
		runnerOpts = append(runnerOpts, interp.Dir(cfg.dir))
	}

	// Construct the runner
	r, err := interp.New(runnerOpts...)
	if err != nil {
		return fmt.Errorf("failed to set up interpreter: %w", err)
	}

	// Execute the parsed program
	return r.Run(ctx, p)
}
