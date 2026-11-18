package sudo

import (
	"slices"
	"testing"
)

func TestMissingScopes(t *testing.T) {
	tests := map[string]struct {
		have string
		want []string
		miss []string
	}{
		"present":                {have: "gist, read:org, repo", want: []string{"repo"}, miss: nil},
		"absent":                 {have: "gist, read:org, repo", want: []string{"admin:org"}, miss: []string{"admin:org"}},
		"user implies read:user": {have: "user", want: []string{"read:user"}, miss: nil},
		"admin:org implies read:org through write:org": {have: "admin:org", want: []string{"read:org"}, miss: nil},
		"repo implies public_repo":                     {have: "repo", want: []string{"public_repo"}, miss: nil},
		"admin:enterprise implies read:enterprise":     {have: "admin:enterprise", want: []string{"read:enterprise"}, miss: nil},
		"child does not imply parent":                  {have: "read:org", want: []string{"admin:org"}, miss: []string{"admin:org"}},
		"empty":                                        {have: "", want: []string{"repo"}, miss: []string{"repo"}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := missingScopes(tt.have, tt.want); !slices.Equal(got, tt.miss) {
				t.Errorf("missingScopes(%q, %v) = %v, want %v", tt.have, tt.want, got, tt.miss)
			}
		})
	}
}
