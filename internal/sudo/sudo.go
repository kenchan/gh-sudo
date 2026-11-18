package sudo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

var TokenEnvVars = []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"}

type Options struct {
	Scopes    []string
	Host      string
	User      string
	Clipboard bool
	Args      []string
}

type Runner struct {
	GH              GH
	Stderr          io.Writer
	Signals         <-chan os.Signal
	Lookenv         func(string) (string, bool)
	Lock            func(host string) (func(), error)
	Sleep           func(time.Duration)
	RestoreAttempts int
}

type account struct {
	State       string `json:"state"`
	Scopes      string `json:"scopes"`
	Active      bool   `json:"active"`
	Login       string `json:"login"`
	TokenSource string `json:"tokenSource"`
	Refreshable bool   `json:"refreshable"`
}

type authStatus struct {
	Hosts map[string][]account `json:"hosts"`
}

func (r *Runner) Run(opts Options) (code int) {
	ctx := context.Background()

	if err := r.preflight(opts); err != nil {
		r.errorf("%v", err)
		return 1
	}

	var interrupted atomic.Bool
	forward := make(chan os.Signal, 1)
	stopSignals := make(chan struct{})
	defer close(stopSignals)
	go func() {
		for {
			select {
			case sig := <-r.Signals:
				interrupted.Store(true)
				select {
				case forward <- sig:
				default:
				}
			case <-stopSignals:
				return
			}
		}
	}()

	// Not inspecting the account before locking: another gh sudo could still be running and show its elevated scopes.
	unlock, err := r.Lock(opts.Host)
	if err != nil {
		r.errorf("%v", err)
		return 1
	}
	defer unlock()

	acct, err := r.activeAccount(ctx, opts.Host)
	if err != nil {
		r.errorf("%v", err)
		return 1
	}
	if err := checkAccount(acct, opts); err != nil {
		r.errorf("%v", err)
		return 1
	}
	user := acct.Login

	original, err := r.token(ctx, opts.Host, user)
	if err != nil {
		r.errorf("could not read the current token: %v", err)
		return 1
	}

	defer func() {
		p := recover()
		if err := r.restore(opts.Host, user, original); err != nil {
			r.restoreFailed(opts.Host, user, err)
			if code == 0 {
				code = 1
			}
		} else {
			r.infof("restored the original token for %s on %s", user, opts.Host)
		}
		if p != nil {
			panic(p)
		}
	}()

	if interrupted.Load() {
		return 130
	}

	commandEnv := []string{"GH_HOST=" + opts.Host}
	if len(missingScopes(acct.Scopes, opts.Scopes)) == 0 {
		// Not refreshing anyway: it would mint another token with the same scopes as the original, and GitHub keeps only
		// the ten newest tokens per user, app and scope set, so repeated runs could get the original token revoked.
		r.infof("%s on %s already has scopes %s; running the command without refreshing", user, opts.Host, strings.Join(opts.Scopes, ","))
		return r.runCommand(ctx, forward, commandEnv, opts.Args)
	}

	scopes := strings.Join(opts.Scopes, ",")
	r.infof("adding scopes %s to %s on %s for one command", scopes, user, opts.Host)
	refreshArgs := []string{"auth", "refresh", "-h", opts.Host, "-s", scopes}
	if opts.Clipboard {
		refreshArgs = append(refreshArgs, "--clipboard")
	}
	// Not letting gh prompt: it would offer to store the new token in a non-gh git credential helper (such as
	// osxkeychain), where the restore could not remove it. The device flow still works; gh prints the URL to open.
	refreshEnv := append([]string{"GH_PROMPT_DISABLED=1"}, internalEnv...)
	refreshCode, err := r.GH.Interactive(ctx, forward, refreshEnv, refreshArgs...)
	if err != nil {
		r.errorf("gh auth refresh failed: %v", err)
		return 1
	}
	if refreshCode != 0 {
		r.errorf("gh auth refresh exited with status %d", refreshCode)
		return refreshCode
	}

	if interrupted.Load() {
		return 130
	}

	return r.runCommand(ctx, forward, commandEnv, opts.Args)
}

func (r *Runner) runCommand(ctx context.Context, forward <-chan os.Signal, env, args []string) int {
	code, err := r.GH.Interactive(ctx, forward, env, args...)
	if err != nil {
		r.errorf("could not run gh: %v", err)
		return 1
	}
	return code
}

func (r *Runner) preflight(opts Options) error {
	for _, name := range TokenEnvVars {
		if v, ok := r.Lookenv(name); ok && v != "" {
			return fmt.Errorf("%s is set; gh would use it instead of the keyring, so gh sudo cannot elevate or restore the stored token. Unset it and try again", name)
		}
	}
	if len(opts.Scopes) == 0 {
		return errors.New("no scopes given; use --scopes")
	}
	if len(opts.Args) == 0 {
		return errors.New("no gh command given; put it after --")
	}
	if opts.Args[0] == "auth" {
		return errors.New("gh sudo cannot wrap gh auth commands")
	}
	return nil
}

func (r *Runner) activeAccount(ctx context.Context, host string) (account, error) {
	out, err := r.GH.Output(ctx, nil, "auth", "status", "--json", "hosts", "-h", host, "--no-refresh")
	var st authStatus
	if jsonErr := json.Unmarshal(out, &st); jsonErr != nil {
		if err != nil {
			return account{}, fmt.Errorf("gh auth status failed: %w", err)
		}
		return account{}, fmt.Errorf("could not parse gh auth status: %w", jsonErr)
	}
	for _, a := range st.Hosts[host] {
		if a.Active {
			return a, nil
		}
	}
	return account{}, fmt.Errorf("not logged in to %s; run gh auth login -h %s first", host, host)
}

func checkAccount(a account, opts Options) error {
	if opts.User != "" && opts.User != a.Login {
		return fmt.Errorf("%s is not the active account on %s (active: %s). gh auth refresh only works on the active account; run gh auth switch -h %s -u %s first", opts.User, opts.Host, a.Login, opts.Host, opts.User)
	}
	if a.State != "success" {
		return fmt.Errorf("the token of %s on %s is not valid (state: %s)", a.Login, opts.Host, a.State)
	}
	if a.TokenSource != "keyring" {
		return fmt.Errorf("the token of %s on %s comes from %s; gh sudo only supports tokens stored in the keyring", a.Login, opts.Host, a.TokenSource)
	}
	if a.Refreshable {
		return fmt.Errorf("the token of %s on %s is an expiring token; restoring it with gh auth login --with-token would drop its refresh token, so gh sudo does not support it", a.Login, opts.Host)
	}
	return nil
}

func (r *Runner) token(ctx context.Context, host, user string) (secret, error) {
	out, err := r.GH.Output(ctx, nil, "auth", "token", "-h", host, "-u", user, "--no-refresh")
	if err != nil {
		return secret{}, errors.New("gh auth token failed")
	}
	t := secret{value: string(bytes.TrimSpace(out))}
	if t.isZero() {
		return secret{}, errors.New("gh auth token returned nothing")
	}
	return t, nil
}

func (r *Runner) restored(ctx context.Context, host, user string, original secret) error {
	current, err := r.token(ctx, host, user)
	if err != nil {
		return err
	}
	if !current.equal(original) {
		return errors.New("gh does not hold the original token")
	}
	acct, err := r.activeAccount(ctx, host)
	if err != nil {
		return err
	}
	if acct.Login != user {
		return fmt.Errorf("the active account is %s, not %s", acct.Login, user)
	}
	// Not trusting gh auth token alone: gh auth login falls back to the plain-text config file when the keyring fails.
	if acct.TokenSource != "keyring" {
		return fmt.Errorf("the original token is stored in %s, not in the keyring", acct.TokenSource)
	}
	return nil
}

const restoreAttemptTimeout = 30 * time.Second

func (r *Runner) restore(host, user string, original secret) error {
	// Not the caller's context: whatever cancelled the command must not also cancel the restore.
	check := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), restoreAttemptTimeout)
		defer cancel()
		return r.restored(ctx, host, user, original)
	}
	if check() == nil {
		return nil
	}

	var lastErr error
	for i := range max(r.RestoreAttempts, 1) {
		if i > 0 {
			r.Sleep(time.Duration(i) * 2 * time.Second)
		}
		ctx, cancel := context.WithTimeout(context.Background(), restoreAttemptTimeout)
		_, err := r.GH.Output(ctx, []byte(original.reveal()+"\n"), "auth", "login", "-h", host, "--with-token")
		cancel()
		if err != nil {
			lastErr = errors.New("gh auth login --with-token failed")
			continue
		}
		if lastErr = check(); lastErr == nil {
			return nil
		}
	}
	return lastErr
}

func (r *Runner) restoreFailed(host, user string, err error) {
	_, _ = fmt.Fprintf(r.Stderr, `
!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!
gh sudo: FAILED TO RESTORE THE ORIGINAL TOKEN for %[1]s on %[2]s: %[3]v

The keyring may still hold the token with the elevated scopes, and every gh
command on this machine may use it. Log in again to replace it:

    gh auth login -h %[2]s

Make sure %[1]s is the active account afterwards (gh auth status).
!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!
`, user, host, err)
}

func (r *Runner) infof(format string, a ...any) {
	_, _ = fmt.Fprintf(r.Stderr, "gh sudo: "+format+"\n", a...)
}

func (r *Runner) errorf(format string, a ...any) {
	_, _ = fmt.Fprintf(r.Stderr, "gh sudo: error: "+format+"\n", a...)
}

var CaughtSignals = []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP}
