# Copilot Instructions

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
   - Keeps Copilot in the caller's current checkout. Local instructions, agents, skills, and files remain the source of context.
   - Leaves all Copilot built-in local tools enabled.
   - Installs the stable user-scoped extension, creates a token-protected per-session manifest, and execs Copilot.
   - Forwards unconsumed arguments, including Copilot's `--resume`, directly to Copilot.
   - Falls back to `gh copilot` when the standalone `copilot` binary is unavailable.

2. **Exec agent** (`gh-copilot-codespace exec`) — `cmd/gh-copilot-codespace/exec.go`
   - Is deployed to each Codespace for structured process execution with working-directory and environment setup.
   - Replaces fragile remote shell-command assembly with Go process management.

3. **Extension host** (`gh-copilot-codespace extension-host`) — `cmd/gh-copilot-codespace/extension_host.go`
   - Is spawned locally by the user-scoped Copilot extension.
   - Owns the shared `mcp.ToolRuntime`, exposing its 20 first-party remote and lifecycle tools through a small stdio JSON protocol.
   - Returns `{tools, systemMessage, customAgents}` from `list_tools`; the JavaScript extension forwards these values to `joinSession`.
   - Always builds current-directory guidance: local files supply project context, while repository reads, edits, commands, tests, dependencies, scripts, and Git operations must use the appropriate `remote_*` tools.
   - Supplies `@remote-explorer` as an inline custom agent whenever at least one Codespace is connected; no project agent file is generated.
   - Wraps registered `ssh.Client` executors with `daemonclient.Executor` by default. Wrap or dial failures fall back transparently to direct SSH.

4. **Sandbox daemon** (`gh-copilot-codespace daemon`) — `cmd/gh-copilot-codespace/daemon.go`
   - Runs as a long-lived stdio JSON server inside each connected Codespace.
   - Implements all `ssh.Executor` verbs natively; file content travels in JSON frames rather than through a base64 shell hop.
   - Dispatches concurrent requests to goroutines and serializes response writes with one mutex.
   - Starts bash children in their own process group so cancellation terminates the full command tree.
   - Refreshes process environment from `/workspaces/.codespaces/shared/.env-secrets` for every request.

## Context and tool routing

- Copilot always stays in the current local checkout.
- Local project components are context only and are not synchronized with a Codespace.
- All local tools remain enabled, but repository implementation work belongs in the remote working copy.
- Use `remote_view`, `remote_edit`, `remote_create`, `remote_grep`, and `remote_glob` for repository files.
- Use `remote_bash` for builds, tests, linters, dependency operations, repository scripts, and Git.
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
- `remote_bash`, `remote_grep`, and `remote_glob` should receive explicit working directories when parallel ordering matters.
- The launcher uses `syscall.Exec`, so it does not remain resident after Copilot starts.
- The user-scoped extension activates only when its per-session manifest and token validate; unrelated Copilot sessions receive no tools from it.

## Installation

Install as a `gh` extension with `gh extension install ekroon/gh-copilot-codespace`, or with mise using `mise use -g github:ekroon/gh-copilot-codespace`.

## Release flow

Every push to `main` runs vet, tests, and cross-platform builds and creates a `dev-{sha}` pre-release on success. Semantic-version tags create extension releases. Promote to `latest` only after integration signoff by running the **Promote to Latest** workflow.
