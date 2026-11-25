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

type fakeGH struct {
	mu          sync.Mutex
	tokens      map[string]string
	tokenOwner  map[string]string
	active      string
	source      string
	refreshable bool
	version     string

	refreshCode         int
	refreshErr          error
	refreshFailsAfterIt bool
	loginFailures       int
	loginIgnores        int
	loginSource         string
	command             func(ctx context.Context, forward <-chan os.Signal, args []string) (int, error)

	scopes             string
	refreshEnv         []string
	commandEnv         []string
	calls              [][]string
	stdins             [][]byte
	tokenSeenByCommand string
}

func newFakeGH() *fakeGH {
	return &fakeGH{
		tokens:     map[string]string{user: origToken, otherUser: otherToken},
		tokenOwner: map[string]string{origToken: user, elevToken: user, otherToken: otherUser},
		active:     user,
		source:     "keyring",
		scopes:     "gist, read:org, repo",
		version:    "gh version 2.105.0 (2026-11-17)\nhttps://github.com/cli/cli/releases/tag/v2.105.0\n",
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
	if slices.Equal(args, []string{"--version"}) {
		return []byte(f.version), nil
	}
	if slices.Contains(args, "--no-refresh") && !supportsNoRefresh([]byte(f.version)) {
		return nil, errors.New("unknown flag: --no-refresh")
	}
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
		if f.loginIgnores > 0 {
			f.loginIgnores--
			return nil, nil
		}
		tok := strings.TrimSpace(string(stdin))
		u, ok := f.tokenOwner[tok]
		if !ok {
			return nil, errors.New("bad token")
		}
		f.tokens[u] = tok
		f.active = u
		if f.loginSource != "" {
			f.source = f.loginSource
		}
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
		if (f.refreshErr != nil || f.refreshCode != 0) && !f.refreshFailsAfterIt {
			return f.refreshCode, f.refreshErr
		}
		f.tokens[f.active] = elevToken
		return f.refreshCode, f.refreshErr
	}
	f.commandEnv = env
	f.tokenSeenByCommand = f.tokens[f.active]
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
		if isAuthCall(c, "login") {
			n++
		}
	}
	return n
}

type harness struct {
	gh          *fakeGH
	stderr      *bytes.Buffer
	signals     chan os.Signal
	env         map[string]string
	locked      bool
	callsAtLock int
	runner      *Runner
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
			h.callsAtLock = len(h.gh.calls)
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
	if h.gh.tokenSeenByCommand != elevToken {
		t.Errorf("command ran with %q, want the elevated token", h.gh.tokenSeenByCommand)
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
	if h.gh.tokenSeenByCommand != origToken {
		t.Error("command did not run with the original token")
	}
	if n := h.gh.loginCalls(); n != 0 {
		t.Errorf("login was called %d times, want 0", n)
	}
}

func TestRunDisablesDebugLoggingForRefresh(t *testing.T) {
	h := newHarness()
	h.runner.Run(defaultOpts())

	if !slices.Contains(h.gh.refreshEnv, "GH_DEBUG=false") {
		t.Errorf("refresh env = %v, want GH_DEBUG=false", h.gh.refreshEnv)
	}
}

func TestRunTargetsHostForCommand(t *testing.T) {
	h := newHarness()
	h.runner.Run(defaultOpts())

	if !slices.Contains(h.gh.commandEnv, "GH_HOST="+host) {
		t.Errorf("command env = %v, want GH_HOST=%s", h.gh.commandEnv, host)
	}
}

func TestRunLocksBeforeInspectingTheAccount(t *testing.T) {
	h := newHarness()
	h.runner.Run(defaultOpts())

	if h.callsAtLock != 0 {
		t.Errorf("gh was called %d times before locking: %v", h.callsAtLock, h.gh.calls[:h.callsAtLock])
	}
}

func TestRunRestoresWhenRefreshFailsAfterStoringTheToken(t *testing.T) {
	h := newHarness()
	h.gh.refreshCode = 1
	h.gh.refreshFailsAfterIt = true

	if code := h.runner.Run(defaultOpts()); code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	h.gh.assertRestored(t)
	if n := h.gh.loginCalls(); n != 1 {
		t.Errorf("login was called %d times, want 1", n)
	}
}

func TestRestoreRetriesWhenVerificationFails(t *testing.T) {
	h := newHarness()
	h.gh.loginIgnores = 1

	if code := h.runner.Run(defaultOpts()); code != 0 {
		t.Fatalf("exit code = %d, stderr: %s", code, h.stderr)
	}
	h.gh.assertRestored(t)
	if n := h.gh.loginCalls(); n != 2 {
		t.Errorf("login was called %d times, want 2", n)
	}
}

func TestRunPassesTokenOnlyOnStdin(t *testing.T) {
	h := newHarness()
	h.runner.Run(defaultOpts())

	for i, c := range h.gh.calls {
		if isAuthCall(c, "login") {
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
				select {
				case s := <-forward:
					forwarded <- s
				case <-time.After(time.Second):
					forwarded <- nil
				}
				return 143, nil
			}

			code := h.runner.Run(defaultOpts())

			if code == 0 {
				t.Error("exit code = 0, want non-zero")
			}
			if got := <-forwarded; got != sig {
				t.Errorf("forwarded %v, want %v", got, sig)
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
		"verification mismatch": func(f *fakeGH) { f.loginIgnores = 10 },
		"plain-text fallback":   func(f *fakeGH) { f.loginSource = "oauth_token" },
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

func TestSupportsNoRefresh(t *testing.T) {
	tests := []struct {
		output string
		want   bool
	}{
		{"gh version 2.102.1 (2026-10-20)\nhttps://github.com/cli/cli/releases/tag/v2.102.1\n", false},
		{"gh version 1.200.0 (2020-01-01)\n", false},
		{"gh version 2.103.0 (2026-10-21)\n", true},
		{"gh version 2.105.0 (2026-11-17)\n", true},
		{"gh version 3.0.0 (2027-01-01)\n", true},
		{"gh version DEV\n", true},
		{"", true},
	}
	for _, tt := range tests {
		if got := supportsNoRefresh([]byte(tt.output)); got != tt.want {
			t.Errorf("supportsNoRefresh(%q) = %v, want %v", tt.output, got, tt.want)
		}
	}
}

func TestRunPassesNoRefreshOnlyToGhThatSupportsIt(t *testing.T) {
	tests := []struct {
		version string
		want    bool
	}{
		{"gh version 2.102.1 (2026-10-20)\n", false},
		{"gh version 2.105.0 (2026-11-17)\n", true},
	}
	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			h := newHarness()
			h.gh.version = tt.version

			if code := h.runner.Run(defaultOpts()); code != 0 {
				t.Fatalf("exit code = %d, stderr: %s", code, h.stderr)
			}
			h.gh.assertRestored(t)
			for _, c := range h.gh.calls {
				if (isAuthCall(c, "status") || isAuthCall(c, "token")) && slices.Contains(c, "--no-refresh") != tt.want {
					t.Errorf("%v: --no-refresh present = %v, want %v", c, !tt.want, tt.want)
				}
			}
		})
	}
}

func isAuthCall(args []string, sub string) bool {
	return len(args) > 1 && args[0] == "auth" && args[1] == sub
}
