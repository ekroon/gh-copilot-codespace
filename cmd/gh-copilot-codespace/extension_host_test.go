package main

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ekroon/gh-copilot-codespace/internal/mcp"
)

// selectiveBlockRuntime blocks specific tools until their channel is closed.
type selectiveBlockRuntime struct {
	mu         sync.Mutex
	blockTools map[string]chan struct{}
}

func (r *selectiveBlockRuntime) Definitions() []mcp.ToolDefinition {
	return []mcp.ToolDefinition{{Name: "list_codespaces", Description: "list"}}
}

func (r *selectiveBlockRuntime) Call(ctx context.Context, name string, args map[string]any) (mcp.RuntimeCallResult, error) {
	r.mu.Lock()
	ch := r.blockTools[name]
	r.mu.Unlock()

	if ch != nil {
		select {
		case <-ch:
		case <-ctx.Done():
			return mcp.RuntimeCallResult{TextResultForLlm: "canceled", ResultType: "failure"}, ctx.Err()
		}
	}
	return mcp.RuntimeCallResult{TextResultForLlm: "ok:" + name, ResultType: "success"}, nil
}

func TestExtensionHostConcurrency_FastResponseBeforeSlow(t *testing.T) {
	blockCh := make(chan struct{})
	rt := &selectiveBlockRuntime{blockTools: map[string]chan struct{}{"slow_tool": blockCh}}

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- runExtensionHostServer(ctx, inR, outW, rt, nil)
	}()

	// Write slow request then fast request.
	enc := json.NewEncoder(inW)
	enc.Encode(extensionHostRequest{ID: float64(1), Method: "call_tool", Tool: "slow_tool", Args: map[string]any{}})
	enc.Encode(extensionHostRequest{ID: float64(2), Method: "call_tool", Tool: "fast", Args: map[string]any{}})

	// Read first response — should be id:2 (fast completes first)
	dec := json.NewDecoder(outR)
	var resp extensionHostResponse
	if err := dec.Decode(&resp); err != nil {
		t.Fatalf("decode first response: %v", err)
	}
	if resp.ID != float64(2) {
		t.Fatalf("first response ID = %v, want 2 (fast should arrive first)", resp.ID)
	}

	// Unblock slow
	close(blockCh)

	// Read second response — should be id:1
	if err := dec.Decode(&resp); err != nil {
		t.Fatalf("decode second response: %v", err)
	}
	if resp.ID != float64(1) {
		t.Fatalf("second response ID = %v, want 1", resp.ID)
	}

	// Close input to trigger EOF
	inW.Close()
	if err := <-done; err != nil {
		t.Fatalf("server error: %v", err)
	}
}

func TestExtensionHostConcurrency_ResponseIDsCorrect(t *testing.T) {
	rt := &selectiveBlockRuntime{blockTools: map[string]chan struct{}{}}

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- runExtensionHostServer(ctx, inR, outW, rt, nil)
	}()

	enc := json.NewEncoder(inW)
	enc.Encode(extensionHostRequest{ID: float64(10), Method: "call_tool", Tool: "a", Args: map[string]any{}})
	enc.Encode(extensionHostRequest{ID: float64(20), Method: "call_tool", Tool: "b", Args: map[string]any{}})
	inW.Close()

	dec := json.NewDecoder(outR)
	ids := map[float64]bool{}
	for {
		var resp extensionHostResponse
		if err := dec.Decode(&resp); err != nil {
			break
		}
		id, _ := resp.ID.(float64)
		ids[id] = true
	}

	<-done
	if !ids[10] || !ids[20] {
		t.Fatalf("expected responses for IDs 10 and 20, got %v", ids)
	}
}

func TestExtensionHostConcurrency_ListToolsSynchronous(t *testing.T) {
	rt := &selectiveBlockRuntime{blockTools: map[string]chan struct{}{}}

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- runExtensionHostServer(ctx, inR, outW, rt, nil)
	}()

	enc := json.NewEncoder(inW)
	enc.Encode(extensionHostRequest{ID: float64(1), Method: "list_tools"})
	inW.Close()

	dec := json.NewDecoder(outR)
	var resp extensionHostResponse
	if err := dec.Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("list_tools error: %v", resp.Error)
	}
	<-done
}

func TestExtensionHostConcurrency_ParentCancelCancelsInFlight(t *testing.T) {
	blockCh := make(chan struct{})
	rt := &selectiveBlockRuntime{blockTools: map[string]chan struct{}{"slow": blockCh}}

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	// Drain output to prevent pipe write blocking.
	go io.Copy(io.Discard, outR)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- runExtensionHostServer(ctx, inR, outW, rt, nil)
	}()

	enc := json.NewEncoder(inW)
	enc.Encode(extensionHostRequest{ID: float64(1), Method: "call_tool", Tool: "slow", Args: map[string]any{}})

	// Give time for request dispatch
	time.Sleep(50 * time.Millisecond)

	// Cancel parent — should unblock the slow tool via context
	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("server did not shut down after parent cancel")
	}
}

func TestExtensionHostConcurrency_EOFCancelsAndWaitsForWorkers(t *testing.T) {
	var callStarted sync.WaitGroup
	callStarted.Add(1)

	rt := &notifyingBlockRuntime{
		blockTools:  map[string]chan struct{}{"slow": make(chan struct{})},
		onCallStart: func(name string) { callStarted.Done() },
	}

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	// Drain output to prevent pipe write blocking.
	go io.Copy(io.Discard, outR)

	ctx := context.Background()
	done := make(chan error, 1)
	go func() {
		done <- runExtensionHostServer(ctx, inR, outW, rt, nil)
	}()

	enc := json.NewEncoder(inW)
	enc.Encode(extensionHostRequest{ID: float64(1), Method: "call_tool", Tool: "slow", Args: map[string]any{}})

	// Wait for the call to start
	callStarted.Wait()

	// Close input — triggers EOF. Server should cancel context and wait.
	inW.Close()

	select {
	case <-done:
		// Good — server exited after cancelling and waiting for workers.
	case <-time.After(3 * time.Second):
		t.Fatal("server did not shut down after EOF")
	}
}

func TestExtensionHostConcurrency_ValidNDJSON(t *testing.T) {
	rt := &selectiveBlockRuntime{blockTools: map[string]chan struct{}{}}

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- runExtensionHostServer(ctx, inR, outW, rt, nil)
	}()

	enc := json.NewEncoder(inW)
	for i := 0; i < 20; i++ {
		enc.Encode(extensionHostRequest{ID: float64(i), Method: "call_tool", Tool: "fast", Args: map[string]any{}})
	}
	inW.Close()

	dec := json.NewDecoder(outR)
	count := 0
	for {
		var resp extensionHostResponse
		if err := dec.Decode(&resp); err != nil {
			break
		}
		count++
		if resp.ID == nil {
			t.Fatalf("response %d has nil ID", count)
		}
	}
	<-done

	if count != 20 {
		t.Fatalf("got %d responses, want 20", count)
	}
}

// failAfterNWriter fails after writing n responses.
type failAfterNWriter struct {
	n       int
	written int
	err     error
}

func (w *failAfterNWriter) Write(p []byte) (int, error) {
	if w.written >= w.n {
		return 0, w.err
	}
	w.written++
	return len(p), nil
}

func TestExtensionHostConcurrency_WriterFailureCancelsWorkersAndReturnsError(t *testing.T) {
	blockCh := make(chan struct{})
	rt := &selectiveBlockRuntime{blockTools: map[string]chan struct{}{"blocked": blockCh}}

	inR, inW := io.Pipe()
	// Writer that fails on the second write.
	failWriter := &failAfterNWriter{n: 1, err: io.ErrShortWrite}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- runExtensionHostServer(ctx, inR, failWriter, rt, nil)
	}()

	enc := json.NewEncoder(inW)
	// First call responds successfully (writer allows first write).
	enc.Encode(extensionHostRequest{ID: float64(1), Method: "call_tool", Tool: "fast", Args: map[string]any{}})
	// Second call will trigger the writer failure.
	time.Sleep(50 * time.Millisecond)
	enc.Encode(extensionHostRequest{ID: float64(2), Method: "call_tool", Tool: "fast", Args: map[string]any{}})
	// Third call — blocked tool — should get cancelled by writer failure.
	time.Sleep(50 * time.Millisecond)
	enc.Encode(extensionHostRequest{ID: float64(3), Method: "call_tool", Tool: "blocked", Args: map[string]any{}})

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected encode error, got nil")
		}
		if !strings.Contains(err.Error(), "encode response") {
			t.Fatalf("expected encode response error, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not shut down after writer failure")
	}
}

func TestExtensionHostConcurrency_ParentCancelWithBlockedInput(t *testing.T) {
	rt := &selectiveBlockRuntime{blockTools: map[string]chan struct{}{}}

	// Use a pipe where we never write — decoder blocks forever on read.
	inR, _ := io.Pipe()
	outR, outW := io.Pipe()
	go io.Copy(io.Discard, outR)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- runExtensionHostServer(ctx, inR, outW, rt, nil)
	}()

	// Give the decoder time to block.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected nil error on parent cancel, got: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server did not shut down after parent cancel with blocked input")
	}
}

func TestExtensionHostConcurrency_DecoderExitsOnCancel(t *testing.T) {
	// Prove the decoder goroutine exits when: a request is queued in the
	// buffered channel, then context is cancelled while the decoder is blocked
	// reading the next request. The decoder must not block sending its error.
	rt := &selectiveBlockRuntime{blockTools: map[string]chan struct{}{}}

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	go io.Copy(io.Discard, outR)

	ctx, cancel := context.WithCancel(context.Background())

	serverDone := make(chan error, 1)
	go func() {
		serverDone <- runExtensionHostServer(ctx, inR, outW, rt, nil)
	}()

	// Write one request — the server must be running to read from the pipe.
	enc := json.NewEncoder(inW)
	enc.Encode(extensionHostRequest{ID: float64(1), Method: "call_tool", Tool: "fast", Args: map[string]any{}})

	// Give time for the request to be consumed and dispatched,
	// and for the decoder to block on the next read.
	time.Sleep(100 * time.Millisecond)

	// Cancel context — decoder must unblock (pipe close) and exit.
	cancel()

	select {
	case <-serverDone:
		// Server returned — decoder exited cleanly.
	case <-time.After(3 * time.Second):
		t.Fatal("server did not return; decoder goroutine likely leaked")
	}
}

func TestExtensionHostConcurrency_BackpressureWithManyWorkersAndCancel(t *testing.T) {
	// >64 fast workers, writer fails after N writes. Writer error triggers
	// hostCancel which should allow all workers to deliver (or drop) responses
	// and server returns promptly without deadlock.
	const numWorkers = 100
	rt := &selectiveBlockRuntime{blockTools: map[string]chan struct{}{}}

	inR, inW := io.Pipe()
	// Writer that fails after 5 writes — simulating broken pipe mid-stream.
	failWriter := &failAfterNWriter{n: 5, err: io.ErrShortWrite}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serverDone := make(chan error, 1)
	go func() {
		serverDone <- runExtensionHostServer(ctx, inR, failWriter, rt, nil)
	}()

	// Send numWorkers fast requests.
	enc := json.NewEncoder(inW)
	for i := 0; i < numWorkers; i++ {
		enc.Encode(extensionHostRequest{ID: float64(i), Method: "call_tool", Tool: "fast", Args: map[string]any{}})
	}

	// Writer fails on 6th response → hostCancel → all workers' contexts
	// cancelled → wg.Wait returns → server exits.
	select {
	case err := <-serverDone:
		if err == nil || !strings.Contains(err.Error(), "encode response") {
			t.Fatalf("expected encode error, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server deadlocked with >64 workers and broken writer")
	}
}

// blockingWriter blocks on every Write until its done channel is closed.
// Implements io.Closer so the server can unblock it during shutdown.
type blockingWriter struct {
	done chan struct{}
}

func (w *blockingWriter) Write(p []byte) (int, error) {
	<-w.done
	return 0, io.ErrClosedPipe
}

func (w *blockingWriter) Close() error {
	select {
	case <-w.done:
	default:
		close(w.done)
	}
	return nil
}

func TestExtensionHostConcurrency_BlockedWriterParentCancelUnblocksWorkers(t *testing.T) {
	// Writer blocks forever (never errors). >64 workers complete their tool
	// calls but can't deliver responses (channel fills, writer stuck).
	// Parent cancel must unblock all workers via hostCtx.Done in sendResponse.
	const numWorkers = 100
	rt := &selectiveBlockRuntime{blockTools: map[string]chan struct{}{}}

	inR, inW := io.Pipe()
	bw := &blockingWriter{done: make(chan struct{})}

	ctx, cancel := context.WithCancel(context.Background())

	serverDone := make(chan error, 1)
	go func() {
		serverDone <- runExtensionHostServer(ctx, inR, bw, rt, nil)
	}()

	// Send numWorkers fast requests. Workers complete instantly but
	// sendResponse blocks: channel fills at 64, writer stuck on first item.
	enc := json.NewEncoder(inW)
	for i := 0; i < numWorkers; i++ {
		enc.Encode(extensionHostRequest{ID: float64(i), Method: "call_tool", Tool: "fast", Args: map[string]any{}})
	}

	// Give workers time to complete and fill the channel.
	time.Sleep(200 * time.Millisecond)

	// Cancel — workers blocked in sendResponse must unblock via hostCtx.Done.
	cancel()

	select {
	case <-serverDone:
		// Good — workers unblocked, server returned.
	case <-time.After(5 * time.Second):
		t.Fatal("server deadlocked: workers stuck in sendResponse with blocked writer")
	}
	// Server's shutdown already called bw.Close() to unblock the writer.
}

// --- helpers ---

type notifyingBlockRuntime struct {
	mu          sync.Mutex
	blockTools  map[string]chan struct{}
	onCallStart func(string)
}

func (r *notifyingBlockRuntime) Definitions() []mcp.ToolDefinition {
	return []mcp.ToolDefinition{{Name: "list_codespaces", Description: "list"}}
}

func (r *notifyingBlockRuntime) Call(ctx context.Context, name string, args map[string]any) (mcp.RuntimeCallResult, error) {
	r.mu.Lock()
	ch := r.blockTools[name]
	notify := r.onCallStart
	r.mu.Unlock()

	if notify != nil {
		notify(name)
	}

	if ch != nil {
		select {
		case <-ch:
		case <-ctx.Done():
			return mcp.RuntimeCallResult{TextResultForLlm: "canceled", ResultType: "failure"}, ctx.Err()
		}
	}
	return mcp.RuntimeCallResult{TextResultForLlm: "ok:" + name, ResultType: "success"}, nil
}
