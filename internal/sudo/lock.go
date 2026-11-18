package sudo

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var errLocked = errors.New("locked")

// LockHost takes an exclusive, non-blocking lock for host. The lock is released when the returned function is called
// or the process exits. The lock file holds no secret.
func LockHost(host string) (func(), error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return nil, err
	}
	dir = filepath.Join(dir, "gh-sudo")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	name := strings.NewReplacer("/", "_", `\`, "_", ":", "_").Replace(host) + ".lock"
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockFile(f); err != nil {
		_ = f.Close()
		if errors.Is(err, errLocked) {
			return nil, fmt.Errorf("another gh sudo is already running for %s", host)
		}
		return nil, err
	}
	return func() { _ = f.Close() }, nil
}
