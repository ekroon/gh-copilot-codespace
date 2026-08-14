# gh-copilot-codespace

Launch Copilot CLI in your current local checkout with tools for working on one or more GitHub Codespaces over SSH.

## How it works

The launcher:

1. Selects, starts, and connects to Codespaces.
2. Deploys the helper binary and establishes multiplexed SSH connections.
3. Installs a user-scoped Copilot extension and starts its local extension host.
4. Launches Copilot without changing directories.

Copilot remains in the checkout where you ran `gh copilot-codespace`. Local instructions, agents, skills, and other project files are the source of session context. Nothing is fetched from a Codespace into a generated mirror, and no project agents, hooks, or remote server configuration are generated locally.

All built-in local tools remain enabled. The extension adds the first-party Codespaces tools, but repository implementation must happen in the Codespace working copy:

- Use `remote_view`, `remote_edit`, `remote_create`, `remote_apply_patch`, `remote_grep`, and `remote_glob` for repository files.
- Use `remote_bash`, `remote_write_bash`, `remote_read_bash`, `remote_list_bash`, and `remote_stop_bash` for builds, tests, linters, dependency operations, repository scripts, Git commands, and interactive diagnostics.
- Use `list_codespaces` first and pass the reported `workdir` as explicit `cwd` on each remote command call.
- Reserve local tools for local context, Copilot session artifacts, and explicit local-only work.

The local and remote checkouts are separate and are not synchronized.

## Runtime architecture

The Go binary has four modes:

1. **Launcher** (default) — connects Codespaces, prepares the extension session, and replaces itself with Copilot.
2. **Exec agent** (`gh-copilot-codespace exec`) — deployed to a Codespace for structured remote process execution.
3. **Extension host** (`gh-copilot-codespace extension-host`) — exposes the 21 first-party remote and lifecycle tools through the Copilot extension API.
4. **Sandbox daemon** (`gh-copilot-codespace daemon`) — runs inside each connected Codespace and handles remote operations over a long-lived JSON protocol stream.

The stable user-scoped extension at `~/.copilot/extensions/copilot-codespace/extension.mjs` activates only when the launcher supplies a valid per-session manifest and token. It forwards the extension host's tools, appended `systemMessage`, and inline `@remote-explorer` agent to `joinSession`.

The `systemMessage` always explains that local files provide context while repository work belongs on the Codespace, and it supplies remote-tool routing guidance. When at least one Codespace is connected, the extension also registers the inline `@remote-explorer` agent for remote codebase exploration. These are delivered through the extension API rather than generated project files.

The extension host wraps each Codespace SSH executor with a daemon client by default. Calls share one multiplexed SSH stream, avoiding a new SSH process and shell setup for every operation. If daemon deployment or connection fails, the host falls back to direct SSH. Set `COPILOT_CODESPACE_NO_DAEMON=1` to force that fallback.

Daemon-backed remote tools probe the connection on demand. If a connected Codespace was stopped, the next tool call automatically wakes it, refreshes stale SSH multiplexing, and reconnects the daemon. Operations are never replayed after an in-flight connection loss because their remote outcome may be unknown; the error reports whether recovery succeeded so the next call can continue on the restored connection.

## Prerequisites

- `gh` CLI authenticated with `codespace` scope
- Permission to list, create, and connect GitHub Codespaces
- [Copilot CLI](https://docs.github.com/copilot/how-tos/copilot-cli) installed, or available through `gh copilot`

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
# Interactive picker (select zero, one, or many)
gh copilot-codespace

# Connect to a specific Codespace
gh copilot-codespace -c my-codespace-name

# Connect to multiple Codespaces
gh copilot-codespace -c codespace-1,codespace-2

# Start without a Codespace, then create or connect from the agent
gh copilot-codespace --no-codespace

# Restrict existing-Codespace access to the startup selection
gh copilot-codespace --selected-only

# Forward Copilot CLI arguments directly
gh copilot-codespace -c my-codespace-name --resume <copilot-session>
gh copilot-codespace --model claude-sonnet-4.5
```

Arguments not consumed by this launcher are forwarded directly to Copilot. In particular, `--resume` is Copilot's conversation-resume option; this project does not create or manage launcher workspaces.

If neither `-c/--codespace` nor `--no-codespace` is supplied, the interactive picker supports multiple selections. Press Enter without selecting a Codespace to launch with none connected. The agent can then use `list_available_codespaces`, `get_codespace_options`, `create_codespace`, or `connect_codespace`.

## Remote tools

The extension registers:

- `remote_view`, `remote_edit`, `remote_create`
- `remote_apply_patch`
- `remote_bash`, `remote_grep`, `remote_glob`
- `remote_write_bash`, `remote_read_bash`, `remote_stop_bash`, `remote_list_bash`
- `remote_cd`, `remote_cwd`
- `remote_copy`
- `list_codespaces`, `list_available_codespaces`
- `get_codespace_options`, `create_codespace`, `connect_codespace`, `delete_codespace`
- `open_shell`

`@remote-explorer` can use the command-capable exploration set: `remote_grep`, `remote_glob`, `remote_view`, `remote_bash`, `remote_write_bash`, `remote_read_bash`, `remote_stop_bash`, `remote_list_bash`, `remote_cwd`, and `list_codespaces`. It starts from `list_codespaces.workdir`, passes `cwd` explicitly on every `remote_bash`/`remote_grep`/`remote_glob` call, and does not use `remote_cd`, `remote_edit`, `remote_create`, `remote_copy`, `remote_apply_patch`, lifecycle discovery/mutation tools, or `open_shell`.

- `remote_view` mirrors the local `view` surface with line ranges, `forceReadLargeFiles`, directory listings, image results, and binary metadata.
- `remote_create` creates parent directories and refuses to overwrite an existing file.
- `remote_grep` mirrors local `rg` options including `path`/`paths`, `output_mode`, `-i`, `-A`, `-B`, `-C`, `-n`, `head_limit`, and `multiline`.
- `remote_glob` mirrors local `glob` path selection with `path`/`paths`; results default to 1,000 matches, accept an optional `limit` capped at 10,000, and report truncation in structured metadata.
- `remote_apply_patch` accepts the canonical `apply_patch` payload and applies it atomically on the Codespace.
- `remote_bash` uses a lightweight daemon-managed process for normal sync commands. Commands that exceed `initial_wait` keep running once under the returned `shellId`; use `mode=async` when stdin or a PTY is required. Use `remote_write_bash`, `remote_read_bash`, `remote_list_bash`, and `remote_stop_bash` to manage those longer-lived sessions.

### Explicit file transfer

`remote_copy` performs an explicit one-time transfer between the current local checkout and a connected Codespace:

```text
remote_copy(source="src/app.go", destination="cs://github/src/app.go")
remote_copy(source="cs://github/src/generated.go", destination="src/generated.go")
```

The `github` segment is the alias shown by `list_codespaces`. The tool copies files only, refuses to overwrite by default, and never establishes synchronization between checkouts.

## Multi-Codespace support

All repository `remote_*` tools accept an optional `codespace` alias. The alias is optional when exactly one Codespace is connected.

```text
remote_bash(codespace="api", cwd="/workspaces/api", command="go test ./...")
remote_view(codespace="web", path="/workspaces/web/src/app.ts")
```

Use `list_codespaces` to see connected aliases, repositories, branches, and working directories. Lifecycle tools can create, connect, and delete Codespaces during the session, but `@remote-explorer` does not use them.

## Selected-only sessions

`--selected-only` limits access to existing Codespaces. It does not disable Codespace creation.

| Tool | Behavior |
|---|---|
| `list_available_codespaces` | Shows only existing Codespaces selected at startup. |
| `connect_codespace` | Can attach only an existing Codespace on that allowlist. |
| `create_codespace` | Remains available; a newly created Codespace is connected immediately. |

Starting with `--no-codespace --selected-only` creates a create-first session: no existing Codespaces are discoverable or connectable, but the agent can create a new one.

## Custom provisioners

Provisioners run setup after the launcher connects a Codespace and after extension lifecycle tools create or connect one. Built-in provisioners upload terminal information and run `git fetch origin`.

For Ghostty, the built-in terminal provisioner uploads `xterm-ghostty`. Custom `bash` provisioners run on the Codespace.

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
      "name": "github-setup",
      "bash": "cd /workspaces/github && bin/setup",
      "match": { "repository": "github/github" }
    }
  ]
}
```

| Field | Description |
|---|---|
| `builtins.terminfo` | Enable or disable terminal information upload (enabled by default) |
| `builtins.git-fetch` | Enable or disable `git fetch origin` (enabled by default) |
| `name` | Provisioner name shown in logs |
| `bash` | Command run on the Codespace |
| `match.terminal` | Run only for the detected local terminal |
| `match.repository` | Run only for the specified repository |

Provisioners without `match` run on every Codespace. Errors are logged but do not block connection.

## Development

```bash
go build ./cmd/gh-copilot-codespace
go vet ./...
go test -race ./...
```

Integration tests require a real Codespace and `gh` authentication:

```bash
./scripts/setup-signoff.sh
./scripts/integration-test.sh
gh signoff integration
```

## Release flow

Every push to `main` runs vet, tests, and cross-platform builds. A successful build creates a `dev-{sha}` pre-release.

Pushing a semantic-version tag triggers a release for `gh extension` users. To promote a development release to `latest` for mise users, run the **Promote to Latest** workflow after integration signoff.

## Environment variables

| Variable | Description |
|---|---|
| `CODESPACE_REGISTRY` | Connected Codespace metadata supplied to the extension host |
| `CODESPACE_LOCAL_WORKDIR` | Current local checkout used as the allowed local root for `remote_copy` |
| `CODESPACE_LIFECYCLE_CONFIG` | Per-session lifecycle access policy |
| `COPILOT_CODESPACE_EXTENSION_TOKEN` | Token activating the user-scoped extension for this launch |
| `COPILOT_CODESPACE_EXTENSION_MANIFEST` | Path to the per-session extension manifest |
| `COPILOT_CODESPACE_NO_DAEMON` | Set to `1` to use direct SSH instead of the sandbox daemon |
