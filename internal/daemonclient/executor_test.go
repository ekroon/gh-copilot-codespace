package daemonclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ekroon/gh-copilot-codespace/internal/daemonproto"
	"github.com/ekroon/gh-copilot-codespace/internal/daemontransport"
	"github.com/ekroon/gh-copilot-codespace/internal/ssh"
)

var daemonBinary string

type pipeTransport struct {
	stream io.ReadWriteCloser
}

type closeSignalConn struct {
	net.Conn
	closeOnce sync.Once
	closed    chan struct{}
}

func newCloseSignalConn(conn net.Conn) *closeSignalConn {
	return &closeSignalConn{Conn: conn, closed: make(chan struct{})}
}

func (c *closeSignalConn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		err = c.Conn.Close()
		close(c.closed)
	})
	return err
}

func (t *pipeTransport) Name() string { return "pipe" }

func (t *pipeTransport) Deploy(context.Context) (string, error) { return "daemon", nil }

func (t *pipeTransport) Spawn(context.Context, string) (io.ReadWriteCloser, error) {
	return t.stream, nil
}

func (t *pipeTransport) Close() error { return nil }

func TestMain(m *testing.M) {
	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Getwd: %v\n", err)
		os.Exit(1)
	}
	dir, err := os.MkdirTemp(wd, ".daemonclient-bin-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "MkdirTemp: %v\n", err)
		os.Exit(1)
	}
	bin := filepath.Join(dir, "gh-copilot-codespace")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/ekroon/gh-copilot-codespace/cmd/gh-copilot-codespace")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "go build daemon binary: %v\n", err)
		_ = os.RemoveAll(dir)
		os.Exit(1)
	}
	daemonBinary = bin
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func dialDaemon(t *testing.T) *Executor {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	e, err := Dial(ctx, daemontransport.NewLocalTransport(daemonBinary))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e
}

func testDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	dir, err := os.MkdirTemp(wd, ".daemonclient-test-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func waitForWriterIdle(t *testing.T, e *Executor) {
	t.Helper()
	select {
	case <-e.writeMu:
		e.writeMu <- struct{}{}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for daemon writer to become idle")
	}
}

func TestExecutorPing(t *testing.T) {
	e := dialDaemon(t)
	result, err := e.Ping(testContext(t))
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if !result.Pong {
		t.Fatal("PingResult.Pong = false, want true")
	}
	if result.PID == 0 {
		t.Fatal("PingResult.PID = 0, want non-zero")
	}
}

func TestExecutorRejectsPreCanceledRequestBeforeWrite(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()

	serverErr := make(chan error, 1)
	go func() {
		enc := daemonproto.NewEncoder(server)
		if err := enc.Write(daemonproto.NewHello(daemonproto.ProtocolVersion, daemonproto.AllVerbs())); err != nil {
			serverErr <- err
			return
		}
		if err := server.SetReadDeadline(time.Now().Add(250 * time.Millisecond)); err != nil {
			serverErr <- err
			return
		}
		_, err := daemonproto.NewDecoder(server).Read()
		if err == nil {
			serverErr <- errors.New("received request for pre-canceled context")
			return
		}
		var netErr net.Error
		if !errors.As(err, &netErr) || !netErr.Timeout() {
			serverErr <- fmt.Errorf("read error = %v, want timeout", err)
			return
		}
		serverErr <- nil
	}()

	e, err := Dial(context.Background(), &pipeTransport{stream: client})
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	defer e.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.Ping(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Ping() error = %v, want context.Canceled", err)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestExecutorBlockedWriteHonorsDeadlineAndKeepsStreamUsable(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()

	serverErr := make(chan error, 1)
	go func() {
		enc := daemonproto.NewEncoder(server)
		if err := enc.Write(daemonproto.NewHello(daemonproto.ProtocolVersion, daemonproto.AllVerbs())); err != nil {
			serverErr <- err
			return
		}

		time.Sleep(600 * time.Millisecond)
		frame, err := daemonproto.NewDecoder(server).Read()
		if err != nil {
			serverErr <- err
			return
		}
		if frame.ID != 2 || frame.Verb != daemonproto.VerbPing {
			serverErr <- fmt.Errorf("first transmitted request = id %d verb %q, want id 2 ping", frame.ID, frame.Verb)
			return
		}
		response, err := daemonproto.NewResponse(frame.ID, daemonproto.PingResult{Pong: true})
		if err == nil {
			err = enc.Write(response)
		}
		serverErr <- err
	}()

	e, err := Dial(context.Background(), &pipeTransport{stream: client})
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	defer e.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	started := time.Now()
	_, err = e.Ping(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked Ping() error = %v, want context deadline", err)
	}
	if elapsed := time.Since(started); elapsed >= 400*time.Millisecond {
		t.Fatalf("blocked Ping() took %v, want prompt cancellation", elapsed)
	}

	result, err := e.Ping(context.Background())
	if err != nil {
		t.Fatalf("second Ping() error = %v", err)
	}
	if !result.Pong {
		t.Fatal("second Ping().Pong = false, want true")
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestExecutorContextCancelWritesCancelFrame(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()

	requestRead := make(chan struct{})
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
		close(requestRead)
		cancel, err := dec.Read()
		if err != nil {
			serverErr <- err
			return
		}
		if cancel.Type != daemonproto.TypeCancel || cancel.ID != request.ID {
			serverErr <- fmt.Errorf("cancel frame = type %q id %d, want cancel id %d", cancel.Type, cancel.ID, request.ID)
			return
		}
		serverErr <- enc.Write(daemonproto.NewErrorResponse(request.ID, daemonproto.ErrCodeCanceled, "canceled"))
	}()

	e, err := Dial(context.Background(), &pipeTransport{stream: client})
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	defer e.Close()

	ctx, cancel := context.WithCancel(context.Background())
	callErr := make(chan error, 1)
	go func() {
		_, err := e.Ping(ctx)
		callErr <- err
	}()

	<-requestRead
	waitForWriterIdle(t, e)
	cancel()
	if err := <-callErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("Ping() error = %v, want context.Canceled", err)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestExecutorContextCancelClosesStreamWhenCancelWriteIsBlocked(t *testing.T) {
	client, server := net.Pipe()
	signaledClient := newCloseSignalConn(client)
	defer server.Close()

	requestRead := make(chan struct{})
	pendingRequestRead := make(chan struct{})
	blockedWriterObserved := make(chan struct{})
	serverErr := make(chan error, 1)
	go func() {
		enc := daemonproto.NewEncoder(server)
		if err := enc.Write(daemonproto.NewHello(daemonproto.ProtocolVersion, daemonproto.AllVerbs())); err != nil {
			serverErr <- err
			return
		}
		request, err := daemonproto.NewDecoder(server).Read()
		if err != nil {
			serverErr <- err
			return
		}
		if request.Type != daemonproto.TypeRequest || request.Verb != daemonproto.VerbPing {
			serverErr <- fmt.Errorf("request = type %q verb %q, want ping request", request.Type, request.Verb)
			return
		}
		close(requestRead)
		pendingRequest, err := daemonproto.NewDecoder(server).Read()
		if err != nil {
			serverErr <- err
			return
		}
		if pendingRequest.Type != daemonproto.TypeRequest || pendingRequest.Verb != daemonproto.VerbPing {
			serverErr <- fmt.Errorf("pending request = type %q verb %q, want ping request", pendingRequest.Type, pendingRequest.Verb)
			return
		}
		close(pendingRequestRead)

		var firstByte [1]byte
		if _, err := io.ReadFull(server, firstByte[:]); err != nil {
			serverErr <- err
			return
		}
		close(blockedWriterObserved)

		select {
		case <-signaledClient.closed:
		case <-time.After(2 * time.Second):
			serverErr <- errors.New("client stream was not closed after blocked cancel write")
			return
		}
		if _, err := server.Read(firstByte[:]); !errors.Is(err, io.EOF) {
			serverErr <- fmt.Errorf("read after client close = %v, want EOF", err)
			return
		}
		serverErr <- nil
	}()

	e, err := Dial(context.Background(), &pipeTransport{stream: signaledClient})
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	defer e.Close()

	ctx, cancel := context.WithCancel(context.Background())
	targetErr := make(chan error, 1)
	go func() {
		_, err := e.Ping(ctx)
		targetErr <- err
	}()
	<-requestRead

	pendingErr := make(chan error, 1)
	go func() {
		_, err := e.Ping(context.Background())
		pendingErr <- err
	}()
	<-pendingRequestRead

	blockedErr := make(chan error, 1)
	go func() {
		_, err := e.call(context.Background(), daemonproto.VerbPing, map[string]string{
			"payload": strings.Repeat("x", 1<<20),
		})
		blockedErr <- err
	}()
	<-blockedWriterObserved

	cancel()
	if err := <-targetErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Ping() error = %v, want context.Canceled", err)
	}
	select {
	case <-signaledClient.closed:
	case <-time.After(time.Second):
		_ = signaledClient.Close()
		<-blockedErr
		<-serverErr
		t.Fatal("client stream remained open after blocked cancel write")
	}
	if err := <-blockedErr; err == nil || errors.Is(err, context.Canceled) {
		t.Fatalf("blocked writer error = %v, want explicit stream write error", err)
	}
	if err := <-pendingErr; err == nil || !strings.Contains(err.Error(), "deliver cancel") {
		t.Fatalf("pending Ping() error = %v, want explicit cancel delivery failure", err)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestDialRejectsMissingFilesystemCapabilities(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()

	go func() {
		verbs := daemonproto.AllVerbs()
		for index, verb := range verbs {
			if verb == daemonproto.VerbReadFile {
				verbs = append(verbs[:index], verbs[index+1:]...)
				break
			}
		}
		_ = daemonproto.NewEncoder(server).Write(daemonproto.NewHello(daemonproto.ProtocolVersion, verbs))
	}()

	_, err := Dial(context.Background(), &pipeTransport{stream: client})
	if err == nil || !strings.Contains(err.Error(), string(daemonproto.VerbReadFile)) {
		t.Fatalf("Dial() error = %v, want missing read_file capability", err)
	}
}

func TestDialRejectsLegacyProtocolVersion(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()

	go func() {
		_ = daemonproto.NewEncoder(server).Write(daemonproto.NewHello("1", daemonproto.AllVerbs()))
	}()

	_, err := Dial(context.Background(), &pipeTransport{stream: client})
	if err == nil || !strings.Contains(err.Error(), `unsupported daemon protocol version "1"`) {
		t.Fatalf("Dial() error = %v, want legacy version rejection", err)
	}
}

func TestExecutorViewFile(t *testing.T) {
	e := dialDaemon(t)
	path := filepath.Join(testDir(t), "sample.txt")
	if err := os.WriteFile(path, []byte("alpha\nbeta\ngamma\ndelta\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	content, err := e.ViewFile(testContext(t), path, []int{2, 3})
	if err != nil {
		t.Fatalf("ViewFile: %v", err)
	}
	if content != "2. beta\n3. gamma\n" {
		t.Fatalf("content = %q, want %q", content, "2. beta\n3. gamma\n")
	}
}

func TestExecutorCreateThenEdit(t *testing.T) {
	e := dialDaemon(t)
	path := filepath.Join(testDir(t), "nested", "file.txt")
	ctx := testContext(t)

	if err := e.CreateFile(ctx, path, "hello world\n"); err != nil {
		t.Fatalf("CreateFile: %v", err)
	}
	if err := e.EditFile(ctx, path, "world", "daemon"); err != nil {
		t.Fatalf("EditFile: %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(content) != "hello daemon\n" {
		t.Fatalf("file content = %q, want %q", string(content), "hello daemon\n")
	}
}

func TestExecutorWriteFileBinaryOverwriteAndPermissions(t *testing.T) {
	e := dialDaemon(t)
	dir := testDir(t)
	path := filepath.Join(dir, "blob.bin")
	ctx := testContext(t)
	first := []byte{0x00, 0xff, 'a'}

	if err := e.WriteFile(ctx, path, first, false); err != nil {
		t.Fatalf("WriteFile(new): %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(new): %v", err)
	}
	if !bytes.Equal(got, first) {
		t.Fatalf("new bytes = %v, want %v", got, first)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(new): %v", err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Fatalf("new mode = %v, want 0644", got)
	}

	second := []byte{'n', 0x00, 0xfe, 'w'}
	if err := e.WriteFile(ctx, path, second, false); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("WriteFile(refuse) error = %v, want already exists", err)
	}
	got, err = os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(refused): %v", err)
	}
	if !bytes.Equal(got, first) {
		t.Fatalf("refused bytes = %v, want unchanged %v", got, first)
	}

	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	if err := e.WriteFile(ctx, path, second, true); err != nil {
		t.Fatalf("WriteFile(overwrite): %v", err)
	}
	got, err = os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(overwrite): %v", err)
	}
	if !bytes.Equal(got, second) {
		t.Fatalf("overwrite bytes = %v, want %v", got, second)
	}
	info, err = os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(overwrite): %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("overwrite mode = %v, want 0600", got)
	}
}

func TestExecutorWriteFileRejectsSymlinkOverwrite(t *testing.T) {
	e := dialDaemon(t)
	dir := testDir(t)
	target := filepath.Join(dir, "target.bin")
	link := filepath.Join(dir, "link.bin")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatalf("WriteFile(target): %v", err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	err := e.WriteFile(testContext(t), link, []byte("replace"), true)
	if err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("WriteFile(symlink) error = %v, want symbolic link rejection", err)
	}
	got, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatalf("ReadFile(target): %v", readErr)
	}
	if string(got) != "keep" {
		t.Fatalf("target = %q, want unchanged", got)
	}
}

func TestExecutorWriteFileRejectsParentSymlinkEscape(t *testing.T) {
	e := dialDaemon(t)
	root := testDir(t)
	outside := testDir(t)
	e.SetWorkdir(root)
	if err := os.Symlink(outside, filepath.Join(root, "outside")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	err := e.WriteFile(testContext(t), "outside/escaped.bin", []byte("escape"), true)
	if err == nil || !strings.Contains(err.Error(), "escapes workdir") {
		t.Fatalf("WriteFile(parent symlink) error = %v, want workdir escape rejection", err)
	}
	if _, statErr := os.Lstat(filepath.Join(outside, "escaped.bin")); !os.IsNotExist(statErr) {
		t.Fatalf("escaped destination exists or Lstat failed: %v", statErr)
	}
}

func TestExecutorRootedCopyIgnoresMutableWorkdirAndProtectsRoot(t *testing.T) {
	e := dialDaemon(t)
	root := testDir(t)
	nested := filepath.Join(root, "internal", "mcp")
	outside := testDir(t)
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("MkdirAll(nested): %v", err)
	}
	e.SetWorkdir(nested)

	path := filepath.Join(root, "root.bin")
	first := []byte{0x00, 0xff, 'a'}
	if err := e.WriteFileRooted(testContext(t), ssh.RootedWriteRequest{
		Path: path,
		Root: root,
		Data: first,
	}); err != nil {
		t.Fatalf("WriteFileRooted(new): %v", err)
	}
	second := []byte{'n', 0x00, 0xfe, 'w'}
	if err := e.WriteFileRooted(testContext(t), ssh.RootedWriteRequest{
		Path:      path,
		Root:      root,
		Data:      second,
		Overwrite: true,
	}); err != nil {
		t.Fatalf("WriteFileRooted(overwrite): %v", err)
	}
	got, err := e.ReadFileRooted(testContext(t), ssh.RootedReadRequest{Path: path, Root: root})
	if err != nil {
		t.Fatalf("ReadFileRooted: %v", err)
	}
	if !bytes.Equal(got, second) {
		t.Fatalf("root bytes = %v, want %v", got, second)
	}

	outsidePath := filepath.Join(outside, "escaped.bin")
	err = e.WriteFileRooted(testContext(t), ssh.RootedWriteRequest{
		Path:      outsidePath,
		Root:      root,
		Data:      []byte("escape"),
		Overwrite: true,
	})
	if err == nil || !strings.Contains(err.Error(), "escapes workdir") {
		t.Fatalf("WriteFileRooted(outside) error = %v, want root escape rejection", err)
	}
	if _, err := e.ReadFileRooted(testContext(t), ssh.RootedReadRequest{Path: outsidePath, Root: root}); err == nil || !strings.Contains(err.Error(), "escapes workdir") {
		t.Fatalf("ReadFileRooted(outside) error = %v, want root escape rejection", err)
	}

	target := filepath.Join(root, "target.bin")
	link := filepath.Join(root, "link.bin")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatalf("WriteFile(target): %v", err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	err = e.WriteFileRooted(testContext(t), ssh.RootedWriteRequest{
		Path:      link,
		Root:      root,
		Data:      []byte("replace"),
		Overwrite: true,
	})
	if err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("WriteFileRooted(symlink) error = %v, want symlink rejection", err)
	}
}

func TestExecutorRunBashExitCode(t *testing.T) {
	e := dialDaemon(t)
	_, _, exitCode, err := e.RunBash(testContext(t), "exit 3", "")
	if err != nil {
		t.Fatalf("RunBash: %v", err)
	}
	if exitCode != 3 {
		t.Fatalf("exitCode = %d, want 3", exitCode)
	}
}

func TestExecutorRunBashWithCwd(t *testing.T) {
	e := dialDaemon(t)
	dir := testDir(t)
	ctx := testContext(t)

	stdout, stderr, exitCode, err := e.RunBash(ctx, "pwd", dir)
	if err != nil {
		t.Fatalf("RunBash explicit cwd: %v", err)
	}
	if exitCode != 0 || stderr != "" || strings.TrimSpace(stdout) != dir {
		t.Fatalf("RunBash explicit cwd = stdout %q stderr %q exit %d, want pwd %q", stdout, stderr, exitCode, dir)
	}

	e.SetWorkdir(dir)
	stdout, stderr, exitCode, err = e.RunBash(ctx, "pwd", "")
	if err != nil {
		t.Fatalf("RunBash default cwd: %v", err)
	}
	if exitCode != 0 || stderr != "" || strings.TrimSpace(stdout) != dir {
		t.Fatalf("RunBash default cwd = stdout %q stderr %q exit %d, want pwd %q", stdout, stderr, exitCode, dir)
	}
}

func TestExecutorContextCancelKillsRemote(t *testing.T) {
	e := dialDaemon(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	time.AfterFunc(100*time.Millisecond, cancel)

	start := time.Now()
	_, _, _, err := e.RunBash(ctx, "sleep 30", "")
	elapsed := time.Since(start)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunBash error = %v, want context.Canceled", err)
	}
	if elapsed >= 5*time.Second {
		t.Fatalf("cancel took %v, want < 5s", elapsed)
	}
}

func TestExecutorRemoteErrorPropagates(t *testing.T) {
	e := dialDaemon(t)
	missing := filepath.Join(testDir(t), "missing.txt")
	err := e.EditFile(testContext(t), missing, "old", "new")
	var remoteErr *RemoteError
	if !errors.As(err, &remoteErr) {
		t.Fatalf("EditFile error = %T %v, want *RemoteError", err, err)
	}
	if remoteErr.Code != daemonproto.ErrCodeExecFailed {
		t.Fatalf("RemoteError.Code = %q, want %q", remoteErr.Code, daemonproto.ErrCodeExecFailed)
	}
}

func TestExecutorConcurrentCalls(t *testing.T) {
	e := dialDaemon(t)
	path := filepath.Join(testDir(t), "sample.txt")
	want := "1. one\n2. two\n3. three\n"
	if err := os.WriteFile(path, []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	ctx := testContext(t)
	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got, err := e.ViewFile(ctx, path, nil)
			if err != nil {
				errs <- fmt.Errorf("ViewFile %d: %w", i, err)
				return
			}
			if got != want {
				errs <- fmt.Errorf("ViewFile %d content = %q, want %q", i, got, want)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestExecutorCloseTerminatesReader(t *testing.T) {
	e := dialDaemon(t)
	if err := e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-e.readerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("readerDone did not close")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := e.Ping(ctx); err == nil {
		t.Fatal("Ping after Close error = nil, want error")
	}
}

func TestExecutorSessionLifecycle(t *testing.T) {
	e := dialDaemon(t)
	ctx := testContext(t)
	sessionID := fmt.Sprintf("daemonclient-%d", time.Now().UnixNano())
	if err := e.StartSession(ctx, sessionID, "bash --noprofile --norc", testDir(t)); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	t.Cleanup(func() { _ = e.StopSession(context.Background(), sessionID) })

	if err := e.WriteSession(ctx, sessionID, "echo done{enter}"); err != nil {
		t.Fatalf("WriteSession: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	var output string
	for time.Now().Before(deadline) {
		var err error
		output, err = e.ReadSession(ctx, sessionID)
		if err != nil {
			t.Fatalf("ReadSession: %v", err)
		}
		if strings.Contains(output, "done") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !strings.Contains(output, "done") {
		t.Fatalf("ReadSession output = %q, want to contain done", output)
	}

	if err := e.StopSession(ctx, sessionID); err != nil {
		t.Fatalf("StopSession: %v", err)
	}
	list, err := e.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if strings.Contains(list, "copilot-"+sessionID) {
		t.Fatalf("ListSessions output = %q, want stopped session absent", list)
	}
}

func TestExecutorProcessSessionLifecycle(t *testing.T) {
	e := dialDaemon(t)
	ctx := testContext(t)
	if !e.SupportsProcessSessions() {
		t.Skip("daemon process sessions require delegated cgroup v2 support")
	}

	sessionID := fmt.Sprintf("daemonclient-process-%d", time.Now().UnixNano())
	if err := e.StartProcessSession(ctx, sessionID, "printf process-session", testDir(t)); err != nil {
		t.Fatalf("StartProcessSession: %v", err)
	}
	t.Cleanup(func() { _ = e.StopSession(context.Background(), sessionID) })

	output, completed, err := e.WaitSession(ctx, sessionID, time.Second)
	if err != nil {
		t.Fatalf("WaitSession: %v", err)
	}
	if !completed {
		t.Fatal("WaitSession completed = false, want true")
	}
	if !strings.Contains(output, "process-session") {
		t.Fatalf("WaitSession output = %q, want process-session", output)
	}
	if err := e.StopSession(ctx, sessionID); err != nil {
		t.Fatalf("StopSession: %v", err)
	}
}

func TestExecutorDuplicateProcessSessionDoesNotRerun(t *testing.T) {
	e := dialDaemon(t)
	if !e.SupportsProcessSessions() {
		t.Skip("daemon process sessions require delegated cgroup v2 support")
	}
	ctx := testContext(t)
	sessionID := fmt.Sprintf("daemonclient-process-duplicate-%d", time.Now().UnixNano())
	path := filepath.Join(testDir(t), sessionID)
	command := fmt.Sprintf("printf x >> %q; sleep 0.2", path)
	if err := e.StartProcessSession(ctx, sessionID, command, testDir(t)); err != nil {
		t.Fatalf("StartProcessSession first: %v", err)
	}
	t.Cleanup(func() {
		_ = e.StopSession(context.Background(), sessionID)
		_ = os.Remove(path)
	})

	if err := e.StartProcessSession(ctx, sessionID, command, testDir(t)); err == nil {
		t.Fatal("StartProcessSession duplicate error = nil")
	}
	if _, _, err := e.WaitSession(ctx, sessionID, time.Second); err != nil {
		t.Fatalf("WaitSession: %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(content) != "x" {
		t.Fatalf("side effects = %q, want one execution", content)
	}
}

func TestExecutorWaitSessionReturnsOnCompletion(t *testing.T) {
	e := dialDaemon(t)
	ctx := testContext(t)
	sessionID := fmt.Sprintf("daemonclient-wait-%d", time.Now().UnixNano())
	if err := e.StartSession(ctx, sessionID, "printf wait-done", testDir(t)); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	t.Cleanup(func() { _ = e.StopSession(context.Background(), sessionID) })

	start := time.Now()
	output, completed, err := e.WaitSession(ctx, sessionID, 5*time.Second)
	if err != nil {
		t.Fatalf("WaitSession: %v", err)
	}
	if !completed {
		t.Fatal("WaitSession completed = false, want true")
	}
	if !strings.Contains(output, "wait-done") || !strings.Contains(output, "[session exited]") {
		t.Fatalf("WaitSession output = %q, want command output and exit marker", output)
	}
	if elapsed := time.Since(start); elapsed >= time.Second {
		t.Fatalf("WaitSession elapsed = %v, want less than 1s", elapsed)
	}
}

func TestExecutorWaitSessionReturnsOnTimeout(t *testing.T) {
	e := dialDaemon(t)
	ctx := testContext(t)
	sessionID := fmt.Sprintf("daemonclient-timeout-%d", time.Now().UnixNano())
	if err := e.StartSession(ctx, sessionID, "sleep 5", testDir(t)); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	t.Cleanup(func() { _ = e.StopSession(context.Background(), sessionID) })

	start := time.Now()
	_, completed, err := e.WaitSession(ctx, sessionID, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("WaitSession: %v", err)
	}
	if completed {
		t.Fatal("WaitSession completed = true, want false")
	}
	if elapsed := time.Since(start); elapsed >= 250*time.Millisecond {
		t.Fatalf("WaitSession elapsed = %v, want less than 250ms", elapsed)
	}
}

func TestExecutorWaitSessionDoesNotReuseCompletionSignal(t *testing.T) {
	e := dialDaemon(t)
	ctx := testContext(t)
	sessionID := fmt.Sprintf("daemonclient-reuse-%d", time.Now().UnixNano())

	if err := e.StartSession(ctx, sessionID, "printf first", testDir(t)); err != nil {
		t.Fatalf("StartSession first: %v", err)
	}
	if output, completed, err := e.WaitSession(ctx, sessionID, 15*time.Second); err != nil {
		t.Fatalf("WaitSession first: %v", err)
	} else if !completed {
		list, _ := e.ListSessions(ctx)
		t.Fatalf("WaitSession first completed = false, want true; output=%q sessions=%q", output, list)
	}
	if err := e.StopSession(ctx, sessionID); err != nil {
		t.Fatalf("StopSession first: %v", err)
	}

	if err := e.StartSession(ctx, sessionID, "sleep 5", testDir(t)); err != nil {
		t.Fatalf("StartSession second: %v", err)
	}
	t.Cleanup(func() { _ = e.StopSession(context.Background(), sessionID) })
	if _, completed, err := e.WaitSession(ctx, sessionID, 50*time.Millisecond); err != nil {
		t.Fatalf("WaitSession second: %v", err)
	} else if completed {
		t.Fatal("WaitSession second completed = true, want false")
	}
}

func TestExecutorDuplicateSessionStartPreservesExistingSession(t *testing.T) {
	e := dialDaemon(t)
	ctx := testContext(t)
	sessionID := fmt.Sprintf("daemonclient-duplicate-%d", time.Now().UnixNano())
	if err := e.StartSession(ctx, sessionID, "bash --noprofile --norc", testDir(t)); err != nil {
		t.Fatalf("StartSession first: %v", err)
	}
	t.Cleanup(func() { _ = e.StopSession(context.Background(), sessionID) })

	if err := e.StartSession(ctx, sessionID, "printf duplicate", testDir(t)); err == nil {
		t.Fatal("StartSession duplicate error = nil")
	}
	if err := e.WriteSession(ctx, sessionID, "echo existing-alive{enter}"); err != nil {
		t.Fatalf("WriteSession existing: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		output, err := e.ReadSession(ctx, sessionID)
		if err != nil {
			t.Fatalf("ReadSession existing: %v", err)
		}
		if strings.Contains(output, "existing-alive") {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("existing session did not survive duplicate start")
}

func TestExecutorWaitSessionDoesNotTrustOutputMarker(t *testing.T) {
	e := dialDaemon(t)
	ctx := testContext(t)
	sessionID := fmt.Sprintf("daemonclient-marker-%d", time.Now().UnixNano())
	if err := e.StartSession(ctx, sessionID, "printf '[session exited]\\n'; sleep 5", testDir(t)); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	t.Cleanup(func() { _ = e.StopSession(context.Background(), sessionID) })

	output, completed, err := e.WaitSession(ctx, sessionID, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("WaitSession: %v", err)
	}
	if completed {
		t.Fatalf("WaitSession completed = true for running command; output = %q", output)
	}
}
