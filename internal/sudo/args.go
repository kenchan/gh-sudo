package sudo

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// ErrHelp is returned by ParseArgs when help was requested.
var ErrHelp = flag.ErrHelp

const usage = `Run one gh command with temporarily elevated OAuth scopes.

USAGE
  gh sudo -s <scopes> [flags] -- <gh command>

FLAGS
  -s, --scopes     Comma separated scopes to add for this command (required)
  -h, --hostname   The GitHub host (default: the default host of gh)
  -u, --user       Expected active account; gh sudo fails if it is not active
  -c, --clipboard  Copy the one-time device code to the clipboard
      --help       Show this help

gh sudo adds the scopes with gh auth refresh (one browser approval), runs the
command, and always writes the original token back to the keyring, also when
the command fails or is interrupted.

The elevated token stays valid on GitHub. See
https://github.com/kenchan/gh-sudo#limitations

EXAMPLES
  gh sudo -s admin:org -- api user
  gh sudo -s delete_repo -- repo delete owner/repo
`

var scopePattern = regexp.MustCompile(`^[a-z][a-z_:]*$`)

// ParseArgs parses the arguments after "gh sudo". defaultHost is used when --hostname is not given.
func ParseArgs(args []string, defaultHost string, out io.Writer) (Options, error) {
	fs := flag.NewFlagSet("gh sudo", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var scopes, host string
	var opts Options
	var help bool
	for _, name := range []string{"s", "scopes"} {
		fs.StringVar(&scopes, name, "", "")
	}
	for _, name := range []string{"h", "hostname"} {
		fs.StringVar(&host, name, "", "")
	}
	for _, name := range []string{"u", "user"} {
		fs.StringVar(&opts.User, name, "", "")
	}
	for _, name := range []string{"c", "clipboard"} {
		fs.BoolVar(&opts.Clipboard, name, false, "")
	}
	fs.BoolVar(&help, "help", false, "")

	if err := fs.Parse(args); err != nil {
		return Options{}, err
	}
	if help {
		_, _ = io.WriteString(out, usage)
		return Options{}, ErrHelp
	}

	for s := range strings.SplitSeq(scopes, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if !scopePattern.MatchString(s) {
			return Options{}, fmt.Errorf("invalid scope %q", s)
		}
		opts.Scopes = append(opts.Scopes, s)
	}
	if len(opts.Scopes) == 0 {
		return Options{}, errors.New("--scopes is required")
	}

	opts.Host = host
	if opts.Host == "" {
		opts.Host = defaultHost
	}
	opts.Host = strings.ToLower(opts.Host)

	opts.Args = fs.Args()
	if len(opts.Args) == 0 {
		return Options{}, errors.New("no gh command given; put it after --")
	}
	return opts, nil
}
