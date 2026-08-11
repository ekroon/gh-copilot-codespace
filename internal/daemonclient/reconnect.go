package daemonclient

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ekroon/gh-copilot-codespace/internal/daemonproto"
)

// ErrClosed is returned when an operation is attempted on a closed Executor.
var ErrClosed = errors.New("daemonclient: executor is closed")

const (
	defaultHelloTimeout      = 10 * time.Second
	defaultReconnectTimeout  = 30 * time.Second
	defaultReconnectCooldown = 2 * time.Second
	defaultMaxCooldown       = 30 * time.Second

	// maxReconnectAttemptsPerCall bounds how many times a single caller may
	// trigger a reconnect before giving up, so an unstable daemon cannot make
	// one tool call spawn endlessly.
	maxReconnectAttemptsPerCall = 2
)

// ConnectionLostError reports that the daemon connection died while an
// operation was in flight. The executor never replays interrupted operations:
// it only restores the connection and reports what happened so the caller can
// decide whether repeating the operation is safe.
type ConnectionLostError struct {
	// Cause is the terminal error of the lost connection generation.
	Cause error
	// ReconnectErr is non-nil when reconnection did not succeed.
	ReconnectErr error
	// Reconnected reports whether a replacement generation is available.
	Reconnected bool
	// OutcomeUnknown reports whether request bytes may have reached the daemon,
	// meaning the operation may have executed remotely.
	OutcomeUnknown bool
	// OldGeneration and NewGeneration identify the connection generations.
	OldGeneration uint64
	NewGeneration uint64
}

func (e *ConnectionLostError) Error() string {
	var b strings.Builder
	b.WriteString("daemonclient: connection to the Codespace was lost")
	if e.Cause != nil {
		fmt.Fprintf(&b, " (%v)", e.Cause)
	}
	if e.Reconnected {
		b.WriteString(" and a new daemon connection was established.")
		if e.OutcomeUnknown {
			b.WriteString(" This operation was not retried and its outcome may be unknown." +
				" Retry read-only operations. Inspect remote state before repeating mutations.")
		} else {
			b.WriteString(" This operation was not sent and was not retried; it is safe to retry.")
		}
		return b.String()
	}
	if e.ReconnectErr != nil {
		fmt.Fprintf(&b, " and reconnection failed: %v.", e.ReconnectErr)
	} else {
		b.WriteString(" and no daemon connection is available.")
	}
	if e.OutcomeUnknown {
		b.WriteString(" This operation was not retried and its outcome may be unknown." +
			" Inspect remote state before repeating mutations.")
	} else {
		b.WriteString(" This operation was not sent and was not retried.")
	}
	b.WriteString(" Retry later or reconnect the Codespace.")
	return b.String()
}

func (e *ConnectionLostError) Unwrap() []error {
	errs := make([]error, 0, 2)
	if e.Cause != nil {
		errs = append(errs, e.Cause)
	}
	if e.ReconnectErr != nil {
		errs = append(errs, e.ReconnectErr)
	}
	return errs
}

// current returns the executor's current connection generation, dead or alive.
func (e *Executor) current() *connection {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.conn
}

// connectionFor returns a live generation for a new operation, reconnecting
// lazily when the current generation is terminal.
func (e *Executor) connectionFor(ctx context.Context) (*connection, error) {
	conn, err := e.obtainConnection(ctx, nil)
	if err == nil {
		return conn, nil
	}
	if errors.Is(err, ErrClosed) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil, err
	}
	lost := &ConnectionLostError{ReconnectErr: err}
	if previous := e.current(); previous != nil {
		lost.OldGeneration = previous.id
		if previous.dead() {
			lost.Cause = previous.cause()
		}
	}
	return nil, lost
}

// connectionLost restores the transport after generation c died and reports
// the outcome. The interrupted operation is never replayed.
func (e *Executor) connectionLost(ctx context.Context, c *connection, outcomeUnknown bool) error {
	lost := &ConnectionLostError{
		Cause:          c.cause(),
		OutcomeUnknown: outcomeUnknown,
		OldGeneration:  c.id,
	}
	next, err := e.obtainConnection(ctx, c)
	if err != nil {
		lost.ReconnectErr = err
		return lost
	}
	lost.Reconnected = true
	lost.NewGeneration = next.id
	return lost
}

// obtainConnection returns a live generation different from avoid. Concurrent
// callers coordinate so that exactly one reconnect attempt runs at a time; a
// caller giving up (its own context expired) never cancels the shared attempt.
func (e *Executor) obtainConnection(ctx context.Context, avoid *connection) (*connection, error) {
	attempts := 0
	for {
		e.mu.Lock()
		if e.closed {
			e.mu.Unlock()
			return nil, ErrClosed
		}
		if c := e.conn; c != nil && c != avoid && !c.dead() {
			e.mu.Unlock()
			return c, nil
		}
		if waiter := e.reconnecting; waiter != nil {
			e.mu.Unlock()
			select {
			case <-waiter:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			continue
		}
		if remaining := time.Until(e.cooldownUntil); remaining > 0 {
			err := e.lastReconnectErr
			e.mu.Unlock()
			if err == nil {
				err = errors.New("daemonclient: daemon connection is unavailable")
			}
			return nil, fmt.Errorf("daemonclient: reconnect on cooldown for another %s: %w", remaining.Round(time.Millisecond), err)
		}
		if attempts >= maxReconnectAttemptsPerCall {
			err := e.lastReconnectErr
			e.mu.Unlock()
			if err == nil {
				err = errors.New("daemonclient: daemon connection is unstable")
			}
			return nil, err
		}
		waiter := make(chan struct{})
		e.reconnecting = waiter
		e.mu.Unlock()

		attempts++
		go e.reconnect(waiter)
		select {
		case <-waiter:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// reconnect performs exactly one Spawn plus handshake on a detached context so
// that an individual caller's cancellation cannot abort shared recovery. It
// never redeploys: the remote helper path from the initial deployment is
// reused.
func (e *Executor) reconnect(waiter chan struct{}) {
	defer close(waiter)

	if previous := e.current(); previous != nil {
		fmt.Fprintf(os.Stderr, "daemonclient: daemon connection lost (%v); reconnecting\n", previous.cause())
	}

	ctx, cancel := context.WithTimeout(context.Background(), e.reconnectTimeout)
	defer cancel()
	go func() {
		select {
		case <-e.closeSignal:
			cancel()
		case <-ctx.Done():
		}
	}()

	conn, err := e.connect(ctx)
	if err != nil {
		e.mu.Lock()
		e.reconnecting = nil
		e.reconnectFailures++
		e.lastReconnectErr = err
		e.cooldownUntil = time.Now().Add(e.reconnectBackoff())
		e.mu.Unlock()
		fmt.Fprintf(os.Stderr, "daemonclient: daemon reconnect failed: %v\n", err)
		return
	}

	e.mu.Lock()
	e.reconnecting = nil
	if e.closed {
		e.mu.Unlock()
		conn.dispose()
		return
	}
	previous := e.conn
	e.conn = conn
	e.reconnectFailures = 0
	e.lastReconnectErr = nil
	e.cooldownUntil = time.Time{}
	e.mu.Unlock()

	if previous != nil {
		previous.dispose()
	}
	fmt.Fprintf(os.Stderr, "daemonclient: reconnected to daemon (generation %d)\n", conn.id)
}

// connect spawns a daemon over the existing transport and completes the
// handshake, validating protocol version and required filesystem capabilities.
func (e *Executor) connect(ctx context.Context) (*connection, error) {
	stream, err := e.transport.Spawn(ctx, e.remotePath)
	if err != nil {
		return nil, err
	}
	conn := newConnection(e.nextGeneration.Add(1), stream)
	if err := e.handshake(ctx, conn); err != nil {
		conn.dispose()
		return nil, err
	}
	return conn, nil
}

// handshake waits for the daemon hello and publishes the immutable capability
// snapshot on conn.
func (e *Executor) handshake(ctx context.Context, conn *connection) error {
	timer := time.NewTimer(e.helloTimeout)
	defer timer.Stop()

	select {
	case frame := <-conn.helloCh:
		if frame.Type != daemonproto.TypeHello {
			return fmt.Errorf("daemonclient: expected hello frame, got %q", frame.Type)
		}
		if frame.Version != daemonproto.ProtocolVersion {
			return fmt.Errorf("daemonclient: unsupported daemon protocol version %q (want %q)", frame.Version, daemonproto.ProtocolVersion)
		}
		if missing := missingDaemonVerbs(frame.Verbs, daemonproto.FilesystemVerbs()); len(missing) > 0 {
			return fmt.Errorf("daemonclient: daemon missing required filesystem capabilities: %s", strings.Join(missing, ", "))
		}
		conn.hello = frame
		conn.verbs = make(map[string]struct{}, len(frame.Verbs))
		for _, verb := range frame.Verbs {
			conn.verbs[verb] = struct{}{}
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return errors.New("daemonclient: timed out waiting for daemon hello")
	case <-conn.readerDone:
		return fmt.Errorf("daemonclient: daemon exited before hello: %w", conn.cause())
	}
}

// reconnectBackoff returns the cooldown applied after a failed attempt. It
// doubles per consecutive failure and is capped, so a deleted Codespace cannot
// make every tool call pay for a spawn attempt.
func (e *Executor) reconnectBackoff() time.Duration {
	base := e.reconnectCooldown
	if base <= 0 {
		base = defaultReconnectCooldown
	}
	max := e.maxReconnectCooldown
	if max <= 0 {
		max = defaultMaxCooldown
	}
	if max < base {
		max = base
	}
	backoff := base
	for i := 1; i < e.reconnectFailures && backoff < max; i++ {
		backoff *= 2
	}
	if backoff > max {
		backoff = max
	}
	return backoff
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
