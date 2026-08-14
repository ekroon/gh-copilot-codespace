package daemonclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ekroon/gh-copilot-codespace/internal/daemonproto"
	"github.com/ekroon/gh-copilot-codespace/internal/daemontransport"
)

// fakeDaemon answers daemonproto frames over one stream so tests can model a
// daemon generation dying and being replaced.
type fakeDaemon struct {
	conn       net.Conn
	requests   chan daemonproto.Frame
	silent     atomic.Bool
	served     chan struct{}
	responseMu sync.Mutex
	blocked    sync.Map // map[daemonproto.Verb]<-chan struct{}
}

func newFakeDaemon(t *testing.T, conn net.Conn, version string, verbs []daemonproto.Verb) *fakeDaemon {
	t.Helper()
	d := &fakeDaemon{
		conn:     conn,
		requests: make(chan daemonproto.Frame, 64),
		served:   make(chan struct{}),
	}
	go d.serve(version, verbs)
	t.Cleanup(func() { d.kill() })
	return d
}

func (d *fakeDaemon) serve(version string, verbs []daemonproto.Verb) {
	defer close(d.served)
	enc := daemonproto.NewEncoder(d.conn)
	if err := enc.Write(daemonproto.NewHello(version, verbs)); err != nil {
		return
	}
	dec := daemonproto.NewDecoder(d.conn)
	for {
		frame, err := dec.Read()
		if err != nil {
			return
		}
		select {
		case d.requests <- frame:
		default:
		}
		if frame.Type != daemonproto.TypeRequest || d.silent.Load() {
			continue
		}
		go d.respond(enc, frame)
	}
}

func (d *fakeDaemon) respond(enc *daemonproto.Encoder, frame daemonproto.Frame) {
	if release, ok := d.blocked.Load(frame.Verb); ok {
		<-release.(<-chan struct{})
	}
	var result any = map[string]any{}
	if frame.Verb == daemonproto.VerbPing {
		result = daemonproto.PingResult{Pong: true, PID: 4242}
	}
	response, err := daemonproto.NewResponse(frame.ID, result)
	if err != nil {
		return
	}
	d.responseMu.Lock()
	defer d.responseMu.Unlock()
	_ = enc.Write(response)
}

func (d *fakeDaemon) kill() { _ = d.conn.Close() }

func (d *fakeDaemon) block(verb daemonproto.Verb, release <-chan struct{}) {
	d.blocked.Store(verb, release)
}

func (d *fakeDaemon) nextRequest(t *testing.T) daemonproto.Frame {
	t.Helper()
	select {
	case frame := <-d.requests:
		return frame
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for daemon request")
		return daemonproto.Frame{}
	}
}

func (d *fakeDaemon) requestCount() int { return len(d.requests) }

// scriptedTransport hands out one stream per Spawn according to a script.
type scriptedTransport struct {
	mu       sync.Mutex
	deploys  int
	spawns   int
	paths    []string
	closes   int
	recovers int

	spawn   func(attempt int) (io.ReadWriteCloser, error)
	recover func(attempt int) error
}

func (t *scriptedTransport) Name() string { return "scripted" }

func (t *scriptedTransport) Deploy(context.Context) (string, error) {
	t.mu.Lock()
	t.deploys++
	t.mu.Unlock()
	return "/remote/helper", nil
}

func (t *scriptedTransport) Spawn(ctx context.Context, remotePath string) (io.ReadWriteCloser, error) {
	t.mu.Lock()
	t.spawns++
	attempt := t.spawns
	t.paths = append(t.paths, remotePath)
	spawn := t.spawn
	t.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return spawn(attempt)
}

func (t *scriptedTransport) Close() error {
	t.mu.Lock()
	t.closes++
	t.mu.Unlock()
	return nil
}

func (t *scriptedTransport) Recover(ctx context.Context) error {
	t.mu.Lock()
	t.recovers++
	attempt := t.recovers
	recoverFunc := t.recover
	t.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if recoverFunc == nil {
		return nil
	}
	return recoverFunc(attempt)
}

func (t *scriptedTransport) spawnCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.spawns
}

func (t *scriptedTransport) deployCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.deploys
}

func (t *scriptedTransport) closeCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closes
}

func (t *scriptedTransport) recoverCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.recovers
}

func (t *scriptedTransport) spawnPaths() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.paths...)
}

// daemonGeneration bundles a stream end and the fake daemon answering it.
type daemonGeneration struct {
	stream *closeSignalConn
	daemon *fakeDaemon
}

func newDaemonGeneration(t *testing.T, version string, verbs []daemonproto.Verb) *daemonGeneration {
	t.Helper()
	client, server := net.Pipe()
	return &daemonGeneration{
		stream: newCloseSignalConn(client),
		daemon: newFakeDaemon(t, server, version, verbs),
	}
}

// dialScripted dials an executor whose generations are produced by gens in
// order. Extra spawn attempts fail with errNoMoreGenerations.
var errNoMoreGenerations = errors.New("scripted transport: no more generations")

func dialScripted(t *testing.T, gens ...*daemonGeneration) (*Executor, *scriptedTransport) {
	t.Helper()
	transport := &scriptedTransport{
		spawn: func(attempt int) (io.ReadWriteCloser, error) {
			if attempt > len(gens) {
				return nil, errNoMoreGenerations
			}
			return gens[attempt-1].stream, nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	e, err := Dial(ctx, transport)
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })
	e.reconnectCooldown = 10 * time.Millisecond
	e.maxReconnectCooldown = 10 * time.Millisecond
	e.helloTimeout = 2 * time.Second
	e.reconnectTimeout = 5 * time.Second
	e.idleProbeAfter = time.Hour
	e.inFlightProbeAfter = time.Hour
	e.probeTimeout = 100 * time.Millisecond
	return e, transport
}

func waitConnectionDead(t *testing.T, c *connection) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if c.dead() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("timed out waiting for connection to die")
}

func TestExecutorReconnectsAfterStreamDeath(t *testing.T) {
	first := newDaemonGeneration(t, daemonproto.ProtocolVersion, daemonproto.AllVerbs())
	second := newDaemonGeneration(t, daemonproto.ProtocolVersion, daemonproto.AllVerbs())
	e, transport := dialScripted(t, first, second)

	if _, err := e.Ping(context.Background()); err != nil {
		t.Fatalf("first Ping() error = %v", err)
	}
	original := e.current()
	first.daemon.kill()
	waitConnectionDead(t, original)

	result, err := e.Ping(context.Background())
	if err != nil {
		t.Fatalf("Ping() after stream death error = %v", err)
	}
	if !result.Pong {
		t.Fatal("Ping().Pong = false, want true")
	}
	if got := transport.spawnCount(); got != 2 {
		t.Fatalf("spawn count = %d, want 2", got)
	}
	if got := transport.deployCount(); got != 1 {
		t.Fatalf("deploy count = %d, want 1 (reconnect must reuse the deployed path)", got)
	}
	for _, path := range transport.spawnPaths() {
		if path != "/remote/helper" {
			t.Fatalf("spawn path = %q, want /remote/helper", path)
		}
	}
	if frame := second.daemon.nextRequest(t); frame.Verb != daemonproto.VerbPing {
		t.Fatalf("replacement daemon received %q, want ping", frame.Verb)
	}
	if e.current().id == original.id {
		t.Fatal("executor kept the dead generation")
	}
}

func TestExecutorIdlePreflightRecoversBeforeSendingUserRequest(t *testing.T) {
	first := newDaemonGeneration(t, daemonproto.ProtocolVersion, daemonproto.AllVerbs())
	second := newDaemonGeneration(t, daemonproto.ProtocolVersion, daemonproto.AllVerbs())
	e, transport := dialScripted(t, first, second)
	e.idleProbeAfter = 0
	e.probeTimeout = 20 * time.Millisecond
	first.daemon.silent.Store(true)

	_, _, _, err := e.RunBash(context.Background(), "printf ready", "/workspaces/project")
	if err != nil {
		t.Fatalf("RunBash() error = %v", err)
	}

	if frame := first.daemon.nextRequest(t); frame.Verb != daemonproto.VerbPing {
		t.Fatalf("stale daemon received %q, want preflight ping", frame.Verb)
	}
	if got := first.daemon.requestCount(); got != 0 {
		t.Fatalf("stale daemon received %d additional requests, want no user operation", got)
	}
	if frame := second.daemon.nextRequest(t); frame.Verb != daemonproto.VerbRunBash {
		t.Fatalf("replacement daemon received %q, want run_bash", frame.Verb)
	}
	if got := transport.spawnCount(); got != 2 {
		t.Fatalf("spawn count = %d, want 2", got)
	}
	if got := transport.recoverCount(); got != 1 {
		t.Fatalf("recover count = %d, want 1", got)
	}
}

func TestExecutorConcurrentIdlePreflightsShareReconnect(t *testing.T) {
	first := newDaemonGeneration(t, daemonproto.ProtocolVersion, daemonproto.AllVerbs())
	second := newDaemonGeneration(t, daemonproto.ProtocolVersion, daemonproto.AllVerbs())
	e, transport := dialScripted(t, first, second)
	e.idleProbeAfter = 0
	e.probeTimeout = 20 * time.Millisecond
	first.daemon.silent.Store(true)

	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		go func() {
			_, _, _, err := e.RunBash(context.Background(), "true", "/workspaces/project")
			errs <- err
		}()
	}
	for i := 0; i < 4; i++ {
		if frame := first.daemon.nextRequest(t); frame.Verb != daemonproto.VerbPing {
			t.Fatalf("stale daemon request %d = %q, want only preflight pings", i, frame.Verb)
		}
	}
	for i := 0; i < 4; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("RunBash() error = %v", err)
		}
	}
	if got := transport.spawnCount(); got != 2 {
		t.Fatalf("spawn count = %d, want one shared reconnect", got)
	}
	if got := transport.recoverCount(); got != 1 {
		t.Fatalf("recover count = %d, want one shared transport recovery", got)
	}
}

func TestExecutorSilentInFlightCallFailsBoundedWithoutReplay(t *testing.T) {
	first := newDaemonGeneration(t, daemonproto.ProtocolVersion, daemonproto.AllVerbs())
	second := newDaemonGeneration(t, daemonproto.ProtocolVersion, daemonproto.AllVerbs())
	e, transport := dialScripted(t, first, second)
	e.inFlightProbeAfter = 10 * time.Millisecond
	e.probeTimeout = 20 * time.Millisecond
	first.daemon.silent.Store(true)

	start := time.Now()
	_, _, _, err := e.RunBash(context.Background(), "touch /tmp/unknown", "/workspaces/project")
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("RunBash() took %s, want bounded liveness failure", elapsed)
	}

	var lost *ConnectionLostError
	if !errors.As(err, &lost) {
		t.Fatalf("RunBash() error = %T %v, want *ConnectionLostError", err, err)
	}
	if !lost.Reconnected || !lost.OutcomeUnknown {
		t.Fatalf("ConnectionLostError = %+v, want reconnected unknown outcome", lost)
	}
	firstRequest := first.daemon.nextRequest(t)
	secondRequest := first.daemon.nextRequest(t)
	if firstRequest.Verb != daemonproto.VerbRunBash || secondRequest.Verb != daemonproto.VerbPing {
		t.Fatalf("stale daemon requests = [%q, %q], want [run_bash, ping]", firstRequest.Verb, secondRequest.Verb)
	}
	time.Sleep(50 * time.Millisecond)
	if got := second.daemon.requestCount(); got != 0 {
		t.Fatalf("replacement daemon received %d requests, want no replay", got)
	}
	if got := transport.recoverCount(); got != 1 {
		t.Fatalf("recover count = %d, want 1", got)
	}
}

func TestExecutorHealthyLongRunningCallSurvivesLivenessProbes(t *testing.T) {
	generation := newDaemonGeneration(t, daemonproto.ProtocolVersion, daemonproto.AllVerbs())
	e, _ := dialScripted(t, generation)
	e.inFlightProbeAfter = 10 * time.Millisecond
	e.probeTimeout = 100 * time.Millisecond

	release := make(chan struct{})
	generation.daemon.block(daemonproto.VerbRunBash, release)
	result := make(chan error, 1)
	go func() {
		_, _, _, err := e.RunBash(context.Background(), "sleep 1", "/workspaces/project")
		result <- err
	}()

	if frame := generation.daemon.nextRequest(t); frame.Verb != daemonproto.VerbRunBash {
		t.Fatalf("first request = %q, want run_bash", frame.Verb)
	}
	if frame := generation.daemon.nextRequest(t); frame.Verb != daemonproto.VerbPing {
		t.Fatalf("liveness request = %q, want ping", frame.Verb)
	}
	close(release)
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("RunBash() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("RunBash() did not complete")
	}
}

func TestExecutorContextCancelDuringProbeSendsOriginalCancel(t *testing.T) {
	generation := newDaemonGeneration(t, daemonproto.ProtocolVersion, daemonproto.AllVerbs())
	e, _ := dialScripted(t, generation)
	e.inFlightProbeAfter = 10 * time.Millisecond
	e.probeTimeout = time.Second

	release := make(chan struct{})
	generation.daemon.block(daemonproto.VerbRunBash, release)
	generation.daemon.block(daemonproto.VerbPing, release)
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, _, _, err := e.RunBash(ctx, "sleep 30", "/workspaces/project")
		result <- err
	}()

	request := generation.daemon.nextRequest(t)
	if request.Verb != daemonproto.VerbRunBash {
		t.Fatalf("first request = %q, want run_bash", request.Verb)
	}
	if frame := generation.daemon.nextRequest(t); frame.Verb != daemonproto.VerbPing {
		t.Fatalf("liveness request = %q, want ping", frame.Verb)
	}
	cancel()

	cancelFrame := generation.daemon.nextRequest(t)
	if cancelFrame.Type != daemonproto.TypeCancel || cancelFrame.ID != request.ID {
		t.Fatalf("cancel frame = type %q id %d, want cancel id %d", cancelFrame.Type, cancelFrame.ID, request.ID)
	}
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("RunBash() error = %v, want context.Canceled", err)
	}
}

func TestExecutorSlowLargeResponseCountsByteProgressAsLiveness(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()

	payload := strings.Repeat("x", 64<<10)
	serverErr := make(chan error, 1)
	go func() {
		enc := daemonproto.NewEncoder(server)
		if err := enc.Write(daemonproto.NewHello(daemonproto.ProtocolVersion, daemonproto.AllVerbs())); err != nil {
			serverErr <- err
			return
		}
		dec := daemonproto.NewDecoder(server)
		request, err := dec.Read()
		if err != nil {
			serverErr <- err
			return
		}
		if request.Verb != daemonproto.VerbRunBash {
			serverErr <- fmt.Errorf("request verb = %q, want run_bash", request.Verb)
			return
		}
		go func() {
			for {
				if _, err := dec.Read(); err != nil {
					return
				}
			}
		}()

		response, err := daemonproto.NewResponse(request.ID, daemonproto.RunBashResult{
			Stdout:   payload,
			ExitCode: 0,
		})
		if err != nil {
			serverErr <- err
			return
		}
		data, err := daemonproto.MarshalFrame(response)
		if err != nil {
			serverErr <- err
			return
		}
		for len(data) > 0 {
			chunk := min(len(data), 1024)
			if _, err := server.Write(data[:chunk]); err != nil {
				serverErr <- err
				return
			}
			data = data[chunk:]
			time.Sleep(3 * time.Millisecond)
		}
		serverErr <- nil
	}()

	e, err := Dial(context.Background(), &pipeTransport{stream: client})
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	defer e.Close()
	e.idleProbeAfter = time.Hour
	e.inFlightProbeAfter = 10 * time.Millisecond
	e.probeTimeout = 20 * time.Millisecond

	stdout, _, code, err := e.RunBash(context.Background(), "large-output", "/tmp")
	if err != nil {
		t.Fatalf("RunBash() error = %v", err)
	}
	if code != 0 || stdout != payload {
		t.Fatalf("RunBash() = code %d, stdout bytes %d; want code 0, bytes %d", code, len(stdout), len(payload))
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestExecutorInterruptedCallIsNotReplayed(t *testing.T) {
	first := newDaemonGeneration(t, daemonproto.ProtocolVersion, daemonproto.AllVerbs())
	second := newDaemonGeneration(t, daemonproto.ProtocolVersion, daemonproto.AllVerbs())
	e, transport := dialScripted(t, first, second)

	first.daemon.silent.Store(true)
	callErr := make(chan error, 1)
	go func() {
		_, err := e.Ping(context.Background())
		callErr <- err
	}()
	if frame := first.daemon.nextRequest(t); frame.Verb != daemonproto.VerbPing {
		t.Fatalf("first daemon received %q, want ping", frame.Verb)
	}
	first.daemon.kill()

	var err error
	select {
	case err = <-callErr:
	case <-time.After(10 * time.Second):
		t.Fatal("interrupted Ping() did not return")
	}

	var lost *ConnectionLostError
	if !errors.As(err, &lost) {
		t.Fatalf("Ping() error = %T %v, want *ConnectionLostError", err, err)
	}
	if !lost.Reconnected {
		t.Fatalf("ConnectionLostError.Reconnected = false, err = %v", err)
	}
	if !lost.OutcomeUnknown {
		t.Fatal("ConnectionLostError.OutcomeUnknown = false, want true for an in-flight request")
	}
	if lost.Cause == nil {
		t.Fatal("ConnectionLostError.Cause = nil, want terminal stream cause")
	}
	if !strings.Contains(err.Error(), "not retried") {
		t.Fatalf("error text = %q, want no-replay guidance", err.Error())
	}
	if got := transport.spawnCount(); got != 2 {
		t.Fatalf("spawn count = %d, want 2", got)
	}
	time.Sleep(100 * time.Millisecond)
	if got := second.daemon.requestCount(); got != 0 {
		t.Fatalf("replacement daemon received %d requests, want 0 (no replay)", got)
	}
}

func TestExecutorConcurrentCallersShareOneReconnect(t *testing.T) {
	first := newDaemonGeneration(t, daemonproto.ProtocolVersion, daemonproto.AllVerbs())
	second := newDaemonGeneration(t, daemonproto.ProtocolVersion, daemonproto.AllVerbs())
	e, transport := dialScripted(t, first, second)

	if _, err := e.Ping(context.Background()); err != nil {
		t.Fatalf("first Ping() error = %v", err)
	}
	original := e.current()
	first.daemon.kill()
	waitConnectionDead(t, original)

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := e.Ping(context.Background()); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Ping() error = %v", err)
	}
	if got := transport.spawnCount(); got != 2 {
		t.Fatalf("spawn count = %d, want exactly one reconnect", got)
	}
}

func TestExecutorPendingCallsOnDeadGenerationFail(t *testing.T) {
	first := newDaemonGeneration(t, daemonproto.ProtocolVersion, daemonproto.AllVerbs())
	second := newDaemonGeneration(t, daemonproto.ProtocolVersion, daemonproto.AllVerbs())
	e, _ := dialScripted(t, first, second)

	first.daemon.silent.Store(true)
	errs := make(chan error, 3)
	for i := 0; i < 3; i++ {
		go func() {
			_, err := e.Ping(context.Background())
			errs <- err
		}()
	}
	for i := 0; i < 3; i++ {
		first.daemon.nextRequest(t)
	}
	first.daemon.kill()

	for i := 0; i < 3; i++ {
		select {
		case err := <-errs:
			var lost *ConnectionLostError
			if !errors.As(err, &lost) {
				t.Fatalf("pending call error = %T %v, want *ConnectionLostError", err, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("pending call did not fail after generation death")
		}
	}
	time.Sleep(100 * time.Millisecond)
	if got := second.daemon.requestCount(); got != 0 {
		t.Fatalf("replacement daemon received %d requests, want 0", got)
	}
}

func TestExecutorStaleGenerationCallCannotKillNewGeneration(t *testing.T) {
	first := newDaemonGeneration(t, daemonproto.ProtocolVersion, daemonproto.AllVerbs())
	second := newDaemonGeneration(t, daemonproto.ProtocolVersion, daemonproto.AllVerbs())
	e, _ := dialScripted(t, first, second)

	stale := e.current()
	first.daemon.kill()
	waitConnectionDead(t, stale)
	if _, err := e.Ping(context.Background()); err != nil {
		t.Fatalf("Ping() after death error = %v", err)
	}
	fresh := e.current()
	if fresh == stale {
		t.Fatal("executor did not publish a new generation")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.callOn(ctx, stale, daemonproto.VerbPing, daemonproto.PingParams{}); err == nil {
		t.Fatal("call on stale generation error = nil, want failure")
	}
	if fresh.dead() {
		t.Fatal("stale generation call killed the live generation")
	}
	if _, err := e.Ping(context.Background()); err != nil {
		t.Fatalf("Ping() on live generation error = %v", err)
	}
}

func TestExecutorClosePreventsReconnect(t *testing.T) {
	first := newDaemonGeneration(t, daemonproto.ProtocolVersion, daemonproto.AllVerbs())
	second := newDaemonGeneration(t, daemonproto.ProtocolVersion, daemonproto.AllVerbs())
	e, transport := dialScripted(t, first, second)

	original := e.current()
	first.daemon.kill()
	waitConnectionDead(t, original)
	if err := e.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if _, err := e.Ping(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("Ping() after Close error = %v, want ErrClosed", err)
	}
	if got := transport.spawnCount(); got != 1 {
		t.Fatalf("spawn count = %d, want 1 (no reconnect after Close)", got)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
	if got := transport.closeCount(); got != 1 {
		t.Fatalf("transport close count = %d, want 1", got)
	}
}

func TestExecutorCloseDuringReconnectDisposesUnpublishedStream(t *testing.T) {
	first := newDaemonGeneration(t, daemonproto.ProtocolVersion, daemonproto.AllVerbs())
	second := newDaemonGeneration(t, daemonproto.ProtocolVersion, daemonproto.AllVerbs())

	spawnEntered := make(chan struct{})
	releaseSpawn := make(chan struct{})
	transport := &scriptedTransport{}
	transport.spawn = func(attempt int) (io.ReadWriteCloser, error) {
		if attempt == 1 {
			return first.stream, nil
		}
		close(spawnEntered)
		<-releaseSpawn
		return second.stream, nil
	}

	e, err := Dial(context.Background(), transport)
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	e.reconnectCooldown = 10 * time.Millisecond
	e.maxReconnectCooldown = 10 * time.Millisecond

	original := e.current()
	first.daemon.kill()
	waitConnectionDead(t, original)

	pingErr := make(chan error, 1)
	go func() {
		_, err := e.Ping(context.Background())
		pingErr <- err
	}()
	<-spawnEntered

	closeDone := make(chan error, 1)
	go func() { closeDone <- e.Close() }()
	time.Sleep(50 * time.Millisecond)
	close(releaseSpawn)

	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close() did not return while reconnect was in flight")
	}
	select {
	case <-second.stream.closed:
	case <-time.After(3 * time.Second):
		t.Fatal("unpublished reconnect stream was not disposed")
	}
	if err := <-pingErr; err == nil {
		t.Fatal("Ping() during Close error = nil, want failure")
	}
	if got := transport.closeCount(); got != 1 {
		t.Fatalf("transport close count = %d, want 1", got)
	}
}

func TestExecutorReconnectFailureObeysCooldown(t *testing.T) {
	first := newDaemonGeneration(t, daemonproto.ProtocolVersion, daemonproto.AllVerbs())
	transport := &scriptedTransport{}
	spawnFailure := errors.New("codespace is gone")
	transport.spawn = func(attempt int) (io.ReadWriteCloser, error) {
		if attempt == 1 {
			return first.stream, nil
		}
		return nil, spawnFailure
	}

	e, err := Dial(context.Background(), transport)
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })
	e.reconnectCooldown = 300 * time.Millisecond
	e.maxReconnectCooldown = 300 * time.Millisecond

	original := e.current()
	first.daemon.kill()
	waitConnectionDead(t, original)

	if _, err := e.Ping(context.Background()); err == nil {
		t.Fatal("Ping() after unrecoverable death error = nil")
	} else if !errors.Is(err, spawnFailure) {
		t.Fatalf("Ping() error = %v, want spawn failure cause", err)
	}
	if got := transport.spawnCount(); got != 2 {
		t.Fatalf("spawn count = %d, want 2", got)
	}

	for i := 0; i < 5; i++ {
		if _, err := e.Ping(context.Background()); err == nil {
			t.Fatal("Ping() during cooldown error = nil")
		}
	}
	if got := transport.spawnCount(); got != 2 {
		t.Fatalf("spawn count during cooldown = %d, want 2", got)
	}

	time.Sleep(350 * time.Millisecond)
	if _, err := e.Ping(context.Background()); err == nil {
		t.Fatal("Ping() after cooldown error = nil")
	}
	if got := transport.spawnCount(); got != 3 {
		t.Fatalf("spawn count after cooldown = %d, want 3", got)
	}
}

func TestExecutorTransportRecoveryFailureIsBoundedAndCooledDown(t *testing.T) {
	first := newDaemonGeneration(t, daemonproto.ProtocolVersion, daemonproto.AllVerbs())
	recoveryFailure := errors.New("codespace wake failed")
	transport := &scriptedTransport{
		spawn: func(attempt int) (io.ReadWriteCloser, error) {
			if attempt != 1 {
				return nil, errNoMoreGenerations
			}
			return first.stream, nil
		},
		recover: func(int) error {
			return recoveryFailure
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	e, err := Dial(ctx, transport)
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })
	e.reconnectCooldown = 300 * time.Millisecond
	e.maxReconnectCooldown = 300 * time.Millisecond

	original := e.current()
	first.daemon.kill()
	waitConnectionDead(t, original)

	start := time.Now()
	if _, err := e.Ping(context.Background()); err == nil {
		t.Fatal("Ping() after wake failure error = nil")
	} else if !errors.Is(err, recoveryFailure) {
		t.Fatalf("Ping() error = %v, want recovery failure", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("wake failure took %s, want bounded failure", elapsed)
	}
	if got := transport.recoverCount(); got != 1 {
		t.Fatalf("recover count = %d, want 1", got)
	}
	if got := transport.spawnCount(); got != 1 {
		t.Fatalf("spawn count = %d, want no Spawn after failed recovery", got)
	}

	for i := 0; i < 3; i++ {
		if _, err := e.Ping(context.Background()); err == nil {
			t.Fatal("Ping() during recovery cooldown error = nil")
		}
	}
	if got := transport.recoverCount(); got != 1 {
		t.Fatalf("recover count during cooldown = %d, want 1", got)
	}
}

func TestExecutorReconnectRejectsCapabilityMismatch(t *testing.T) {
	first := newDaemonGeneration(t, daemonproto.ProtocolVersion, daemonproto.AllVerbs())
	verbs := daemonproto.AllVerbs()
	for index, verb := range verbs {
		if verb == daemonproto.VerbReadFile {
			verbs = append(verbs[:index], verbs[index+1:]...)
			break
		}
	}
	second := newDaemonGeneration(t, daemonproto.ProtocolVersion, verbs)
	e, transport := dialScripted(t, first, second)
	e.reconnectCooldown = time.Second
	e.maxReconnectCooldown = time.Second

	original := e.current()
	first.daemon.kill()
	waitConnectionDead(t, original)

	_, err := e.Ping(context.Background())
	if err == nil || !strings.Contains(err.Error(), string(daemonproto.VerbReadFile)) {
		t.Fatalf("Ping() error = %v, want missing read_file capability", err)
	}
	if got := transport.spawnCount(); got != 2 {
		t.Fatalf("spawn count = %d, want a single bounded attempt", got)
	}
}

func TestExecutorReconnectRejectsProtocolMismatch(t *testing.T) {
	first := newDaemonGeneration(t, daemonproto.ProtocolVersion, daemonproto.AllVerbs())
	second := newDaemonGeneration(t, "1", daemonproto.AllVerbs())
	e, transport := dialScripted(t, first, second)
	e.reconnectCooldown = time.Second
	e.maxReconnectCooldown = time.Second

	original := e.current()
	first.daemon.kill()
	waitConnectionDead(t, original)

	_, err := e.Ping(context.Background())
	if err == nil || !strings.Contains(err.Error(), `unsupported daemon protocol version "1"`) {
		t.Fatalf("Ping() error = %v, want protocol rejection", err)
	}
	if got := transport.spawnCount(); got != 2 {
		t.Fatalf("spawn count = %d, want a single bounded attempt", got)
	}
}

func TestExecutorReconnectRefreshesCapabilitiesAndKeepsWorkdir(t *testing.T) {
	limited := daemonproto.AllVerbs()
	for index, verb := range limited {
		if verb == daemonproto.VerbWaitSession {
			limited = append(limited[:index], limited[index+1:]...)
			break
		}
	}
	first := newDaemonGeneration(t, daemonproto.ProtocolVersion, limited)
	second := newDaemonGeneration(t, daemonproto.ProtocolVersion, daemonproto.AllVerbs())
	e, _ := dialScripted(t, first, second)
	e.SetWorkdir("/workspaces/project")

	if e.SupportsWaitSession() {
		t.Fatal("SupportsWaitSession() = true on limited daemon, want false")
	}
	original := e.current()
	first.daemon.kill()
	waitConnectionDead(t, original)

	if _, _, _, err := e.RunBash(context.Background(), "true", ""); err != nil {
		t.Fatalf("RunBash() after reconnect error = %v", err)
	}
	if got := e.GetWorkdir(); got != "/workspaces/project" {
		t.Fatalf("GetWorkdir() = %q, want preserved workdir", got)
	}
	frame := second.daemon.nextRequest(t)
	if !strings.Contains(string(frame.Params), "/workspaces/project") {
		t.Fatalf("run_bash params = %s, want workdir cwd", frame.Params)
	}
	if !e.SupportsWaitSession() {
		t.Fatal("SupportsWaitSession() = false after reconnect to a full daemon, want true")
	}
}

func TestExecutorRequestIDsStayMonotonicAcrossGenerations(t *testing.T) {
	first := newDaemonGeneration(t, daemonproto.ProtocolVersion, daemonproto.AllVerbs())
	second := newDaemonGeneration(t, daemonproto.ProtocolVersion, daemonproto.AllVerbs())
	e, _ := dialScripted(t, first, second)

	if _, err := e.Ping(context.Background()); err != nil {
		t.Fatalf("first Ping() error = %v", err)
	}
	firstFrame := first.daemon.nextRequest(t)
	original := e.current()
	first.daemon.kill()
	waitConnectionDead(t, original)

	if _, err := e.Ping(context.Background()); err != nil {
		t.Fatalf("second Ping() error = %v", err)
	}
	secondFrame := second.daemon.nextRequest(t)
	if secondFrame.ID <= firstFrame.ID {
		t.Fatalf("request ids = %d then %d, want strictly increasing across generations", firstFrame.ID, secondFrame.ID)
	}
}

func TestConnectionLostErrorText(t *testing.T) {
	cause := errors.New("ssh exited with status 255")
	reconnected := &ConnectionLostError{Cause: cause, Reconnected: true, OutcomeUnknown: true, OldGeneration: 1, NewGeneration: 2}
	if !strings.Contains(reconnected.Error(), "not retried") ||
		!strings.Contains(reconnected.Error(), "outcome may be unknown") ||
		!strings.Contains(reconnected.Error(), cause.Error()) {
		t.Fatalf("reconnected error text = %q", reconnected.Error())
	}
	if !errors.Is(reconnected, cause) {
		t.Fatal("reconnected error does not unwrap to its cause")
	}

	reconnectErr := errors.New("codespace is shut down")
	failed := &ConnectionLostError{Cause: cause, ReconnectErr: reconnectErr}
	if !strings.Contains(failed.Error(), "automatic wake/reconnect failed") ||
		!strings.Contains(failed.Error(), reconnectErr.Error()) {
		t.Fatalf("failed error text = %q", failed.Error())
	}
	if !errors.Is(failed, reconnectErr) {
		t.Fatal("failed error does not unwrap to its reconnect error")
	}
}

type causeStream struct {
	io.ReadWriteCloser
	cause error
}

func (s *causeStream) TerminalCause() error { return s.cause }

type reportingStream struct {
	io.ReadWriteCloser
	cause error
	grace atomic.Int64
}

func (s *reportingStream) TerminalError(waitGrace time.Duration) error {
	s.grace.Store(int64(waitGrace))
	return s.cause
}

func TestConnectionPrefersTransportTerminalCause(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	exitErr := fmt.Errorf("ssh exited with status 255")
	c := newConnection(1, &causeStream{ReadWriteCloser: client, cause: exitErr})
	_ = client.Close()
	<-c.readerDone

	if got := c.cause(); !errors.Is(got, exitErr) {
		t.Fatalf("cause() = %v, want transport terminal cause", got)
	}
}

func TestConnectionUsesGraceAwareTerminalErrorReporter(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	exitErr := fmt.Errorf("ssh exited with status 255")
	stream := &reportingStream{ReadWriteCloser: client, cause: exitErr}
	c := newConnection(1, stream)
	_ = client.Close()
	<-c.readerDone

	if got := c.cause(); !errors.Is(got, exitErr) {
		t.Fatalf("cause() = %v, want transport terminal error", got)
	}
	if got := time.Duration(stream.grace.Load()); got != terminalCauseGrace {
		t.Fatalf("TerminalError grace = %v, want %v", got, terminalCauseGrace)
	}
}

func TestConnectionKeepsMeaningfulRecordedCause(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	stream := &reportingStream{ReadWriteCloser: client, cause: errors.New("process exited")}
	c := newConnection(1, stream)
	recorded := errors.New("daemonclient: deliver cancel for request 1: blocked")
	c.fail(recorded)
	<-c.readerDone

	if got := c.cause(); !errors.Is(got, recorded) {
		t.Fatalf("cause() = %v, want recorded cause", got)
	}
}

// The structural interfaces daemonclient consumes must stay assignable from
// the transport contract, otherwise terminal causes would silently disappear
// from connection-loss reports.
var _ terminalErrorReporter = (daemontransport.TerminalErrorReporter)(nil)
