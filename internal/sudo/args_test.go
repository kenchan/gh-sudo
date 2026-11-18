package sudo

import (
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
)

func TestParseArgs(t *testing.T) {
	tests := map[string]struct {
		args []string
		want Options
	}{
		"short flags": {
			args: []string{"-s", "admin:org,delete_repo", "--", "repo", "delete", "o/r"},
			want: Options{Scopes: []string{"admin:org", "delete_repo"}, Host: "github.com", Args: []string{"repo", "delete", "o/r"}},
		},
		"long flags": {
			args: []string{"--scopes", "admin:enterprise", "--hostname", "GHE.example.com", "--user", "octocat", "--clipboard", "--", "api", "-X", "PUT", "/x"},
			want: Options{Scopes: []string{"admin:enterprise"}, Host: "ghe.example.com", User: "octocat", Clipboard: true, Args: []string{"api", "-X", "PUT", "/x"}},
		},
		"gh flags after -- are not parsed": {
			args: []string{"-s", "admin:org", "--", "api", "-h", "x", "--help"},
			want: Options{Scopes: []string{"admin:org"}, Host: "github.com", Args: []string{"api", "-h", "x", "--help"}},
		},
		"spaces in scopes": {
			args: []string{"-s", " admin:org , delete_repo ", "--", "api", "user"},
			want: Options{Scopes: []string{"admin:org", "delete_repo"}, Host: "github.com", Args: []string{"api", "user"}},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := ParseArgs(tt.args, "github.com", io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got.Scopes, tt.want.Scopes) || got.Host != tt.want.Host || got.User != tt.want.User ||
				got.Clipboard != tt.want.Clipboard || !slices.Equal(got.Args, tt.want.Args) {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParseArgsErrors(t *testing.T) {
	tests := map[string]struct {
		args []string
		want string
	}{
		"no scopes":    {args: []string{"--", "api", "user"}, want: "--scopes is required"},
		"empty scopes": {args: []string{"-s", ",", "--", "api", "user"}, want: "--scopes is required"},
		"bad scope":    {args: []string{"-s", "admin:org;rm", "--", "api", "user"}, want: "invalid scope"},
		"no command":   {args: []string{"-s", "admin:org"}, want: "no gh command"},
		"unknown flag": {args: []string{"-x", "--", "api"}, want: "flag provided but not defined"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := ParseArgs(tt.args, "github.com", io.Discard)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestParseArgsHelp(t *testing.T) {
	var out strings.Builder
	_, err := ParseArgs([]string{"--help"}, "github.com", &out)
	if !errors.Is(err, ErrHelp) {
		t.Fatalf("err = %v, want ErrHelp", err)
	}
	if !strings.Contains(out.String(), "USAGE") {
		t.Errorf("help output lacks usage: %s", out.String())
	}
}
