// Package daemonproto defines the wire protocol used between the launcher
// (DaemonExecutor) and the in-sandbox gh-copilot-codespace daemon subcommand.
//
// The protocol is intentionally transport-agnostic: it is framed as
// newline-delimited JSON over a full-duplex byte stream, so the same daemon
// binary serves codespaces over SSH, devcontainers over `docker exec`, WSL
// distros over `wsl -- ...`, and a local test harness over an in-process pipe.
// All implementations rely on json.Decoder / json.Encoder so payloads can
// exceed bufio.Scanner's default token size — required for file reads and
// large command outputs.
//
// Each frame is a single JSON object terminated by a newline. The Type field
// is the discriminator. There are four frame types:
//
//   - TypeRequest  ("req")     — client → server, carries an id, verb, params.
//   - TypeResponse ("resp")    — server → client, references the request id.
//   - TypeCancel   ("cancel")  — client → server, asks the daemon to terminate
//     an in-flight request by id (SIGTERM the
//     process group, drop pending verb work).
//   - TypeHello    ("hello")   — server → client, sent once on connect to
//     announce protocol version and supported verbs.
//
// IDs are caller-allocated positive integers, unique per connection. The
// server never reuses an id in a response; once a response or terminal error
// is written for an id, that id is retired.
//
// # Cancellation
//
// The daemon tracks each in-flight request by id. When a TypeCancel frame
// arrives, the daemon SIGTERMs the request's process group (if any), records
// the cancellation, and the request's eventual response carries
// ErrCodeCanceled. Cancellation is best-effort: a response may already be
// in-flight when the cancel arrives, in which case the cancel is a no-op.
//
// # Mutation safety
//
// Mutating verbs (create_file, write_file, edit_file, apply_patch, write_session,
// stop_session, run_bash, start_session) accept an optional caller-generated
// IdempotencyKey. v2 of the protocol reserves the field but neither the
// client nor the daemon dedupes on it — the client does not auto-retry
// mutating requests on transport failure, so accidental double-execution does
// not happen. Servers MAY choose to implement an LRU dedup cache in a later
// revision; clients SHOULD include the key when calling mutating verbs to
// enable that future change without a wire break.
//
// # Error model
//
// Errors are reported in the Response.Error field. The Code is a stable
// machine-readable identifier; Message is human-readable diagnostic text. A
// successful response has Result set and Error nil; a failed response has
// Error set and Result nil. The two are mutually exclusive.
package daemonproto

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ProtocolVersion is the current daemonproto version. The server sends it in
// the TypeHello frame on connect; clients refuse to proceed if the version is
// not recognized. v2 establishes the safe rooted-filesystem and bounded file
// transfer contract; clients must fall back rather than use older daemons.
const ProtocolVersion = "2"

const (
	// MaxFileTransferBytes bounds raw file-copy payloads before JSON/base64
	// amplification. View and command-output frames have separate limits.
	MaxFileTransferBytes = 16 * 1024 * 1024
)

var ErrFileTransferTooLarge = errors.New("daemonproto: file transfer exceeds safe maximum")

// Frame type discriminators.
const (
	TypeRequest  = "req"
	TypeResponse = "resp"
	TypeCancel   = "cancel"
	TypeHello    = "hello"
)

// Verb is the daemon-side operation name carried in Request.Verb. Verb names
// are stable wire identifiers, chosen to map 1:1 to the methods on
// ssh.Executor and follow snake_case.
type Verb string

const (
	VerbViewFile     Verb = "view_file"
	VerbReadFile     Verb = "read_file"
	VerbEditFile     Verb = "edit_file"
	VerbCreateFile   Verb = "create_file"
	VerbWriteFile    Verb = "write_file"
	VerbRunBash      Verb = "run_bash"
	VerbGrep         Verb = "grep"
	VerbGlob         Verb = "glob"
	VerbApplyPatch   Verb = "apply_patch"
	VerbStartSession Verb = "start_session"
	VerbWriteSession Verb = "write_session"
	VerbReadSession  Verb = "read_session"
	VerbStopSession  Verb = "stop_session"
	VerbListSessions Verb = "list_sessions"
	VerbPing         Verb = "ping"
)

// AllVerbs returns every verb implemented by the current daemon and
// advertised in the TypeHello handshake.
func AllVerbs() []Verb {
	return []Verb{
		VerbViewFile,
		VerbReadFile,
		VerbEditFile,
		VerbCreateFile,
		VerbWriteFile,
		VerbRunBash,
		VerbGrep,
		VerbGlob,
		VerbApplyPatch,
		VerbStartSession,
		VerbWriteSession,
		VerbReadSession,
		VerbStopSession,
		VerbListSessions,
		VerbPing,
	}
}

// AllDefinedVerbs returns every currently defined protocol verb, including
// additive verbs that may not yet be advertised by a specific daemon build.
func AllDefinedVerbs() []Verb {
	return []Verb{
		VerbViewFile,
		VerbReadFile,
		VerbEditFile,
		VerbCreateFile,
		VerbWriteFile,
		VerbRunBash,
		VerbGrep,
		VerbGlob,
		VerbApplyPatch,
		VerbStartSession,
		VerbWriteSession,
		VerbReadSession,
		VerbStopSession,
		VerbListSessions,
		VerbPing,
	}
}

// FilesystemVerbs returns the filesystem/search protocol verbs shared between
// local tools and remote codespace tools.
func FilesystemVerbs() []Verb {
	return []Verb{
		VerbViewFile,
		VerbReadFile,
		VerbWriteFile,
		VerbGrep,
		VerbGlob,
		VerbApplyPatch,
	}
}

// IsMutating reports whether a verb has side effects on the sandbox. Callers
// SHOULD attach an IdempotencyKey to mutating verbs even though v2 of the
// daemon does not yet enforce dedup; this leaves room for future safe
// retry-on-reconnect without changing the wire.
func (v Verb) IsMutating() bool {
	switch v {
	case VerbEditFile, VerbCreateFile, VerbWriteFile, VerbApplyPatch, VerbWriteSession, VerbStopSession, VerbRunBash, VerbStartSession:
		return true
	default:
		return false
	}
}

// Error codes returned in Response.Error.Code. Stable wire identifiers.
const (
	// ErrCodeBadRequest — request payload was malformed or violated a verb's
	// schema. Re-sending the same payload will fail again.
	ErrCodeBadRequest = "BAD_REQUEST"

	// ErrCodeUnknownVerb — the server does not implement Request.Verb. The
	// client should not retry; instead inspect TypeHello.Verbs for the
	// authoritative list.
	ErrCodeUnknownVerb = "UNKNOWN_VERB"

	// ErrCodeCanceled — the request was terminated because the client sent a
	// matching TypeCancel frame, or because the daemon is shutting down.
	ErrCodeCanceled = "CANCELED"

	// ErrCodeExecFailed — the verb ran but the underlying operation failed
	// (non-zero shell exit, file not found, etc.). Inspect verb-specific
	// result fields for detail.
	ErrCodeExecFailed = "EXEC_FAILED"

	// ErrCodeInternal — the daemon hit an unexpected condition. Indicates a
	// bug or environmental fault; not retryable from the client's side.
	ErrCodeInternal = "INTERNAL"

	// ErrCodeUnsupportedVersion — used in handshake; the client refused the
	// server's ProtocolVersion.
	ErrCodeUnsupportedVersion = "UNSUPPORTED_VERSION"
)

// Frame is the union envelope shared by all directions. Exactly one of the
// type-specific embedded structs is populated, selected by Type.
//
// The wire layout uses inlined fields rather than a `payload: ...` nested
// object so that frames remain compact and the JSON looks readable on the
// wire when debugging.
type Frame struct {
	Type string `json:"type"`

	// Request fields (Type == TypeRequest).
	ID             uint64          `json:"id,omitempty"`
	Verb           Verb            `json:"verb,omitempty"`
	Params         json.RawMessage `json:"params,omitempty"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
	DeadlineMS     int64           `json:"deadline_ms,omitempty"`

	// Response fields (Type == TypeResponse). ID is reused from the
	// originating Request.
	Result json.RawMessage `json:"result,omitempty"`
	Error  *ErrorPayload   `json:"error,omitempty"`

	// Cancel fields (Type == TypeCancel). Only ID is meaningful.

	// Hello fields (Type == TypeHello).
	Version string   `json:"version,omitempty"`
	Verbs   []string `json:"verbs,omitempty"`
}

// ErrorPayload is the structured error returned in a Response frame.
type ErrorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// NewRequest builds a TypeRequest frame with the given id and verb. Params
// must marshal to a JSON object; pass nil for verbs with no parameters.
func NewRequest(id uint64, verb Verb, params any, idempotencyKey string) (Frame, error) {
	frame := Frame{
		Type:           TypeRequest,
		ID:             id,
		Verb:           verb,
		IdempotencyKey: idempotencyKey,
	}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return Frame{}, fmt.Errorf("marshal params: %w", err)
		}
		frame.Params = raw
	}
	return frame, nil
}

// NewResponse builds a successful TypeResponse frame.
func NewResponse(id uint64, result any) (Frame, error) {
	frame := Frame{
		Type: TypeResponse,
		ID:   id,
	}
	if result != nil {
		raw, err := json.Marshal(result)
		if err != nil {
			return Frame{}, fmt.Errorf("marshal result: %w", err)
		}
		frame.Result = raw
	}
	return frame, nil
}

// NewErrorResponse builds a failed TypeResponse frame.
func NewErrorResponse(id uint64, code, message string) Frame {
	return Frame{
		Type:  TypeResponse,
		ID:    id,
		Error: &ErrorPayload{Code: code, Message: message},
	}
}

// NewCancel builds a TypeCancel frame for the given in-flight request id.
func NewCancel(id uint64) Frame {
	return Frame{Type: TypeCancel, ID: id}
}

// NewHello builds the server's handshake frame.
func NewHello(version string, verbs []Verb) Frame {
	names := make([]string, len(verbs))
	for i, v := range verbs {
		names[i] = string(v)
	}
	return Frame{Type: TypeHello, Version: version, Verbs: names}
}
