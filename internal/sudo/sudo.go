// Package sudo runs one gh command with temporarily elevated OAuth scopes and always restores the original token.
package sudo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// TokenEnvVars are the environment variables that make gh ignore the keyring.
var TokenEnvVars = []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"}

// Options describe one gh sudo invocation.
type Options struct {
	Scopes    []string
	Host      string
	User      string
	Clipboard bool
	Args      []string
}

// Runner holds the dependencies of Run so that tests can replace them.
type Runner struct {
	GH     GH
	Stderr io.Writer
	// Signals delivers the signals the process caught. Run never returns before restoring because of a signal.
	Signals <-chan os.Signal
	Lookenv func(string) (string, bool)
	Lock    func(host string) (func(), error)
	Sleep   func(time.Duration)
	// RestoreAttempts is the number of times the restore is tried. It needs the network.
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

// Run runs the command and returns the process exit code.
func (r *Runner) Run(opts Options) (code int) {
	ctx := context.Background()

	if err := r.preflight(opts); err != nil {
		r.errorf("%v", err)
		return 1
	}

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

	unlock, err := r.Lock(opts.Host)
	if err != nil {
		r.errorf("%v", err)
		return 1
	}
	defer unlock()

	var interrupted atomic.Bool
	forward := make(chan os.Signal, 1)
	stopSignals := make(chan struct{})
	defer close(stopSignals)
	go func() {
		for {
			select {
			case sig := <-r.Signals:
				interrupted.Store(true)
				// A SIGINT from the terminal already reaches the child through the process group.
				if sig != os.Interrupt {
					select {
					case forward <- sig:
					default:
					}
				}
			case <-stopSignals:
				return
			}
		}
	}()

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

	if missing := missingScopes(acct.Scopes, opts.Scopes); len(missing) == 0 {
		// Refreshing anyway would mint another token with the same scopes as the original, and GitHub keeps only the
		// ten newest tokens per user, app and scope set, so repeated runs could get the original token revoked.
		r.infof("%s on %s already has scopes %s; running the command without refreshing", user, opts.Host, strings.Join(opts.Scopes, ","))
		cmdCode, err := r.GH.Interactive(ctx, forward, nil, opts.Args...)
		if err != nil {
			r.errorf("could not run gh: %v", err)
			return 1
		}
		return cmdCode
	}

	scopes := strings.Join(opts.Scopes, ",")
	r.infof("adding scopes %s to %s on %s for one command", scopes, user, opts.Host)
	refreshArgs := []string{"auth", "refresh", "-h", opts.Host, "-s", scopes}
	if opts.Clipboard {
		refreshArgs = append(refreshArgs, "--clipboard")
	}
	// Without a prompt, gh does not offer to store the new token in a non-gh git credential helper (such as
	// osxkeychain), where the restore could not remove it. The device flow still works; gh prints the URL to open.
	refreshCode, err := r.GH.Interactive(ctx, forward, []string{"GH_PROMPT_DISABLED=1"}, refreshArgs...)
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

	cmdCode, err := r.GH.Interactive(ctx, forward, nil, opts.Args...)
	if err != nil {
		r.errorf("could not run gh: %v", err)
		return 1
	}
	return cmdCode
}

func missingScopes(have string, want []string) []string {
	var current []string
	for s := range strings.SplitSeq(have, ",") {
		current = append(current, strings.TrimSpace(s))
	}
	var missing []string
	for _, s := range want {
		if !slices.Contains(current, s) {
			missing = append(missing, s)
		}
	}
	return missing
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
	// The wrapped command must not change the credentials that gh sudo restores.
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

// restored reports whether the keyring holds original for user and user is the active account.
func (r *Runner) restored(ctx context.Context, host, user string, original secret) (bool, error) {
	current, err := r.token(ctx, host, user)
	if err != nil {
		return false, err
	}
	if !current.equal(original) {
		return false, nil
	}
	acct, err := r.activeAccount(ctx, host)
	if err != nil {
		return false, err
	}
	return acct.Login == user, nil
}

func (r *Runner) restore(host, user string, original secret) error {
	// A fresh context: whatever cancelled the command must not cancel the restore.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if ok, err := r.restored(ctx, host, user, original); err == nil && ok {
		return nil
	}

	attempts := max(r.RestoreAttempts, 1)
	var lastErr error
	for i := range attempts {
		if i > 0 {
			r.Sleep(time.Duration(i) * 2 * time.Second)
		}
		_, err := r.GH.Output(ctx, []byte(original.reveal()+"\n"), "auth", "login", "-h", host, "--with-token")
		if err == nil {
			lastErr = nil
			break
		}
		lastErr = errors.New("gh auth login --with-token failed")
	}
	if lastErr != nil {
		return lastErr
	}

	ok, err := r.restored(ctx, host, user, original)
	if err != nil {
		return fmt.Errorf("could not verify the restore: %w", err)
	}
	if !ok {
		return errors.New("the keyring does not hold the original token after the restore")
	}
	return nil
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

// CaughtSignals are the signals Run catches so that it can restore before exiting.
var CaughtSignals = []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP}
