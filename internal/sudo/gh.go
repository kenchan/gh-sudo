package sudo

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
)

type GH interface {
	Output(ctx context.Context, stdin []byte, args ...string) ([]byte, error)
	Interactive(ctx context.Context, forward <-chan os.Signal, env []string, args ...string) (int, error)
}

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

func (g ExecGH) Interactive(ctx context.Context, forward <-chan os.Signal, env []string, args ...string) (int, error) {
	cmd := exec.CommandContext(ctx, g.Path, args...)
	cmd.Env = append(os.Environ(), env...)
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
		return 1, nil
	}
	if err != nil {
		return 1, err
	}
	return 0, nil
}
