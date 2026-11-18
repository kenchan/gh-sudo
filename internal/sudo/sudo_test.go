package sudo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const (
	host       = "github.com"
	user       = "octocat"
	origToken  = "gho_ORIGINAL_SECRET_TOKEN"
	elevToken  = "gho_ELEVATED_SECRET_TOKEN"
	otherUser  = "hubot"
	otherToken = "gho_OTHER_SECRET_TOKEN"
)

// fakeGH models the parts of gh that gh sudo relies on: per-user keyring slots, the active account, refresh and
// login --with-token.
type fakeGH struct {
	mu          sync.Mutex
	tokens      map[string]string
	owners      map[string]string // token -> user, as GET /user would answer
	active      string
	source      string
	refreshable bool

	refreshCode   int
	refreshErr    error
	loginFailures int // number of login --with-token calls that fail before one succeeds
	loginIgnores  bool
	command       func(ctx context.Context, forward <-chan os.Signal, args []string) (int, error)

	scopes      string
	refreshEnv  []string
	calls       [][]string
	stdins      [][]byte
	commandSeen string // token in the keyring while the wrapped command ran
}

func newFakeGH() *fakeGH {
	return &fakeGH{
		tokens: map[string]string{user: origToken, otherUser: otherToken},
		owners: map[string]string{origToken: user, elevToken: user, otherToken: otherUser},
		active: user,
		source: "keyring",
		scopes: "gist, read:org, repo",
	}
}

func (f *fakeGH) record(args []string, stdin []byte) {
	f.calls = append(f.calls, slices.Clone(args))
	f.stdins = append(f.stdins, slices.Clone(stdin))
}

func (f *fakeGH) Output(_ context.Context, stdin []byte, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(args, stdin)
	switch {
	case slices.Equal(args[:2], []string{"auth", "status"}):
		var accts []account
		for _, u := range []string{user, otherUser} {
			accts = append(accts, account{State: "success", Active: f.active == u, Login: u, Scopes: f.scopes, TokenSource: f.source, Refreshable: f.refreshable && u == user})
		}
		return json.Marshal(authStatus{Hosts: map[string][]account{host: accts}})
	case slices.Equal(args[:2], []string{"auth", "token"}):
		u := args[slices.Index(args, "-u")+1]
		return []byte(f.tokens[u] + "\n"), nil
	case slices.Equal(args[:2], []string{"auth", "login"}):
		if !slices.Contains(args, "--with-token") {
			return nil, errors.New("unexpected login")
		}
		if f.loginFailures > 0 {
			f.loginFailures--
			return nil, errors.New("network down")
		}
		if f.loginIgnores {
			return nil, nil
		}
		tok := strings.TrimSpace(string(stdin))
		u, ok := f.owners[tok]
		if !ok {
			return nil, errors.New("bad token")
		}
		f.tokens[u] = tok
		f.active = u
		return nil, nil
	}
	return nil, fmt.Errorf("unexpected gh call %v", args)
}

func (f *fakeGH) Interactive(ctx context.Context, forward <-chan os.Signal, env []string, args ...string) (int, error) {
	f.mu.Lock()
	f.record(args, nil)
	if slices.Equal(args[:2], []string{"auth", "refresh"}) {
		f.refreshEnv = env
		defer f.mu.Unlock()
		if f.refreshErr != nil || f.refreshCode != 0 {
			return f.refreshCode, f.refreshErr
		}
		f.tokens[f.active] = elevToken
		return 0, nil
	}
	f.commandSeen = f.tokens[f.active]
	cmd := f.command
	f.mu.Unlock()
	if cmd == nil {
		return 0, nil
	}
	return cmd(ctx, forward, args)
}

func (f *fakeGH) assertRestored(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tokens[user] != origToken {
		t.Errorf("keyring holds %q for %s, want the original token", f.tokens[user], user)
	}
	if f.active != user {
		t.Errorf("active account is %q, want %q", f.active, user)
	}
}

func (f *fakeGH) assertNoTokenInArgv(t *testing.T) {
	t.Helper()
	for _, c := range f.calls {
		joined := strings.Join(c, " ")
		for _, tok := range []string{origToken, elevToken} {
			if strings.Contains(joined, tok) {
				t.Errorf("token found in argv: %v", c)
			}
		}
	}
}

func (f *fakeGH) loginCalls() int {
	n := 0
	for _, c := range f.calls {
		if slices.Equal(c[:2], []string{"auth", "login"}) {
			n++
		}
	}
	return n
}

type harness struct {
	gh      *fakeGH
	stderr  *bytes.Buffer
	signals chan os.Signal
	env     map[string]string
	locked  bool
	runner  *Runner
}

func newHarness() *harness {
	h := &harness{
		gh:      newFakeGH(),
		stderr:  &bytes.Buffer{},
		signals: make(chan os.Signal, 4),
		env:     map[string]string{},
	}
	h.runner = &Runner{
		GH:      h.gh,
		Stderr:  h.stderr,
		Signals: h.signals,
		Lookenv: func(k string) (string, bool) { v, ok := h.env[k]; return v, ok },
		Lock: func(string) (func(), error) {
			if h.locked {
				return nil, errors.New("another gh sudo is already running for github.com")
			}
			h.locked = true
			return func() { h.locked = false }, nil
		},
		Sleep:           func(time.Duration) {},
		RestoreAttempts: 3,
	}
	return h
}

func defaultOpts() Options {
	return Options{Scopes: []string{"admin:org"}, Host: host, Args: []string{"api", "user"}}
}

func (h *harness) assertNoTokenInOutput(t *testing.T) {
	t.Helper()
	for _, tok := range []string{origToken, elevToken} {
		if strings.Contains(h.stderr.String(), tok) {
			t.Errorf("token found in stderr: %s", h.stderr.String())
		}
	}
}

func TestRunElevatesAndRestores(t *testing.T) {
	h := newHarness()

	code := h.runner.Run(defaultOpts())

	if code != 0 {
		t.Fatalf("exit code = %d, stderr: %s", code, h.stderr)
	}
	if h.gh.commandSeen != elevToken {
		t.Errorf("command ran with %q, want the elevated token", h.gh.commandSeen)
	}
	h.gh.assertRestored(t)
	h.gh.assertNoTokenInArgv(t)
	h.assertNoTokenInOutput(t)
	if h.locked {
		t.Error("lock was not released")
	}
	if !slices.ContainsFunc(h.gh.calls, func(c []string) bool {
		return slices.Equal(c, []string{"auth", "refresh", "-h", host, "-s", "admin:org"})
	}) {
		t.Errorf("refresh was not called as expected: %v", h.gh.calls)
	}
}

func TestRunRefreshesWithoutPrompts(t *testing.T) {
	h := newHarness()
	h.runner.Run(defaultOpts())

	if !slices.Contains(h.gh.refreshEnv, "GH_PROMPT_DISABLED=1") {
		t.Errorf("refresh env = %v, want GH_PROMPT_DISABLED=1", h.gh.refreshEnv)
	}
}

func TestRunSkipsRefreshWhenScopesArePresent(t *testing.T) {
	h := newHarness()
	h.gh.scopes = "admin:org, gist, read:org, repo"
	opts := defaultOpts()
	opts.Scopes = []string{"admin:org", "repo"}

	if code := h.runner.Run(opts); code != 0 {
		t.Fatalf("exit code = %d, stderr: %s", code, h.stderr)
	}
	if slices.ContainsFunc(h.gh.calls, func(c []string) bool { return len(c) > 1 && c[1] == "refresh" }) {
		t.Error("refresh ran although all scopes were present")
	}
	if h.gh.commandSeen != origToken {
		t.Error("command did not run with the original token")
	}
	if n := h.gh.loginCalls(); n != 0 {
		t.Errorf("login was called %d times, want 0", n)
	}
}

func TestRunPassesTokenOnlyOnStdin(t *testing.T) {
	h := newHarness()
	h.runner.Run(defaultOpts())

	for i, c := range h.gh.calls {
		if slices.Equal(c[:2], []string{"auth", "login"}) {
			if got := strings.TrimSpace(string(h.gh.stdins[i])); got != origToken {
				t.Errorf("login stdin = %q, want the original token", got)
			}
			return
		}
	}
	t.Fatal("login --with-token was not called")
}

func TestRunReturnsCommandExitCodeAndRestores(t *testing.T) {
	h := newHarness()
	h.gh.command = func(context.Context, <-chan os.Signal, []string) (int, error) { return 3, nil }

	if code := h.runner.Run(defaultOpts()); code != 3 {
		t.Errorf("exit code = %d, want 3", code)
	}
	h.gh.assertRestored(t)
}

func TestRunRestoresWhenCommandCannotStart(t *testing.T) {
	h := newHarness()
	h.gh.command = func(context.Context, <-chan os.Signal, []string) (int, error) { return 1, errors.New("exec failed") }

	if code := h.runner.Run(defaultOpts()); code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	h.gh.assertRestored(t)
}

func TestRunRestoresWhenRefreshFails(t *testing.T) {
	for name, setup := range map[string]func(*fakeGH){
		"non-zero exit": func(f *fakeGH) { f.refreshCode = 1 },
		"error":         func(f *fakeGH) { f.refreshErr = errors.New("boom") },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness()
			setup(h.gh)

			if code := h.runner.Run(defaultOpts()); code == 0 {
				t.Error("exit code = 0, want non-zero")
			}
			h.gh.assertRestored(t)
			if slices.ContainsFunc(h.gh.calls, func(c []string) bool { return c[0] == "api" }) {
				t.Error("command ran although refresh failed")
			}
			// The keyring still holds the original token, so nothing needs to be written.
			if n := h.gh.loginCalls(); n != 0 {
				t.Errorf("login was called %d times, want 0", n)
			}
		})
	}
}

func TestRunRestoresOnPanic(t *testing.T) {
	h := newHarness()
	h.gh.command = func(context.Context, <-chan os.Signal, []string) (int, error) { panic("kaboom") }

	func() {
		defer func() {
			if p := recover(); p != "kaboom" {
				t.Errorf("recovered %v, want the original panic", p)
			}
		}()
		h.runner.Run(defaultOpts())
	}()

	h.gh.assertRestored(t)
}

func TestRunRestoresOnSignalDuringCommand(t *testing.T) {
	for _, sig := range []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) {
			h := newHarness()
			forwarded := make(chan os.Signal, 1)
			h.gh.command = func(_ context.Context, forward <-chan os.Signal, _ []string) (int, error) {
				h.signals <- sig
				if sig == os.Interrupt {
					// The terminal delivers SIGINT to the child directly; nothing is forwarded.
					select {
					case s := <-forward:
						t.Errorf("SIGINT was forwarded as %v", s)
					case <-time.After(50 * time.Millisecond):
					}
					return 130, nil
				}
				select {
				case s := <-forward:
					forwarded <- s
				case <-time.After(time.Second):
					t.Error("signal was not forwarded to the command")
				}
				return 143, nil
			}

			code := h.runner.Run(defaultOpts())

			if code == 0 {
				t.Error("exit code = 0, want non-zero")
			}
			if sig != os.Interrupt {
				if got := <-forwarded; got != sig {
					t.Errorf("forwarded %v, want %v", got, sig)
				}
			}
			h.gh.assertRestored(t)
		})
	}
}

func TestRunStopsBeforeCommandWhenInterruptedDuringRefresh(t *testing.T) {
	h := newHarness()
	h.runner.GH = &interruptingRefresh{fakeGH: h.gh, signals: h.signals}

	if code := h.runner.Run(defaultOpts()); code != 130 {
		t.Errorf("exit code = %d, want 130", code)
	}
	if slices.ContainsFunc(h.gh.calls, func(c []string) bool { return c[0] == "api" }) {
		t.Error("command ran although gh sudo was interrupted")
	}
	h.gh.assertRestored(t)
}

// interruptingRefresh delivers SIGINT while the refresh runs, after the keyring already holds the elevated token.
type interruptingRefresh struct {
	*fakeGH
	signals chan os.Signal
}

func (g *interruptingRefresh) Interactive(ctx context.Context, forward <-chan os.Signal, env []string, args ...string) (int, error) {
	code, err := g.fakeGH.Interactive(ctx, forward, env, args...)
	if args[1] == "refresh" {
		g.signals <- os.Interrupt
		time.Sleep(50 * time.Millisecond)
	}
	return code, err
}

func TestRestoreRetriesTransientFailures(t *testing.T) {
	h := newHarness()
	h.gh.loginFailures = 2

	if code := h.runner.Run(defaultOpts()); code != 0 {
		t.Fatalf("exit code = %d, stderr: %s", code, h.stderr)
	}
	h.gh.assertRestored(t)
	if n := h.gh.loginCalls(); n != 3 {
		t.Errorf("login was called %d times, want 3", n)
	}
}

func TestRestoreFailureIsReported(t *testing.T) {
	for name, setup := range map[string]func(*fakeGH){
		"login keeps failing":   func(f *fakeGH) { f.loginFailures = 10 },
		"verification mismatch": func(f *fakeGH) { f.loginIgnores = true },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness()
			setup(h.gh)

			if code := h.runner.Run(defaultOpts()); code != 1 {
				t.Errorf("exit code = %d, want 1", code)
			}
			if !strings.Contains(h.stderr.String(), "FAILED TO RESTORE") ||
				!strings.Contains(h.stderr.String(), "gh auth login -h github.com") {
				t.Errorf("stderr lacks the warning and recovery steps: %s", h.stderr)
			}
			h.assertNoTokenInOutput(t)
		})
	}
}

func TestRestoreFailureKeepsNonZeroCommandCode(t *testing.T) {
	h := newHarness()
	h.gh.loginFailures = 10
	h.gh.command = func(context.Context, <-chan os.Signal, []string) (int, error) { return 4, nil }

	if code := h.runner.Run(defaultOpts()); code != 4 {
		t.Errorf("exit code = %d, want 4", code)
	}
}

func TestRunRefusesUnsupportedStates(t *testing.T) {
	tests := map[string]struct {
		setup func(*harness)
		opts  func(*Options)
		want  string
	}{
		"GH_TOKEN set":        {setup: func(h *harness) { h.env["GH_TOKEN"] = "x" }, want: "GH_TOKEN is set"},
		"GITHUB_TOKEN set":    {setup: func(h *harness) { h.env["GITHUB_TOKEN"] = "x" }, want: "GITHUB_TOKEN is set"},
		"GH_ENTERPRISE_TOKEN": {setup: func(h *harness) { h.env["GH_ENTERPRISE_TOKEN"] = "x" }, want: "GH_ENTERPRISE_TOKEN is set"},
		"inactive user":       {opts: func(o *Options) { o.User = otherUser }, want: "not the active account"},
		"not keyring":         {setup: func(h *harness) { h.gh.source = "oauth_token" }, want: "only supports tokens stored in the keyring"},
		"refreshable":         {setup: func(h *harness) { h.gh.refreshable = true }, want: "expiring token"},
		"wrapping gh auth":    {opts: func(o *Options) { o.Args = []string{"auth", "logout"} }, want: "cannot wrap gh auth"},
		"already running":     {setup: func(h *harness) { h.locked = true }, want: "already running"},
		"unknown host":        {opts: func(o *Options) { o.Host = "ghe.example.com" }, want: "not logged in to ghe.example.com"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			h := newHarness()
			if tt.setup != nil {
				tt.setup(h)
			}
			opts := defaultOpts()
			if tt.opts != nil {
				tt.opts(&opts)
			}

			if code := h.runner.Run(opts); code == 0 {
				t.Error("exit code = 0, want non-zero")
			}
			if !strings.Contains(h.stderr.String(), tt.want) {
				t.Errorf("stderr = %q, want it to contain %q", h.stderr, tt.want)
			}
			if slices.ContainsFunc(h.gh.calls, func(c []string) bool { return len(c) > 1 && c[1] == "refresh" }) {
				t.Error("refresh ran")
			}
		})
	}
}

func TestRunAcceptsActiveUserFlag(t *testing.T) {
	h := newHarness()
	opts := defaultOpts()
	opts.User = user

	if code := h.runner.Run(opts); code != 0 {
		t.Fatalf("exit code = %d, stderr: %s", code, h.stderr)
	}
	h.gh.assertRestored(t)
}

func TestSecretIsRedacted(t *testing.T) {
	s := secret{value: origToken}
	for _, format := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x"} {
		if got := fmt.Sprintf(format, s); strings.Contains(got, "ORIGINAL") || strings.Contains(got, "4f524947") {
			t.Errorf("Sprintf(%q) leaked the token: %s", format, got)
		}
	}
	if got := fmt.Sprintf("%v", struct{ T secret }{s}); strings.Contains(got, "ORIGINAL") {
		t.Errorf("token leaked through a struct: %s", got)
	}
}
