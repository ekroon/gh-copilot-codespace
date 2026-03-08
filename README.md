# gh-copilot-codespace

Launch Copilot CLI with all file/bash operations executing on remote GitHub Codespace(s) via SSH. Supports multiple codespaces, session resume, on-demand codespace creation, and an opt-in headless delegate lane for autonomous remote Copilot work.

## How it works

A single Go binary (`gh-copilot-codespace`) serves four roles:

1. **Launcher mode** (default) — Lists your codespaces, lets you pick one or more, starts them if needed, deploys the exec agent, fetches instruction files and project-level components, then launches `copilot` with:
   - `--excluded-tools` — disables local shell/search tools
   - `--additional-mcp-config` — adds itself as the MCP server (plus any remote MCP configs)

2. **MCP server mode** (`gh-copilot-codespace mcp`) — Spawned by copilot, provides 17 remote tools over SSH:
    - `remote_view`, `remote_edit`, `remote_create` — file operations
    - `remote_bash` (session-backed fast path + async), `remote_grep`, `remote_glob` — commands & search
    - `remote_write_bash`, `remote_read_bash`, `remote_stop_bash`, `remote_list_bash` — async session management (tmux-based)
    - `remote_cd`, `remote_cwd` — default working directory navigation
    - `list_codespaces`, `create_codespace`, `connect_codespace`, `delete_codespace` — codespace lifecycle
     - `open_shell` — open interactive SSH session
     - Optional extra: `delegate_task`, `read_delegate_task`, `cancel_delegate_task` — run and manage remote headless Copilot delegate tasks

3. **Exec agent** (`gh-copilot-codespace exec`) — Deployed to the codespace at startup. Provides structured command execution with workdir/env setup, replacing fragile shell escaping in SSH forwarding.

4. **Workspace management** (`gh-copilot-codespace workspaces`) — Lists and manages workspace sessions for `--resume`.

## Prerequisites

- `gh` CLI authenticated with `codespace` scope
- `gh` permission to list, create, and connect GitHub Codespaces
- [Copilot CLI](https://docs.github.com/copilot/how-tos/copilot-cli) installed (or available via `gh copilot`)

## Installation

```bash
# As a gh extension (recommended)
gh extension install ekroon/gh-copilot-codespace

# With mise
mise use -g github:ekroon/gh-copilot-codespace

# Or build from source
go build -o gh-copilot-codespace ./cmd/gh-copilot-codespace
```

## Quick start

```bash
# Via gh extension — interactive picker (select zero, one, or many)
gh copilot-codespace

# Connect to a specific codespace
gh copilot-codespace -c my-codespace-name

# Connect to multiple codespaces
gh copilot-codespace -c codespace-1,codespace-2

# Start non-interactively with no codespaces, then create/connect from the agent
gh copilot-codespace --no-codespace --name bootstrap-session

# Name the session for later resume
gh copilot-codespace --name my-session

# Resume a previous session
gh copilot-codespace --resume my-session

# Enable the headless delegate extra
gh copilot-codespace --headless-delegate -c my-codespace

# List workspace sessions
gh copilot-codespace workspaces

# Pass extra copilot flags
gh copilot-codespace --model claude-sonnet-4.5
```

If you launch without `-c/--codespace` or `--no-codespace`, the interactive picker supports selecting multiple codespaces. Press Enter without toggling any codespaces to start with no codespaces connected, or use `--no-codespace` to skip the picker entirely for non-interactive launches. From there, use `list_available_codespaces`, `create_codespace`, or `connect_codespace` from the agent.

## What gets fetched from the codespace

The launcher fetches all project-level Copilot CLI components in a single SSH call:

| Component | Remote path | Local handling |
|---|---|---|
| Copilot instructions | `.github/copilot-instructions.md` | Mirrored |
| Scoped instructions | `.github/instructions/*.instructions.md` | Mirrored |
| Agent files | `AGENTS.md`, `CLAUDE.md`, `GEMINI.md` (recursive) | Mirrored |
| **Custom agents** | `.github/agents/*.agent.md`, `.claude/agents/*.agent.md` | Mirrored |
| **Skills** | `.github/skills/`, `.agents/skills/`, `.claude/skills/` (full trees) | Mirrored |
| **Commands** | `.claude/commands/` | Mirrored |
| **Hooks** | `.github/hooks/*.json` | Rewritten for SSH forwarding |
| **MCP servers** | `.copilot/mcp-config.json`, `.vscode/mcp.json`, `.mcp.json`, `.github/mcp.json` | Parsed & forwarded over SSH |

**Skills** include supporting files (scripts, templates) so Copilot can read them during skill loading. Actual script execution happens remotely via `remote_bash`.

**Hooks** have their bash commands rewritten to execute on the codespace via SSH. Stdin/stdout piping through SSH preserves `preToolUse` allow/deny behavior.

**MCP servers** are rewritten to forward stdio over SSH, so remote MCP tools appear as local tools to Copilot.

## Multi-codespace support

When connecting to multiple codespaces, all `remote_*` MCP tools accept an optional `codespace` parameter (the alias). When only one codespace is connected, this parameter is optional.

For `remote_bash`, `remote_grep`, and `remote_glob`, prefer passing `cwd` explicitly when you need predictable behavior across parallel tool calls. `remote_cd` still updates the default cwd for later sequential calls, but it should not be treated as an ordering dependency inside a parallel batch.

The agent can also create, connect to, and delete codespaces on the fly using `create_codespace`, `connect_codespace`, and `delete_codespace` tools. Starting with zero connected codespaces is supported, so you can bootstrap a brand-new session and create the first codespace from inside the agent.

## Headless delegate extra

Pass `--headless-delegate` to enable an additive delegate lane behind the MCP bridge. This exposes:

- `delegate_task` — start a background remote Copilot worker on a codespace
- `read_delegate_task` — read progress and final output
- `cancel_delegate_task` — stop a running delegate task

The delegate lane is opt-in and leaves the default `remote_*` workflow unchanged.

## Session resume

Workspace sessions are saved to `~/.copilot/workspaces/` with a manifest (`workspace.json`) tracking connected codespaces. Empty sessions are resumable too, which is useful when you want to launch first and create/connect codespaces later from the agent. Use `--resume` to reconnect:

```bash
# First session
gh copilot-codespace --name my-feature -c my-codespace

# Later: resume
gh copilot-codespace --resume my-feature
```

Local files created in the workspace `files/` directory persist across sessions.

## Custom provisioners

Provisioners run custom setup on codespaces after connection or creation. Built-in provisioners handle terminal info upload and git fetch automatically.

Add custom provisioners in `~/.config/copilot-codespace/provisioners.json`:

```json
{
  "provisioners": [
    {
      "name": "ghostty-terminfo",
      "bash": "infocmp -x xterm-ghostty 2>/dev/null | tic -x - 2>/dev/null",
      "match": { "terminal": "xterm-ghostty" }
    },
    {
      "name": "my-dotfiles",
      "bash": "curl -fsSL https://raw.githubusercontent.com/me/dotfiles/main/setup.sh | bash"
    },
    {
      "name": "github-setup",
      "bash": "cd /workspaces/github && bin/setup",
      "match": { "repository": "github/github" }
    }
  ]
}
```

| Field | Description |
|-------|-------------|
| `name` | Provisioner name (shown in logs) |
| `bash` | Command to run on the codespace via SSH |
| `match.terminal` | Only run when `$TERM` matches (e.g., `"xterm-ghostty"`) |
| `match.repository` | Only run for this repository (e.g., `"github/github"`) |

Provisioners without `match` run on every codespace. Errors are logged but don't block connection.

## Development

### Running tests

```bash
go test -race ./...
```

### Integration testing & signoff

Integration tests require a real codespace and `gh` CLI authentication. They run locally, not in CI.

```bash
# One-time setup: install gh-signoff
./scripts/setup-signoff.sh

# Run integration tests
./scripts/integration-test.sh

# Sign off on the current commit (sets a GitHub commit status)
gh signoff integration
```

### Release flow

Every push to `main` triggers CI (vet, test, cross-platform build). If CI passes, a pre-release (`dev-{sha}`) is created automatically.

To create a stable release for gh extension users, push a semver tag (e.g., `git tag v0.1.0 && git push origin v0.1.0`). This triggers a release via `cli/gh-extension-precompile` which builds binaries compatible with `gh extension install/upgrade`.

To promote a dev pre-release to `latest` (for mise users), run the "Promote to Latest" workflow from the GitHub Actions tab (or `gh workflow run promote-to-latest.yml`). It checks signoff on the latest main commit and promotes the existing pre-release to `latest`.

## Environment variables

| Variable | Description | Set by |
|---|---|---|
| `CODESPACE_NAME` | Codespace name | Launcher → MCP server |
| `CODESPACE_WORKDIR` | Working directory on codespace | Launcher → MCP server |
| `COPILOT_CUSTOM_INSTRUCTIONS_DIRS` | Temp dir with fetched instruction files | Launcher → copilot |
