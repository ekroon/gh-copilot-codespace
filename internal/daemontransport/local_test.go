package daemontransport

import (
	"bufio"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLocalTransportSpawnsAndStreamsEcho(t *testing.T) {
	binaryPath := writeEchoDaemonScript(t)
	transport := NewLocalTransport(binaryPath)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := transport.Spawn(ctx, binaryPath)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	if _, err := io.WriteString(stream, "hi\n"); err != nil {
		t.Fatalf("Write: %v", err)
	}

	line := readLine(t, stream)
	if line != "echo:hi\n" {
		t.Fatalf("Read = %q, want echo:hi\\n", line)
	}

	process, ok := stream.(*processStream)
	if !ok {
		t.Fatalf("stream type = %T, want *processStream", stream)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-process.done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not terminate child process")
	}
}

func TestLocalTransportSpawnSurvivesContextCancel(t *testing.T) {
	binaryPath := writeEchoDaemonScript(t)
	transport := NewLocalTransport(binaryPath)

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := transport.Spawn(ctx, binaryPath)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer stream.Close()

	// Cancelling the Spawn context after Start returns must NOT kill the
	// daemon: lifetime is owned by stream.Close(). This regression check
	// guards against binding the long-lived process to a short-lived dial
	// context (see daemontransport/{local,ssh}.go Spawn).
	cancel()

	if _, err := io.WriteString(stream, "still-alive\n"); err != nil {
		t.Fatalf("Write after cancel: %v", err)
	}

	line := readLine(t, stream)
	if line != "echo:still-alive\n" {
		t.Fatalf("Read after cancel = %q, want echo:still-alive\\n", line)
	}
}

func TestLocalTransportCloseTerminatesProcess(t *testing.T) {
	binaryPath := writeEchoDaemonScript(t)
	transport := NewLocalTransport(binaryPath)

	stream, err := transport.Spawn(context.Background(), binaryPath)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	process, ok := stream.(*processStream)
	if !ok {
		t.Fatalf("stream type = %T, want *processStream", stream)
	}

	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-process.done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not terminate child process")
	}
}

func TestLocalTransportDeployIsNoop(t *testing.T) {
	binaryPath := filepath.Join("project", "daemon")
	transport := NewLocalTransport(binaryPath)

	got, err := transport.Deploy(context.Background())
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if got != binaryPath {
		t.Fatalf("Deploy path = %q, want %q", got, binaryPath)
	}
}

func readLine(t *testing.T, r io.Reader) string {
	t.Helper()
	result := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		line, err := bufio.NewReader(r).ReadString('\n')
		if err != nil {
			errCh <- err
			return
		}
		result <- line
	}()

	select {
	case line := <-result:
		return line
	case err := <-errCh:
		t.Fatalf("ReadString: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out reading daemon output")
	}
	return ""
}

func writeEchoDaemonScript(t *testing.T) string {
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

	path := filepath.Join(dir, "echo-daemon.sh")
	content := "#!/bin/sh\nwhile read line; do echo \"echo:$line\"; done\n"
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("Abs: %v", err)
	}
	return abs
}
