package daemonproto

import "github.com/ekroon/gh-copilot-codespace/internal/ssh"

// Verb param/result types. One struct pair per Verb. The wire layout is
// inlined into Frame.Params / Frame.Result as json.RawMessage; decode via
// json.Unmarshal(frame.Params, &VerbXxxParams{}).

// PingParams / PingResult — health check. No fields. The daemon responds
// immediately. Used by clients to validate the connection is up.
type (
	PingParams struct{}
	PingResult struct {
		Pong      bool   `json:"pong"`
		PID       int    `json:"pid,omitempty"`
		StartedAt string `json:"started_at,omitempty"`
	}
)

// ViewFileParams / ViewFileResult — read a file with optional line range plus
// additive fields for local-view parity. Legacy callers still rely on
// Path/ViewRange and Content.
type (
	ViewFileParams = ssh.ViewRequest
	ViewFileResult = ssh.ViewResult
)

// ReadFileParams / ReadFileResult — read arbitrary bytes after resolving the
// path against an explicit immutable root.
type (
	ReadFileParams = ssh.RootedReadRequest
	ReadFileResult struct {
		Data []byte `json:"data"`
	}
)

// EditFileParams / EditFileResult — replace exactly one occurrence of OldStr
// with NewStr in the file at Path. Fails with ErrCodeExecFailed if OldStr is
// missing or occurs more than once. Mutating.
type (
	EditFileParams struct {
		Path   string `json:"path"`
		OldStr string `json:"old_str"`
		NewStr string `json:"new_str"`
	}
	EditFileResult struct {
		Replaced int `json:"replaced"`
	}
)

// CreateFileParams / CreateFileResult — write a new file. Mutating. Parent
// directories are created. Daemon implementations MUST refuse to overwrite an
// existing file unless explicitly asked via the AllowOverwrite flag, to
// mirror the local `create` tool semantics.
type (
	CreateFileParams struct {
		Path           string `json:"path"`
		Content        string `json:"content"`
		AllowOverwrite bool   `json:"allow_overwrite,omitempty"`
	}
	CreateFileResult struct {
		Bytes int `json:"bytes"`
	}
)

// WriteFileParams / WriteFileResult — atomically write arbitrary bytes.
// Existing destinations are refused unless Overwrite is true. Implementations
// preserve regular-file permissions on overwrite and reject symbolic links.
type (
	WriteFileParams = ssh.RootedWriteRequest
	WriteFileResult struct {
		Bytes int `json:"bytes"`
	}
)

// RunBashParams / RunBashResult — run a bash command. The daemon spawns
// `bash -c <command>` in its own process group so a TypeCancel frame can kill
// the whole tree. Mutating (commands can have arbitrary side effects).
type (
	RunBashParams struct {
		Command string `json:"command"`
		Cwd     string `json:"cwd,omitempty"`
	}
	RunBashResult struct {
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
		ExitCode int    `json:"exit_code"`
	}
)

// GrepParams / GrepResult — search file contents with legacy-compatible
// defaults plus additive fields for local search parity. Read-only.
type (
	GrepParams = ssh.GrepRequest
	GrepResult = ssh.GrepResult
)

// GlobParams / GlobResult — find files by pattern with additive local glob
// parity fields. Read-only.
type (
	GlobParams = ssh.GlobRequest
	GlobResult = ssh.GlobResult
)

// ApplyPatchParams / ApplyPatchResult — apply an additive multi-file patch.
// Mutating.
type (
	ApplyPatchParams = ssh.ApplyPatchRequest
	ApplyPatchResult = ssh.ApplyPatchResult
)

// StartSessionParams / StartSessionResult — create a named long-running
// session (typically tmux-backed). Mutating but inherently idempotent on
// SessionID — re-issuing for an existing session returns the existing one.
type (
	StartSessionParams struct {
		SessionID string `json:"session_id"`
		Command   string `json:"command"`
		Cwd       string `json:"cwd,omitempty"`
	}
	StartSessionResult struct {
		SessionID string `json:"session_id"`
	}
)

// WriteSessionParams / WriteSessionResult — send input to a running session.
// Mutating. Supports key macros like {enter}, {up} per ssh.Executor contract.
type (
	WriteSessionParams struct {
		SessionID string `json:"session_id"`
		Input     string `json:"input"`
	}
	WriteSessionResult struct{}
)

// ReadSessionParams / ReadSessionResult — capture current pane output for a
// session. Read-only.
type (
	ReadSessionParams struct {
		SessionID string `json:"session_id"`
	}
	ReadSessionResult struct {
		Output string `json:"output"`
	}
)

// WaitSessionParams / WaitSessionResult — wait until a session exits or the
// requested timeout elapses, then return its current output.
type (
	WaitSessionParams struct {
		SessionID string `json:"session_id"`
		TimeoutMS int64  `json:"timeout_ms"`
	}
	WaitSessionResult struct {
		Output    string `json:"output"`
		Completed bool   `json:"completed"`
	}
)

// StopSessionParams / StopSessionResult — terminate a session. Mutating.
type (
	StopSessionParams struct {
		SessionID string `json:"session_id"`
	}
	StopSessionResult struct{}
)

// ListSessionsParams / ListSessionsResult — list active sessions. Read-only.
type (
	ListSessionsParams struct{}
	ListSessionsResult struct {
		Output string `json:"output"`
	}
)
