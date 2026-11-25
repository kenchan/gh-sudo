package sudo

import (
	"context"
	"regexp"
	"strconv"
)

// gh 2.103.0 added --no-refresh to gh auth status and gh auth token, together with the token refresh it turns off.
var noRefreshSince = [3]int{2, 103, 0}

var ghVersionPattern = regexp.MustCompile(`^gh version (\d+)\.(\d+)\.(\d+)`)

func (r *Runner) noRefreshArgs(ctx context.Context) []string {
	out, err := r.GH.Output(ctx, nil, "--version")
	if err == nil && !supportsNoRefresh(out) {
		return nil
	}
	return []string{"--no-refresh"}
}

func supportsNoRefresh(versionOutput []byte) bool {
	m := ghVersionPattern.FindSubmatch(versionOutput)
	if m == nil {
		// Not dropping the flag for an unknown version (such as a source build): a gh that refreshes tokens would rewrite
		// the saved original token behind gh sudo's back.
		return true
	}
	for i, min := range noRefreshSince {
		n, _ := strconv.Atoi(string(m[i+1]))
		if n != min {
			return n > min
		}
	}
	return true
}
