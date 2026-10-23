# Copilot Instructions

<sub>🤖 Remote-native guidance drafted with AI assistance</sub>

## Build, test, and lint

```bash
go build ./cmd/gh-copilot-codespace
go vet ./...
go test -race ./...
go test -race -run TestParseInput ./internal/ssh
```

Integration tests require a real Codespace and `gh` authentication. Run `./scripts/integration-test.sh` locally, then use `gh signoff integration`.

## Architecture

The single Go binary operates in four modes:

1. **Launcher** (`gh-copilot-codespace [flags]`) — `cmd/gh-copilot-codespace/main.go`
   - Selects zero or more Codespaces, starts them when needed, detects their repository working directories, and establishes multiplexed SSH connections.
   - Deploys the helper binary and runs provisioners for initially connected Codespaces.
   - By default, keeps Copilot in the caller's current checkout with local context and built-in tools.
   - `--remote-native` instead uses a non-repository local host directory, pins ordinary source tools to one initial Codespace, and disables host Git context and ambient native skills.
   - Native launches scope UI settings with a private `COPILOT_HOME` profile, hide local Git/cwd/navigation indicators, and retain host MCP, model/context/usage and permission preferences without rewriting global settings.
   - Installs the stable user-scoped extension, creates a token-protected per-session manifest, and execs Copilot.
   - Default mode forwards unconsumed arguments, including Copilot's `--resume`; remote-native mode rejects context-changing arguments it cannot safely support.
   - Falls back to `gh copilot` when the standalone `copilot` binary is unavailable.

2. **Exec agent** (`gh-copilot-codespace exec`) — `cmd/gh-copilot-codespace/exec.go`
   - Is deployed to each Codespace for structured process execution with working-directory and environment setup.
   - Replaces fragile remote shell-command assembly with Go process management.

3. **Extension host** (`gh-copilot-codespace extension-host`) — `cmd/gh-copilot-codespace/extension_host.go`
   - Is spawned locally by the user-scoped Copilot extension.
   - Owns the shared `mcp.ToolRuntime`, exposing its first-party remote and lifecycle tools through a small stdio JSON protocol. Remote-native mode wraps it in `mcp.WorkspaceRuntime` for pinned ordinary tools, scoped local artifacts, and explicit remote skills.
   - Returns `{tools, systemMessage, customAgents}` from `list_tools`; the JavaScript extension forwards these values to `joinSession`.
   - Builds mode-specific guidance: default mode uses local context and explicit `remote_*` operations; remote-native mode uses the remote source workspace and separates the local host/artifact surfaces.
   - Supplies `@remote-explorer` as an inline custom agent whenever at least one Codespace is connected; no project agent file is generated.
   - Wraps registered `ssh.Client` executors with `daemonclient.Executor` by default. Wrap or dial failures fall back transparently to direct SSH.
   - Native status samples actual Git metadata once per second through the existing pinned live daemon connection into an atomic local cache, with a two-second deadline and no overlapping samples. The one-second renderer never queries local Git or opens SSH per repaint, and reports age/staleness/errors explicitly. Status never wakes/reconnects/replays; only ordinary tools retain recovery. Direct-SSH fallback or executors without passive-query support have no live status.

4. **Sandbox daemon** (`gh-copilot-codespace daemon`) — `cmd/gh-copilot-codespace/daemon.go`
   - Runs as a long-lived stdio JSON server inside each connected Codespace.
   - Implements all `ssh.Executor` verbs natively; file content travels in JSON frames rather than through a base64 shell hop.
   - Dispatches concurrent requests to goroutines and serializes response writes with one mutex.
   - Starts bash children in their own process group so cancellation terminates the full command tree.
   - Refreshes process environment from `/workspaces/.codespaces/shared/.env-secrets` for every request.

## Context and tool routing

- In default mode, Copilot stays in the current local checkout. In `--remote-native` mode, it runs in a separate non-repository local host directory, and ordinary source/command tool names route to the pinned remote workspace.
- After remote-native extension readiness, use only `workspace_skill` from explicit remote `--skill-root` directories; native `/skills` is not that catalog. The stock CLI may enumerate ambient skill metadata before the extension disables native skills, so do not claim zero ambient startup reads.
- Remote-native local output uses scoped artifact tools and explicit import/export. Its `remote_copy` local root is the artifact directory, not the source checkout.
- Stock CLI `!`, `/cwd`, `/diff`, worktree UI, and `@` attachments are not a remote filesystem API. Do not represent them as remote; consult the README for supported host controls and limitations.
- Native `footer`, `tabs`, and `statusLine` configuration is supported independently of `session.ui`. Hiding the local navigation bar does not disable its commands or the local `!` shell. Keep ordinary-mode UI unchanged.
- Preserve local MCP servers and normal permissions. Never bulk-forward local credentials, bypass approval, or assume local sandbox rules enforce remote shell access.
- Ordinary remote-native names are custom extension tools, not locally executing built-ins; retain truthful remote labels and custom-tool permission prompts. Global host startup hooks may still run, so this is not a clean-room profile.
- Local project components are context only and are not synchronized with a Codespace.
- In default mode, local tools remain enabled, but repository implementation work belongs in the remote working copy.
- Use `remote_view`, `remote_edit`, `remote_create`, `remote_apply_patch`, `remote_grep`, and `remote_glob` for repository files.
- Use `remote_bash`, `remote_write_bash`, `remote_read_bash`, `remote_list_bash`, and `remote_stop_bash` for builds, tests, linters, dependency operations, repository scripts, Git, and interactive diagnostics.
- Start with `list_codespaces`, use each target repository's `workdir` as the initial base, and pass explicit `cwd` on every `remote_bash`/`remote_grep`/`remote_glob` call.
- Do not rely on `remote_cd`; it is unavailable to `@remote-explorer`.
- Use `remote_copy` only for an explicit one-time file transfer between the local checkout and `cs://<alias>/<path>`; it is not synchronization.
- Use the inline `@remote-explorer` agent for remote codebase exploration.

## Key packages

- `internal/ssh` — `Client` implements `Executor` over `gh codespace ssh`; asynchronous sessions use tmux.
- `internal/daemonproto` — newline-delimited JSON protocol shared by the daemon and daemon client, including cancellation and error envelopes.
- `internal/daemontransport` — transport abstraction. `SSHTransport` is production; `LocalTransport` supports tests; devcontainer and WSL transports are future seams.
- `internal/daemonclient` — multiplexes requests over a transport, demultiplexes responses by ID, and satisfies `ssh.Executor`.
- `internal/registry` — maps Codespace aliases to `ManagedCodespace` instances and their executors.
- `internal/mcp` — retains the shared tool definitions, handlers, lifecycle state, and transport-neutral `ToolRuntime` used by the extension host.
- `internal/provisioner` — applies built-in and user-defined setup when Codespaces are connected.

## Conventions

- `ssh.Executor` is the testing seam for tool handlers; tests use mock executors rather than real SSH.
- Extension-host tool calls return transport-neutral `RuntimeCallResult` values.
- Tool handlers report operational failures as tool results instead of protocol errors.
- Async bash sessions use tmux names prefixed with `copilot-`.
- `remote_view` mirrors local `view` capabilities for ranges, directory listings, large-file overrides, and image metadata.
- `remote_bash`, `remote_grep`, and `remote_glob` should receive explicit working directories when parallel ordering matters.
- `@remote-explorer` can use the command-capable exploration set (`remote_grep`, `remote_glob`, `remote_view`, `remote_bash`, `remote_write_bash`, `remote_read_bash`, `remote_stop_bash`, `remote_list_bash`, `remote_cwd`, `list_codespaces`) and does not use `remote_cd` or lifecycle/mutation tools such as `list_available_codespaces`, `get_codespace_options`, `create_codespace`, `connect_codespace`, `delete_codespace`, `remote_edit`, `remote_create`, `remote_copy`, `remote_apply_patch`, or `open_shell`.
- The launcher uses `syscall.Exec`, so it does not remain resident after Copilot starts.
- The user-scoped extension activates only when its per-session manifest and token validate; unrelated Copilot sessions receive no tools from it.
- Daemon-backed remote tools probe connections on demand. A stopped Codespace is automatically woken, its SSH multiplexing is refreshed, and the daemon reconnects.
- Never replay an operation after an in-flight connection loss because its remote outcome may be unknown. Only a preflight failure before the operation is sent may recover transparently and execute once.

## Installation

Install as a `gh` extension with `gh extension install ekroon/gh-copilot-codespace`, or with mise using `mise use -g github:ekroon/gh-copilot-codespace`.

## Release flow

Every push to `main` runs vet, tests, and cross-platform builds and creates a `dev-{sha}` pre-release on success. Semantic-version tags create extension releases. Promote to `latest` only after integration signoff by running the **Promote to Latest** workflow.
