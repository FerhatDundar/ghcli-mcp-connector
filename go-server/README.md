# go-server

MCP server source for the GitHub CLI connector. Single-file (`main.go`),
built with [mark3labs/mcp-go](https://github.com/mark3labs/mcp-go). No
GitHub SDK dependency — it shells out to the `gh` binary itself rather
than reimplementing it.

## Build

```bash
go mod tidy                          # fetches deps, writes go.sum
go build -o ghcli-connector-server .
cp ghcli-connector-server ../plugin/servers/go/
```

Requires Go 1.25+ and the `gh` CLI installed and authenticated
(`gh auth login`) — see `../SETUP.md`.

## Manual test against real GitHub

There's no official local sandbox for the GitHub CLI itself, so this
connector was end-to-end verified against real GitHub: `ghcli_whoami`
against the real authenticated identity, `ghcli_help` for syntax lookup,
`ghcli_exec` for read-only calls (`repo view`, `pr list`, `issue list`),
confirmed state-changing commands are refused without
`GHCLI_MCP_ALLOW_WRITE=true` + `confirm=true`, confirmed the
`GHCLI_MCP_ALLOWED_REPOS` allowlist blocks out-of-scope repos, and
confirmed that once both write gates are open the command genuinely
reaches GitHub (verified against a dedicated disposable sandbox repo, not
a repo that matters).

Minimal manual-test pattern — run the server directly and drive it over
stdio with a JSON-RPC test script (or any MCP client):

```bash
./ghcli-connector-server
```

Then send `initialize`, `notifications/initialized`, and `tools/call`
JSON-RPC messages over stdin (one per line), reading one JSON reply per
line from stdout.

## Code layout

- **State-changing command classification** — `isMutatingCommand` /
  `mutatingVerbs` / `mutatingMethods` — the read-only safety gate's core
  logic, kept separate and unit-testable.
- **Server config** — `loadServerConfig` resolves `GHCLI_MCP_ALLOW_WRITE`,
  `GHCLI_MCP_ALLOWED_REPOS`, `GHCLI_MCP_CLI_PATH` from the environment
  once at startup.
- **Core operations** — `execGHCommand`, `runGHHelp`, `runWhoami`, each
  returning a `map[string]any` for uniform JSON/Markdown rendering.
- **Markdown rendering** — one `*MD` function per operation.
- **MCP wiring** — `main()` registers all 3 tools on an `mcp-go` server
  and serves over stdio.
