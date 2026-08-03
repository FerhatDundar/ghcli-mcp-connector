# Changelog

## [0.1.1](https://github.com/FerhatDundar/ghcli-mcp-connector/compare/v0.1.0...v0.1.1) (2026-08-03)


### Bug Fixes

* trim whitespace from -R/--repo flag value in repoFlagValue ([d80530b](https://github.com/FerhatDundar/ghcli-mcp-connector/commit/d80530bad6674ed7d15cb7d28a7b1ea311c125da))

## 0.1.0 (2026-08-02)

Initial release.

### ✨ Features

- MCP server exposing 3 tools for running the GitHub CLI directly:
  `ghcli_exec`, `ghcli_help`, `ghcli_whoami`
- Shells out to the `gh` binary rather than reimplementing a GitHub API
  client, so it covers every command the CLI supports (repo, issue, pr,
  release, gist, workflow, run, secret, variable, project, ruleset,
  codespace, extension, api, ...), not a hand-curated subset
- Read-only by default: commands that change state on GitHub
  (`create`, `delete`, `edit`, `close`, `merge`, `comment`, etc., or
  `gh api` calls with `--method` other than `GET`) are refused unless
  `GHCLI_MCP_ALLOW_WRITE=true` is set on the server *and* `confirm=true`
  is passed on the specific call. Local-only side effects (clone,
  checkout, download) are never gated
- Optional `GHCLI_MCP_ALLOWED_REPOS` allowlist to further scope which
  `owner/repo` values are reachable, when a call passes `-R`/`--repo`
- Auth via the gh CLI's own credential resolution (`gh auth login`
  keychain storage, or `GH_TOKEN`/`GITHUB_TOKEN`) — no credential
  handling in this connector itself
- `markdown`/`json` response formats on every tool
- `--version` flag; ldflag-injected build version

### 🧪 Quality

- Unit tests (including the state-changing-command classifier and every
  safety gate), `go vet`, `gofmt`, `golangci-lint` (including gosec),
  `govulncheck`, and CodeQL all wired into CI
- End-to-end verified against real GitHub during development:
  `ghcli_whoami` and `ghcli_help` against the real account, `ghcli_exec`
  reads (`repo view`, `issue list`), confirmed the read-only and
  allowlist gates block correctly, and confirmed the write-gate
  genuinely opens the pipe to GitHub once both
  `GHCLI_MCP_ALLOW_WRITE=true` and `confirm=true` are set — verified via
  a real create+close issue cycle against a dedicated, disposable
  sandbox repo

### 🤝 Project infrastructure

- Contribution guide, Code of Conduct, security policy, issue/PR templates
- Branch protection: all changes (including the maintainer's) land via
  reviewed, CI-green pull requests
- Automated semver releases via [release-please](https://github.com/googleapis/release-please),
  starting from this baseline
- Cross-platform (linux/darwin/windows × amd64/arm64) zipped plugin
  bundles attached to every release

---

*From here on, this file is maintained automatically by release-please
based on [Conventional Commits](https://www.conventionalcommits.org/).*
