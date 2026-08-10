---
name: remote-tools
description: >-
  Explain how all remote Codespace tools work for codespace development. Use this skill when the user asks
  about "remote tools", "remote commands", "codespace tools", "how do I run commands on a codespace",
  "remote_bash", "remote_view", "remote_edit", "remote_grep", "remote_glob", "how do remote tools work",
  "what tools are available on the codespace", or mentions working "on a codespace", "remotely",
  or "on the remote". Also trigger when the user seems confused about whether to use local or remote tools.
---

# Remote Codespace Tools Reference

This project provides remote tools that execute on a GitHub Codespace via SSH. They are supplied by the stable user-scoped Copilot extension and shared extension host. Copilot stays in the current local checkout for context, while repository reads, edits, patches, searches, and commands go through the `remote_*` tools.

## Tool Routing — When to Use What

| Task | Tool to use | Why |
|------|------------|-----|
| View/edit/create/patch **source code** | `remote_view`, `remote_edit`, `remote_create`, `remote_apply_patch` | Source code lives on the codespace |
| Run **shell commands** (build, test, lint) | `remote_bash` | Commands must execute where the code is |
| **Search** code (grep, find files) | `remote_grep`, `remote_glob` | Files are on the codespace |
| Copy files between local and remote | `remote_copy` | Explicit one-time transfer between local cwd and a codespace workdir |
| Edit **local session files** (plan.md, notes) | `view`, `edit`, `create` (built-in local) | Session state lives locally |
| **Change directory** on codespace | `remote_cd` | Affects all subsequent remote commands |
| **Interactive/PTY** commands | `remote_bash mode=async` + `remote_write_bash`/`remote_read_bash` | Backed by tmux sessions on the codespace |
| **Open a terminal** to the codespace | `open_shell` | Opens `gh codespace ssh` in a new terminal window |

## File Operations

### `remote_view`

View a file or directory on the codespace. Mirrors the local `view` surface with line-number ranges, large-file overrides, directory listings, image payloads, and binary metadata.

**Parameters:**
- `path` (required) — Path on the codespace
- `view_range` (optional) — `[start_line, end_line]` to view a range. Use `-1` for end_line to read to EOF.
- `forceReadLargeFiles` (optional) — bypasses the 20 KB safeguard for large text files and image payloads.

**Example:** View lines 10-20 of a file:
```
remote_view(path="/workspaces/repo/main.go", view_range=[10, 20])
```

### `remote_edit`

Replace exactly one occurrence of a string in a file on the codespace.

**Parameters:**
- `path` (required) — File path
- `old_str` (required) — Exact string to find (must match exactly once)
- `new_str` (required) — Replacement string

**How it works:** Reads the file via base64-encoded SSH transfer, performs the replacement in Go, writes it back. The `old_str` must be unique in the file.

### `remote_create`

Create a new file on the codespace. Parent directories are created automatically.

**Parameters:**
- `path` (required) — Path for the new file
- `file_text` (required) — Content of the file

`remote_create` refuses to overwrite an existing file.

### `remote_apply_patch`

Apply the canonical `apply_patch` payload on the codespace.

**Parameters:**
- `patch` (required) — The canonical patch text beginning with `*** Begin Patch` and ending with `*** End Patch`
- `cwd` (optional) — Working directory for relative patch paths

### `remote_copy`

Copy one file between the local working directory and a connected codespace. Use local paths for local files and `cs://<alias>/<path>` for remote files under a codespace workdir. Aliases come from `list_codespaces`.

**Parameters:**
- `source` (required) — Local path or remote URI, e.g. `src/app.go` or `cs://github/src/app.go`
- `destination` (required) — Local path or remote URI
- `overwrite` (optional) — Set `true` to replace an existing destination file. Defaults to `false`.

**Examples:**
```
remote_copy(source="src/app.go", destination="cs://github/src/app.go")
remote_copy(source="cs://github/src/generated.go", destination="src/generated.go")
```

`remote_copy` is a one-time copy, not synchronization. In `--here` sessions, use it when local WIP should move to the codespace or when a selected remote result should be brought back locally.

## Shell Commands

### `remote_bash`

Execute a bash command on the codespace. By default it uses a lightweight daemon-managed non-PTY process:

**Default mode** — starts the command once, waits briefly, then:
- returns final output immediately if the command exits quickly, or
- retains the same process and returns partial output plus a `shellId` if it is still running.

Retained default-mode sessions support reads, waits, and stops, but not stdin or PTY behavior. Use `mode=async` for interactive commands.

Example quick command:
```
remote_bash(command="go test -race ./...", description="Run tests")
```

**Default mode with longer `initial_wait`** — waits up to N seconds before falling back to a `shellId`:
```
remote_bash(command="go test -race ./...", initial_wait=120, description="Run tests")
→ partial output + "[shellId: sh-123 — use remote_read_bash to check for more output]"
```

**Async mode** — starts command in a tmux session, returns a `shellId`:
```
remote_bash(command="npm run dev", mode="async", description="Start dev server")
→ "Started async session: sh-1709540000000"
```

**Parameters:**
- `command` (required) — The bash command
- `description` (optional) — Short description of what the command does
- `mode` (optional) — `"sync"` (default) or `"async"`
- `initial_wait` (optional) — Seconds to wait in default/sync mode before returning partial output (default: 2). Use larger values when you want more inline output before switching to follow-up reads.
- `shellId` (optional) — Custom session ID for async mode or retained sync commands

### `remote_write_bash`

Send input to an async/tmux `remote_bash` session. Supports special keys. Retained default-mode process sessions are non-interactive and reject writes.

**Parameters:**
- `shellId` (required) — Session ID from `remote_bash`
- `input` (optional) — Text or special keys: `{enter}`, `{up}`, `{down}`, `{left}`, `{right}`, `{backspace}`
- `delay` (optional) — Seconds to wait before reading output (default: 2)

### `remote_read_bash`

Read output from a `remote_bash` session.

**Parameters:**
- `shellId` (required) — Session ID
- `delay` (optional) — Seconds to wait before reading (default: 2)

### `remote_stop_bash`

Stop (kill) a `remote_bash` session.

**Parameters:**
- `shellId` (required) — Session ID to stop

### `remote_list_bash`

List all active `remote_bash` sessions on the codespace.

## Search Tools

### `remote_grep`

Search for a regex pattern in files on the codespace. Mirrors the local `rg` surface. Uses `ripgrep` when available and falls back without losing structured path/type filtering.

**Parameters:**
- `pattern` (required) — Regex pattern to search for
- `path` (optional) — Legacy single directory or file to search in
- `paths` (optional) — One path or multiple paths to search in
- `output_mode` (optional) — `content`, `files_with_matches`, or `count`
- `glob` (optional) — Glob pattern to filter files (e.g., `"*.go"`, `"*.ts"`)
- `type` (optional) — File type filter (for example `go`, `ts`, `tsx`, `js`, `jsx`)
- `-i`, `-A`, `-B`, `-C`, `-n`, `head_limit`, `multiline` — the same options exposed by the local `rg` tool

### `remote_glob`

Find files matching a glob pattern on the codespace. Mirrors the local `glob` tool with `path`/`paths` selection. Uses `fd` if available and falls back to the built-in walker.

**Parameters:**
- `pattern` (required) — Glob pattern (e.g., `"**/*.go"`, `"src/**/*.test.js"`)
- `path` (optional) — Legacy single directory to search in
- `paths` (optional) — One directory or multiple directories to search in

## Navigation

### `remote_cd`

Change the working directory on the codespace. Affects all subsequent `remote_bash`, `remote_grep`, and `remote_glob` commands.

**Parameters:**
- `path` (required) — Directory path on the codespace

### `remote_cwd`

Show the current working directory on the codespace. No parameters.

## Access

### `open_shell`

Open an interactive SSH shell to the codespace in a new terminal window. Useful for manual debugging or exploration alongside the agent session.

## Common Patterns

### TDD Workflow
```
1. remote_view the test file to understand existing patterns
2. remote_edit to add a new test case
3. remote_bash(command="go test -race -run TestNewThing ./pkg/...")
4. remote_edit the implementation to make it pass
5. remote_bash(command="go test -race ./pkg/...")
```

### Long-Running Commands
```
1. remote_bash(command="npm run dev", mode="async") → shellId
2. remote_read_bash(shellId=shellId, delay=5)  — check output
3. remote_bash(command="curl localhost:3000")    — test the server
4. remote_stop_bash(shellId=shellId)             — stop when done
```

### Multi-File Edits
```
1. remote_view(path="/workspaces/repo/pkg/api.go")     — understand current code
2. remote_edit(path="/workspaces/repo/pkg/api.go", ...) — make change
3. remote_edit(path="/workspaces/repo/pkg/api.go", ...) — second edit in same file
4. remote_bash(command="go vet ./...")                    — verify
```

### Local WIP Handoff in `--here`

When launched with `--here`, Copilot is running in the user's local repo and remote codespace tools are also available. Local and remote are separate worktrees.

Recommended flow:
```
1. Use local git status / file tools to inspect local WIP
2. remote_bash(command="git status --short && git branch --show-current") to inspect remote state
3. remote_bash(command="git switch -c handoff/<name>") or switch to the intended remote branch
4. remote_copy selected local files to cs://<alias>/... paths
5. remote_bash(command="git status --short && go test ./...") to continue remotely
6. Use remote_copy in reverse only for selected remote files that should return to local
```

For broad tracked changes, prefer a deliberate patch workflow (`git diff --binary`, transfer, `git apply --check`, then `git apply`) rather than copying many files blindly.

## Tips

- **All remote paths are absolute** on the codespace (e.g., `/workspaces/repo/...`)
- **Remote bash starts commands once** — quick sync commands finish inline; slow sync commands retain the same non-PTY process under a shellId
- **Async sessions survive disconnects** — they run in tmux on the codespace
- **`remote_cd` is sticky** — it affects all subsequent commands until changed
- **grep falls back gracefully** — if ripgrep isn't installed, it uses grep
- **Don't use local `bash`** for project commands — it won't find the source code
- **Use local `view`/`edit`/`create`** only for session state files (plan.md, notes under `~/.copilot/`)
- **In `--here` mode**, local tools operate on the current repo and `remote_*` tools operate on the codespace; use `remote_copy` for explicit one-time transfers.
