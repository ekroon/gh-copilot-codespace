package daemonclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
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
	enc       *daemonproto.Encoder
	encMu     sync.Mutex
	dec       *daemonproto.Decoder

	nextID  atomic.Uint64
	pending sync.Map // map[uint64]chan daemonproto.Frame

	workdir   string
	workdirMu sync.RWMutex

	readerDone chan struct{}
	readerErr  atomic.Value // error from the reader goroutine; sticky

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
		enc:        daemonproto.NewEncoder(stream),
		dec:        daemonproto.NewDecoder(stream),
		readerDone: make(chan struct{}),
	}

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
			e.readerErr.Store(err)
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
	id := e.nextID.Add(1)
	frame, err := daemonproto.NewRequest(id, verb, params, "")
	if err != nil {
		return nil, fmt.Errorf("daemonclient: encode %s request: %w", verb, err)
	}

	responseCh := make(chan daemonproto.Frame, 1)
	e.pending.Store(id, responseCh)
	defer e.pending.Delete(id)

	if err := e.writeFrame(frame); err != nil {
		return nil, fmt.Errorf("daemonclient: write %s request: %w", verb, err)
	}

	select {
	case response := <-responseCh:
		return e.processResponse(verb, response)
	case <-ctx.Done():
		_ = e.writeFrame(daemonproto.NewCancel(id))
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

func (e *Executor) writeFrame(frame daemonproto.Frame) error {
	e.encMu.Lock()
	defer e.encMu.Unlock()
	return e.enc.Write(frame)
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

func (e *Executor) ViewFile(ctx context.Context, path string, viewRange []int) (string, error) {
	raw, err := e.call(ctx, daemonproto.VerbViewFile, daemonproto.ViewFileParams{Path: path, ViewRange: viewRange})
	if err != nil {
		return "", err
	}
	var result daemonproto.ViewFileResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", fmt.Errorf("daemonclient: decode view_file result: %w", err)
	}
	return result.Content, nil
}

func (e *Executor) EditFile(ctx context.Context, path, oldStr, newStr string) error {
	raw, err := e.call(ctx, daemonproto.VerbEditFile, daemonproto.EditFileParams{Path: path, OldStr: oldStr, NewStr: newStr})
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
	raw, err := e.call(ctx, daemonproto.VerbCreateFile, daemonproto.CreateFileParams{Path: path, Content: content})
	if err != nil {
		return err
	}
	var result daemonproto.CreateFileResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("daemonclient: decode create_file result: %w", err)
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

func (e *Executor) Grep(ctx context.Context, pattern, path, globPattern, cwd string) (string, error) {
	if cwd == "" {
		cwd = e.GetWorkdir()
	}
	raw, err := e.call(ctx, daemonproto.VerbGrep, daemonproto.GrepParams{Pattern: pattern, Path: path, Glob: globPattern, Cwd: cwd})
	if err != nil {
		return "", err
	}
	var result daemonproto.GrepResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", fmt.Errorf("daemonclient: decode grep result: %w", err)
	}
	return result.Output, nil
}

func (e *Executor) Glob(ctx context.Context, pattern, path, cwd string) (string, error) {
	if cwd == "" {
		cwd = e.GetWorkdir()
	}
	raw, err := e.call(ctx, daemonproto.VerbGlob, daemonproto.GlobParams{Pattern: pattern, Path: path, Cwd: cwd})
	if err != nil {
		return "", err
	}
	var result daemonproto.GlobResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", fmt.Errorf("daemonclient: decode glob result: %w", err)
	}
	return result.Output, nil
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
