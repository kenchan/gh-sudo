package sudo

import (
	"slices"
	"strings"
)

var impliedScopes = map[string][]string{
	"repo":                      {"repo:status", "repo_deployment", "public_repo", "repo:invite", "security_events"},
	"admin:repo_hook":           {"write:repo_hook"},
	"write:repo_hook":           {"read:repo_hook"},
	"admin:org":                 {"write:org", "manage_runners:org"},
	"write:org":                 {"read:org"},
	"admin:public_key":          {"write:public_key"},
	"write:public_key":          {"read:public_key"},
	"user":                      {"read:user", "user:email", "user:follow"},
	"project":                   {"read:project"},
	"write:packages":            {"read:packages"},
	"admin:gpg_key":             {"write:gpg_key"},
	"write:gpg_key":             {"read:gpg_key"},
	"admin:ssh_signing_key":     {"write:ssh_signing_key"},
	"write:ssh_signing_key":     {"read:ssh_signing_key"},
	"admin:enterprise":          {"manage_runners:enterprise", "manage_billing:enterprise", "read:enterprise"},
	"manage_billing:enterprise": {"read:enterprise"},
	"codespace":                 {"codespace:secrets"},
	"copilot":                   {"manage_billing:copilot"},
	"audit_log":                 {"read:audit_log"},
}

func expandScopes(scopes []string) []string {
	var out []string
	queue := slices.Clone(scopes)
	for len(queue) > 0 {
		s := queue[0]
		queue = queue[1:]
		if slices.Contains(out, s) {
			continue
		}
		out = append(out, s)
		queue = append(queue, impliedScopes[s]...)
	}
	return out
}

func missingScopes(have string, want []string) []string {
	var current []string
	for s := range strings.SplitSeq(have, ",") {
		if s = strings.TrimSpace(s); s != "" {
			current = append(current, s)
		}
	}
	current = expandScopes(current)
	var missing []string
	for _, s := range want {
		if !slices.Contains(current, s) {
			missing = append(missing, s)
		}
	}
	return missing
}
