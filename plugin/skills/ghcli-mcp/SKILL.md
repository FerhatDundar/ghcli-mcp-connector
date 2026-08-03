---
name: ghcli-mcp
description: >
  This skill should be used for any GitHub operation the user asks for —
  e.g. "check PRs on owner/repo", "list open issues", "what's the status
  of this workflow run", "create an issue", "merge this PR", "run a gh
  command", "who am I logged in as on GitHub", or any request naming a
  GitHub repo, issue, PR, release, gist, workflow run, or the `gh` CLI by
  name. Use it instead of guessing at the GitHub REST API directly —
  this connector shells out to the real `gh` CLI, so it covers every
  command the CLI supports.
metadata:
  version: "0.1.0"
---

# ghcli-mcp

Use the `ghcli_exec`, `ghcli_help`, and `ghcli_whoami` tools for any
GitHub operation. They shell out to the real `gh` CLI on the host, so
every `gh` command is available — not a hand-picked subset.

## Tools

- **`ghcli_whoami`** — show the authenticated GitHub identity. Call this
  first if it's unclear whether auth is even working, or the user asks
  "who am I logged in as".
- **`ghcli_help`** — show `gh <command> --help` text. Call this before
  `ghcli_exec` whenever unsure of exact flag names or syntax — it's
  always safe, never gated.
- **`ghcli_exec`** — run any `gh <command> <subcommand> ...` command.
  Pass the arguments as an array, without the leading `"gh"`, e.g.
  `args: ["pr", "list", "--repo", "owner/repo"]`.

## Always pass `-R`/`--repo` explicitly

This connector's process has no working-directory git repo, so it can't
infer which repo a command targets the way an interactive `gh` session
would. Every repo-scoped command needs an explicit `-R owner/repo` or
`--repo owner/repo`. If a user names a repo in conversation, use it; if
they don't and the command needs one, ask which repo rather than
guessing.

## Safety model — read-only by default

Commands that change state on GitHub (`create`, `delete`, `edit`,
`close`, `merge`, `comment`, `reopen`, `archive`, and `gh api` calls with
`--method` other than `GET`) are blocked unless the connector's server
has `GHCLI_MCP_ALLOW_WRITE=true` configured *and* the tool call passes
`confirm=true`.

- If a write is attempted while the server is in read-only mode,
  `ghcli_exec` returns a clear error saying so — relay that to the user
  rather than retrying with different arguments; the fix is a config
  change on their end (see the connector's SETUP.md), not a different
  command.
- If the server does have writes enabled, still confirm with the user in
  plain language before calling `ghcli_exec` with `confirm=true` on
  anything destructive or hard to undo (closing/merging a PR, deleting
  something, editing a title/body) — the tool's `confirm` flag is a
  mechanical gate, not a substitute for actually checking with the
  person you're acting on behalf of.
- Local-only side effects — `repo clone`, `pr checkout`, `release
  download` — are never gated, since they don't touch anything on
  GitHub. No need to ask before those.

## Common patterns

List open PRs on a repo:
```
ghcli_exec: { args: ["pr", "list", "--repo", "owner/repo"] }
```

View a specific issue:
```
ghcli_exec: { args: ["issue", "view", "42", "--repo", "owner/repo"] }
```

Check CI status for a PR:
```
ghcli_exec: { args: ["pr", "checks", "17", "--repo", "owner/repo"] }
```

Create an issue (state-changing — needs write mode + confirm):
```
ghcli_exec: {
  args: ["issue", "create", "--repo", "owner/repo", "--title", "...", "--body", "..."],
  confirm: true
}
```

Hit an arbitrary API endpoint read-only:
```
ghcli_exec: { args: ["api", "repos/owner/repo/releases/latest"] }
```

Look up exact flags before an unfamiliar command:
```
ghcli_help: { args: ["release", "create"] }
```

## Output size

`ghcli_exec` caps output at 200,000 bytes by default (raise via
`max_bytes`, hard cap 5,000,000). If a listing looks truncated, narrow
it first with `gh`'s own filters (`--limit`, `--search`, `--state`)
rather than just raising the cap — it's usually faster and the user
didn't want the whole history anyway.
