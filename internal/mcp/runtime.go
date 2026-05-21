package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ekroon/gh-copilot-codespace/internal/registry"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// ToolDefinition is the transport-neutral shape used by extension-host.
type ToolDefinition struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Parameters  mcpsdk.ToolInputSchema `json:"parameters"`
}

// RuntimeCallResult is the transport-neutral result returned by extension-host.
type RuntimeCallResult struct {
	TextResultForLlm string `json:"textResultForLlm"`
	ResultType       string `json:"resultType"`
}

type runtimeTool struct {
	tool    mcpsdk.Tool
	handler server.ToolHandlerFunc
}

// ToolRuntime owns the first-party Codespaces tools and adapts them to MCP or extensions.
type ToolRuntime struct {
	tools    []runtimeTool
	handlers map[string]server.ToolHandlerFunc
}

// NewToolRuntime creates the shared first-party tool runtime.
func NewToolRuntime(reg *registry.Registry, cfg LifecycleConfig) *ToolRuntime {
	state := newLifecycleState(cfg)
	tools := []runtimeTool{
		{tool: viewTool(), handler: viewHandler(reg)},
		{tool: editTool(), handler: editHandler(reg)},
		{tool: createTool(), handler: createHandler(reg)},
		{tool: bashTool(), handler: bashHandler(reg)},
		{tool: grepTool(), handler: grepHandler(reg)},
		{tool: globTool(), handler: globHandler(reg)},
		{tool: writeBashTool(), handler: writeBashHandler(reg)},
		{tool: readBashTool(), handler: readBashHandler(reg)},
		{tool: stopBashTool(), handler: stopBashHandler(reg)},
		{tool: listBashTool(), handler: listBashHandler(reg)},
		{tool: remoteCopyTool(), handler: remoteCopyHandler(reg, state.cfg.LocalWorkdir)},
		{tool: openShellTool(), handler: openShellHandler(reg)},
		{tool: cdTool(), handler: cdHandler(reg)},
		{tool: cwdTool(), handler: cwdHandler(reg)},
		{tool: listCodespacesTool(), handler: listCodespacesHandler(reg)},
		{tool: listAvailableCodespacesTool(), handler: listAvailableCodespacesHandlerWithState(state)},
		{tool: getCodespaceOptionsTool(), handler: getCodespaceOptionsHandler(state.cfg.GHRunner)},
		{tool: createCodespaceTool(), handler: createCodespaceHandlerWithState(reg, state)},
		{tool: connectCodespaceTool(), handler: connectCodespaceHandlerWithState(reg, state)},
		{tool: deleteCodespaceTool(), handler: deleteCodespaceHandlerWithState(reg, state)},
	}
	handlers := make(map[string]server.ToolHandlerFunc, len(tools))
	for _, t := range tools {
		handlers[t.tool.Name] = t.handler
	}
	return &ToolRuntime{tools: tools, handlers: handlers}
}

// AddToServer registers all runtime tools on an MCP server.
func (r *ToolRuntime) AddToServer(s *server.MCPServer) {
	for _, t := range r.tools {
		s.AddTool(t.tool, t.handler)
	}
}

// Definitions returns tool metadata for extension registration.
func (r *ToolRuntime) Definitions() []ToolDefinition {
	defs := make([]ToolDefinition, 0, len(r.tools))
	for _, t := range r.tools {
		defs = append(defs, ToolDefinition{
			Name:        t.tool.Name,
			Description: t.tool.Description,
			Parameters:  t.tool.InputSchema,
		})
	}
	return defs
}

// Call invokes a named runtime tool with JSON-decoded arguments.
func (r *ToolRuntime) Call(ctx context.Context, name string, args map[string]any) (RuntimeCallResult, error) {
	handler, ok := r.handlers[name]
	if !ok {
		return RuntimeCallResult{}, fmt.Errorf("unknown tool %q", name)
	}
	req := mcpsdk.CallToolRequest{
		Params: mcpsdk.CallToolParams{
			Name:      name,
			Arguments: args,
		},
	}
	result, err := handler(ctx, req)
	if err != nil {
		return RuntimeCallResult{}, err
	}
	resultType := "success"
	if result.IsError {
		resultType = "failure"
	}
	return RuntimeCallResult{
		TextResultForLlm: callToolResultText(result),
		ResultType:       resultType,
	}, nil
}

func callToolResultText(result *mcpsdk.CallToolResult) string {
	if result == nil {
		return ""
	}
	parts := make([]string, 0, len(result.Content))
	for _, content := range result.Content {
		switch c := content.(type) {
		case mcpsdk.TextContent:
			parts = append(parts, c.Text)
		case *mcpsdk.TextContent:
			parts = append(parts, c.Text)
		default:
			if b, err := json.Marshal(c); err == nil {
				parts = append(parts, string(b))
			} else {
				parts = append(parts, fmt.Sprintf("%v", c))
			}
		}
	}
	return strings.Join(parts, "\n")
}
