// Package daemontransport provides byte-stream transports for daemonproto.
package daemontransport

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ErrNotImplemented is returned by transport stubs that do not yet have an
// executable implementation.
var ErrNotImplemented = errors.New("daemontransport: not implemented")

// StderrTailLimit is the maximum number of trailing stderr bytes retained for
// terminal-error diagnostics. Stderr is still forwarded in full to os.Stderr.
const StderrTailLimit = 8 << 10

// Transport opens a full-duplex byte stream to a daemon process running inside
// a sandbox (codespace over SSH, devcontainer over docker exec, WSL distro over
// `wsl`, etc.). Implementations own the lifecycle of getting the binary in
// place (Deploy) and starting the long-lived daemon process (Spawn). The
// DaemonExecutor speaks daemonproto over the io.ReadWriteCloser returned by
// Spawn.
type Transport interface {
	// Name returns a short diagnostic identifier for the transport, such as
	// "ssh", "local", "devcontainer", or "wsl".
	Name() string

	// Deploy makes sure the daemon binary is present at the canonical remote
	// path. Returns the absolute remote path the daemon should be launched from.
	// May be a no-op for transports where the binary is provided by the sandbox
	// image. Called at most once per Transport instance.
	Deploy(ctx context.Context) (remotePath string, err error)

	// Spawn starts the daemon subcommand at remotePath and returns the
	// bidirectional stream over which daemonproto frames are exchanged. The
	// ReadWriteCloser is owned by the caller; closing it terminates the remote
	// daemon process. Multiple Spawn calls on the same Transport are allowed
	// (e.g., to reconnect after a stream death), but each call produces an
	// independent stream and process.
	Spawn(ctx context.Context, remotePath string) (io.ReadWriteCloser, error)

	// Close releases any resources held by the transport itself (control sockets,
	// helper processes, etc.). Does NOT terminate already-spawned streams — those
	// are owned by their callers.
	Close() error
}

// TerminalErrorReporter is implemented by streams whose underlying process can
// explain why the stream died. Clients that observe EOF or a write failure may
// type-assert their io.ReadWriteCloser to this interface to obtain a richer
// cause than "connection lost".
type TerminalErrorReporter interface {
	// TerminalError waits up to waitGrace for the backing process to exit and
	// returns the cause of its termination. It returns nil when the process is
	// still running after waitGrace, or when the stream was closed
	// intentionally by the caller.
	TerminalError(waitGrace time.Duration) error
}

// TerminalProcessError describes an unexpected exit of the process backing a
// transport stream. Stderr holds the captured tail of the process's standard
// error, truncated to StderrTailLimit bytes.
type TerminalProcessError struct {
	Transport string
	WaitErr   error
	Stderr    string
}

func (e *TerminalProcessError) Error() string {
	var b strings.Builder
	b.WriteString("daemontransport: ")
	if e.Transport != "" {
		b.WriteString(e.Transport)
		b.WriteString(" ")
	}
	b.WriteString("daemon process exited")
	if e.WaitErr != nil {
		b.WriteString(": ")
		b.WriteString(e.WaitErr.Error())
	}
	if e.Stderr != "" {
		b.WriteString(": stderr: ")
		b.WriteString(e.Stderr)
	}
	return b.String()
}

func (e *TerminalProcessError) Unwrap() error { return e.WaitErr }

// stderrTail forwards every write to sink while retaining the trailing
// StderrTailLimit bytes for diagnostics. It is safe for concurrent use.
type stderrTail struct {
	sink  io.Writer
	limit int

	mu      sync.Mutex
	buf     []byte
	written int64
}

func newStderrTail(sink io.Writer, limit int) *stderrTail {
	return &stderrTail{sink: sink, limit: limit}
}

func (t *stderrTail) Write(p []byte) (int, error) {
	t.mu.Lock()
	t.append(p)
	t.mu.Unlock()

	if t.sink == nil {
		return len(p), nil
	}
	return t.sink.Write(p)
}

func (t *stderrTail) append(p []byte) {
	if t.limit <= 0 {
		return
	}
	t.written += int64(len(p))
	if len(p) >= t.limit {
		t.buf = append(t.buf[:0], p[len(p)-t.limit:]...)
		return
	}
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.limit {
		t.buf = append(t.buf[:0], t.buf[len(t.buf)-t.limit:]...)
	}
}

// String returns the retained tail, prefixed with an ellipsis when earlier
// output was dropped.
func (t *stderrTail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.buf) == 0 {
		return ""
	}
	tail := strings.TrimSpace(string(t.buf))
	if tail == "" {
		return ""
	}
	if t.written > int64(len(t.buf)) {
		return "..." + tail
	}
	return tail
}

type processStream struct {
	stdout io.ReadCloser
	stdin  io.WriteCloser
	cmd    *exec.Cmd
	name   string
	stderr *stderrTail

	writeMu   sync.Mutex
	closeOnce sync.Once
	done      chan struct{}
	waitErr   error
	closeErr  error

	stateMu       sync.Mutex
	closeRequests bool
}

func newProcessStream(name string, cmd *exec.Cmd, stdin io.WriteCloser, stdout io.ReadCloser, stderr *stderrTail) *processStream {
	s := &processStream{
		stdout: stdout,
		stdin:  stdin,
		cmd:    cmd,
		name:   name,
		stderr: stderr,
		done:   make(chan struct{}),
	}
	go func() {
		// waitErr is written before done is closed and read only after done
		// synchronization, so no additional locking is required.
		s.waitErr = cmd.Wait()
		close(s.done)
	}()
	return s
}

func (s *processStream) Read(p []byte) (int, error) {
	return s.stdout.Read(p)
}

func (s *processStream) Write(p []byte) (int, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.stdin.Write(p)
}

func (s *processStream) Close() error {
	s.closeOnce.Do(func() {
		s.markCloseRequested()
		s.closeErr = s.stdin.Close()
		select {
		case <-s.done:
		case <-time.After(2 * time.Second):
			if s.cmd.Process != nil {
				_ = s.cmd.Process.Kill()
			}
			<-s.done
		}
	})
	return s.closeErr
}

// TerminalError implements TerminalErrorReporter. It returns nil when the
// stream was closed intentionally or when the process is still running after
// waitGrace, so a normal Close never surfaces as a connection-loss cause.
func (s *processStream) TerminalError(waitGrace time.Duration) error {
	if s.closeWasRequested() {
		return nil
	}

	if waitGrace > 0 {
		timer := time.NewTimer(waitGrace)
		defer timer.Stop()
		select {
		case <-s.done:
		case <-timer.C:
		}
	}

	select {
	case <-s.done:
	default:
		return nil
	}

	// Close may have been requested while we waited for the process to exit.
	if s.closeWasRequested() {
		return nil
	}

	return &TerminalProcessError{
		Transport: s.name,
		WaitErr:   s.waitErr,
		Stderr:    s.stderrTail(),
	}
}

func (s *processStream) stderrTail() string {
	if s.stderr == nil {
		return ""
	}
	return s.stderr.String()
}

func (s *processStream) markCloseRequested() {
	s.stateMu.Lock()
	s.closeRequests = true
	s.stateMu.Unlock()
}

func (s *processStream) closeWasRequested() bool {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.closeRequests
}

var _ TerminalErrorReporter = (*processStream)(nil)
