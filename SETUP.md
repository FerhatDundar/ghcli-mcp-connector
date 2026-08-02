# Setup guide — GitHub CLI connector

This connector shells out to the `gh` binary already installed and
authenticated on the host. It does not manage credentials itself — it
uses whatever `gh auth login` already configured (stored in the OS
keychain), or `GH_TOKEN`/`GITHUB_TOKEN` if you set them. If
`gh auth status` works in your terminal, this connector will work too.

---

## 1. Prerequisites

1. **GitHub CLI installed** — `gh --version` to check; install from
   <https://cli.github.com> if not.
2. **Authenticated** — one of:
   - `gh auth login` (interactive, stores a token in your OS keychain —
     simplest)
   - A `GH_TOKEN`/`GITHUB_TOKEN` environment variable (a personal access
     token or fine-grained token)

   Verify with:

   ```bash
   gh auth status
   ```

   If that prints "✓ Logged in to github.com", you're set. If it errors,
   fix that first — this connector's startup check runs the exact same
   command and will refuse to start otherwise.

## 2. Get the server binary

**Option A — download a release (no Go needed):** grab
`ghcli-mcp-connector-plugin-<version>-<os>-<arch>.zip` from the
[latest release](https://github.com/FerhatDundar/ghcli-mcp-connector/releases/latest)
and unzip it — the `plugin/` folder inside is ready to install, skip to
step 4.

**Option B — build from source:** you need Go 1.25+ installed
(`go version` to check; get it from <https://go.dev/dl/> if not).

```bash
cd subprojects/ghcli-mcp-connector/go-server
go mod tidy                          # fetches deps, writes go.sum
go build -o ghcli-connector-server .
cp ghcli-connector-server ../plugin/servers/go/
```

## 3. Configure the plugin

Edit `plugin/.mcp.json`:

```json
{
  "mcpServers": {
    "ghcli": {
      "command": "${CLAUDE_PLUGIN_ROOT}/servers/go/ghcli-connector-server",
      "env": {
        "GH_TOKEN": "",
        "GH_HOST": "",
        "GHCLI_MCP_ALLOW_WRITE": "false",
        "GHCLI_MCP_ALLOWED_REPOS": ""
      }
    }
  }
}
```

- Leave `GH_TOKEN` empty to use whatever `gh auth login` already
  configured (stored in your OS keychain). Set it to a PAT/fine-grained
  token only if you want this connector to use different credentials
  than your interactive `gh` sessions.
- Leave `GH_HOST` empty for github.com, or set it to a GitHub Enterprise
  hostname.
- **`GHCLI_MCP_ALLOW_WRITE` defaults to `"false"` — read-only.**
  State-changing commands (`create`, `delete`, `edit`, `close`, `merge`,
  `comment`, etc., and `gh api` calls with `--method` other than `GET`)
  are refused by the server before they ever reach GitHub. Set it to
  `"true"` only once you're comfortable letting the agent make real
  changes — and even then, every individual state-changing call still
  needs `confirm=true` passed explicitly. Local-only side effects (repo
  clone, release download, pr checkout) are never gated — they don't
  touch GitHub state.
- `GHCLI_MCP_ALLOWED_REPOS` is optional extra scoping — a comma-separated
  allowlist like `"me/proj,me/other"` if you want to restrict which repos
  are reachable, when a call passes an explicit `-R`/`--repo` flag.

Keep this file out of any shared/committed location if you put a static
token in it. (The subproject's `.gitignore` already blocks `.env`/
`*.token` files; the `.mcp.json` inside the *installed* plugin lives in
Cowork's plugin directory, not the repo.)

## 4. Install the plugin and verify

1. In the Claude desktop app: **Settings → Capabilities** → add a plugin
   from a local folder, and point it at:
   `subprojects/ghcli-mcp-connector/plugin/`
   (A plugin can't be registered from inside a chat session — it has to
   be added here.)
2. Restart / reload so the new MCP server is picked up.
3. Back in a chat, verify:
   - Ask: **"Run ghcli_whoami"** → should return your GitHub login.
   - Ask: **"List open PRs on cli/cli"** (`ghcli_exec` with
     `args: ["pr","list","--repo","cli/cli"]`) → should return real PRs.
   - Ask it to do something state-changing, e.g. **"close issue 5 on
     my-org/my-repo"** → should be refused with a clear read-only-mode
     message unless you've set `GHCLI_MCP_ALLOW_WRITE=true` and it passes
     `confirm=true`.

Done. From here you can ask things like *"what issues are open on
owner/repo?"*, *"show me the diff for PR 42,"* or *"what's the help text
for `gh release create`?"*


---

## Troubleshooting

| Symptom | Likely cause / fix |
|---------|--------------------|
| Server refuses to start, `not logged in to the GitHub CLI` | `gh auth status` fails in your terminal too — fix auth first (`gh auth login`, or set `GH_TOKEN`). |
| `gh CLI not found` | The `gh` binary isn't on `PATH` for the process running this connector — install it, or set `GHCLI_MCP_CLI_PATH` to its full path. |
| Every state-changing command is refused | Expected by default. Set `GHCLI_MCP_ALLOW_WRITE=true` in `.mcp.json` and pass `confirm=true` on the specific tool call. |
| `repo "X" is not in the allowed list` | `GHCLI_MCP_ALLOWED_REPOS` is set and doesn't include that repo — add it or clear the variable. |
| `no repository context` / `no git remotes found` | The command relies on gh's current-directory git detection, but this connector's process isn't running inside a git repo. Pass `-R owner/repo` explicitly. |
| Output looks cut off | `max_bytes` was hit — raise it on the call (hard cap 5,000,000), or narrow the query (e.g. add `--limit`, a `--search` filter). |
| Tools don't appear at all | Binary not built/copied (step 2), or plugin not installed/reloaded (step 4). |
