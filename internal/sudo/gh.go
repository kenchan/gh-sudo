package sudo

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
)

// GH runs gh. Tokens are passed only through stdin and read only from stdout, never through argv.
type GH interface {
	// Output runs gh non-interactively, writes stdin to it and returns its stdout. Its stderr goes to the user.
	Output(ctx context.Context, stdin []byte, args ...string) ([]byte, error)
	// Interactive runs gh attached to the terminal and returns its exit code. Signals received on forward are
	// delivered to the child while it runs.
	Interactive(ctx context.Context, forward <-chan os.Signal, args ...string) (int, error)
}

// ExecGH runs the gh executable at Path.
type ExecGH struct {
	Path string
}

func (g ExecGH) Output(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, g.Path, args...)
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	return stdout.Bytes(), err
}

func (g ExecGH) Interactive(ctx context.Context, forward <-chan os.Signal, args ...string) (int, error) {
	cmd := exec.CommandContext(ctx, g.Path, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return 1, err
	}

	done := make(chan struct{})
	go func() {
		for {
			select {
			case sig := <-forward:
				_ = cmd.Process.Signal(sig)
			case <-done:
				return
			}
		}
	}()
	err := cmd.Wait()
	close(done)

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if code := exitErr.ExitCode(); code >= 0 {
			return code, nil
		}
		// Killed by a signal; ExitCode reports -1.
		return 1, nil
	}
	if err != nil {
		return 1, err
	}
	return 0, nil
}
