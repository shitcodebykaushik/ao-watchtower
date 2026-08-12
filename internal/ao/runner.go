// Package ao adapts the public AO executable without using a shell.
package ao

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// ProcessRunner is injectable so command construction and failure behavior need no live AO daemon.
type ProcessRunner interface {
	Run(context.Context, string, []string, io.Writer, io.Writer) error
}

type execProcessRunner struct{}

func (execProcessRunner) Run(ctx context.Context, executable string, args []string, stdout, stderr io.Writer) error {
	command := exec.CommandContext(ctx, executable, args...)
	command.Stdout = stdout
	command.Stderr = stderr
	return command.Run()
}

// CommandResult retains bounded command output for auditing and decoding.
type CommandResult struct {
	Stdout          []byte
	Stderr          []byte
	StdoutTruncated bool
	StderrTruncated bool
}

// ErrorKind distinguishes actionable runner failures without inspecting error strings.
type ErrorKind string

const (
	ErrorTimeout     ErrorKind = "timeout"
	ErrorExecution   ErrorKind = "execution"
	ErrorOutputLimit ErrorKind = "output_limit"
)

// CommandError is a typed error that never includes unbounded process output.
type CommandError struct {
	Kind       ErrorKind
	Executable string
	Args       []string
	Result     CommandResult
	Cause      error
}

func (e *CommandError) Error() string {
	return fmt.Sprintf("AO command %q failed: %s", e.Executable, e.Kind)
}
func (e *CommandError) Unwrap() error { return e.Cause }

// Runner executes one executable plus discrete argv, always with bounded output.
type Runner struct {
	Executable  string
	Timeout     time.Duration
	OutputLimit int
	process     ProcessRunner
}

// NewRunner validates runner settings and uses os/exec only through argv-based execution.
func NewRunner(executable string, timeout time.Duration, outputLimit int, process ProcessRunner) (*Runner, error) {
	if strings.TrimSpace(executable) == "" || strings.TrimSpace(executable) != executable {
		return nil, fmt.Errorf("AO executable is required")
	}
	if timeout <= 0 {
		return nil, fmt.Errorf("AO timeout must be positive")
	}
	if outputLimit <= 0 {
		return nil, fmt.Errorf("AO output limit must be positive")
	}
	if process == nil {
		process = execProcessRunner{}
	}
	return &Runner{Executable: executable, Timeout: timeout, OutputLimit: outputLimit, process: process}, nil
}

func (r *Runner) Run(ctx context.Context, args ...string) (CommandResult, error) {
	if r == nil {
		return CommandResult{}, fmt.Errorf("AO runner is nil")
	}
	ctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()

	stdout := newBoundedBuffer(r.OutputLimit)
	stderr := newBoundedBuffer(r.OutputLimit)
	err := r.process.Run(ctx, r.Executable, append([]string(nil), args...), stdout, stderr)
	result := CommandResult{stdout.Bytes(), stderr.Bytes(), stdout.truncated, stderr.truncated}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return result, &CommandError{Kind: ErrorTimeout, Executable: r.Executable, Args: append([]string(nil), args...), Result: result, Cause: ctx.Err()}
	}
	if err != nil {
		return result, &CommandError{Kind: ErrorExecution, Executable: r.Executable, Args: append([]string(nil), args...), Result: result, Cause: err}
	}
	if result.StdoutTruncated || result.StderrTruncated {
		return result, &CommandError{Kind: ErrorOutputLimit, Executable: r.Executable, Args: append([]string(nil), args...), Result: result}
	}
	return result, nil
}

type boundedBuffer struct {
	data      []byte
	limit     int
	truncated bool
}

func newBoundedBuffer(limit int) *boundedBuffer { return &boundedBuffer{limit: limit} }

func (b *boundedBuffer) Write(data []byte) (int, error) {
	remaining := b.limit - len(b.data)
	if remaining <= 0 {
		b.truncated = true
		return len(data), nil
	}
	if len(data) > remaining {
		b.data = append(b.data, data[:remaining]...)
		b.truncated = true
		return len(data), nil
	}
	b.data = append(b.data, data...)
	return len(data), nil
}

func (b *boundedBuffer) Bytes() []byte { return append([]byte(nil), b.data...) }
