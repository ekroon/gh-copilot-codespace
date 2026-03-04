---
name: remote-tools
description: >-
  Explain how all remote MCP tools work for codespace development. Use this skill when the user asks
  about "remote tools", "remote commands", "codespace tools", "how do I run commands on a codespace",
  "remote_bash", "remote_view", "remote_edit", "remote_grep", "remote_glob", "how do remote tools work",
  "what tools are available on the codespace", or mentions working "on a codespace", "remotely",
  or "on the remote". Also trigger when the user seems confused about whether to use local or remote tools.
---

# Remote Codespace Tools Reference

This project provides MCP tools that execute on a remote GitHub Codespace via SSH. These tools replace the built-in local tools for all source-code and shell operations.

## Tool Routing — When to Use What

| Task | Tool to use | Why |
|------|------------|-----|
| View/edit/create **source code** | `remote_view`, `remote_edit`, `remote_create` | Source code lives on the codespace |
| Run **shell commands** (build, test, lint) | `remote_bash` | Commands must execute where the code is |
| **Search** code (grep, find files) | `remote_grep`, `remote_glob` | Files are on the codespace |
| Edit **local session files** (plan.md, notes) | `view`, `edit`, `create` (built-in local) | Session state lives locally |
| **Change directory** on codespace | `remote_cd` | Affects all subsequent remote commands |
| **Interactive/long-running** commands | `remote_bash mode=async` + `remote_write_bash`/`remote_read_bash` | Backed by tmux sessions on the codespace |
| **Open a terminal** to the codespace | `open_shell` | Opens `gh codespace ssh` in a new terminal window |

## File Operations

### `remote_view`

View a file or directory on the codespace. Returns file contents with line numbers.

**Parameters:**
- `path` (required) — Absolute path on the codespace
- `view_range` (optional) — `[start_line, end_line]` to view a range. Use `-1` for end_line to read to EOF.

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

## Shell Commands

### `remote_bash`

Execute a bash command on the codespace. Supports two modes:

**Sync mode** (default) — runs command, waits for completion, returns output:
```
remote_bash(command="go test -race ./...", description="Run tests")
```

**Sync with initial_wait** — waits up to N seconds, returns partial output + shellId:
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
- `initial_wait` (optional) — Seconds to wait in sync mode before returning partial output (e.g., 120 for builds). Command continues in background with a shellId for follow-up reads.
- `shellId` (optional) — Custom session ID for async mode

### `remote_write_bash`

Send input to an async bash session. Supports special keys.

**Parameters:**
- `shellId` (required) — Session ID from `remote_bash`
- `input` (optional) — Text or special keys: `{enter}`, `{up}`, `{down}`, `{left}`, `{right}`, `{backspace}`
- `delay` (optional) — Seconds to wait before reading output (default: 2)

### `remote_read_bash`

Read output from an async bash session.

**Parameters:**
- `shellId` (required) — Session ID
- `delay` (optional) — Seconds to wait before reading (default: 2)

### `remote_stop_bash`

Stop (kill) an async bash session.

**Parameters:**
- `shellId` (required) — Session ID to stop

### `remote_list_bash`

List all active async bash sessions on the codespace.

## Search Tools

### `remote_grep`

Search for a regex pattern in files on the codespace. Uses `ripgrep` if available, falls back to `grep`.

**Parameters:**
- `pattern` (required) — Regex pattern to search for
- `path` (optional) — Directory or file to search in (defaults to workspace root)
- `glob` (optional) — Glob pattern to filter files (e.g., `"*.go"`, `"*.ts"`)

### `remote_glob`

Find files matching a glob pattern on the codespace. Uses `fd` if available, falls back to `find`.

**Parameters:**
- `pattern` (required) — Glob pattern (e.g., `"**/*.go"`, `"src/**/*.test.js"`)
- `path` (optional) — Directory to search in (defaults to workspace root)

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

## Tips

- **All remote paths are absolute** on the codespace (e.g., `/workspaces/repo/...`)
- **Async sessions survive disconnects** — they run in tmux on the codespace
- **`remote_cd` is sticky** — it affects all subsequent commands until changed
- **grep falls back gracefully** — if ripgrep isn't installed, it uses grep
- **Don't use local `bash`** for project commands — it won't find the source code
- **Use local `view`/`edit`/`create`** only for session state files (plan.md, notes under `~/.copilot/`)
