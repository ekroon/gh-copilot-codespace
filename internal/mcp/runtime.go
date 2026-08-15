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
	TextResultForLlm    string                `json:"textResultForLlm"`
	ResultType          string                `json:"resultType"`
	StructuredContent   any                   `json:"structuredContent,omitempty"`
	BinaryResultsForLlm []RuntimeBinaryResult `json:"binaryResultsForLlm,omitempty"`
}

type RuntimeBinaryResult struct {
	Data        string `json:"data"`
	MIMEType    string `json:"mimeType"`
	Type        string `json:"type"`
	Description string `json:"description,omitempty"`
}

const (
	truncatedResultWarning = "WARNING: The result is truncated and incomplete. Make a narrower request to retrieve complete output."
	truncatedImageWarning  = "ERROR: The image result is truncated or incomplete, so no binary image was returned. Retry with forceReadLargeFiles=true or make a narrower request."
)

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
		{tool: applyPatchTool(), handler: applyPatchHandler(reg)},
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
	}
	if !cfg.AccessPolicy.SelectedOnly {
		tools = append(tools,
			runtimeTool{tool: listAvailableCodespacesTool(), handler: listAvailableCodespacesHandlerWithState(state)},
			runtimeTool{tool: getCodespaceOptionsTool(), handler: getCodespaceOptionsHandler(state.cfg.GHRunner)},
			runtimeTool{tool: createCodespaceTool(), handler: createCodespaceHandlerWithState(reg, state)},
			runtimeTool{tool: connectCodespaceTool(), handler: connectCodespaceHandlerWithState(reg, state)},
			runtimeTool{tool: deleteCodespaceTool(), handler: deleteCodespaceHandlerWithState(reg, state)},
		)
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
	textResult, binaryResults := callToolResultPayload(result)
	metadata := structuredMetadata(result.StructuredContent)
	if truncated, image := structuredTruncation(metadata); truncated {
		if image {
			resultType = "failure"
			binaryResults = nil
			textResult = prependResultWarning(textResult, truncatedImageWarning)
		} else {
			textResult = prependResultWarning(textResult, truncatedResultWarning)
		}
	}
	return RuntimeCallResult{
		TextResultForLlm:    textResult,
		ResultType:          resultType,
		StructuredContent:   metadata,
		BinaryResultsForLlm: binaryResults,
	}, nil
}

func callToolResultPayload(result *mcpsdk.CallToolResult) (string, []RuntimeBinaryResult) {
	if result == nil {
		return "", nil
	}
	parts := make([]string, 0, len(result.Content))
	binaryResults := make([]RuntimeBinaryResult, 0)
	for _, content := range result.Content {
		switch c := content.(type) {
		case mcpsdk.TextContent:
			parts = append(parts, c.Text)
		case *mcpsdk.TextContent:
			if c == nil {
				continue
			}
			parts = append(parts, c.Text)
		case mcpsdk.ImageContent:
			appendBinaryResult(&binaryResults, c.Data, c.MIMEType, "image", "")
		case *mcpsdk.ImageContent:
			if c == nil {
				continue
			}
			appendBinaryResult(&binaryResults, c.Data, c.MIMEType, "image", "")
		case mcpsdk.AudioContent:
			appendBinaryResult(&binaryResults, c.Data, c.MIMEType, "resource", "audio")
		case *mcpsdk.AudioContent:
			if c == nil {
				continue
			}
			appendBinaryResult(&binaryResults, c.Data, c.MIMEType, "resource", "audio")
		case mcpsdk.ResourceLink:
			appendJSONText(&parts, c)
		case *mcpsdk.ResourceLink:
			if c == nil {
				continue
			}
			appendJSONText(&parts, c)
		case mcpsdk.EmbeddedResource:
			appendEmbeddedResource(&parts, &binaryResults, c.Resource)
		case *mcpsdk.EmbeddedResource:
			if c == nil {
				continue
			}
			appendEmbeddedResource(&parts, &binaryResults, c.Resource)
		default:
			appendJSONText(&parts, c)
		}
	}
	return strings.Join(parts, "\n"), binaryResults
}

func appendBinaryResult(results *[]RuntimeBinaryResult, data, mimeType, resultType, description string) {
	if data == "" {
		return
	}
	*results = append(*results, RuntimeBinaryResult{
		Data:        data,
		MIMEType:    defaultMIMEType(mimeType),
		Type:        resultType,
		Description: description,
	})
}

func appendJSONText(parts *[]string, value any) {
	if b, err := json.Marshal(value); err == nil {
		*parts = append(*parts, string(b))
	} else {
		*parts = append(*parts, fmt.Sprintf("%v", value))
	}
}

func appendEmbeddedResource(parts *[]string, binaryResults *[]RuntimeBinaryResult, resource mcpsdk.ResourceContents) {
	if textResource, ok := mcpsdk.AsTextResourceContents(resource); ok && textResource.Text != "" {
		*parts = append(*parts, textResource.Text)
	}
	if _, ok := mcpsdk.AsTextResourceContents(resource); ok {
		return
	}
	if blobResource, ok := mcpsdk.AsBlobResourceContents(resource); ok {
		appendBinaryResult(binaryResults, blobResource.Blob, blobResource.MIMEType, "resource", blobResource.URI)
	}
}

func structuredTruncation(value any) (truncated bool, image bool) {
	metadata, ok := value.(map[string]any)
	if !ok {
		return false, false
	}
	for key, value := range metadata {
		switch normalizedStructuredKey(key) {
		case "truncated":
			truncated, _ = value.(bool)
		case "kind", "type":
			kind, _ := value.(string)
			image = image || strings.EqualFold(kind, "image")
		}
	}
	return truncated, image
}

func prependResultWarning(text, warning string) string {
	if text == "" {
		return warning
	}
	if strings.HasPrefix(text, warning) {
		return text
	}
	return warning + "\n\n" + text
}

func structuredMetadata(value any) any {
	if value == nil {
		return nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var metadata any
	if err := json.Unmarshal(data, &metadata); err != nil {
		return nil
	}
	stripStructuredPayload(metadata)
	data, err = json.Marshal(metadata)
	if err != nil || string(data) == "{}" || string(data) == "[]" || string(data) == "null" {
		return nil
	}
	return metadata
}

func stripStructuredPayload(value any) {
	switch v := value.(type) {
	case map[string]any:
		for key, child := range v {
			normalized := normalizedStructuredKey(key)
			switch normalized {
			case "base64data", "content", "entries", "output":
				delete(v, key)
				continue
			}
			if normalized == "data" && (v["type"] == "image" || v["type"] == "audio") {
				delete(v, key)
				continue
			}
			if normalized == "blob" {
				if _, hasURI := v["uri"]; hasURI {
					delete(v, key)
					continue
				}
			}
			stripStructuredPayload(child)
		}
	case []any:
		for _, child := range v {
			stripStructuredPayload(child)
		}
	}
}

func normalizedStructuredKey(key string) string {
	return strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(key))
}

func defaultMIMEType(value string) string {
	if value == "" {
		return "application/octet-stream"
	}
	return value
}
