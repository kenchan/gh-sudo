//go:build unix

package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const fakeGH = `#!/bin/sh
D="$FAKE_DIR"
echo "$* GH_DEBUG=$GH_DEBUG" >> "$D/calls"
case "$1 $2" in
"--version ") echo "gh version 2.105.0 (2026-11-17)";;
"auth status")
  printf '{"hosts":{"github.com":[{"state":"success","active":true,"host":"github.com","login":"octo","tokenSource":"keyring","scopes":"gist, read:org, repo","gitProtocol":"https"}]}}\n';;
"auth token") cat "$D/token";;
"auth refresh") echo T1-elevated > "$D/token";;
"auth login")
  sleep 1
  cat > "$D/token"
  echo login-done >> "$D/calls";;
*)
  echo "command-started GH_HOST=$GH_HOST" >> "$D/calls"
  sleep 30 </dev/null >/dev/null 2>&1 &
  trap 'kill $!; exit 143' TERM
  trap 'kill $!; exit 129' HUP
  trap 'kill $!; exit 130' INT
  wait;;
esac
`

var binary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "gh-sudo-e2e")
	if err != nil {
		panic(err)
	}
	binary = filepath.Join(dir, "gh-sudo")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		panic(string(out))
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

type e2e struct {
	t      *testing.T
	dir    string
	cmd    *exec.Cmd
	stderr bytes.Buffer
}

func startE2E(t *testing.T) *e2e {
	t.Helper()
	dir := t.TempDir()
	gh := filepath.Join(dir, "gh")
	if err := os.WriteFile(gh, []byte(fakeGH), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("T0-original\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := &e2e{t: t, dir: dir}
	e.cmd = exec.Command(binary, "-s", "admin:org", "--", "api", "user")
	e.cmd.Env = append(os.Environ(),
		"FAKE_DIR="+dir, "GH_PATH="+gh, "GH_DEBUG=api",
		"HOME="+dir, "XDG_CACHE_HOME="+dir,
		"GH_TOKEN=", "GITHUB_TOKEN=", "GH_ENTERPRISE_TOKEN=", "GITHUB_ENTERPRISE_TOKEN=")
	e.cmd.Stderr = &e.stderr
	e.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := e.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	e.waitFor("command-started")
	return e
}

func (e *e2e) calls() string {
	b, _ := os.ReadFile(filepath.Join(e.dir, "calls"))
	return string(b)
}

func (e *e2e) waitFor(s string) {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(e.calls(), s) {
		if time.Now().After(deadline) {
			e.t.Fatalf("timed out waiting for %q; calls:\n%s", s, e.calls())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Not signalling only the PID for terminal signals: a terminal delivers them to the whole foreground process group.
func (e *e2e) signalGroup(sig syscall.Signal) {
	e.t.Helper()
	if err := syscall.Kill(-e.cmd.Process.Pid, sig); err != nil {
		e.t.Fatal(err)
	}
}

func (e *e2e) wait() int {
	e.t.Helper()
	err := e.cmd.Wait()
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		return exitErr.ExitCode()
	}
	if err != nil {
		e.t.Fatal(err)
	}
	return 0
}

func (e *e2e) assertRestored() {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(e.dir, "token"))
	if err != nil {
		e.t.Fatal(err)
	}
	if got := strings.TrimSpace(string(b)); got != "T0-original" {
		e.t.Errorf("token = %q, want T0-original; stderr:\n%s\ncalls:\n%s", got, e.stderr.String(), e.calls())
	}
	if !strings.Contains(e.stderr.String(), "restored the original token") {
		e.t.Errorf("stderr lacks the restore message:\n%s", e.stderr.String())
	}
}

func TestSignalsRestoreTheOriginalToken(t *testing.T) {
	tests := map[string]struct {
		send func(*e2e)
		want int
	}{
		"SIGINT to the process group": {send: func(e *e2e) { e.signalGroup(syscall.SIGINT) }, want: 130},
		"SIGTERM to the process":      {send: func(e *e2e) { _ = e.cmd.Process.Signal(syscall.SIGTERM) }, want: 143},
		"SIGHUP to the process":       {send: func(e *e2e) { _ = e.cmd.Process.Signal(syscall.SIGHUP) }, want: 129},
		"SIGINT to the process only":  {send: func(e *e2e) { _ = e.cmd.Process.Signal(syscall.SIGINT) }, want: 130},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			e := startE2E(t)
			tt.send(e)
			if code := e.wait(); code != tt.want {
				t.Errorf("exit code = %d, want %d", code, tt.want)
			}
			e.assertRestored()
		})
	}
}

func TestSecondInterruptDoesNotStopTheRestore(t *testing.T) {
	e := startE2E(t)
	e.signalGroup(syscall.SIGINT)
	e.waitFor("auth login")
	e.signalGroup(syscall.SIGINT)
	e.wait()
	e.assertRestored()
	if n := strings.Count(e.calls(), "auth login"); n != 1 || !strings.Contains(e.calls(), "login-done") {
		t.Errorf("the restore was interrupted (%d login attempts); calls:\n%s", n, e.calls())
	}
}

func TestInternalCallsDoNotInheritDebugLogging(t *testing.T) {
	e := startE2E(t)
	e.signalGroup(syscall.SIGINT)
	e.wait()

	for line := range strings.SplitSeq(strings.TrimSpace(e.calls()), "\n") {
		if strings.HasPrefix(line, "auth ") && !strings.HasSuffix(line, "GH_DEBUG=false") {
			t.Errorf("internal call inherited GH_DEBUG: %s", line)
		}
	}
	if !strings.Contains(e.calls(), "command-started GH_HOST=github.com") {
		t.Errorf("command did not get GH_HOST; calls:\n%s", e.calls())
	}
}
