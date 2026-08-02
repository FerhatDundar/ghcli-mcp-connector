package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestIsTruthy(t *testing.T) {
	cases := map[string]bool{
		"true": true, "True": true, "1": true, "yes": true, "on": true,
		"": false, "false": false, "0": false, "nope": false, "  ": false,
		" TRUE ": true,
	}
	for in, want := range cases {
		if got := isTruthy(in); got != want {
			t.Errorf("isTruthy(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestIsMutatingCommand(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{[]string{"repo", "view", "cli/cli"}, false},
		{[]string{"issue", "list", "--repo", "owner/repo"}, false},
		{[]string{"pr", "list"}, false},
		{[]string{"repo", "clone", "cli/cli"}, false},
		{[]string{"pr", "checkout", "123"}, false},
		{[]string{"release", "download", "v1.0.0"}, false},
		{[]string{"run", "watch", "123"}, false},
		{[]string{"issue", "create", "--title", "x"}, true},
		{[]string{"pr", "merge", "123"}, true},
		{[]string{"repo", "delete", "owner/repo"}, true},
		{[]string{"issue", "close", "5"}, true},
		{[]string{"secret", "set", "FOO"}, true},
		{[]string{"api", "repos/owner/repo/issues"}, false},
		{[]string{"api", "repos/owner/repo/issues", "--method", "POST"}, true},
		{[]string{"api", "-X", "DELETE", "repos/owner/repo/issues/1"}, true},
		{[]string{"api", "repos/owner/repo/issues", "--method=GET"}, false},
		{[]string{"api", "graphql", "-f", "query=x"}, false},
	}
	for _, c := range cases {
		if got := isMutatingCommand(c.args); got != c.want {
			t.Errorf("isMutatingCommand(%v) = %v, want %v", c.args, got, c.want)
		}
	}
}

func TestRepoFlagValue(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"issue", "list"}, ""},
		{[]string{"issue", "list", "-R", "owner/repo"}, "owner/repo"},
		{[]string{"issue", "list", "--repo", "owner/repo"}, "owner/repo"},
		{[]string{"issue", "list", "--repo=owner/repo"}, "owner/repo"},
	}
	for _, c := range cases {
		if got := repoFlagValue(c.args); got != c.want {
			t.Errorf("repoFlagValue(%v) = %q, want %q", c.args, got, c.want)
		}
	}
}

func TestFriendlyGHErrorNil(t *testing.T) {
	if err := friendlyGHError("do a thing", "", nil); err != nil {
		t.Errorf("expected nil error to stay nil, got %v", err)
	}
}

func TestFriendlyGHErrorKnownPatterns(t *testing.T) {
	cases := []struct {
		stderr string
		want   string
	}{
		{"gh auth login", "not authenticated"},
		{"HTTP 404: Not Found", "not found"},
		{"HTTP 403: Forbidden", "access denied"},
		{"no git remotes found", "no repository context"},
	}
	for _, c := range cases {
		err := friendlyGHError("run a thing", c.stderr, errTest("boom"))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("friendlyGHError(stderr=%q) = %v, want to contain %q", c.stderr, err, c.want)
		}
	}
}

type errTest string

func (e errTest) Error() string { return string(e) }

// The following tests exercise the input-validation and safety-gate guards
// on the core operations. All of them return before touching os/exec, so
// they run without a network connection or the gh CLI binary installed.

func TestExecGHCommandRequiresArgs(t *testing.T) {
	cfg := serverConfig{cliPath: "gh"}
	if _, err := execGHCommand(context.Background(), cfg, nil, false, 0, 0); err == nil {
		t.Error("expected error for empty args")
	}
}

func TestExecGHCommandEnforcesAllowedRepos(t *testing.T) {
	cfg := serverConfig{cliPath: "gh", allowedRepos: map[string]bool{"owner/allowed": true}}
	_, err := execGHCommand(context.Background(), cfg, []string{"issue", "list", "--repo", "owner/other"}, false, 0, 0)
	if err == nil || !strings.Contains(err.Error(), "not in the allowed list") {
		t.Errorf("expected allowlist error, got %v", err)
	}
}

func TestExecGHCommandAllowsUnscopedWhenNoRepoFlag(t *testing.T) {
	// A command with no -R/--repo flag can't be checked against the
	// allowlist here (it would need a real gh binary + git context to
	// resolve), so it should pass this particular gate.
	cfg := serverConfig{cliPath: "gh", allowedRepos: map[string]bool{"owner/allowed": true}}
	_, err := execGHCommand(context.Background(), cfg, []string{"---not-a-real-flag---"}, false, 0, 0)
	if err != nil && strings.Contains(err.Error(), "not in the allowed list") {
		t.Errorf("did not expect allowlist rejection without a -R/--repo flag, got %v", err)
	}
}

func TestExecGHCommandBlocksMutatingInReadOnlyMode(t *testing.T) {
	cfg := serverConfig{cliPath: "gh", allowWrite: false}
	_, err := execGHCommand(context.Background(), cfg, []string{"issue", "close", "5"}, true, 0, 0)
	if err == nil || !strings.Contains(err.Error(), "read-only mode") {
		t.Errorf("expected read-only-mode error, got %v", err)
	}
}

func TestExecGHCommandRequiresConfirmForMutating(t *testing.T) {
	cfg := serverConfig{cliPath: "gh", allowWrite: true}
	_, err := execGHCommand(context.Background(), cfg, []string{"issue", "close", "5"}, false, 0, 0)
	if err == nil || !strings.Contains(err.Error(), "confirm=true") {
		t.Errorf("expected confirm=true error, got %v", err)
	}
}

func TestRunGHHelpRequiresArgs(t *testing.T) {
	cfg := serverConfig{cliPath: "gh"}
	if _, err := runGHHelp(context.Background(), cfg, nil); err == nil {
		t.Error("expected error for empty args")
	}
}

func TestCapBytes(t *testing.T) {
	text, truncated := capBytes("hello world", 5)
	if text != "hello" || !truncated {
		t.Errorf("capBytes short cap = (%q, %v), want (\"hello\", true)", text, truncated)
	}
	text, truncated = capBytes("hi", 5)
	if text != "hi" || truncated {
		t.Errorf("capBytes under limit = (%q, %v), want (\"hi\", false)", text, truncated)
	}
}

func TestRenderJSONRoundTrip(t *testing.T) {
	data := map[string]any{"a": 1, "b": "two"}
	out := renderJSON(data)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("renderJSON output did not parse as JSON: %v", err)
	}
	if parsed["b"] != "two" {
		t.Errorf("round-tripped value mismatch: %v", parsed)
	}
}
