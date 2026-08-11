package daemontransport

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStderrTailForwardsAndBounds(t *testing.T) {
	sink := &bytes.Buffer{}
	tail := newStderrTail(sink, 8)

	if _, err := tail.Write([]byte("abcd")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := tail.String(); got != "abcd" {
		t.Fatalf("tail = %q, want abcd", got)
	}

	if _, err := tail.Write([]byte("efghij")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := tail.String(); got != "...cdefghij" {
		t.Fatalf("tail = %q, want ...cdefghij", got)
	}

	if sink.String() != "abcdefghij" {
		t.Fatalf("sink = %q, want abcdefghij", sink.String())
	}
}

func TestStderrTailKeepsTailOfOversizedWrite(t *testing.T) {
	tail := newStderrTail(io.Discard, 4)
	if _, err := tail.Write([]byte("0123456789")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := tail.String(); got != "...6789" {
		t.Fatalf("tail = %q, want ...6789", got)
	}
}

func TestStderrTailLimitMatchesEightKiB(t *testing.T) {
	if StderrTailLimit != 8*1024 {
		t.Fatalf("StderrTailLimit = %d, want 8192", StderrTailLimit)
	}
}

func TestStderrTailConcurrentWritesAndReads(t *testing.T) {
	tail := newStderrTail(io.Discard, 64)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_, _ = tail.Write([]byte("noisy stderr line\n"))
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = tail.String()
			}
		}()
	}
	wg.Wait()
	if got := tail.String(); len(strings.TrimPrefix(got, "...")) > 64 {
		t.Fatalf("tail = %q, want at most 64 retained bytes", got)
	}
}

func TestTerminalErrorReportsExitStatusAndStderr(t *testing.T) {
	binaryPath := writeDaemonScript(t, "#!/bin/sh\necho 'daemon: boom' >&2\nexit 3\n")
	stream := spawnLocal(t, binaryPath)

	if _, err := io.Copy(io.Discard, stream); err != nil {
		t.Fatalf("drain stdout: %v", err)
	}

	reporter, ok := stream.(TerminalErrorReporter)
	if !ok {
		t.Fatalf("stream type %T does not implement TerminalErrorReporter", stream)
	}

	err := reporter.TerminalError(2 * time.Second)
	if err == nil {
		t.Fatal("TerminalError = nil, want terminal process error")
	}

	var terminal *TerminalProcessError
	if !errors.As(err, &terminal) {
		t.Fatalf("TerminalError = %v (%T), want *TerminalProcessError", err, err)
	}
	if terminal.Transport != "local" {
		t.Fatalf("Transport = %q, want local", terminal.Transport)
	}
	if !strings.Contains(terminal.Stderr, "daemon: boom") {
		t.Fatalf("Stderr = %q, want it to contain daemon: boom", terminal.Stderr)
	}

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("TerminalError does not unwrap to *exec.ExitError: %v", err)
	}
	if exitErr.ExitCode() != 3 {
		t.Fatalf("exit code = %d, want 3", exitErr.ExitCode())
	}
	if !strings.Contains(err.Error(), "daemon: boom") || !strings.Contains(err.Error(), "local") {
		t.Fatalf("error message = %q, want transport name and stderr tail", err.Error())
	}
}

func TestSpawnedStreamCapturesBoundedStderrAndStillForwards(t *testing.T) {
	stream := spawnLocal(t, writeDaemonScript(t, "#!/bin/sh\nexit 1\n"))
	t.Cleanup(func() { _ = stream.Close() })

	process, ok := stream.(*processStream)
	if !ok {
		t.Fatalf("stream type = %T, want *processStream", stream)
	}
	if process.stderr == nil {
		t.Fatal("stderr tail was not installed on the spawned stream")
	}
	if process.stderr.limit != StderrTailLimit {
		t.Fatalf("stderr limit = %d, want %d", process.stderr.limit, StderrTailLimit)
	}
	if process.stderr.sink != os.Stderr {
		t.Fatalf("stderr sink = %v, want os.Stderr", process.stderr.sink)
	}
	if process.cmd.Stderr != process.stderr {
		t.Fatal("cmd.Stderr is not the capturing tail writer")
	}
}

func TestTerminalErrorBoundsCapturedStderr(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	stderr := newStderrTail(io.Discard, StderrTailLimit)
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	process := newProcessStream("local", cmd, stdin, stdout, stderr)

	for i := 0; i < 64; i++ {
		if _, err := stderr.Write(bytes.Repeat([]byte("x"), 1024)); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	err = process.TerminalError(2 * time.Second)
	if err == nil {
		t.Fatal("TerminalError = nil, want terminal process error")
	}
	var terminal *TerminalProcessError
	if !errors.As(err, &terminal) {
		t.Fatalf("TerminalError = %v (%T), want *TerminalProcessError", err, err)
	}
	if got := len(strings.TrimPrefix(terminal.Stderr, "...")); got > StderrTailLimit {
		t.Fatalf("captured stderr = %d bytes, want at most %d", got, StderrTailLimit)
	}
	if !strings.HasPrefix(terminal.Stderr, "...") {
		t.Fatal("captured stderr is missing the truncation marker")
	}
}

func TestTerminalErrorNilAfterIntentionalClose(t *testing.T) {
	stream := spawnLocal(t, writeDaemonScript(t, "#!/bin/sh\nwhile read line; do echo \"echo:$line\"; done\n"))

	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reporter, ok := stream.(TerminalErrorReporter)
	if !ok {
		t.Fatalf("stream type %T does not implement TerminalErrorReporter", stream)
	}
	if err := reporter.TerminalError(time.Second); err != nil {
		t.Fatalf("TerminalError after Close = %v, want nil", err)
	}
}

func TestTerminalErrorNilWhileProcessStillRunning(t *testing.T) {
	stream := spawnLocal(t, writeDaemonScript(t, "#!/bin/sh\nwhile read line; do echo \"echo:$line\"; done\n"))
	t.Cleanup(func() { _ = stream.Close() })

	reporter, ok := stream.(TerminalErrorReporter)
	if !ok {
		t.Fatalf("stream type %T does not implement TerminalErrorReporter", stream)
	}
	if err := reporter.TerminalError(50 * time.Millisecond); err != nil {
		t.Fatalf("TerminalError while running = %v, want nil", err)
	}
}

func TestTerminalErrorIsRaceFreeAlongsideClose(t *testing.T) {
	stream := spawnLocal(t, writeDaemonScript(t, "#!/bin/sh\nwhile read line; do echo \"echo:$line\"; done\n"))
	process, ok := stream.(*processStream)
	if !ok {
		t.Fatalf("stream type = %T, want *processStream", stream)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = process.TerminalError(200 * time.Millisecond)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = process.Close()
	}()
	wg.Wait()

	if err := process.TerminalError(time.Second); err != nil {
		t.Fatalf("TerminalError after Close = %v, want nil", err)
	}
}

func spawnLocal(t *testing.T, binaryPath string) io.ReadWriteCloser {
	t.Helper()
	transport := NewLocalTransport(binaryPath)
	stream, err := transport.Spawn(context.Background(), binaryPath)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	return stream
}

func writeDaemonScript(t *testing.T, content string) string {
	t.Helper()
	name := strings.NewReplacer("/", "_", " ", "_", ":", "_").Replace(t.Name())
	dir := filepath.Join(".test-artifacts", name)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(dir)
		_ = os.Remove(".test-artifacts")
	})

	path := filepath.Join(dir, "daemon.sh")
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("Abs: %v", err)
	}
	return abs
}
