package daemonclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ekroon/gh-copilot-codespace/internal/daemonproto"
)

// errConnectionDead reports that a generation was already terminal when a
// caller tried to use it. Callers translate it into a ConnectionLostError with
// the generation's recorded cause.
var errConnectionDead = errors.New("daemonclient: daemon connection is no longer usable")

// terminalCauser is the optional contract a transport stream may implement to
// explain why the underlying process or connection ended (subprocess exit
// status, stderr tail, ...). daemonclient depends on these structural
// interfaces only, so no concrete transport type is imported here.
type terminalCauser interface {
	TerminalCause() error
}

// terminalErrorReporter is the grace-aware variant of terminalCauser: it waits
// up to waitGrace for the underlying process result before reporting.
type terminalErrorReporter interface {
	TerminalError(waitGrace time.Duration) error
}

// terminalCauseGrace bounds how long a terminal-cause lookup may wait for the
// transport's subprocess result.
const terminalCauseGrace = 250 * time.Millisecond

type daemonWriteDeadlineSetter interface {
	SetWriteDeadline(time.Time) error
}

// connection is a single daemon stream generation. It owns the stream, its
// decoder, write serialization, the pending request map, the terminal error,
// reader completion, and the immutable handshake snapshot. Everything that can
// die with a stream lives here; the Executor keeps only state that must
// survive reconnection.
type connection struct {
	id        uint64
	stream    io.ReadWriteCloser
	dec       *daemonproto.Decoder
	writeMu   chan struct{}
	pending   sync.Map // map[uint64]chan daemonproto.Frame
	activity  atomic.Int64
	readBytes atomic.Uint64

	helloCh    chan daemonproto.Frame
	readerDone chan struct{}
	errOnce    sync.Once
	err        atomic.Value // terminal error; sticky

	// hello and verbs are written once during the handshake, before the
	// generation is published to other goroutines, and are read-only after.
	hello daemonproto.Frame
	verbs map[string]struct{}
}

func newConnection(id uint64, stream io.ReadWriteCloser) *connection {
	c := &connection{
		id:         id,
		stream:     stream,
		writeMu:    make(chan struct{}, 1),
		helloCh:    make(chan daemonproto.Frame, 1),
		readerDone: make(chan struct{}),
	}
	c.dec = daemonproto.NewDecoder(&activityReader{reader: stream, connection: c})
	c.touch()
	c.writeMu <- struct{}{}
	go c.readLoop()
	return c
}

func (c *connection) readLoop() {
	defer close(c.readerDone)
	sawHello := false
	for {
		frame, err := c.dec.Read()
		if err != nil {
			c.setErr(err)
			return
		}
		c.touch()

		if !sawHello {
			sawHello = true
			select {
			case c.helloCh <- frame:
			default:
			}
			continue
		}

		switch frame.Type {
		case daemonproto.TypeResponse:
			if ch, ok := c.pending.Load(frame.ID); ok {
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

func (c *connection) setErr(err error) {
	if err == nil {
		return
	}
	c.errOnce.Do(func() { c.err.Store(err) })
}

// fail records a terminal error for this generation and closes its stream. It
// can never affect any other generation.
func (c *connection) fail(err error) {
	c.setErr(err)
	_ = c.stream.Close()
}

// dispose closes the stream and waits for the reader to drain so pending
// callers observe the terminal state.
func (c *connection) dispose() {
	_ = c.stream.Close()
	<-c.readerDone
}

func (c *connection) dead() bool {
	select {
	case <-c.readerDone:
		return true
	default:
	}
	return c.err.Load() != nil
}

func (c *connection) recordedErr() error {
	if v := c.err.Load(); v != nil {
		if err, ok := v.(error); ok {
			return err
		}
	}
	return nil
}

// cause returns the best available explanation for the generation's death.
// A transport-supplied terminal cause is preferred over closed-pipe symptoms.
func (c *connection) cause() error {
	recorded := c.recordedErr()
	terminal := c.streamCause()
	switch {
	case terminal == nil && recorded == nil:
		return errors.New("daemonclient: daemon connection closed")
	case terminal == nil:
		return recorded
	case recorded == nil:
		return terminal
	case isClosedStreamSymptom(recorded):
		return fmt.Errorf("%w: %w", recorded, terminal)
	default:
		return recorded
	}
}

func (c *connection) streamCause() error {
	switch s := c.stream.(type) {
	case terminalErrorReporter:
		return s.TerminalError(terminalCauseGrace)
	case terminalCauser:
		return s.TerminalCause()
	default:
		return nil
	}
}

func isClosedStreamSymptom(err error) bool {
	return errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, io.ErrClosedPipe) ||
		errors.Is(err, os.ErrClosed) ||
		errors.Is(err, net.ErrClosed)
}

func (c *connection) supports(verb daemonproto.Verb) bool {
	_, ok := c.verbs[string(verb)]
	return ok
}

func (c *connection) touch() {
	c.activity.Store(time.Now().UnixNano())
}

func (c *connection) idleFor() time.Duration {
	last := c.activity.Load()
	if last == 0 {
		return 0
	}
	return time.Since(time.Unix(0, last))
}

func (c *connection) readProgress() uint64 {
	return c.readBytes.Load()
}

type activityReader struct {
	reader     io.Reader
	connection *connection
}

func (r *activityReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 {
		r.connection.readBytes.Add(uint64(n))
		r.connection.touch()
	}
	return n, err
}

// writeFrame serializes one frame onto this generation's stream. It reports
// whether any bytes reached the stream so callers can tell a never-sent
// request from one whose outcome is unknown.
func (c *connection) writeFrame(ctx context.Context, frame daemonproto.Frame) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if c.dead() {
		return false, errConnectionDead
	}
	select {
	case <-c.writeMu:
	case <-ctx.Done():
		return false, ctx.Err()
	case <-c.readerDone:
		return false, errConnectionDead
	}
	defer func() { c.writeMu <- struct{}{} }()

	if err := ctx.Err(); err != nil {
		return false, err
	}
	if c.dead() {
		return false, errConnectionDead
	}
	data, err := daemonproto.MarshalFrame(frame)
	if err != nil {
		return false, err
	}

	if deadlineWriter, ok := c.stream.(daemonWriteDeadlineSetter); ok {
		return c.writeFrameWithDeadline(ctx, deadlineWriter, data)
	}

	stopClose := context.AfterFunc(ctx, func() {
		c.fail(fmt.Errorf("daemonclient: stream write abandoned: %w", context.Cause(ctx)))
	})
	written, writeErr := writeDaemonFrameBytes(c.stream, data)
	_ = stopClose()
	if ctx.Err() != nil {
		return written > 0, ctx.Err()
	}
	if writeErr != nil {
		if written > 0 {
			c.fail(writeErr)
		}
		return written > 0, writeErr
	}
	return true, nil
}

func (c *connection) writeFrameWithDeadline(ctx context.Context, writer daemonWriteDeadlineSetter, data []byte) (bool, error) {
	if deadline, ok := ctx.Deadline(); ok {
		if err := writer.SetWriteDeadline(deadline); err != nil {
			return false, fmt.Errorf("set write deadline: %w", err)
		}
	}

	deadlineSet := make(chan struct{})
	stopDeadline := context.AfterFunc(ctx, func() {
		_ = writer.SetWriteDeadline(time.Now())
		close(deadlineSet)
	})
	written, writeErr := writeDaemonFrameBytes(c.stream, data)
	if !stopDeadline() {
		<-deadlineSet
	}
	resetErr := writer.SetWriteDeadline(time.Time{})

	if ctx.Err() != nil {
		if written > 0 {
			c.fail(fmt.Errorf("daemonclient: stream write abandoned after %d bytes: %w", written, ctx.Err()))
		}
		return written > 0, ctx.Err()
	}
	if writeErr != nil {
		if written > 0 {
			c.fail(writeErr)
		}
		var timeoutErr interface{ Timeout() bool }
		if errors.As(writeErr, &timeoutErr) && timeoutErr.Timeout() {
			if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
				return written > 0, context.DeadlineExceeded
			}
		}
		return written > 0, writeErr
	}
	if resetErr != nil {
		c.fail(fmt.Errorf("daemonclient: reset write deadline: %w", resetErr))
		return true, fmt.Errorf("reset write deadline: %w", resetErr)
	}
	return true, nil
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
