package daemonclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	pathpkg "path"
	"strings"
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
// independent id and the writer is serialized through a mutex.
type Executor struct {
	transport daemontransport.Transport
	stream    io.ReadWriteCloser // owned; closed by Close()
	writeMu   chan struct{}
	dec       *daemonproto.Decoder

	nextID  atomic.Uint64
	pending sync.Map // map[uint64]chan daemonproto.Frame

	workdir   string
	workdirMu sync.RWMutex

	readerDone    chan struct{}
	readerErr     atomic.Value // terminal stream error; sticky
	readerErrOnce sync.Once

	helloOnce sync.Once
	hello     daemonproto.Frame
	helloErr  error
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

	stream, err := t.Spawn(ctx, remotePath)
	if err != nil {
		return nil, err
	}

	e := &Executor{
		transport:  t,
		stream:     stream,
		writeMu:    make(chan struct{}, 1),
		dec:        daemonproto.NewDecoder(stream),
		readerDone: make(chan struct{}),
	}
	e.writeMu <- struct{}{}

	helloCh := make(chan daemonproto.Frame, 1)
	e.pending.Store(uint64(0), helloCh)
	go e.readLoop()

	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()

	select {
	case frame := <-helloCh:
		e.pending.Delete(uint64(0))
		if e.helloErr != nil {
			_ = stream.Close()
			<-e.readerDone
			return nil, e.helloErr
		}
		if frame.Type != daemonproto.TypeHello {
			_ = stream.Close()
			<-e.readerDone
			return nil, fmt.Errorf("daemonclient: expected hello frame, got %q", frame.Type)
		}
		if frame.Version != daemonproto.ProtocolVersion {
			_ = stream.Close()
			<-e.readerDone
			return nil, fmt.Errorf("daemonclient: unsupported daemon protocol version %q (want %q)", frame.Version, daemonproto.ProtocolVersion)
		}
		if missing := missingDaemonVerbs(frame.Verbs, daemonproto.FilesystemVerbs()); len(missing) > 0 {
			_ = stream.Close()
			<-e.readerDone
			return nil, fmt.Errorf("daemonclient: daemon missing required filesystem capabilities: %s", strings.Join(missing, ", "))
		}
		return e, nil
	case <-ctx.Done():
		e.pending.Delete(uint64(0))
		_ = stream.Close()
		<-e.readerDone
		return nil, ctx.Err()
	case <-timer.C:
		e.pending.Delete(uint64(0))
		_ = stream.Close()
		<-e.readerDone
		return nil, errors.New("daemonclient: timed out waiting for daemon hello")
	case <-e.readerDone:
		e.pending.Delete(uint64(0))
		_ = stream.Close()
		return nil, fmt.Errorf("daemonclient: daemon exited before hello: %w", e.readErr())
	}
}

func (e *Executor) readLoop() {
	defer close(e.readerDone)
	sawHello := false
	for {
		frame, err := e.dec.Read()
		if err != nil {
			e.setReaderErr(err)
			return
		}

		if !sawHello {
			if frame.Type != daemonproto.TypeHello {
				e.helloErr = fmt.Errorf("daemonclient: expected hello frame, got %q", frame.Type)
				if ch, ok := e.pending.Load(uint64(0)); ok {
					select {
					case ch.(chan daemonproto.Frame) <- frame:
					default:
					}
				}
				continue
			}
			sawHello = true
			e.helloOnce.Do(func() { e.hello = frame })
			if ch, ok := e.pending.Load(uint64(0)); ok {
				select {
				case ch.(chan daemonproto.Frame) <- frame:
				default:
				}
			} else {
				fmt.Fprintln(os.Stderr, "daemonclient: ignoring unexpected hello frame")
			}
			continue
		}

		switch frame.Type {
		case daemonproto.TypeResponse:
			if ch, ok := e.pending.Load(frame.ID); ok {
				select {
				case ch.(chan daemonproto.Frame) <- frame:
				default:
				}
			}
		case daemonproto.TypeHello:
			fmt.Fprintln(os.Stderr, "daemonclient: ignoring late hello frame")
		case daemonproto.TypeRequest, daemonproto.TypeCancel:
			fmt.Fprintf(os.Stderr, "daemonclient: ignoring unexpected %s frame for id %d\n", frame.Type, frame.ID)
		default:
			fmt.Fprintf(os.Stderr, "daemonclient: ignoring unknown frame type %q\n", frame.Type)
		}
	}
}

func (e *Executor) call(ctx context.Context, verb daemonproto.Verb, params any) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id := e.nextID.Add(1)
	frame, err := daemonproto.NewRequest(id, verb, params, "")
	if err != nil {
		return nil, fmt.Errorf("daemonclient: encode %s request: %w", verb, err)
	}

	responseCh := make(chan daemonproto.Frame, 1)
	e.pending.Store(id, responseCh)
	defer e.pending.Delete(id)

	if err := e.writeFrame(ctx, frame); err != nil {
		return nil, fmt.Errorf("daemonclient: write %s request: %w", verb, err)
	}

	select {
	case response := <-responseCh:
		return e.processResponse(verb, response)
	case <-ctx.Done():
		cancelCtx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		cancelErr := e.writeFrame(cancelCtx, daemonproto.NewCancel(id))
		cancel()
		if cancelErr != nil {
			e.failStream(fmt.Errorf("daemonclient: deliver cancel for request %d: %w", id, cancelErr))
		}
		timer := time.NewTimer(250 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-responseCh:
		case <-e.readerDone:
		case <-timer.C:
		}
		return nil, ctx.Err()
	case <-e.readerDone:
		return nil, e.readErr()
	}
}

func (e *Executor) failStream(err error) {
	e.setReaderErr(err)
	_ = e.stream.Close()
}

func (e *Executor) setReaderErr(err error) {
	if err != nil {
		e.readerErrOnce.Do(func() {
			e.readerErr.Store(err)
		})
	}
}

type daemonWriteDeadlineSetter interface {
	SetWriteDeadline(time.Time) error
}

func (e *Executor) writeFrame(ctx context.Context, frame daemonproto.Frame) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-e.writeMu:
	case <-ctx.Done():
		return ctx.Err()
	case <-e.readerDone:
		return e.readErr()
	}
	defer func() { e.writeMu <- struct{}{} }()

	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := daemonproto.MarshalFrame(frame)
	if err != nil {
		return err
	}

	if deadlineWriter, ok := e.stream.(daemonWriteDeadlineSetter); ok {
		return e.writeFrameWithDeadline(ctx, deadlineWriter, data)
	}

	stopClose := context.AfterFunc(ctx, func() {
		_ = e.stream.Close()
	})
	written, writeErr := writeDaemonFrameBytes(e.stream, data)
	_ = stopClose()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if writeErr != nil {
		if written > 0 {
			_ = e.stream.Close()
		}
		return writeErr
	}
	return nil
}

func (e *Executor) writeFrameWithDeadline(ctx context.Context, writer daemonWriteDeadlineSetter, data []byte) error {
	if deadline, ok := ctx.Deadline(); ok {
		if err := writer.SetWriteDeadline(deadline); err != nil {
			return fmt.Errorf("set write deadline: %w", err)
		}
	}

	deadlineSet := make(chan struct{})
	stopDeadline := context.AfterFunc(ctx, func() {
		_ = writer.SetWriteDeadline(time.Now())
		close(deadlineSet)
	})
	written, writeErr := writeDaemonFrameBytes(e.stream, data)
	if !stopDeadline() {
		<-deadlineSet
	}
	resetErr := writer.SetWriteDeadline(time.Time{})

	if ctx.Err() != nil {
		if written > 0 {
			_ = e.stream.Close()
		}
		return ctx.Err()
	}
	if writeErr != nil {
		if written > 0 {
			_ = e.stream.Close()
		}
		var timeoutErr interface{ Timeout() bool }
		if errors.As(writeErr, &timeoutErr) && timeoutErr.Timeout() {
			if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
				return context.DeadlineExceeded
			}
		}
		return writeErr
	}
	if resetErr != nil {
		_ = e.stream.Close()
		return fmt.Errorf("reset write deadline: %w", resetErr)
	}
	return nil
}

func writeDaemonFrameBytes(writer io.Writer, data []byte) (int, error) {
	total := 0
	for len(data) > 0 {
		written, err := writer.Write(data)
		total += written
		if written > 0 {
			data = data[written:]
		}
		if err != nil {
			return total, err
		}
		if written == 0 {
			return total, io.ErrShortWrite
		}
	}
	return total, nil
}

func missingDaemonVerbs(advertised []string, required []daemonproto.Verb) []string {
	available := make(map[string]struct{}, len(advertised))
	for _, verb := range advertised {
		available[verb] = struct{}{}
	}
	var missing []string
	for _, verb := range required {
		if _, ok := available[string(verb)]; !ok {
			missing = append(missing, string(verb))
		}
	}
	return missing
}

func (e *Executor) processResponse(verb daemonproto.Verb, frame daemonproto.Frame) (json.RawMessage, error) {
	if frame.Error != nil {
		if frame.Error.Code == daemonproto.ErrCodeCanceled {
			return nil, context.Canceled
		}
		return nil, &RemoteError{Code: frame.Error.Code, Message: frame.Error.Message}
	}
	return frame.Result, nil
}

func (e *Executor) readErr() error {
	if v := e.readerErr.Load(); v != nil {
		if err, ok := v.(error); ok {
			return err
		}
	}
	return errors.New("daemonclient: reader stopped")
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

func (e *Executor) Close() error {
	if e.stream != nil {
		_ = e.stream.Close()
	}
	<-e.readerDone
	if e.transport != nil {
		_ = e.transport.Close()
	}
	return nil
}

// Compile-time check: Executor must implement ssh.Executor.
var _ ssh.Executor = (*Executor)(nil)
var _ ssh.FilesystemExecutor = (*Executor)(nil)
var _ ssh.ApplyPatchExecutor = (*Executor)(nil)
var _ ssh.RootedFileExecutor = (*Executor)(nil)
