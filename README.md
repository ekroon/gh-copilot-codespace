# gh-copilot-codespace

Launch Copilot CLI with all file/bash operations executing on remote GitHub Codespace(s) via SSH. Supports multiple codespaces, session resume, selected-only restrictions, and on-demand codespace creation.

## How it works

A single Go binary (`gh-copilot-codespace`) serves four roles:

1. **Launcher mode** (default) — Lists your codespaces, lets you pick one or more, starts them if needed, deploys the exec agent, fetches instruction files and project-level components, then launches `copilot` with:
    - `--excluded-tools` — disables local shell/search tools
    - `--additional-mcp-config` — adds itself as the MCP server (plus any remote MCP configs), unless first-party tools are supplied by the experimental extension mode

2. **MCP server mode** (`gh-copilot-codespace mcp`) — Spawned by copilot, provides 17 remote tools over SSH:
    - `remote_view`, `remote_edit`, `remote_create` — file operations
    - `remote_bash` (session-backed fast path + async), `remote_grep`, `remote_glob` — commands & search
    - `remote_write_bash`, `remote_read_bash`, `remote_stop_bash`, `remote_list_bash` — async session management (tmux-based)
    - `remote_cd`, `remote_cwd` — default working directory navigation
    - `list_codespaces`, `create_codespace`, `connect_codespace`, `delete_codespace` — codespace lifecycle
    - `open_shell` — open interactive SSH session

3. **Exec agent** (`gh-copilot-codespace exec`) — Deployed to the codespace at startup. Provides structured command execution with workdir/env setup, replacing fragile shell escaping in SSH forwarding.

4. **Extension host** (`gh-copilot-codespace extension-host`) — Used by generated Copilot CLI extensions when `--extension-tools` is enabled. It exposes the same first-party remote tools through the extension API while keeping tool state in one long-lived helper process.

5. **Workspace management** (`gh-copilot-codespace workspaces`) — Lists and manages workspace sessions for `--resume`.

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

# Keep Copilot in the current local directory while adding remote tools
gh copilot-codespace --here -c my-codespace-name

# Same, while passing Copilot CLI flags such as conversation resume
gh copilot-codespace --here -c my-codespace-name -- --resume <copilot-session>

# Start non-interactively with no codespaces, then create/connect from the agent
gh copilot-codespace --no-codespace --name bootstrap-session

# Restrict existing-codespace access to the codespaces selected at startup
gh copilot-codespace --selected-only

# Start a restricted bootstrap session with no existing codespaces selected
gh copilot-codespace --no-codespace --selected-only --name restricted-bootstrap

# Name the session for later resume
gh copilot-codespace --name my-session

# Resume a previous session by name
gh copilot-codespace --resume my-session

# Resume and keep local bash/grep/glob tools enabled too
gh copilot-codespace --resume my-session --local-tools

# Experimental: register first-party remote tools through a generated Copilot extension
gh copilot-codespace -c my-codespace-name --extension-tools

# Resume by choosing from saved sessions
gh copilot-codespace --resume

# List workspace sessions
gh copilot-codespace workspaces

# Pass extra copilot flags
gh copilot-codespace --model claude-sonnet-4.5
```

If you launch without `-c/--codespace` or `--no-codespace`, the interactive picker supports selecting multiple codespaces. Press Enter without toggling any codespaces to start with no codespaces connected, or use `--no-codespace` to skip the picker entirely for non-interactive launches. In unrestricted sessions, you can then use `list_available_codespaces`, `create_codespace`, or `connect_codespace` from the agent. In `--selected-only` sessions, existing-codespace access is limited to the codespaces selected at startup, and a zero-selection launch becomes create-only until you create a codespace.

## Current-directory mode

Use `--here` when you already have useful local context (local WIP, a plan, local instructions, or an existing Copilot conversation) and want to add Codespaces tools without moving Copilot into the generated mirror directory.

In `--here` mode, Copilot starts in the current local directory, local tools stay enabled by default, and remote tools are still available through the Codespaces MCP server. Remote project instructions, hooks, skills, and forwarded remote MCP configs are **not** mirrored into the current repository in this mode, so generated files are not written into your worktree.

The local and remote checkouts are separate worktrees. Copying between them is explicit with `remote_copy`:

```text
remote_copy(source="src/app.go", destination="cs://github/src/app.go")
remote_copy(source="cs://github/src/generated.go", destination="src/generated.go")
```

The `github` segment is the connected codespace alias shown by `list_codespaces`. `remote_copy` refuses to overwrite destination files unless `overwrite=true`; each copy is a one-time transfer, not synchronization.

## Selected-only sessions

`--selected-only` restricts access to **existing** codespaces. It does not disable `create_codespace`; it narrows which already-existing codespaces the agent can discover or attach to.

| Tool | Behavior in `--selected-only` sessions |
|---|---|
| `list_available_codespaces` | Shows only the existing codespaces selected at startup. After `--resume`, it also shows codespaces that were created from inside that session and preserved in the session allowlist. |
| `connect_codespace` | Can attach only existing codespaces on that allowlist. Other existing codespaces stay hidden from `list_available_codespaces` and are rejected if you try to connect to them directly. |
| `create_codespace` | Always remains available. Newly created codespaces are connected immediately and added to the allowlist for future `--resume` sessions. |

If you start with `--no-codespace --selected-only` (or leave the picker empty with the flag enabled), no existing codespaces are allowlisted. That session is **create-only** for adding codespaces: `list_available_codespaces` returns no connectable existing codespaces, and `connect_codespace` rejects existing codespaces until you create one with `create_codespace`.

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

With `--here`, this remote component mirroring is skipped to avoid writing generated instructions, hooks, agents, or skills into the current repository.

## Experimental extension tools

`--extension-tools` registers this project’s first-party Codespaces tools through a generated Copilot CLI extension instead of the first-party `codespace` MCP server. The user-facing tool names stay the same (`remote_bash`, `remote_view`, `create_codespace`, and the rest of the first-party remote tools), and the generated extension delegates calls to a long-lived `gh-copilot-codespace extension-host` helper so working-directory changes, connected codespaces, and lifecycle state persist across calls.

Remote project MCP configs are still forwarded through `--additional-mcp-config`; extension mode only replaces this project’s built-in Codespaces tool server. In normal mirrored-workspace launches, the generated extension is written under the mirror’s `.github/extensions/copilot-codespace/`. In `--here` mode, the launcher installs or updates one stable user-scoped wrapper extension at `~/.copilot/extensions/copilot-codespace/extension.mjs`, then passes a per-session manifest path and token through the Copilot process environment. The wrapper registers tools only when that manifest/token pair validates, so unrelated Copilot sessions load no tools from it. Legacy per-session generated user extension directories older than 24 hours are cleaned up on later `--extension-tools --here` launches.

## Multi-codespace support

When connecting to multiple codespaces, all first-party `remote_*` tools accept an optional `codespace` parameter (the alias), whether they are supplied by MCP or by the generated extension. When only one codespace is connected, this parameter is optional.

For `remote_bash`, `remote_grep`, and `remote_glob`, prefer passing `cwd` explicitly when you need predictable behavior across parallel tool calls. `remote_cd` still updates the default cwd for later sequential calls, but it should not be treated as an ordering dependency inside a parallel batch.

`remote_copy` uses aliases in `cs://<alias>/<path>` endpoints. For example, `cs://api/src/server.go` copies to or from the codespace connected as alias `api`.

The agent can also create, connect to, and delete codespaces on the fly using `create_codespace`, `connect_codespace`, and `delete_codespace` tools. Starting with zero connected codespaces is supported, so you can bootstrap a brand-new session and create the first codespace from inside the agent. With `--selected-only`, that zero-codespace bootstrap flow stays create-first unless you already preserved codespaces selected at startup or created from the session in the resumed allowlist.

## Session resume

Workspace sessions are saved to `~/.copilot/workspaces/` with a manifest (`workspace.json`) tracking connected codespaces. Empty sessions are resumable too, which is useful when you want to launch first and create/connect codespaces later from the agent. Use `--resume` to reconnect by name, or pass bare `--resume` to choose interactively from saved sessions:

```bash
# First session
gh copilot-codespace --name my-feature -c my-codespace

# Later: resume by name
gh copilot-codespace --resume my-feature

# Or choose from saved sessions
gh copilot-codespace --resume
```

`gh copilot-codespace workspaces` now shows richer workspace metadata including repositories, codespace names, branches, the local workspace path, and recent activity time. The interactive `--resume` picker also includes that metadata in each entry so you can search on it directly.

Local files created in the workspace `files/` directory persist across sessions.

Workspace manifests also persist session behavior, including `--local-tools` and the selected-only access policy. Resume uses those saved settings by default.

You can override persisted booleans on resume:

- bare `--local-tools` / `--selected-only` still mean `true`
- `--local-tools=true|false`
- `--selected-only=true|false`

Launch identity flags are still not valid with resume: `--codespace`, `--workdir`, and `--name` are creation-time inputs, while resume reuses the saved workspace session and its persisted codespace metadata.

When `--selected-only` was enabled, resume preserves the allowlist too: the **existing** codespaces selected at startup stay eligible, and any codespaces created from inside that session stay eligible as well. Resuming does not reopen access to other pre-existing codespaces that were not selected at startup.

## Custom provisioners

Provisioners run custom setup on codespaces after connection or creation. Built-in provisioners handle terminal info upload and git fetch automatically.

Provisioners are loaded in both places:
- when the launcher initially connects to selected/resumed codespaces
- when the MCP lifecycle tools (`create_codespace` / `connect_codespace`) attach new codespaces later in the session

For Ghostty, you usually do **not** need to copy your Ghostty config file into the codespace. The built-in `terminfo` provisioner uploads local terminfo entries into the codespace; when Ghostty is detected it always uploads `xterm-ghostty`, even if you override `$TERM` locally. That matches the intent of the legacy shell script. If you want extra Ghostty-specific setup beyond that, add a custom provisioner matched on `"terminal": "xterm-ghostty"`; Ghostty sessions are normalized to that detected terminal name for matching too. Custom `bash` provisioners still run on the codespace itself; the local-to-remote terminfo upload is specific to the built-in provisioner.

Add custom provisioners in `~/.config/copilot-codespace/provisioners.json`:

```json
{
  "builtins": {
    "terminfo": true,
    "git-fetch": true
  },
  "provisioners": [
    {
      "name": "ghostty-shell-setup",
      "bash": "echo 'export BAT_THEME=GitHub' >> ~/.bashrc",
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

Set a built-in to `false` if you want to disable it entirely.

| Field | Description |
|-------|-------------|
| `builtins.terminfo` | Enable/disable the built-in terminfo upload provisioner (default: enabled) |
| `builtins.git-fetch` | Enable/disable the built-in `git fetch origin` provisioner (default: enabled) |
| `name` | Provisioner name (shown in logs) |
| `bash` | Command to run on the codespace via SSH |
| `match.terminal` | Only run when the detected local terminal matches (e.g., Ghostty normalizes to `"xterm-ghostty"` even if `$TERM` is overridden) |
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
