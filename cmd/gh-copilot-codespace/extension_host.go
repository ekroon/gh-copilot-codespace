package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/ekroon/gh-copilot-codespace/internal/mcp"
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

func runExtensionHost() error {
	return runExtensionHostIO(os.Stdin, os.Stdout)
}

func runExtensionHostIO(in io.Reader, out io.Writer) error {
	reg, lifecycleCfg, err := toolRuntimeInputsFromEnv()
	if err != nil {
		return err
	}
	runtime := mcp.NewToolRuntime(reg, lifecycleCfg)
	decoder := json.NewDecoder(in)
	encoder := json.NewEncoder(out)
	ctx := context.Background()

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
			resp.Result = runtime.Definitions()
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
