package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ekroon/gh-copilot-codespace/internal/daemonproto"
)

type daemonTestHarness struct {
	enc   *daemonproto.Encoder
	dec   *daemonproto.Decoder
	inW   *io.PipeWriter
	done  chan error
	close func()
}

func startDaemonTestHarness(t *testing.T) *daemonTestHarness {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() {
		defer outW.Close()
		done <- runDaemonIO(context.Background(), inR, outW)
	}()

	h := &daemonTestHarness{
		enc:  daemonproto.NewEncoder(inW),
		dec:  daemonproto.NewDecoder(outR),
		inW:  inW,
		done: done,
	}
	h.close = func() {
		_ = inW.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("runDaemonIO() error = %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Errorf("runDaemonIO() did not exit after input close")
		}
	}
	return h
}

func readFrameWithTimeout(t *testing.T, dec *daemonproto.Decoder) daemonproto.Frame {
	t.Helper()
	type result struct {
		frame daemonproto.Frame
		err   error
	}
	ch := make(chan result, 1)
	go func() {
		frame, err := dec.Read()
		ch <- result{frame: frame, err: err}
	}()
	select {
	case got := <-ch:
		if got.err != nil {
			t.Fatalf("Read() error = %v", got.err)
		}
		return got.frame
	case <-time.After(5 * time.Second):
		t.Fatal("timed out reading daemon frame")
		return daemonproto.Frame{}
	}
}

func mustRequest(t *testing.T, id uint64, verb daemonproto.Verb, params any) daemonproto.Frame {
	t.Helper()
	frame, err := daemonproto.NewRequest(id, verb, params, "")
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	return frame
}

func writeFrame(t *testing.T, enc *daemonproto.Encoder, frame daemonproto.Frame) {
	t.Helper()
	if err := enc.Write(frame); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
}

func requireHello(t *testing.T, frame daemonproto.Frame) {
	t.Helper()
	if frame.Type != daemonproto.TypeHello {
		t.Fatalf("frame.Type = %q, want %q", frame.Type, daemonproto.TypeHello)
	}
	if frame.Version != daemonproto.ProtocolVersion {
		t.Fatalf("frame.Version = %q, want %q", frame.Version, daemonproto.ProtocolVersion)
	}
	verbs := make(map[string]bool, len(frame.Verbs))
	for _, verb := range frame.Verbs {
		verbs[verb] = true
	}
	for _, verb := range daemonproto.AllVerbs() {
		if !verbs[string(verb)] {
			t.Fatalf("hello verbs missing %q in %v", verb, frame.Verbs)
		}
	}
}

func decodeDaemonResult(t *testing.T, frame daemonproto.Frame, target any) {
	t.Helper()
	if frame.Type != daemonproto.TypeResponse {
		t.Fatalf("frame.Type = %q, want %q", frame.Type, daemonproto.TypeResponse)
	}
	if frame.Error != nil {
		t.Fatalf("response error = %+v", *frame.Error)
	}
	if err := json.Unmarshal(frame.Result, target); err != nil {
		t.Fatalf("Unmarshal result error = %v", err)
	}
}

func TestRunDaemonHelloOnConnect(t *testing.T) {
	h := startDaemonTestHarness(t)
	defer h.close()

	requireHello(t, readFrameWithTimeout(t, h.dec))
}

func TestRunDaemonPingRoundTrip(t *testing.T) {
	h := startDaemonTestHarness(t)
	defer h.close()
	requireHello(t, readFrameWithTimeout(t, h.dec))

	writeFrame(t, h.enc, mustRequest(t, 1, daemonproto.VerbPing, daemonproto.PingParams{}))
	resp := readFrameWithTimeout(t, h.dec)
	if resp.ID != 1 {
		t.Fatalf("resp.ID = %d, want 1", resp.ID)
	}
	var result daemonproto.PingResult
	decodeDaemonResult(t, resp, &result)
	if !result.Pong {
		t.Fatal("PingResult.Pong = false, want true")
	}
	if result.PID == 0 {
		t.Fatal("PingResult.PID = 0, want non-zero")
	}
	if result.StartedAt == "" {
		t.Fatal("PingResult.StartedAt is empty")
	}
}

func TestRunDaemonViewFile(t *testing.T) {
	h := startDaemonTestHarness(t)
	defer h.close()
	requireHello(t, readFrameWithTimeout(t, h.dec))

	path := filepath.Join(t.TempDir(), "sample.txt")
	if err := os.WriteFile(path, []byte("a\nb\nc\nd\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	writeFrame(t, h.enc, mustRequest(t, 1, daemonproto.VerbViewFile, daemonproto.ViewFileParams{Path: path, ViewRange: []int{2, 3}}))
	resp := readFrameWithTimeout(t, h.dec)
	var result daemonproto.ViewFileResult
	decodeDaemonResult(t, resp, &result)
	if result.Content != "2. b\n3. c\n" {
		t.Fatalf("Content = %q, want %q", result.Content, "2. b\n3. c\n")
	}
}

func TestRunDaemonCreateThenEdit(t *testing.T) {
	h := startDaemonTestHarness(t)
	defer h.close()
	requireHello(t, readFrameWithTimeout(t, h.dec))

	path := filepath.Join(t.TempDir(), "nested", "file.txt")
	writeFrame(t, h.enc, mustRequest(t, 1, daemonproto.VerbCreateFile, daemonproto.CreateFileParams{Path: path, Content: "hello world\n"}))
	var createResult daemonproto.CreateFileResult
	decodeDaemonResult(t, readFrameWithTimeout(t, h.dec), &createResult)
	if createResult.Bytes != len("hello world\n") {
		t.Fatalf("CreateFileResult.Bytes = %d, want %d", createResult.Bytes, len("hello world\n"))
	}

	writeFrame(t, h.enc, mustRequest(t, 2, daemonproto.VerbEditFile, daemonproto.EditFileParams{Path: path, OldStr: "world", NewStr: "daemon"}))
	var editResult daemonproto.EditFileResult
	decodeDaemonResult(t, readFrameWithTimeout(t, h.dec), &editResult)
	if editResult.Replaced != 1 {
		t.Fatalf("EditFileResult.Replaced = %d, want 1", editResult.Replaced)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(content) != "hello daemon\n" {
		t.Fatalf("file content = %q, want %q", string(content), "hello daemon\n")
	}
}

func TestRunDaemonRunBashCapturesOutput(t *testing.T) {
	h := startDaemonTestHarness(t)
	defer h.close()
	requireHello(t, readFrameWithTimeout(t, h.dec))

	writeFrame(t, h.enc, mustRequest(t, 1, daemonproto.VerbRunBash, daemonproto.RunBashParams{Command: "echo hello"}))
	resp := readFrameWithTimeout(t, h.dec)
	var result daemonproto.RunBashResult
	decodeDaemonResult(t, resp, &result)
	if result.Stdout != "hello\n" || result.Stderr != "" || result.ExitCode != 0 {
		t.Fatalf("RunBashResult = %+v, want stdout hello newline, empty stderr, exit 0", result)
	}
}

func TestRunDaemonRunBashCancelKillsProcess(t *testing.T) {
	h := startDaemonTestHarness(t)
	defer h.close()
	requireHello(t, readFrameWithTimeout(t, h.dec))

	start := time.Now()
	writeFrame(t, h.enc, mustRequest(t, 1, daemonproto.VerbRunBash, daemonproto.RunBashParams{Command: "sleep 30"}))
	time.Sleep(100 * time.Millisecond)
	writeFrame(t, h.enc, daemonproto.NewCancel(1))

	resp := readFrameWithTimeout(t, h.dec)
	if resp.ID != 1 {
		t.Fatalf("resp.ID = %d, want 1", resp.ID)
	}
	if resp.Error == nil {
		t.Fatal("resp.Error = nil, want cancellation error")
	}
	if resp.Error.Code != daemonproto.ErrCodeCanceled {
		t.Fatalf("resp.Error.Code = %q, want %q", resp.Error.Code, daemonproto.ErrCodeCanceled)
	}
	if elapsed := time.Since(start); elapsed >= 5*time.Second {
		t.Fatalf("cancel took %v, want < 5s", elapsed)
	}
}

func TestRunDaemonUnknownVerbReturnsError(t *testing.T) {
	h := startDaemonTestHarness(t)
	defer h.close()
	requireHello(t, readFrameWithTimeout(t, h.dec))

	writeFrame(t, h.enc, mustRequest(t, 1, daemonproto.Verb("nonsense"), nil))
	resp := readFrameWithTimeout(t, h.dec)
	if resp.Error == nil {
		t.Fatal("resp.Error = nil, want error")
	}
	if resp.Error.Code != daemonproto.ErrCodeUnknownVerb {
		t.Fatalf("resp.Error.Code = %q, want %q", resp.Error.Code, daemonproto.ErrCodeUnknownVerb)
	}
}

func TestRunDaemonConcurrentRequests(t *testing.T) {
	h := startDaemonTestHarness(t)
	defer h.close()
	requireHello(t, readFrameWithTimeout(t, h.dec))

	var writeMu sync.Mutex
	var wg sync.WaitGroup
	for i := 1; i <= 10; i++ {
		id := uint64(i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			frame := mustRequest(t, id, daemonproto.VerbPing, daemonproto.PingParams{})
			writeMu.Lock()
			defer writeMu.Unlock()
			if err := h.enc.Write(frame); err != nil {
				t.Errorf("Write() error = %v", err)
			}
		}()
	}
	wg.Wait()

	seen := make(map[uint64]bool)
	for i := 0; i < 10; i++ {
		resp := readFrameWithTimeout(t, h.dec)
		if resp.Error != nil {
			t.Fatalf("response %d error = %+v", resp.ID, *resp.Error)
		}
		var result daemonproto.PingResult
		decodeDaemonResult(t, resp, &result)
		if !result.Pong {
			t.Fatalf("response %d Pong = false, want true", resp.ID)
		}
		seen[resp.ID] = true
	}
	for i := 1; i <= 10; i++ {
		if !seen[uint64(i)] {
			t.Fatalf("missing response for id %d; seen %v", i, seen)
		}
	}
}
