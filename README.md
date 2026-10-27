# gh-copilot-codespace

Launch Copilot CLI with tools for working on GitHub Codespaces over SSH. The default mode keeps local checkout context; opt into `--remote-native` to make a Codespace the source workspace.

## Remote-native mode

<sub>🤖 Remote-native startup guidance drafted with AI assistance</sub>

```bash
gh copilot-codespace --remote-native -c my-codespace-name

# Create a fresh Codespace for the current GitHub repository, then pin it.
gh copilot-codespace --remote-native --no-codespace

# Or create one for a different repository.
gh copilot-codespace --remote-native --no-codespace --create-repo github/graph-hopper

# Skills are explicit REMOTE directories, not ambient local skill sources.
gh copilot-codespace --remote-native -c my-codespace-name \
  --skill-root /workspaces/my-repo/skills \
  --artifact-dir /absolute/local/output-directory
```

Remote-native mode pins one initial Codespace. With `--no-codespace`, it skips the picker, resolves the current checkout's GitHub repository (or `--create-repo OWNER/REPO`), chooses the smallest available machine, and creates a **new** Codespace with the repository's default branch and devcontainer explicitly supplied to avoid interactive selection. If no default devcontainer is reported, it chooses the first available path in lexical order; if none exist, GitHub's built-in default is used. It never skips a permissions review: if creation requires additional permissions, the launcher opens GitHub's review page in your browser; approve there, then rerun the command. It waits for SSH readiness and connects before Copilot starts. Creation does not copy uncommitted local changes into the Codespace or delete the Codespace when the session ends. Outside remote-native mode, `--no-codespace` still starts without a connection. Additional Codespaces remain accessible through the explicit `remote_*` and lifecycle tools; attaching another target does not silently change the primary workspace. The ordinary `view`, `edit`, `create`, `apply_patch`, `rg`, `glob`, `bash`, and bash-session tools use the existing daemon/SSH backend. Their paths and default cwd stay within the pinned startup workdir; use explicit `remote_*` tools for other remote paths. Remote failures are reported, never retried against the local checkout.

Only `--model`, `--reasoning-effort`, and Copilot's valueless permission flags (`--allow-all`, `--yolo`, `--allow-all-tools`, `--allow-all-paths`, `--allow-all-urls`) are forwarded in this mode. Conversation resume, arbitrary CLI flags, and UI workspace/session switching are not supported; relaunch for a different workspace. Legacy argument forwarding is unchanged.

The stock CLI still runs locally, in a separate non-repository **host directory** rather than the caller's source checkout. Local MCP servers remain local; their configuration is not replaced, and local credentials are not bulk-forwarded to the Codespace. Repository reads, edits, Git commands, builds, and tests belong to the remote backend. This is not a source mirror or automatic synchronization system.

### Explicit skills

Once the extension is ready, native ambient skills are disabled. Personal, project, plugin, and bundled CLI skills are not the configured workspace skill source. Use `workspace_skill({})` to list skills and `workspace_skill({name: "example"})` to load an exact listed name from the repeatable `--skill-root` remote directories. With no roots, no workspace skills are configured. Skill instructions and companion files stay remote.

This suppression takes effect when the extension joins, not through a CLI startup isolation switch. Actual terminal validation observed an ambient skill count during startup before activation; the settled native skill picker then contained only its host-side "Add skill" action. The implementation does not guarantee zero ambient skill enumeration or metadata reads before readiness. Wait for `Remote-native workspace ready` before submitting repository work, and use `workspace_skill`, not the host skill-management UI, for the explicit remote catalog.

Each root contributes its own `SKILL.md` or immediate child skill directories. Files require YAML `name` and `description` frontmatter and are limited to 256 KiB. Duplicate names use the first configured root, then the lexically first path; ignored duplicates are reported. Invalid files or unsafe rooted reads fail explicitly. This catalog is not a recursive scan of all remote skills.

This intentionally uses a separate explicit loader: the stock extension API does not accept the SDK's experimental `skillProvider`. `skillDirectories` alone is additive, not a discovery-isolation switch. The native `/skills` screen is not the workspace skill catalog. Local MCP discovery is preserved instead of disabling all configuration discovery to suppress skills.

### Deliberate local artifacts

The artifact directory is the only local file root exposed by the workspace artifact tools. Without `--artifact-dir`, it is a private sibling of the host directory and its path is printed at launch. Use `artifact_write` and text-only `artifact_view` for deliberate local output, and `artifact_import` / `artifact_export` for explicit one-time transfers:

```text
artifact_write(path="report.txt", content="Deliberate local output")
artifact_view(path="report.txt")
artifact_import(remote_path="generated/report.txt", path="report.txt", overwrite=true)
artifact_export(path="report.txt", remote_path="imported/report.txt")
```

Import means remote workspace to local artifacts; export means local artifacts to remote workspace. The remote paths stay within the pinned workdir. `remote_copy` also uses this scoped artifact root, not the checkout from which the launcher was invoked. Parent traversal and symlink escapes are rejected; existing files are not overwritten unless explicitly requested. Local output is not a reason to run source commands locally.

### Host UI and safety boundaries

The extension uses the supported built-in-tool override API and disables host Git context, local custom-instruction discovery, on-demand instruction discovery, and repository file hooks through session controls. The launcher applies source-aware tool filters before extension discovery: ordinary built-in source tools are excluded, while extension tools, MCP tools, `task`, and `ask_user` remain eligible under normal permissions. A failed extension therefore does not intentionally restore built-in local source tools. Supported CLI settings hide misleading local indicators and add a separately labeled remote status line; tool overrides do **not** replace the stock CLI's entire filesystem or terminal UI.

The ordinary names are model-facing compatibility, not a claim of local execution: `bash`, `view`, and `rg` are Codespace-backed **custom extension tools**, with custom-tool permission handling distinct from the stock built-in shell. Their descriptions and approval dialogs must retain the remote target. Normal confirmations can still be required; the extension neither sets `skipPermission` nor answers approval prompts. Validation instructions should request ordinary model-facing names backed by the Codespace, while forbidding explicit `remote_*` substitutions and unintended local source execution.

The ordinary `bash` description identifies the selected Codespace alias/name and default remote cwd. It executes arbitrary shell commands that may read, write, or delete remote files, run Git, access the network, and use the process environment and credentials available on that target. The description does not classify arbitrary commands as safe or read-only, and approval or human escalation remains the permission system's decision.

This is not a clean-room host profile. Local MCP servers and other configured host integrations remain usable. Global startup hooks can execute before extension session controls apply; actual terminal validation observed a successful global `sessionStart` hook. Disabling native skills does not disable every host plugin or hook.

- Native `/cwd`, `/diff`, `/ide`, worktree commands, `!` shell escapes, and `@` file attachments remain host-side surfaces, not remote equivalents. Hiding navigation does not disable their commands. Do not use them for Codespace repository work.
- A local path shown in a host dialog or session history is not remote context. Use the explicitly labeled remote status, `list_codespaces`, `remote_cwd`, and remote Git commands for remote information; the ordinary tools remain pinned to their startup workdir even after an explicit `remote_cd`.
- Tool overrides do not by themselves prove subagent routing. Keep subagent work on the supplied remote tools and verify the target in runtime evidence.
- Normal permission and trust prompts remain enabled unless you explicitly pass Copilot's `--allow-all`/`--yolo` (or a granular `--allow-all-*`) flag, which then applies to the Codespace-backed tools too. Local filesystem sandbox rules do not automatically become a remote shell sandbox.
- If the extension fails to load or its backend is unavailable, stop repository work rather than using stock local tools as a substitute.

There is no supported extension API here for relabeling every stock UI surface as a remote repository. These limitations are explicit rather than hidden through patches to Copilot internals. Default launcher behavior remains available without `--remote-native`.

### Native UI configuration

The inspected CLI `1.0.101-1` supports `footer`, `tabs`, and `statusLine` settings even though these are not extension `session.ui` controls. These UI settings cannot be repository overrides in `.github/copilot/settings.local.json`. Instead, each native launch creates a private `profile/settings.json` beside its `host` and artifact directories and supplies that profile through the documented `COPILOT_HOME` environment variable.

The profile snapshots the existing user settings, then changes only native UI fields. The official CLI configuration writer handles JSONC before the overlay is applied. Original user settings and existing sessions are not rewritten. Other host resources, including MCP configuration, installed plugins, authentication/state, and host hooks, remain linked to the original configuration root. The profile contains one user-scoped Codespaces extension; neither the original owned extension nor a second project copy is loaded through that profile. This is scoped UI configuration, not a clean-room profile or a new permission policy.

The launcher establishes the host cwd and profile before the CLI starts. The extension joins that already-created session without resupplying cwd, configuration-directory or discovery options. In the inspected SDK, supplying a cwd during resume can rebuild installed-plugin metadata rather than inherit the live activation snapshot, even when the cwd is unchanged. The extension preserves the host's existing plugin/MCP activation instead; snapshot validation is not disabled.

| Surface | Native-mode behavior |
|---|---|
| Local directory, branch, pull request, code-change and CI footer indicators | Hidden with their supported `footer.show*` settings |
| Current/Session, Sessions/Agents, Issues, Pull requests and Gists navigation bar | Entire bar hidden with `tabs.enabled: false`; underlying commands are not disabled |
| Local IDE auto-connection | Disabled with `ide.autoConnect: false`; manual `/ide` remains host-side |
| Model/effort, context, usage/quota, mode and permission indicators | Existing preferences retained |
| Local MCP configuration and UI | Retained; servers keep their own execution locations and normal permissions |
| Custom status line | Remote alias, actual remote branch, observation age and pinned remote cwd |
| New-session `Create on: This computer` / Ctrl+E selector | Still stock host UI; not mapped to the pinned Codespace. No supported hide control was found in the inspected public settings/API |

The status line is live, not the registry's branch-at-connection value. The extension host samples a fixed Git metadata query through its existing pinned daemon connection every second, with a two-second request deadline and one sample in flight; slow samples coalesce rather than overlap. The local status command refreshes every second and on CLI events, rendering only an atomic cache; it does not run local Git, trust the CLI's local cwd/branch payload, or start SSH on every UI repaint. Its age label makes the observation time explicit, and samples expire after 30 seconds. Missing, invalid, expired, disconnected or failed samples show an explicit unknown/error state rather than silently retaining a successful branch. Detached HEAD is identified separately.

Live status is explicitly unavailable with the direct-SSH fallback or an executor without passive-query support; ordinary remote tools still work. Status queries use only the current live daemon channel: they never preflight, wake, reconnect or replay. After connection loss, status remains unknown until an ordinary tool call restores the channel through its existing recovery behavior. A stalled or terminated host cannot leave a branch displayed indefinitely as fresh: the renderer expires its cache independently.

The freshness tradeoff is approximately 60 small remote Git metadata queries and 60 local cache-render commands per minute per active native session (previously 4 and 12). Queries read only the symbolic branch or detached commit, not diffs or index status. The one-second sampling and painting cadences target roughly one to two seconds of healthy-connection UI latency; these are not a hard wall-clock guarantee under network delays or a stalled host.

**Interactive `!` is not disabled.** In the inspected CLI/SDK version `1.0.101-1`, no supported per-session setting or command capability was found for disabling this UI path. `ShellOptions` exposes initialization, process flags, and credentials; the shell RPC exposes user-requested execution and cancellation, not a disable switch. The commands RPC manages slash commands, not `!` interception. Model tool filters and overrides do not control this path: an actual native-session `!` probe returned the isolated local host cwd and Darwin, while ordinary model-facing `bash` returned the Codespace cwd and Linux. Do not use `!` for remote work. No bundled CLI changes, key interception, shell-init sabotage, or permission changes are used to simulate disabling it.

## How it works

In the default mode, the launcher:

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

The user-scoped extension activates only when the launcher supplies a valid per-session manifest and token. Default mode installs it at `~/.copilot/extensions/copilot-codespace/extension.mjs`; native mode installs one copy inside its private profile. Neither native host directories nor the profile contain a second project-scoped copy. It forwards the extension host's tools, appended `systemMessage`, and inline `@remote-explorer` agent to `joinSession`.

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

<sub>🤖 Startup examples drafted with AI assistance</sub>

```bash
# Interactive picker (select zero, one, or many)
gh copilot-codespace

# Connect to a specific Codespace
gh copilot-codespace -c my-codespace-name

# Connect to multiple Codespaces
gh copilot-codespace -c codespace-1,codespace-2

# Start without a Codespace, then create or connect from the agent
gh copilot-codespace --no-codespace

# Connect to a Codespace and run an initial prompt in interactive Autopilot mode
gh copilot-codespace -c my-codespace-name --autopilot --prompt "Inspect the remote workspace"

# Start without a Codespace and let the initial prompt connect one
gh copilot-codespace --no-codespace --autopilot --prompt "Connect to my Codespace and inspect it"

# Limit the agent to the Codespaces selected at startup
gh copilot-codespace --selected-only

# Forward Copilot CLI arguments directly
gh copilot-codespace -c my-codespace-name --resume <copilot-session>
gh copilot-codespace --model claude-sonnet-4.5

# Remote-native without approval prompts (Copilot's --allow-all / --yolo)
gh copilot-codespace --remote-native -c my-codespace-name --yolo
```

`--prompt` starts an interactive Copilot session and submits the prompt immediately; add `--autopilot` for autonomous continuation while retaining normal tool approvals. This is different from Copilot's `-p`, which runs non-interactively; use `-- -p "text"` to forward that option in default mode. The same startup flags work with `--remote-native -c NAME` and `--remote-native --no-codespace`. In the default mode, other arguments not consumed by this launcher are forwarded directly to Copilot. In particular, `--resume` is Copilot's conversation-resume option. Remote-native mode restricts arguments that would restore or redirect local workspace context.

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

Use `list_codespaces` to see connected aliases, repositories, branches, and working directories. In default sessions, lifecycle tools can create, connect, and delete Codespaces during the session, but `@remote-explorer` does not use them.

## Selected-only sessions

`--selected-only` limits the agent to the Codespaces selected at startup. At least one Codespace must be supplied with `-c/--codespace` or chosen in the interactive picker.

| Tool group | Behavior |
|---|---|
| Repository `remote_*` tools | Operate on the connected startup Codespaces. |
| `list_codespaces` | Lists the connected startup Codespaces. |
| `list_available_codespaces`, `get_codespace_options` | Not registered. |
| `create_codespace`, `connect_codespace`, `delete_codespace` | Not registered. The agent cannot create, attach, disconnect, or delete Codespaces through the extension lifecycle tools. |

`--no-codespace --selected-only` and an empty interactive selection are rejected because a selected-only session has no lifecycle tools for adding a Codespace later.

## Custom provisioners

Provisioners run setup after the launcher connects a Codespace and after extension lifecycle tools create or connect one. Built-in provisioners upload terminal information and start `git fetch origin` in the background. Fetch output is written to `~/.local/state/gh-copilot-codespace/git-fetch.log` unless `XDG_STATE_HOME` is set. Branch-specific provisioning only blocks for a fetch when the requested branch is not already available locally.

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
| `builtins.git-fetch` | Enable or disable background `git fetch origin` (enabled by default) |
| `name` | Provisioner name shown in logs |
| `bash` | Command run on the Codespace |
| `match.terminal` | Run only for the detected local terminal |
| `match.repository` | Run only for the specified repository |

Provisioners without `match` run on every Codespace. Errors are logged but do not block connection.

## Development

For a remotely built development version on a non-Linux host, build both the host binary and the matching Linux helper in the Codespace. Export those two artifacts deliberately and set `COPILOT_CODESPACE_LINUX_HELPER` to the absolute local helper artifact path when launching the host binary. The override validates the Linux ELF/Go target architecture and fails rather than substituting another binary when invalid. It does not delete the supplied artifact or copy local source into the Codespace.

Native deployment never implicitly cross-compiles the local checkout, including lifecycle connections made after launch. Without an explicit helper, it can reuse the running binary on matching Linux or download an exact-release helper; otherwise deployment fails with guidance to provide the remotely built artifact. Default-mode development cross-compilation is unchanged.

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
| `COPILOT_CODESPACE_TIMINGS` | Set to enable connection-stage timings on stderr for launcher and lifecycle connect flows |
| `COPILOT_CODESPACE_EXTENSION_TOKEN` | Token activating the user-scoped extension for this launch |
| `COPILOT_CODESPACE_EXTENSION_MANIFEST` | Path to the per-session extension manifest |
| `COPILOT_CODESPACE_NO_DAEMON` | Set to `1` to use direct SSH instead of the sandbox daemon |
| `COPILOT_CODESPACE_LINUX_HELPER` | Explicit absolute local path to a remotely built Linux helper artifact for deployment; validated against the Codespace architecture |
| `CODESPACE_WORKSPACE_CONFIG` | Launcher-generated native workspace configuration: pinned alias, local artifact directory, explicit remote skill roots |
| `COPILOT_HOME` | Native launches use a private UI settings profile while retaining the original host configuration resources |
| `COPILOT_CODESPACE_STATUS_FILE` | Launcher-generated native status cache path; written by the extension host and rendered locally by `native-status` |
