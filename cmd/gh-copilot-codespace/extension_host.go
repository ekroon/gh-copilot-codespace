package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/ekroon/gh-copilot-codespace/internal/mcp"
	"github.com/ekroon/gh-copilot-codespace/internal/registry"
)

// extensionHostRuntime is a narrow test seam for the extension-host protocol server.
type extensionHostRuntime interface {
	Definitions() []mcp.ToolDefinition
	Call(ctx context.Context, name string, args map[string]any) (mcp.RuntimeCallResult, error)
}

type extensionHostRequest struct {
	ID     any            `json:"id,omitempty"`
	Method string         `json:"method"`
	Tool   string         `json:"tool,omitempty"`
	Args   map[string]any `json:"args,omitempty"`
}

type extensionHostResponse struct {
	ID     any `json:"id,omitempty"`
	Result any `json:"result,omitempty"`
	Error  any `json:"error,omitempty"`
}

// bootstrapPayload is the wire shape returned by the list_tools method. The JS
// extension destructures this and forwards systemMessage and customAgents to
// joinSession in addition to tools. The shape is intentionally tolerant of
// older callers that expect a bare array of tool definitions.
type bootstrapPayload struct {
	Tools         []mcp.ToolDefinition `json:"tools"`
	SystemMessage *systemMessageWire   `json:"systemMessage,omitempty"`
	CustomAgents  []customAgentWire    `json:"customAgents,omitempty"`
}

// systemMessageWire matches @github/copilot-sdk's SystemMessageAppendConfig.
type systemMessageWire struct {
	Mode    string `json:"mode"`
	Content string `json:"content,omitempty"`
}

// customAgentWire matches @github/copilot-sdk's CustomAgentConfig.
type customAgentWire struct {
	Name        string   `json:"name"`
	DisplayName string   `json:"displayName,omitempty"`
	Description string   `json:"description,omitempty"`
	Prompt      string   `json:"prompt"`
	Model       string   `json:"model,omitempty"`
	Tools       []string `json:"tools,omitempty"`
}

func runExtensionHost() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	return runExtensionHostIO(ctx, os.Stdin, os.Stdout)
}

func runExtensionHostIO(ctx context.Context, in io.Reader, out io.Writer) error {
	reg, lifecycleCfg, err := toolRuntimeInputsFromEnv()
	if err != nil {
		return err
	}
	wrapExecutorsWithDaemon(ctx, reg)
	lifecycleCfg.ExecutorSetup = func(setupCtx context.Context, cs *registry.ManagedCodespace) error {
		_, err := wrapExecutorWithDaemon(setupCtx, cs)
		if err != nil {
			return err
		}
		return nil
	}
	runtime := mcp.NewToolRuntime(reg, lifecycleCfg)
	mode := preambleModeFromEnv(os.Getenv(codespaceExtensionModeEnv))
	preamble := BuildPreamble(PreambleContext{
		Mode:         mode,
		Codespaces:   preambleCodespacesFromRegistry(reg),
		AccessPolicy: lifecycleCfg.AccessPolicy,
	})

	var bootstrap *bootstrapPayload
	if preamble != "" || reg.Len() > 0 {
		bootstrap = &bootstrapPayload{}
		if preamble != "" {
			bootstrap.SystemMessage = &systemMessageWire{
				Mode:    "append",
				Content: preamble,
			}
		}
		if reg.Len() > 0 {
			if agent := remoteExplorerInlineAgent(true); agent != nil {
				bootstrap.CustomAgents = []customAgentWire{*agent}
			}
		}
	}

	// Cleanup uses ManagedCodespace.Cleanup as the authoritative idempotent
	// cleanup; no separate shared closers slice.
	defer func() {
		for _, cs := range reg.All() {
			if cs.Cleanup != nil {
				cs.Cleanup()
			}
		}
	}()

	return runExtensionHostServer(ctx, in, out, runtime, bootstrap)
}

// runExtensionHostServer is the context-aware concurrent protocol coordinator.
// It uses the narrow extensionHostRuntime interface for testability.
func runExtensionHostServer(ctx context.Context, in io.Reader, out io.Writer, rt extensionHostRuntime, bootstrap *bootstrapPayload) error {
	hostCtx, hostCancel := context.WithCancel(ctx)
	defer hostCancel()

	// Serialized response writer: one goroutine owns the encoder.
	// writerStopped is a broadcast channel (closed once); writerErr stores
	// the first error for later retrieval. outputClosed is set when we
	// intentionally close output during shutdown (not a real error).
	responses := make(chan extensionHostResponse, 64)
	writerStopped := make(chan struct{})
	var writerErr error
	var writerOnce sync.Once

	go func() {
		encoder := json.NewEncoder(out)
		for resp := range responses {
			if err := encoder.Encode(resp); err != nil {
				writerOnce.Do(func() {
					writerErr = err
					close(writerStopped)
					hostCancel()
				})
				// Drain remaining responses so workers don't block.
				for range responses {
				}
				return
			}
		}
		// Normal close after all responses written.
		writerOnce.Do(func() { close(writerStopped) })
	}()

	var wg sync.WaitGroup

	// sendResponse delivers a response to the writer. Uses a non-blocking fast
	// path when the channel has capacity, falling back to a cancellation-aware
	// send only when the channel is full (backpressure from a blocked writer).
	sendResponse := func(resp extensionHostResponse) {
		select {
		case responses <- resp:
			return
		default:
		}
		// Channel full — blocked writer or burst. Wait with cancellation.
		select {
		case responses <- resp:
		case <-writerStopped:
		case <-hostCtx.Done():
		}
	}

	// Decoder goroutine. Sends decoded requests or errors. Every send selects
	// on hostCtx.Done so the goroutine never blocks on the channel after
	// cancellation. decoderDone is closed when the goroutine exits.
	type decodedRequest struct {
		req extensionHostRequest
		err error
	}
	requests := make(chan decodedRequest, 1)
	decoderDone := make(chan struct{})
	go func() {
		defer close(decoderDone)
		decoder := json.NewDecoder(in)
		for {
			var req extensionHostRequest
			if err := decoder.Decode(&req); err != nil {
				select {
				case requests <- decodedRequest{err: err}:
				case <-hostCtx.Done():
				}
				return
			}
			select {
			case requests <- decodedRequest{req: req}:
			case <-hostCtx.Done():
				return
			}
		}
	}()

	var decodeErr error
loop:
	for {
		select {
		case dr := <-requests:
			if dr.err != nil {
				if errors.Is(dr.err, io.EOF) || errors.Is(dr.err, io.ErrClosedPipe) {
					decodeErr = nil
				} else {
					decodeErr = fmt.Errorf("decode request: %w", dr.err)
				}
				break loop
			}
			req := dr.req

			switch req.Method {
			case "list_tools":
				payload := bootstrapPayload{
					Tools: rt.Definitions(),
				}
				if bootstrap != nil {
					payload.SystemMessage = bootstrap.SystemMessage
					payload.CustomAgents = bootstrap.CustomAgents
				}
				sendResponse(extensionHostResponse{ID: req.ID, Result: payload})

			case "call_tool":
				if req.Tool == "" {
					sendResponse(extensionHostResponse{ID: req.ID, Error: "missing tool"})
					continue
				}
				args := req.Args
				if args == nil {
					args = map[string]any{}
				}
				reqID := req.ID
				toolName := req.Tool

				wg.Add(1)
				go func() {
					defer wg.Done()
					callCtx, callCancel := context.WithCancel(hostCtx)
					defer callCancel()

					result, err := rt.Call(callCtx, toolName, args)
					var resp extensionHostResponse
					resp.ID = reqID
					if err != nil {
						resp.Error = err.Error()
					} else {
						resp.Result = result
					}
					sendResponse(resp)
				}()

			default:
				sendResponse(extensionHostResponse{ID: req.ID, Error: fmt.Sprintf("unknown method %q", req.Method)})
			}

		case <-hostCtx.Done():
			// Parent or writer cancelled. Close input to unblock decoder.
			if c, ok := in.(io.Closer); ok {
				c.Close()
			}
			decodeErr = nil
			break loop
		}
	}

	// Cancel in-flight worker contexts (unblocks context-aware tools).
	hostCancel()

	// Workers blocked in sendResponse unblock via hostCtx.Done.
	wg.Wait()
	close(responses)

	// Wait briefly for writer to drain the closed channel. If the writer is
	// stuck in Encode (blocked output), don't block the server return — the
	// caller's pipe teardown or process exit will clean it up.
	select {
	case <-writerStopped:
	case <-time.After(100 * time.Millisecond):
	}

	// Close output to signal EOF to pipe readers and unblock a stuck writer.
	if c, ok := out.(io.Closer); ok {
		c.Close()
	}

	// After closing output, wait for writer to finish (it unblocks from the
	// close and runs writerOnce). This prevents a data race on server locals.
	<-writerStopped

	// Wait for decoder goroutine to exit (unblocked by input close or ctx).
	<-decoderDone

	if decodeErr != nil {
		return decodeErr
	}
	if writerErr != nil {
		return fmt.Errorf("encode response: %w", writerErr)
	}
	return nil
}

func preambleModeFromEnv(string) PreambleMode {
	return PreambleModeHere
}
