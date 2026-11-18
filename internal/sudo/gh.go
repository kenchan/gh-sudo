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

// Not inheriting GH_DEBUG: with GH_DEBUG=api, gh logs the full OAuth exchange, including the token in the response body.
var internalEnv = []string{"GH_DEBUG=false"}

type ExecGH struct {
	Path string
}

func (g ExecGH) Output(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, g.Path, args...)
	cmd.Env = append(os.Environ(), internalEnv...)
	// Not in the terminal's process group: a second Ctrl-C must not kill the restore and its verification.
	isolateFromTerminalSignals(cmd)
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

	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		return exitCode(exitErr), nil
	}
	if err != nil {
		return 1, err
	}
	return 0, nil
}
