package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/ekroon/gh-copilot-codespace/internal/mcp"
	"github.com/ekroon/gh-copilot-codespace/internal/registry"
)

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
	return runExtensionHostIO(os.Stdin, os.Stdout)
}

func runExtensionHostIO(in io.Reader, out io.Writer) error {
	reg, lifecycleCfg, err := toolRuntimeInputsFromEnv()
	if err != nil {
		return err
	}
	ctx := context.Background()
	closers := wrapExecutorsWithDaemon(ctx, reg)
	lifecycleCfg.ExecutorSetup = func(ctx context.Context, cs *registry.ManagedCodespace) error {
		closeExecutor, err := wrapExecutorWithDaemon(ctx, cs)
		if err != nil {
			return err
		}
		if closeExecutor != nil {
			closers = append(closers, closeExecutor)
		}
		return nil
	}
	defer func() {
		for _, c := range closers {
			c()
		}
	}()
	runtime := mcp.NewToolRuntime(reg, lifecycleCfg)
	mode := preambleModeFromEnv(os.Getenv(codespaceExtensionModeEnv))
	preamble := BuildPreamble(PreambleContext{
		Mode:         mode,
		Codespaces:   preambleCodespacesFromRegistry(reg),
		AccessPolicy: lifecycleCfg.AccessPolicy,
	})
	decoder := json.NewDecoder(in)
	encoder := json.NewEncoder(out)

	for {
		var req extensionHostRequest
		if err := decoder.Decode(&req); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("decode request: %w", err)
		}

		var resp extensionHostResponse
		resp.ID = req.ID
		switch req.Method {
		case "list_tools":
			payload := bootstrapPayload{
				Tools: runtime.Definitions(),
			}
			if preamble != "" {
				payload.SystemMessage = &systemMessageWire{
					Mode:    "append",
					Content: preamble,
				}
			}
			// Advertise the remote-explorer sub-agent whenever at least one
			// codespace is connected so the parent agent can delegate
			// exploration to it. With zero codespaces the remote_* tools are
			// useless, so suppress the agent.
			if reg.Len() > 0 {
				if agent := remoteExplorerInlineAgent(true); agent != nil {
					payload.CustomAgents = []customAgentWire{*agent}
				}
			}
			resp.Result = payload
		case "call_tool":
			if req.Tool == "" {
				resp.Error = "missing tool"
				break
			}
			args := req.Args
			if args == nil {
				args = map[string]any{}
			}
			result, err := runtime.Call(ctx, req.Tool, args)
			if err != nil {
				resp.Error = err.Error()
			} else {
				resp.Result = result
			}
		default:
			resp.Error = fmt.Sprintf("unknown method %q", req.Method)
		}

		if err := encoder.Encode(resp); err != nil {
			return fmt.Errorf("encode response: %w", err)
		}
	}
}

func preambleModeFromEnv(string) PreambleMode {
	return PreambleModeHere
}
