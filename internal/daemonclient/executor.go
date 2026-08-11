package daemonclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	pathpkg "path"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ekroon/gh-copilot-codespace/internal/daemonproto"
	"github.com/ekroon/gh-copilot-codespace/internal/daemontransport"
	"github.com/ekroon/gh-copilot-codespace/internal/ssh"
)

// Executor speaks daemonproto over a daemontransport.Transport. It satisfies
// ssh.Executor so callers can substitute it for *ssh.Client without changing
// any consumers. Executor is safe for concurrent use; each verb call gets an
// independent id and the writer is serialized per connection generation.
//
// All stream-scoped state lives in a connection generation. When a generation
// dies, the executor restores the connection lazily on the next call. It never
// replays an interrupted operation: interrupted callers receive a
// *ConnectionLostError describing what is known about the outcome.
type Executor struct {
	transport  daemontransport.Transport
	remotePath string

	nextID         atomic.Uint64
	nextGeneration atomic.Uint64

	mu                sync.Mutex
	conn              *connection
	closed            bool
	reconnecting      chan struct{}
	reconnectFailures int
	lastReconnectErr  error
	cooldownUntil     time.Time

	closeSignal    chan struct{}
	closeOnce      sync.Once
	closeTransport sync.Once

	workdir   string
	workdirMu sync.RWMutex

	helloTimeout         time.Duration
	reconnectTimeout     time.Duration
	reconnectCooldown    time.Duration
	maxReconnectCooldown time.Duration
}

// RemoteError is an error returned by the remote daemon.
type RemoteError struct {
	Code    string
	Message string
}

func (e *RemoteError) Error() string {
	return fmt.Sprintf("daemonclient: %s: %s", e.Code, e.Message)
}

// Dial deploys and spawns a daemon over t, then waits for the protocol hello.
func Dial(ctx context.Context, t daemontransport.Transport) (*Executor, error) {
	remotePath, err := t.Deploy(ctx)
	if err != nil {
		return nil, err
	}

	e := &Executor{
		transport:            t,
		remotePath:           remotePath,
		closeSignal:          make(chan struct{}),
		helloTimeout:         defaultHelloTimeout,
		reconnectTimeout:     defaultReconnectTimeout,
		reconnectCooldown:    defaultReconnectCooldown,
		maxReconnectCooldown: defaultMaxCooldown,
	}

	conn, err := e.connect(ctx)
	if err != nil {
		return nil, err
	}
	e.conn = conn
	return e, nil
}

func (e *Executor) call(ctx context.Context, verb daemonproto.Verb, params any) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	conn, err := e.connectionFor(ctx)
	if err != nil {
		return nil, err
	}
	return e.callOn(ctx, conn, verb, params)
}

// callOn issues one request on the captured generation c. Every response,
// cancellation, and failure stays pinned to that generation.
func (e *Executor) callOn(ctx context.Context, c *connection, verb daemonproto.Verb, params any) (json.RawMessage, error) {
	id := e.nextID.Add(1)
	frame, err := daemonproto.NewRequest(id, verb, params, "")
	if err != nil {
		return nil, fmt.Errorf("daemonclient: encode %s request: %w", verb, err)
	}

	responseCh := make(chan daemonproto.Frame, 1)
	c.pending.Store(id, responseCh)
	defer c.pending.Delete(id)

	written, writeErr := c.writeFrame(ctx, frame)
	if writeErr != nil {
		// Caller-driven cancellation or deadline is not a connection failure:
		// the generation stays usable for later calls.
		if errors.Is(writeErr, context.Canceled) || errors.Is(writeErr, context.DeadlineExceeded) {
			return nil, writeErr
		}
		if !errors.Is(writeErr, errConnectionDead) {
			c.fail(fmt.Errorf("daemonclient: write %s request: %w", verb, writeErr))
		}
		return nil, e.connectionLost(ctx, c, written)
	}

	select {
	case response := <-responseCh:
		return processResponse(response)
	case <-ctx.Done():
		cancelCtx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		_, cancelErr := c.writeFrame(cancelCtx, daemonproto.NewCancel(id))
		cancel()
		if cancelErr != nil && !errors.Is(cancelErr, errConnectionDead) {
			c.fail(fmt.Errorf("daemonclient: deliver cancel for request %d: %w", id, cancelErr))
		}
		timer := time.NewTimer(250 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-responseCh:
		case <-c.readerDone:
		case <-timer.C:
		}
		return nil, ctx.Err()
	case <-c.readerDone:
		return nil, e.connectionLost(ctx, c, true)
	}
}

func processResponse(frame daemonproto.Frame) (json.RawMessage, error) {
	if frame.Error != nil {
		if frame.Error.Code == daemonproto.ErrCodeCanceled {
			return nil, context.Canceled
		}
		return nil, &RemoteError{Code: frame.Error.Code, Message: frame.Error.Message}
	}
	return frame.Result, nil
}

// supportsVerb reports whether the current generation advertised verb.
func (e *Executor) supportsVerb(verb daemonproto.Verb) bool {
	conn := e.current()
	if conn == nil {
		return false
	}
	return conn.supports(verb)
}

// Ping checks whether the daemon connection is healthy.
func (e *Executor) Ping(ctx context.Context) (daemonproto.PingResult, error) {
	raw, err := e.call(ctx, daemonproto.VerbPing, daemonproto.PingParams{})
	if err != nil {
		return daemonproto.PingResult{}, err
	}
	var result daemonproto.PingResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return daemonproto.PingResult{}, fmt.Errorf("daemonclient: decode ping result: %w", err)
	}
	return result, nil
}

func (e *Executor) View(ctx context.Context, req ssh.ViewRequest) (ssh.ViewResult, error) {
	req = req.Normalize()
	req.Path = e.resolvePath(req.Path)
	raw, err := e.call(ctx, daemonproto.VerbViewFile, daemonproto.ViewFileParams(req))
	if err != nil {
		return ssh.ViewResult{}, err
	}
	var result ssh.ViewResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return ssh.ViewResult{}, fmt.Errorf("daemonclient: decode view_file result: %w", err)
	}
	return result, nil
}

func (e *Executor) ViewFile(ctx context.Context, path string, viewRange []int) (string, error) {
	result, err := e.View(ctx, ssh.ViewRequest{Path: path, ViewRange: viewRange})
	if err != nil {
		return "", err
	}
	return result.Content, nil
}

func (e *Executor) EditFile(ctx context.Context, path, oldStr, newStr string) error {
	raw, err := e.call(ctx, daemonproto.VerbEditFile, daemonproto.EditFileParams{Path: e.resolvePath(path), OldStr: oldStr, NewStr: newStr})
	if err != nil {
		return err
	}
	var result daemonproto.EditFileResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("daemonclient: decode edit_file result: %w", err)
	}
	return nil
}

func (e *Executor) CreateFile(ctx context.Context, path, content string) error {
	raw, err := e.call(ctx, daemonproto.VerbCreateFile, daemonproto.CreateFileParams{Path: e.resolvePath(path), Content: content})
	if err != nil {
		return err
	}
	var result daemonproto.CreateFileResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("daemonclient: decode create_file result: %w", err)
	}
	return nil
}

func (e *Executor) WriteFile(ctx context.Context, path string, content []byte, overwrite bool) error {
	return e.WriteFileRooted(ctx, ssh.RootedWriteRequest{
		Path:      e.resolvePath(path),
		Root:      e.GetWorkdir(),
		Data:      content,
		Overwrite: overwrite,
	})
}

func (e *Executor) ReadFileRooted(ctx context.Context, req ssh.RootedReadRequest) ([]byte, error) {
	req.Path = resolveRootedPath(req.Path, req.Root)
	raw, err := e.call(ctx, daemonproto.VerbReadFile, daemonproto.ReadFileParams(req))
	if err != nil {
		return nil, err
	}
	var result daemonproto.ReadFileResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("daemonclient: decode read_file result: %w", err)
	}
	return result.Data, nil
}

func (e *Executor) WriteFileRooted(ctx context.Context, req ssh.RootedWriteRequest) error {
	if len(req.Data) > daemonproto.MaxFileTransferBytes {
		return fmt.Errorf("%w: %d bytes exceeds %d", daemonproto.ErrFileTransferTooLarge, len(req.Data), daemonproto.MaxFileTransferBytes)
	}
	req.Path = resolveRootedPath(req.Path, req.Root)
	raw, err := e.call(ctx, daemonproto.VerbWriteFile, daemonproto.WriteFileParams{
		Path:      req.Path,
		Data:      req.Data,
		Overwrite: req.Overwrite,
		Root:      req.Root,
	})
	if err != nil {
		return err
	}
	var result daemonproto.WriteFileResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("daemonclient: decode write_file result: %w", err)
	}
	return nil
}

func (e *Executor) RunBash(ctx context.Context, command, cwd string) (stdout, stderr string, exitCode int, err error) {
	if cwd == "" {
		cwd = e.GetWorkdir()
	}
	raw, err := e.call(ctx, daemonproto.VerbRunBash, daemonproto.RunBashParams{Command: command, Cwd: cwd})
	if err != nil {
		return "", "", -1, err
	}
	var result daemonproto.RunBashResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", "", -1, fmt.Errorf("daemonclient: decode run_bash result: %w", err)
	}
	return result.Stdout, result.Stderr, result.ExitCode, nil
}

func (e *Executor) GrepFiles(ctx context.Context, req ssh.GrepRequest) (ssh.GrepResult, error) {
	req = req.Normalize()
	if req.Cwd == "" {
		req.Cwd = e.GetWorkdir()
	}
	raw, err := e.call(ctx, daemonproto.VerbGrep, daemonproto.GrepParams(req))
	if err != nil {
		return ssh.GrepResult{}, err
	}
	var result ssh.GrepResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return ssh.GrepResult{}, fmt.Errorf("daemonclient: decode grep result: %w", err)
	}
	return result, nil
}

func (e *Executor) Grep(ctx context.Context, pattern, path, globPattern, cwd string) (string, error) {
	result, err := e.GrepFiles(ctx, ssh.GrepRequest{Pattern: pattern, Path: path, Glob: globPattern, Cwd: cwd})
	if err != nil {
		return "", err
	}
	return result.Output, nil
}

func (e *Executor) GlobFiles(ctx context.Context, req ssh.GlobRequest) (ssh.GlobResult, error) {
	req = req.Normalize()
	if req.Cwd == "" {
		req.Cwd = e.GetWorkdir()
	}
	raw, err := e.call(ctx, daemonproto.VerbGlob, daemonproto.GlobParams(req))
	if err != nil {
		return ssh.GlobResult{}, err
	}
	var result ssh.GlobResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return ssh.GlobResult{}, fmt.Errorf("daemonclient: decode glob result: %w", err)
	}
	return result, nil
}

func (e *Executor) Glob(ctx context.Context, pattern, path, cwd string) (string, error) {
	result, err := e.GlobFiles(ctx, ssh.GlobRequest{Pattern: pattern, Path: path, Cwd: cwd})
	if err != nil {
		return "", err
	}
	return result.Output, nil
}

func (e *Executor) ApplyPatch(ctx context.Context, req ssh.ApplyPatchRequest) (ssh.ApplyPatchResult, error) {
	if req.Cwd == "" {
		req.Cwd = e.GetWorkdir()
	}
	raw, err := e.call(ctx, daemonproto.VerbApplyPatch, daemonproto.ApplyPatchParams(req))
	if err != nil {
		return ssh.ApplyPatchResult{}, err
	}
	var result ssh.ApplyPatchResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return ssh.ApplyPatchResult{}, fmt.Errorf("daemonclient: decode apply_patch result: %w", err)
	}
	return result, nil
}

func (e *Executor) StartSession(ctx context.Context, sessionID, command, cwd string) error {
	if cwd == "" {
		cwd = e.GetWorkdir()
	}
	raw, err := e.call(ctx, daemonproto.VerbStartSession, daemonproto.StartSessionParams{SessionID: sessionID, Command: command, Cwd: cwd})
	if err != nil {
		return err
	}
	var result daemonproto.StartSessionResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("daemonclient: decode start_session result: %w", err)
	}
	return nil
}

func (e *Executor) SupportsProcessSessions() bool {
	return e.supportsVerb(daemonproto.VerbStartProcessSession)
}

func (e *Executor) StartProcessSession(ctx context.Context, sessionID, command, cwd string) error {
	if cwd == "" {
		cwd = e.GetWorkdir()
	}
	raw, err := e.call(ctx, daemonproto.VerbStartProcessSession, daemonproto.StartProcessSessionParams{
		SessionID: sessionID,
		Command:   command,
		Cwd:       cwd,
	})
	if err != nil {
		return err
	}
	var result daemonproto.StartProcessSessionResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("daemonclient: decode start_process_session result: %w", err)
	}
	return nil
}

func (e *Executor) WriteSession(ctx context.Context, sessionID, input string) error {
	raw, err := e.call(ctx, daemonproto.VerbWriteSession, daemonproto.WriteSessionParams{SessionID: sessionID, Input: input})
	if err != nil {
		return err
	}
	var result daemonproto.WriteSessionResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("daemonclient: decode write_session result: %w", err)
	}
	return nil
}

func (e *Executor) ReadSession(ctx context.Context, sessionID string) (string, error) {
	raw, err := e.call(ctx, daemonproto.VerbReadSession, daemonproto.ReadSessionParams{SessionID: sessionID})
	if err != nil {
		return "", err
	}
	var result daemonproto.ReadSessionResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", fmt.Errorf("daemonclient: decode read_session result: %w", err)
	}
	return result.Output, nil
}

func (e *Executor) SupportsWaitSession() bool {
	return e.supportsVerb(daemonproto.VerbWaitSession)
}

func (e *Executor) WaitSession(ctx context.Context, sessionID string, timeout time.Duration) (string, bool, error) {
	raw, err := e.call(ctx, daemonproto.VerbWaitSession, daemonproto.WaitSessionParams{
		SessionID: sessionID,
		TimeoutMS: timeout.Milliseconds(),
	})
	if err != nil {
		return "", false, err
	}
	var result daemonproto.WaitSessionResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", false, fmt.Errorf("daemonclient: decode wait_session result: %w", err)
	}
	return result.Output, result.Completed, nil
}

func (e *Executor) StopSession(ctx context.Context, sessionID string) error {
	raw, err := e.call(ctx, daemonproto.VerbStopSession, daemonproto.StopSessionParams{SessionID: sessionID})
	if err != nil {
		return err
	}
	var result daemonproto.StopSessionResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("daemonclient: decode stop_session result: %w", err)
	}
	return nil
}

func (e *Executor) ListSessions(ctx context.Context) (string, error) {
	raw, err := e.call(ctx, daemonproto.VerbListSessions, daemonproto.ListSessionsParams{})
	if err != nil {
		return "", err
	}
	var result daemonproto.ListSessionsResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", fmt.Errorf("daemonclient: decode list_sessions result: %w", err)
	}
	return result.Output, nil
}

func (e *Executor) SetWorkdir(dir string) {
	e.workdirMu.Lock()
	e.workdir = dir
	e.workdirMu.Unlock()
}

func (e *Executor) GetWorkdir() string {
	e.workdirMu.RLock()
	defer e.workdirMu.RUnlock()
	return e.workdir
}

func (e *Executor) resolvePath(path string) string {
	if path == "" || pathpkg.IsAbs(path) {
		return path
	}
	if workdir := e.GetWorkdir(); workdir != "" {
		return pathpkg.Join(workdir, path)
	}
	return path
}

func resolveRootedPath(path, root string) string {
	if path == "" || pathpkg.IsAbs(path) || root == "" {
		return path
	}
	return pathpkg.Join(root, path)
}

// Close disposes the current generation, prevents any further reconnect, and
// closes the transport exactly once. It is safe to call concurrently with an
// in-flight reconnect: the reconnect either observes the closed state and
// disposes its unpublished stream, or publishes before Close claims it.
func (e *Executor) Close() error {
	e.mu.Lock()
	e.closed = true
	conn := e.conn
	reconnecting := e.reconnecting
	e.mu.Unlock()

	e.closeOnce.Do(func() { close(e.closeSignal) })

	if conn != nil {
		conn.dispose()
	}
	if reconnecting != nil {
		<-reconnecting
	}

	e.mu.Lock()
	published := e.conn
	e.mu.Unlock()
	if published != nil && published != conn {
		published.dispose()
	}

	e.closeTransport.Do(func() {
		if e.transport != nil {
			_ = e.transport.Close()
		}
	})
	return nil
}

// Compile-time check: Executor must implement ssh.Executor.
var _ ssh.Executor = (*Executor)(nil)
var _ ssh.FilesystemExecutor = (*Executor)(nil)
var _ ssh.ApplyPatchExecutor = (*Executor)(nil)
var _ ssh.RootedFileExecutor = (*Executor)(nil)
