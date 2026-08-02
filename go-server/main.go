// Command ghcli-connector-server is an MCP server that lets an agent run
// GitHub CLI (`gh`) commands. It shells out to the `gh` binary already
// installed and authenticated on the host (gh auth login — stored in the
// OS keychain, or GH_TOKEN/GITHUB_TOKEN env vars) rather than
// reimplementing a GitHub API client, so it inherits the full breadth of
// the CLI (repo, issue, pr, release, gist, workflow, run, secret,
// variable, project, ruleset, codespace, extension, ...) for free instead
// of a hand-curated subset. Built with mark3labs/mcp-go, mirroring the
// layout and conventions of the sibling {slug}-mcp-connector repos
// (s3-mcp-connector, discord-mcp-connector, github-mcp-connector,
// sqlite-mcp-connector, aws-mcp-connector).
//
// This is deliberately a *different* connector than github-mcp-connector:
// that one talks to the GitHub REST API directly via google/go-github with
// a hand-curated set of 18 tools. This one shells out to the `gh` CLI
// itself, the same way aws-mcp-connector shells out to `aws` — trading a
// curated tool surface for full CLI coverage (gh extensions, gh's own
// output formatting/filters, workflow/run/codespace/project/ruleset
// management, etc.) via a single ghcli_exec escape hatch.
//
// Safety: commands that change state on GitHub (create/delete/edit/close/
// merge/etc.) are blocked unless both GHCLI_MCP_ALLOW_WRITE=true is set
// for the server process AND the caller passes confirm=true on that
// specific tool call. Read-only by default. Local-filesystem-only side
// effects (repo clone, release download, pr checkout) are not gated —
// they never mutate anything on GitHub itself.
//
// Auth / config (all via environment variables, passed through by the
// plugin's .mcp.json):
//
//	GH_TOKEN / GITHUB_TOKEN — optional. gh CLI's own env vars for a token;
//	    leave unset to use whatever `gh auth login` already configured
//	    (stored in the OS keychain).
//	GH_HOST                 — optional. Target a GitHub Enterprise
//	    hostname instead of github.com; passed straight through to gh.
//	GHCLI_MCP_ALLOW_WRITE    — "true" to permit state-changing commands at
//	    all. Defaults to "false" (read-only mode).
//	GHCLI_MCP_ALLOWED_REPOS  — optional comma-separated allowlist of
//	    "owner/repo" values. Enforced only when a command passes an
//	    explicit -R/--repo flag; commands relying on cwd git detection or
//	    that aren't repo-scoped (gh api, gh gist, ...) are not checked.
//	GHCLI_MCP_CLI_PATH       — optional path to the gh binary. Defaults to
//	    "gh" resolved via PATH.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

const (
	defaultTimeoutSeconds = 30
	maxTimeoutSeconds     = 120
	defaultMaxBytes       = 200_000
	hardMaxBytes          = 5_000_000
)

// ---------------------------------------------------------------------
// State-changing command classification — the read-only safety gate
// ---------------------------------------------------------------------

// mutatingVerbs catches gh's state-changing subcommand verbs (second-level
// commands like `gh issue create`, `gh pr merge`, `gh repo delete`). gh's
// CLI vocabulary is word-based (not hyphen-prefixed like AWS's), so this
// is an exact-match set rather than a prefix list. Deliberately
// conservative: local-only side effects (clone/checkout/download/watch)
// are *not* here — they never change anything on GitHub, only the local
// filesystem/git state — but anything that installs code (extension
// install) or changes GitHub-side state is included, even in ambiguous
// cases, matching the same philosophy as aws-mcp-connector's
// isMutatingCommand: err toward classifying ambiguous things as mutating
// rather than risk silently allowing a destructive call through.
var mutatingVerbs = map[string]bool{
	"create": true, "delete": true, "delete-asset": true, "edit": true,
	"close": true, "reopen": true, "transfer": true, "pin": true, "unpin": true,
	"comment": true, "lock": true, "unlock": true, "develop": true,
	"merge": true, "ready": true, "review": true, "update-branch": true,
	"rename": true, "archive": true, "unarchive": true, "fork": true,
	"sync": true, "set-default": true, "set": true, "unset": true,
	"upgrade": true, "remove": true, "upload": true, "cancel": true,
	"rerun": true, "enable": true, "disable": true, "refresh": true,
	"switch": true, "setup-git": true, "copy": true, "link": true,
	"unlink": true, "field-create": true, "field-delete": true,
	"item-add": true, "item-archive": true, "item-create": true,
	"item-delete": true, "item-edit": true, "add": true, "login": true,
	"logout": true, "import": true, "rebuild": true, "stop": true,
	"install": true, "uninstall": true,
}

// mutatingMethods are HTTP methods that mutate state when passed to
// `gh api` via --method/-X. GET (the default) and HEAD are read-only.
var mutatingMethods = map[string]bool{
	"POST": true, "PUT": true, "PATCH": true, "DELETE": true,
}

// isMutatingCommand reports whether any positional (non-flag) token in the
// command matches a known state-changing verb, or — for `gh api` calls —
// whether an explicit --method/-X value other than GET/HEAD is present.
func isMutatingCommand(args []string) bool {
	isAPI := len(args) > 0 && strings.ToLower(args[0]) == "api"
	for i, a := range args {
		low := strings.ToLower(a)
		if strings.HasPrefix(a, "-") {
			if !isAPI {
				continue
			}
			if (low == "--method" || low == "-x") && i+1 < len(args) {
				if mutatingMethods[strings.ToUpper(args[i+1])] {
					return true
				}
			} else if strings.HasPrefix(low, "--method=") {
				val := strings.TrimPrefix(low, "--method=")
				if mutatingMethods[strings.ToUpper(val)] {
					return true
				}
			}
			continue
		}
		if mutatingVerbs[low] {
			return true
		}
	}
	return false
}

// repoFlagValue returns the value of an explicit -R/--repo flag if present,
// or "" if the command doesn't pass one (in which case GHCLI_MCP_ALLOWED_REPOS
// cannot be enforced — see the package doc comment).
func repoFlagValue(args []string) string {
	for i, a := range args {
		if a == "-R" || a == "--repo" {
			if i+1 < len(args) {
				return args[i+1]
			}
		}
		if strings.HasPrefix(a, "--repo=") {
			return strings.TrimPrefix(a, "--repo=")
		}
	}
	return ""
}

// ---------------------------------------------------------------------
// Server config, resolved once at startup from the environment
// ---------------------------------------------------------------------

type serverConfig struct {
	cliPath      string
	allowWrite   bool
	allowedRepos map[string]bool // nil/empty means unrestricted; keys are lowercase "owner/repo"
}

func loadServerConfig() serverConfig {
	cfg := serverConfig{cliPath: "gh"}
	if p := strings.TrimSpace(os.Getenv("GHCLI_MCP_CLI_PATH")); p != "" {
		cfg.cliPath = p
	}
	cfg.allowWrite = isTruthy(os.Getenv("GHCLI_MCP_ALLOW_WRITE"))
	if raw := strings.TrimSpace(os.Getenv("GHCLI_MCP_ALLOWED_REPOS")); raw != "" {
		cfg.allowedRepos = map[string]bool{}
		for _, r := range strings.Split(raw, ",") {
			r = strings.ToLower(strings.TrimSpace(r))
			if r != "" {
				cfg.allowedRepos[r] = true
			}
		}
	}
	return cfg
}

func isTruthy(v string) bool {
	v = strings.ToLower(strings.TrimSpace(v))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

// checkStartup verifies the gh binary is on PATH and that auth resolves,
// by making one cheap call (`gh auth status`). This is a network call, but
// for a CLI-subprocess connector it's the only way to catch "gh not
// installed" / "not logged in" / "expired token" before the first real
// tool call surfaces a confusing error.
func checkStartup(cfg serverConfig) error {
	if _, err := exec.LookPath(cfg.cliPath); err != nil {
		return fmt.Errorf("gh CLI not found (looked for %q on PATH): %w — install it (https://cli.github.com) or set GHCLI_MCP_CLI_PATH", cfg.cliPath, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// #nosec G204 -- cfg.cliPath is operator-configured (GHCLI_MCP_CLI_PATH
	// env var, defaults to "gh" resolved via PATH), never derived from an
	// MCP tool call argument. The fixed arg list here is not tainted.
	cmd := exec.CommandContext(ctx, cfg.cliPath, "auth", "status")
	cmd.Env = append(os.Environ(), "GH_PAGER=", "NO_COLOR=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		return fmt.Errorf("gh auth status failed — not logged in to the GitHub CLI: %s (%w). Run `gh auth login`, or set GH_TOKEN/GITHUB_TOKEN", msg, err)
	}
	return nil
}

// friendlyGHError adds actionable hints to common gh-cli failures instead
// of surfacing the raw stderr blob.
func friendlyGHError(action, stderrText string, err error) error {
	if err == nil {
		return nil
	}
	msg := strings.TrimSpace(stderrText)
	if msg == "" {
		msg = err.Error()
	}
	switch {
	case strings.Contains(msg, "gh auth login"), strings.Contains(msg, "authentication required"), strings.Contains(msg, "not logged"), strings.Contains(msg, "HTTP 401"):
		return fmt.Errorf("%s failed: not authenticated — run `gh auth login`, or check GH_TOKEN/GITHUB_TOKEN. (%s)", action, msg)
	case strings.Contains(msg, "HTTP 403"), strings.Contains(msg, "must have admin"), strings.Contains(msg, "Resource not accessible"):
		return fmt.Errorf("%s failed: access denied — the configured account/token lacks permission for this action (or a rate limit was hit). (%s)", action, msg)
	case strings.Contains(msg, "HTTP 404"), strings.Contains(msg, "Could not resolve to a"), strings.Contains(msg, "not found"):
		return fmt.Errorf("%s failed: not found — double check the owner/repo/number/name. (%s)", action, msg)
	case strings.Contains(msg, "HTTP 422"), strings.Contains(msg, "Validation Failed"):
		return fmt.Errorf("%s failed: validation error — check the arguments/fields being sent. (%s)", action, msg)
	case strings.Contains(msg, "no git remotes found"), strings.Contains(msg, "not a git repository"):
		return fmt.Errorf("%s failed: no repository context — pass -R owner/repo explicitly (this connector's process has no working-directory git repo). (%s)", action, msg)
	case strings.Contains(msg, "Could not resolve host"), strings.Contains(msg, "connection refused"), strings.Contains(msg, "context deadline exceeded"):
		return fmt.Errorf("%s failed: could not reach GitHub — check network connectivity and GH_HOST. (%s)", action, msg)
	case strings.Contains(msg, "unknown command"), strings.Contains(msg, "unknown flag"), strings.Contains(msg, "unknown shorthand flag"), strings.Contains(msg, "Usage:"):
		return fmt.Errorf("%s failed: invalid command/arguments — use ghcli_help with the same command to see valid syntax. (%s)", action, msg)
	case strings.Contains(msg, "command not found"):
		return fmt.Errorf("%s failed: the gh CLI is not installed or not on PATH in the connector's environment. (%s)", action, msg)
	default:
		return fmt.Errorf("%s failed: %s", action, msg)
	}
}

// ---------------------------------------------------------------------
// Core operations — each returns a map[string]any for uniform rendering
// ---------------------------------------------------------------------

func execGHCommand(ctx context.Context, cfg serverConfig, args []string, confirmed bool, timeoutSeconds, maxBytes int) (map[string]any, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf(`args is required, e.g. ["repo","view","cli/cli"] or ["issue","list","--repo","owner/repo"]`)
	}

	if len(cfg.allowedRepos) > 0 {
		if repo := repoFlagValue(args); repo != "" && !cfg.allowedRepos[strings.ToLower(repo)] {
			allowed := make([]string, 0, len(cfg.allowedRepos))
			for r := range cfg.allowedRepos {
				allowed = append(allowed, r)
			}
			sort.Strings(allowed)
			return nil, fmt.Errorf("repo %q is not in the allowed list (%s) — set GHCLI_MCP_ALLOWED_REPOS to include it, or leave it unset to allow all repos", repo, strings.Join(allowed, ", "))
		}
	}

	mutating := isMutatingCommand(args)
	if mutating {
		if !cfg.allowWrite {
			return nil, fmt.Errorf("this looks like a state-changing command (gh %s) but the server is in read-only mode — set GHCLI_MCP_ALLOW_WRITE=true in the connector's env to permit writes", strings.Join(args, " "))
		}
		if !confirmed {
			return nil, fmt.Errorf("this looks like a state-changing command (gh %s) — pass confirm=true to actually run it", strings.Join(args, " "))
		}
	}

	if timeoutSeconds <= 0 {
		timeoutSeconds = defaultTimeoutSeconds
	}
	if timeoutSeconds > maxTimeoutSeconds {
		timeoutSeconds = maxTimeoutSeconds
	}
	if maxBytes <= 0 {
		maxBytes = defaultMaxBytes
	}
	if maxBytes > hardMaxBytes {
		maxBytes = hardMaxBytes
	}

	runCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSeconds)*time.Second)
	defer cancel()

	start := time.Now()
	// #nosec G204 -- args comes from the caller's MCP tool-call arguments by
	// design: this tool's entire purpose is to run arbitrary `gh` CLI
	// commands. It runs the gh binary directly (no shell), so there's no
	// shell-injection risk, only "the gh CLI does what it's told" — which
	// is why isMutatingCommand + GHCLI_MCP_ALLOW_WRITE + confirm=true gate
	// anything that changes GitHub-side state, and GHCLI_MCP_ALLOWED_REPOS
	// can restrict which repos (when passed via -R/--repo) are reachable.
	cmd := exec.CommandContext(runCtx, cfg.cliPath, args...)
	cmd.Env = append(os.Environ(), "GH_PAGER=", "NO_COLOR=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	duration := time.Since(start)

	exitCode := 0
	if runErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(runErr, &exitErr) {
			return nil, friendlyGHError(fmt.Sprintf("run `gh %s`", strings.Join(args, " ")), stderr.String(), runErr)
		}
		exitCode = exitErr.ExitCode()
	}

	outText, outTruncated := capBytes(stdout.String(), maxBytes)
	errText, errTruncated := capBytes(stderr.String(), maxBytes)

	result := map[string]any{
		"command":      "gh " + strings.Join(args, " "),
		"exit_code":    exitCode,
		"succeeded":    exitCode == 0,
		"stdout":       outText,
		"stderr":       errText,
		"truncated":    outTruncated || errTruncated,
		"duration_ms":  duration.Milliseconds(),
		"was_mutating": mutating,
	}
	if exitCode != 0 {
		result["error_hint"] = friendlyGHError(fmt.Sprintf("run `gh %s`", strings.Join(args, " ")), stderr.String(), fmt.Errorf("exit code %d", exitCode)).Error()
	}
	return result, nil
}

func capBytes(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	return s[:max], true
}

func runGHHelp(ctx context.Context, cfg serverConfig, args []string) (map[string]any, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf(`args is required, e.g. ["pr"] or ["pr","create"]`)
	}
	fullArgs := append(append([]string{}, args...), "--help")
	runCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	// #nosec G204 -- fullArgs is caller-supplied, but this always appends
	// "--help" and only ever prints documentation; there is no mutating
	// counterpart to `gh ... --help`, so it needs none of ghcli_exec's
	// gating.
	cmd := exec.CommandContext(runCtx, cfg.cliPath, fullArgs...)
	cmd.Env = append(os.Environ(), "GH_PAGER=", "NO_COLOR=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, friendlyGHError(fmt.Sprintf("run `gh %s`", strings.Join(fullArgs, " ")), stderr.String(), err)
	}
	text, truncated := capBytes(stdout.String(), defaultMaxBytes)
	return map[string]any{
		"command":   "gh " + strings.Join(fullArgs, " "),
		"help_text": text,
		"truncated": truncated,
	}, nil
}

func runWhoami(ctx context.Context, cfg serverConfig) (map[string]any, error) {
	runCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	// #nosec G204 -- cfg.cliPath is operator-configured, not tool-call
	// input; the rest of the arg list is fixed. See checkStartup.
	cmd := exec.CommandContext(runCtx, cfg.cliPath, "api", "user")
	cmd.Env = append(os.Environ(), "GH_PAGER=", "NO_COLOR=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, friendlyGHError("get authenticated user", stderr.String(), err)
	}
	var user map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &user); err != nil {
		return nil, fmt.Errorf("could not parse `gh api user` output: %w", err)
	}
	result := map[string]any{
		"login":    user["login"],
		"id":       user["id"],
		"name":     user["name"],
		"html_url": user["html_url"],
	}
	// gh api's JSON number decodes to float64 via map[string]any; render the
	// user ID as a plain integer instead of e.g. "6.8921879e+07".
	if idf, ok := user["id"].(float64); ok {
		result["id"] = int64(idf)
	}
	if host := strings.TrimSpace(os.Getenv("GH_HOST")); host != "" {
		result["host"] = host
	} else {
		result["host"] = "github.com"
	}
	return result, nil
}

// ---------------------------------------------------------------------
// Markdown rendering
// ---------------------------------------------------------------------

func successLabel(v any) string {
	if b, ok := v.(bool); ok && b {
		return "success"
	}
	return "failed"
}

func execMD(d map[string]any) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# `%v`\n\n", d["command"])
	fmt.Fprintf(&b, "- **Exit code**: %v (%s)\n", d["exit_code"], successLabel(d["succeeded"]))
	fmt.Fprintf(&b, "- **Duration**: %vms\n", d["duration_ms"])
	if h, ok := d["error_hint"].(string); ok && h != "" {
		fmt.Fprintf(&b, "- **Error hint**: %s\n", h)
	}
	b.WriteString("\n**stdout:**\n```\n")
	fmt.Fprintf(&b, "%v", d["stdout"])
	b.WriteString("\n```\n")
	if s, ok := d["stderr"].(string); ok && strings.TrimSpace(s) != "" {
		b.WriteString("\n**stderr:**\n```\n")
		b.WriteString(s)
		b.WriteString("\n```\n")
	}
	if t, _ := d["truncated"].(bool); t {
		b.WriteString("\n_Output truncated by max_bytes — raise it (hard cap 5,000,000) to see more._\n")
	}
	return b.String()
}

func helpMD(d map[string]any) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Help: `%v`\n\n```\n%v\n```\n", d["command"], d["help_text"])
	if t, _ := d["truncated"].(bool); t {
		b.WriteString("\n_Help text truncated._\n")
	}
	return b.String()
}

func whoamiMD(d map[string]any) string {
	var b strings.Builder
	b.WriteString("# GitHub identity\n\n")
	fmt.Fprintf(&b, "- **Login**: %v\n", d["login"])
	fmt.Fprintf(&b, "- **Name**: %v\n", d["name"])
	fmt.Fprintf(&b, "- **ID**: %v\n", d["id"])
	fmt.Fprintf(&b, "- **Profile**: %v\n", d["html_url"])
	fmt.Fprintf(&b, "- **Host**: %v\n", d["host"])
	return b.String()
}

// ---------------------------------------------------------------------
// MCP wiring
// ---------------------------------------------------------------------

func getFormat(req mcp.CallToolRequest) string {
	f := req.GetString("response_format", "markdown")
	if f != "json" {
		f = "markdown"
	}
	return f
}

func resultOrError(data map[string]any, err error, format string, mdFn func(map[string]any) string) *mcp.CallToolResult {
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Error: %v", err))
	}
	if format == "json" {
		return mcp.NewToolResultText(renderJSON(data))
	}
	return mcp.NewToolResultText(mdFn(data))
}

func renderJSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf(`{"error": %q}`, err.Error())
	}
	return string(b)
}

// version is set at build time via -ldflags "-X main.version=vX.Y.Z" by the
// release workflow. Local `go build` leaves it at "dev".
var version = "dev"

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-version") {
		fmt.Println("ghcli-connector-server " + version)
		return
	}

	cfg := loadServerConfig()
	if err := checkStartup(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "ghcli-connector-server: %v\n", err)
		os.Exit(1)
	}

	s := server.NewMCPServer("ghcli-mcp-connector", "0.1.0", server.WithToolCapabilities(false))

	s.AddTool(mcp.NewTool("ghcli_exec",
		mcp.WithDescription(`Run a GitHub CLI (gh) command, e.g. args=["repo","view","cli/cli"] or args=["issue","list","--repo","owner/repo","--state","open"]. This process has no working-directory git repo, so repo-scoped commands need an explicit -R/--repo owner/repo (or a full URL where the command accepts one) rather than relying on gh's cwd-based repo detection. Do not include the leading "gh" itself. Read-only by default: commands that change state on GitHub (create/delete/edit/close/merge/comment/etc., or gh api calls with --method other than GET) are blocked unless the server has GHCLI_MCP_ALLOW_WRITE=true set AND confirm=true is passed on this call. Local-only side effects (clone/checkout/download) are never blocked. Use ghcli_help first to check exact subcommand/flag syntax if unsure.`),
		mcp.WithArray("args", mcp.Required(), mcp.Items(map[string]any{"type": "string"}), mcp.Description(`The CLI arguments after "gh", as separate array elements — e.g. ["pr","list","--repo","owner/repo"] or ["api","repos/owner/repo/issues"]. Do not include the leading "gh" itself.`)),
		mcp.WithBoolean("confirm", mcp.DefaultBool(false), mcp.Description("Must be true to execute a state-changing command. Ignored for read-only commands.")),
		mcp.WithNumber("timeout_seconds", mcp.DefaultNumber(defaultTimeoutSeconds), mcp.Description("Max seconds to let the command run before killing it (hard cap 120).")),
		mcp.WithNumber("max_bytes", mcp.DefaultNumber(defaultMaxBytes), mcp.Description("Max bytes of stdout/stderr to return (hard cap 5,000,000).")),
		mcp.WithString("response_format", mcp.Enum("markdown", "json"), mcp.DefaultString("markdown"), mcp.Description("Output format: 'markdown' or 'json'")),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(true),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args, err := req.RequireStringSlice("args")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		confirm := req.GetBool("confirm", false)
		timeoutSeconds := req.GetInt("timeout_seconds", defaultTimeoutSeconds)
		maxBytes := req.GetInt("max_bytes", defaultMaxBytes)
		data, err := execGHCommand(ctx, cfg, args, confirm, timeoutSeconds, maxBytes)
		return resultOrError(data, err, getFormat(req), execMD), nil
	})

	s.AddTool(mcp.NewTool("ghcli_help",
		mcp.WithDescription(`Show gh CLI help text for a command, e.g. args=["pr"] or args=["pr","create"]. Always safe to call — use this to check exact syntax before calling ghcli_exec.`),
		mcp.WithArray("args", mcp.Required(), mcp.Items(map[string]any{"type": "string"}), mcp.Description(`Command and optional subcommand to get help for, e.g. ["issue","create"].`)),
		mcp.WithString("response_format", mcp.Enum("markdown", "json"), mcp.DefaultString("markdown"), mcp.Description("Output format: 'markdown' or 'json'")),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(true),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args, err := req.RequireStringSlice("args")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		data, err := runGHHelp(ctx, cfg, args)
		return resultOrError(data, err, getFormat(req), helpMD), nil
	})

	s.AddTool(mcp.NewTool("ghcli_whoami",
		mcp.WithDescription("Show the GitHub identity (login, name, profile URL) the connector's gh CLI auth resolves to. Good first call to confirm auth is working."),
		mcp.WithString("response_format", mcp.Enum("markdown", "json"), mcp.DefaultString("markdown"), mcp.Description("Output format: 'markdown' or 'json'")),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(true),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		data, err := runWhoami(ctx, cfg)
		return resultOrError(data, err, getFormat(req), whoamiMD), nil
	})

	if err := server.ServeStdio(s); err != nil {
		fmt.Fprintf(os.Stderr, "ghcli-connector-server error: %v\n", err)
		os.Exit(1)
	}
}
