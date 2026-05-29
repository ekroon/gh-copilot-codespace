package daemonproto

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

// ViewFileParams / ViewFileResult — read a file with optional line range.
// Mirrors ssh.Executor.ViewFile.
type (
	ViewFileParams struct {
		Path      string `json:"path"`
		ViewRange []int  `json:"view_range,omitempty"` // [start, end]; -1 end = EOF
	}
	ViewFileResult struct {
		Content string `json:"content"`
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

// GrepParams / GrepResult — search file contents with ripgrep (or grep
// fallback). Read-only.
type (
	GrepParams struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path,omitempty"`
		Glob    string `json:"glob,omitempty"`
		Cwd     string `json:"cwd,omitempty"`
	}
	GrepResult struct {
		Output string `json:"output"`
	}
)

// GlobParams / GlobResult — find files by pattern. Read-only.
type (
	GlobParams struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path,omitempty"`
		Cwd     string `json:"cwd,omitempty"`
	}
	GlobResult struct {
		Output string `json:"output"`
	}
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
