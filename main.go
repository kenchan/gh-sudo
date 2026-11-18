// Command gh-sudo is a gh extension that runs one gh command with temporarily elevated OAuth scopes.
package main

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/cli/go-gh/v2"
	"github.com/cli/go-gh/v2/pkg/auth"

	"github.com/kenchan/gh-sudo/internal/sudo"
)

func main() {
	os.Exit(run())
}

// run owns every deferred action so that os.Exit in main never skips the restore.
func run() int {
	// Catch signals before anything else so that none can kill the process between saving and restoring the token.
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, sudo.CaughtSignals...)
	defer signal.Stop(signals)

	defaultHost, _ := auth.DefaultHost()
	opts, err := sudo.ParseArgs(os.Args[1:], defaultHost, os.Stdout)
	if errors.Is(err, sudo.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "gh sudo: error: %v\nRun 'gh sudo --help' for usage.\n", err)
		return 2
	}
	opts.Host = auth.NormalizeHostname(opts.Host)

	ghPath, err := gh.Path()
	if err != nil {
		fmt.Fprintf(os.Stderr, "gh sudo: error: could not find gh: %v\n", err)
		return 1
	}

	r := &sudo.Runner{
		GH:              sudo.ExecGH{Path: ghPath},
		Stderr:          os.Stderr,
		Signals:         signals,
		Lookenv:         os.LookupEnv,
		Lock:            sudo.LockHost,
		Sleep:           time.Sleep,
		RestoreAttempts: 3,
	}
	return r.Run(opts)
}
