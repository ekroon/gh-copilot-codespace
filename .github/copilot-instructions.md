# Copilot Instructions

## Build, test, and lint

```bash
go build ./cmd/gh-copilot-codespace    # build the binary
go vet ./...                        # lint
go test -race ./...                 # all tests
go test -race -run TestParseInput ./internal/ssh  # single test
```

Integration tests require a real codespace and `gh` CLI auth — run `./scripts/integration-test.sh` locally (not in CI). Sign off with `gh signoff integration` after they pass.

## Architecture

Single Go binary that operates in five modes, selected by the first argument:

1. **Launcher** (`gh-copilot-codespace [flags]`) — `cmd/gh-copilot-codespace/main.go`
   - Lists codespaces via `gh`, picks one, starts it if stopped
   - Deploys exec agent binary to the codespace (`deploy.go`)
   - Fetches project-level components (instructions, skills, agents, commands, hooks, MCP configs) into a local mirror dir
   - Rewrites MCP servers and hooks for SSH forwarding (using remote exec agent when available)
   - Execs `copilot` with `--excluded-tools` (disabling 10 local file/shell tools) and `--additional-mcp-config` (adding itself as the MCP server)
   - Falls back to `gh copilot` if standalone `copilot` binary is not in PATH

2. **MCP server** (`gh-copilot-codespace mcp`) — `internal/mcp/server.go`
   - Spawned by Copilot CLI as a child process, communicates via stdio JSON-RPC
   - Provides `remote_*` tools (view, edit, create, bash, grep, glob, write/read/stop/list_bash) that mirror Copilot's built-in local tools
   - Delegates all operations to `ssh.Executor` interface

3. **Exec agent** (`gh-copilot-codespace exec`) — `cmd/gh-copilot-codespace/exec.go`
   - Deployed to codespace at startup, used for structured remote command execution
   - `exec [--workdir DIR] [--env K=V]... -- COMMAND [ARGS...]`
   - Replaces fragile `bash -c 'cd WD && export K=V && exec CMD'` shell assembly with proper Go process management

4. **Extension host** (`gh-copilot-codespace extension-host`) — `cmd/gh-copilot-codespace/extension_host.go`
   - Spawned locally by the generated Copilot CLI extension under `--extension-tools`
   - On `list_tools` returns a `{tools, systemMessage, customAgents}` bootstrap payload — the JS extension forwards `systemMessage` (codespace context preamble) and `customAgents` (inline `@remote-explorer` agent) to `joinSession` in addition to tools
   - Mode is selected from `COPILOT_CODESPACE_EXTENSION_MODE` (`mirror`/`here`/`resume`) so the preamble wording matches the launch flavor
   - `remoteExplorerInlineAgent` (in `remote_explorer.go`) is the single source of truth for the sub-agent; the on-disk variant `.github/agents/remote-explorer.agent.md` is generated only for MCP mode
   - On startup wraps each registered `ssh.Client` with a `daemonclient.Executor` (see daemon mode below) unless `COPILOT_CODESPACE_NO_DAEMON=1` is set. Wrap-and-dial failures fall back transparently to the original `ssh.Client`.

5. **Sandbox daemon** (`gh-copilot-codespace daemon`) — `cmd/gh-copilot-codespace/daemon.go`
   - Long-lived stdio JSON-RPC server inside the sandbox (codespace). Reads `daemonproto` frames from stdin, writes responses to stdout.
   - All 12 `Executor` verbs implemented natively (no base64-over-bash). `run_bash` spawns children with `Setpgid: true` so cancel frames SIGTERM the whole process group.
   - Concurrent requests dispatched to goroutines; the response writer is serialized through a single mutex.
   - Per-request env refresh from `/workspaces/.codespaces/shared/.env-secrets` via `codespaceenv.ApplyProcessBootstrap`.

Key packages:
- `internal/ssh` — `Client` implements `Executor` by running commands over SSH (via `gh codespace ssh` or multiplexed ControlMaster). Async sessions use tmux on the codespace. Used directly by MCP mode and as the byte-stream substrate for extension-mode's daemon transport.
- `internal/daemonproto` — wire protocol shared by daemon and DaemonExecutor. Frame envelope, 12 verbs, six error codes, Encoder/Decoder over newline-delimited JSON (chose `json.Decoder` over `bufio.Scanner` so large file payloads don't hit token limits). Mutating verbs reserve an `idempotency_key` field for future dedup.
- `internal/daemontransport` — `Transport` interface (`Deploy(ctx) → remotePath`, `Spawn(ctx, remotePath) → io.ReadWriteCloser`, `Close`, `Name`). Today: `SSHTransport` (production, reuses ControlMaster multiplexing) + `LocalTransport` (test-only). Stubs: `DevContainerTransport`, `WSLTransport`. The Transport abstraction is the seam that makes future devcontainer/WSL/gh-sandbox targets purely additive.
- `internal/daemonclient` — `Executor` satisfies `ssh.Executor` by speaking `daemonproto` over any Transport. Reader goroutine demuxes responses by id; ctx cancellation issues a Cancel frame and returns `context.Canceled`. Compile-time check (`var _ ssh.Executor = (*Executor)(nil)`) keeps the interface in lock-step.
- `internal/registry` — `Registry` maps codespace aliases to `ManagedCodespace` instances, each with its own `ssh.Executor`. Supports multi-codespace sessions.
- `internal/workspace` — Manages local workspace sessions with `workspace.json` manifests for `--resume` support.
- `internal/provisioner` — Provisioner interface for custom codespace setup (terminfo upload, git fetch, user-defined hooks).

## Conventions

- The `ssh.Executor` interface is the seam for testing MCP handlers — tests use `mockExecutor` (defined in `server_test.go`), not real SSH.
- File transfers use base64 encoding over SSH for the MCP path (`base64 < file` to read, `echo <b64> | base64 -d > file` to write). The daemon path transfers raw bytes inside JSON frames — no base64 hop.
- Async bash sessions are backed by tmux on the codespace. Session names are prefixed with `copilot-` (see `tmuxPrefix` constant). The daemon's session verbs shell out to the same tmux commands.
- MCP tool handlers never return Go errors — they return `toolError()` results with `IsError: true` so the MCP protocol layer stays clean.
- The binary uses `syscall.Exec` to replace itself with `copilot` (or `node` for `--experimental-shell`), so the launcher process doesn't stay resident.
- Daemon mode and MCP mode are independent. MCP stays on plain `ssh.Client` deliberately (blast-radius minimization); extension-tools mode is opt-in and the daemon transport runs only there. Both consume the same `ssh.Executor` interface, so swapping MCP to the daemon later is a one-line registry change.

## Installation

Installable as a gh CLI extension (`gh extension install ekroon/gh-copilot-codespace`) or via mise (`mise use -g github:ekroon/gh-copilot-codespace`).

## Release flow

Every push to `main` triggers CI (vet, test, cross-platform build). If CI passes, a pre-release (`dev-{sha}`) is created. Pushing a semver tag (`v*`) triggers a `cli/gh-extension-precompile` release for gh extension users. The `latest` tag is only updated by running the "Promote to Latest" workflow (`gh workflow run promote-to-latest.yml`) after `gh signoff integration` has been run against the commit.
