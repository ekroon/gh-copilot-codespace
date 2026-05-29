package daemonclient

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ekroon/gh-copilot-codespace/internal/daemonproto"
	"github.com/ekroon/gh-copilot-codespace/internal/daemontransport"
)

var daemonBinary string

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
