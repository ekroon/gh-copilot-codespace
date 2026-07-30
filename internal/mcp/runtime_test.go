package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	mcpsdk "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func TestToolRuntimeCallUsesSDKBinaryResultShape(t *testing.T) {
	runtime := &ToolRuntime{
		handlers: map[string]server.ToolHandlerFunc{
			"test_result": func(context.Context, mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
				return &mcpsdk.CallToolResult{
					Content: []mcpsdk.Content{
						mcpsdk.TextContent{Type: "text", Text: "hello"},
						mcpsdk.ImageContent{Type: "image", Data: "aW1hZ2U=", MIMEType: "image/png"},
						mcpsdk.EmbeddedResource{
							Type: "resource",
							Resource: mcpsdk.BlobResourceContents{
								URI:      "file:///artifact.bin",
								MIMEType: "application/octet-stream",
								Blob:     "YmluYXJ5",
							},
						},
					},
					StructuredContent: map[string]any{"path": "artifact.bin", "size": 6},
				}, nil
			},
		},
	}

	result, err := runtime.Call(context.Background(), "test_result", nil)
	if err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	if result.ResultType != "success" {
		t.Fatalf("result = %+v", result)
	}
	if len(result.BinaryResultsForLlm) != 2 {
		t.Fatalf("binary results = %#v, want image and resource", result.BinaryResultsForLlm)
	}
	if got := result.BinaryResultsForLlm[0]; got.Data != "aW1hZ2U=" || got.MIMEType != "image/png" || got.Type != "image" {
		t.Fatalf("image binary result = %#v", got)
	}
	if got := result.BinaryResultsForLlm[1]; got.Data != "YmluYXJ5" || got.MIMEType != "application/octet-stream" || got.Type != "resource" {
		t.Fatalf("resource binary result = %#v", got)
	}
	if result.TextResultForLlm != "hello" {
		t.Fatalf("text result = %q, want legacy plain text", result.TextResultForLlm)
	}
	if !reflect.DeepEqual(result.StructuredContent, map[string]any{"path": "artifact.bin", "size": float64(6)}) {
		t.Fatalf("structured content = %#v, want separate metadata", result.StructuredContent)
	}

	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if _, ok := raw["structuredContent"]; !ok {
		t.Fatalf("runtime result omitted structuredContent: %s", data)
	}
	if _, ok := raw["contents"]; ok {
		t.Fatalf("runtime result emitted unsupported contents: %s", data)
	}
	if got := bytes.Count(data, []byte("aW1hZ2U=")); got != 1 {
		t.Fatalf("serialized image payload count = %d, want 1", got)
	}
}

func TestToolRuntimeCallSerializesLargeImageOnce(t *testing.T) {
	imageData := strings.Repeat("QUJD", 256*1024)
	runtime := &ToolRuntime{
		handlers: map[string]server.ToolHandlerFunc{
			"large_image": func(context.Context, mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
				return &mcpsdk.CallToolResult{
					Content: []mcpsdk.Content{
						mcpsdk.TextContent{Type: "text", Text: "large.png (image/png)"},
						mcpsdk.ImageContent{Type: "image", Data: imageData, MIMEType: "image/png"},
					},
					StructuredContent: map[string]any{
						"kind":        "image",
						"content":     "large.png (image/png)",
						"mime_type":   "image/png",
						"base64_data": imageData,
						"truncated":   false,
					},
				}, nil
			},
		},
	}

	result, err := runtime.Call(context.Background(), "large_image", nil)
	if err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	if len(result.BinaryResultsForLlm) != 1 {
		t.Fatalf("binary results = %#v, want one image", result.BinaryResultsForLlm)
	}
	if got := result.BinaryResultsForLlm[0]; got.Data != imageData || got.MIMEType != "image/png" || got.Type != "image" {
		t.Fatalf("image binary result = %#v", got)
	}
	if strings.Contains(result.TextResultForLlm, imageData) ||
		strings.Contains(result.TextResultForLlm, "base64_data") {
		t.Fatalf("text result repeats image payload: %s", result.TextResultForLlm)
	}
	if result.TextResultForLlm != "large.png (image/png)" {
		t.Fatalf("text result = %q, want legacy plain text", result.TextResultForLlm)
	}
	wantMetadata := map[string]any{
		"kind":      "image",
		"mime_type": "image/png",
		"truncated": false,
	}
	if !reflect.DeepEqual(result.StructuredContent, wantMetadata) {
		t.Fatalf("structured content = %#v, want %#v", result.StructuredContent, wantMetadata)
	}

	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if got := bytes.Count(data, []byte(imageData)); got != 1 {
		t.Fatalf("serialized image payload count = %d, want 1", got)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if _, ok := raw["contents"]; ok {
		t.Fatalf("runtime result emitted unsupported contents: %s", data)
	}
	if metadata, ok := raw["structuredContent"].(map[string]any); !ok ||
		metadata["kind"] != "image" || metadata["mime_type"] != "image/png" {
		t.Fatalf("structuredContent = %#v, want image metadata", raw["structuredContent"])
	}
}

func TestToolRuntimeCallWarnsOnTruncatedStructuredText(t *testing.T) {
	tests := []struct {
		name       string
		text       string
		structured any
	}{
		{
			name: "directory",
			text: "internal/\ninternal/mcp/\n",
			structured: map[string]any{
				"kind":      "directory",
				"truncated": true,
			},
		},
		{
			name: "glob",
			text: "one.go\ntwo.go\n",
			structured: map[string]any{
				"paths":     []string{"."},
				"truncated": true,
				"limit":     2,
			},
		},
		{
			name: "text",
			text: "1. one\n2. two\n",
			structured: map[string]any{
				"kind":      "file",
				"truncated": true,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runtime := &ToolRuntime{
				handlers: map[string]server.ToolHandlerFunc{
					"truncated": func(context.Context, mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
						return &mcpsdk.CallToolResult{
							Content: []mcpsdk.Content{
								mcpsdk.TextContent{Type: "text", Text: tt.text},
							},
							StructuredContent: tt.structured,
						}, nil
					},
				},
			}

			result, err := runtime.Call(context.Background(), "truncated", nil)
			if err != nil {
				t.Fatalf("Call() error = %v", err)
			}
			if result.ResultType != "success" {
				t.Fatalf("result type = %q, want successful partial text result", result.ResultType)
			}
			if !strings.Contains(result.TextResultForLlm, tt.text) {
				t.Fatalf("text result lost partial content: %q", result.TextResultForLlm)
			}
			lower := strings.ToLower(result.TextResultForLlm)
			if !strings.Contains(lower, "warning") || !strings.Contains(lower, "truncated") ||
				!strings.Contains(lower, "narrower") {
				t.Fatalf("text result lacks clear truncation warning: %q", result.TextResultForLlm)
			}
			if len(result.BinaryResultsForLlm) != 0 {
				t.Fatalf("binary results = %#v, want none", result.BinaryResultsForLlm)
			}
		})
	}
}

func TestToolRuntimeCallRejectsTruncatedImageBinary(t *testing.T) {
	runtime := &ToolRuntime{
		handlers: map[string]server.ToolHandlerFunc{
			"truncated_image": func(context.Context, mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
				return &mcpsdk.CallToolResult{
					Content: []mcpsdk.Content{
						mcpsdk.TextContent{Type: "text", Text: "large.png (image/png)"},
						mcpsdk.ImageContent{Type: "image", Data: "aW5jb21wbGV0ZQ==", MIMEType: "image/png"},
					},
					StructuredContent: map[string]any{
						"kind":      "image",
						"mime_type": "image/png",
						"truncated": true,
					},
				}, nil
			},
		},
	}

	result, err := runtime.Call(context.Background(), "truncated_image", nil)
	if err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	if result.ResultType != "failure" {
		t.Fatalf("result type = %q, want failure for incomplete image", result.ResultType)
	}
	if len(result.BinaryResultsForLlm) != 0 {
		t.Fatalf("binary results = %#v, want truncated image suppressed", result.BinaryResultsForLlm)
	}
	for _, want := range []string{"large.png", "truncated", "forceReadLargeFiles", "narrower"} {
		if !strings.Contains(result.TextResultForLlm, want) {
			t.Fatalf("text result = %q, want %q", result.TextResultForLlm, want)
		}
	}
}
