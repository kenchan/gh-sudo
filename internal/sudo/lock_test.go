package sudo

import (
	"strings"
	"testing"
)

func TestLockHostIsExclusive(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CACHE_HOME", dir)
	t.Setenv("LocalAppData", dir)

	unlock, err := LockHost("github.com")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := LockHost("github.com"); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Errorf("second lock: err = %v, want already running", err)
	}
	other, err := LockHost("ghe.example.com")
	if err != nil {
		t.Errorf("lock for another host: %v", err)
	} else {
		other()
	}

	unlock()
	again, err := LockHost("github.com")
	if err != nil {
		t.Fatalf("lock after unlock: %v", err)
	}
	again()
}
