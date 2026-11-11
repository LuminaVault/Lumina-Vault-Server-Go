package hermescontainers

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
)

// Result is DockerResult.
type Result struct {
	Stdout, Stderr string
	ExitCode       int
}

// OK is DockerResult.ok.
func (r Result) OK() bool { return r.ExitCode == 0 }

// SpawnError is DockerExecError.spawnFailed: the docker binary could not be
// started at all, which is every call in a pod with no docker.
type SpawnError struct{ Err error }

func (e *SpawnError) Error() string { return fmt.Sprintf("spawnFailed(%q)", e.Err.Error()) }
func (e *SpawnError) Unwrap() error { return e.Err }

// NonZeroExitError is DockerExecError.nonZeroExit.
type NonZeroExitError struct {
	Args     []string
	Stderr   string
	ExitCode int
}

func (e *NonZeroExitError) Error() string {
	return fmt.Sprintf("nonZeroExit(args: %q, stderr: %q, exitCode: %d)", e.Args, e.Stderr, e.ExitCode)
}

// Streaming is StreamingExecHandle: a running `docker exec` whose stdout arrives
// line by line.
type Streaming interface {
	Lines() <-chan string
	// Wait returns the exit code once the process ends.
	Wait(ctx context.Context) (int, error)
	Cancel()
}

// Docker is the container layer (P4): the DockerExec operations every Hermes
// container feature goes through. ProcessDocker forks the docker binary as Swift's
// ProcessDockerExec does, so in a pod with no docker each call fails the way
// Swift's does. Tests inject a stub.
type Docker interface {
	Run(ctx context.Context, args []string) (Result, error)
	Exec(ctx context.Context, container string, command []string, stdin []byte) (Result, error)
	ExecStreaming(ctx context.Context, container string, command []string) (Streaming, error)
	IsRunning(ctx context.Context, container string) (bool, error)
	EnsureNetworkExists(ctx context.Context, name string) error
}

// ProcessDocker is ProcessDockerExec.
type ProcessDocker struct {
	BinaryPath string
}

var _ Docker = ProcessDocker{}

func (d ProcessDocker) run(ctx context.Context, args []string, stdin []byte) (Result, error) {
	cmd := exec.CommandContext(ctx, d.BinaryPath, args...) //nolint:gosec // docker.binaryPath is operator config; args are argv, never a shell
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	if err := cmd.Start(); err != nil {
		return Result{}, &SpawnError{Err: err}
	}
	err := cmd.Wait()
	res := Result{Stdout: out.String(), Stderr: errb.String()}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		res.ExitCode = exitErr.ExitCode()
	} else if err != nil {
		return Result{}, err
	}
	return res, nil
}

func (d ProcessDocker) Run(ctx context.Context, args []string) (Result, error) {
	return d.run(ctx, args, nil)
}

func (d ProcessDocker) Exec(ctx context.Context, container string, command []string, stdin []byte) (Result, error) {
	return d.run(ctx, append([]string{"exec", "-i", container}, command...), stdin)
}

func (d ProcessDocker) IsRunning(ctx context.Context, container string) (bool, error) {
	res, err := d.run(ctx, []string{"inspect", "--format={{.State.Running}}", container}, nil)
	if err != nil {
		return false, err
	}
	return res.OK() && strings.TrimSpace(res.Stdout) == "true", nil
}

func (d ProcessDocker) EnsureNetworkExists(ctx context.Context, name string) error {
	inspect, err := d.run(ctx, []string{"network", "inspect", name}, nil)
	if err != nil {
		return err
	}
	if inspect.OK() {
		return nil
	}
	create, err := d.run(ctx, []string{"network", "create", "--driver=bridge", name}, nil)
	if err != nil {
		return err
	}
	if !create.OK() {
		return &NonZeroExitError{Args: []string{"network", "create", name}, Stderr: create.Stderr, ExitCode: create.ExitCode}
	}
	return nil
}

// ExecStreaming is ProcessStreamingHandle.spawn. The process outlives ctx: the
// OAuth flows park it and wait on it from a later request.
func (d ProcessDocker) ExecStreaming(_ context.Context, container string, command []string) (Streaming, error) {
	pctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(pctx, d.BinaryPath, append([]string{"exec", "-i", container}, command...)...) //nolint:gosec // as run: argv, never a shell
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, &SpawnError{Err: err}
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, &SpawnError{Err: err}
	}
	h := &processStream{cancel: cancel, lines: make(chan string, 64), done: make(chan struct{})}
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			// A parked process nobody reads any more must still reach Wait, so a
			// full buffer drops the line instead of blocking.
			select {
			case h.lines <- sc.Text():
			default:
			}
		}
		close(h.lines)
		err := cmd.Wait()
		var exitErr *exec.ExitError
		switch {
		case errors.As(err, &exitErr):
			h.code = exitErr.ExitCode()
		case err != nil:
			h.code, h.err = -1, err
		}
		close(h.done)
	}()
	return h, nil
}

type processStream struct {
	cancel context.CancelFunc
	lines  chan string
	done   chan struct{}
	once   sync.Once
	code   int
	err    error
}

func (p *processStream) Lines() <-chan string { return p.lines }

func (p *processStream) Wait(ctx context.Context) (int, error) {
	select {
	case <-p.done:
		return p.code, p.err
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func (p *processStream) Cancel() { p.once.Do(p.cancel) }
