package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/ekroon/gh-copilot-codespace/internal/registry"
	"github.com/ekroon/gh-copilot-codespace/internal/ssh"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

type structuredExecutorMock struct {
	*mockExecutor

	viewReq  ssh.ViewRequest
	viewResp ssh.ViewResult
	viewErr  error

	grepReq  ssh.GrepRequest
	grepResp ssh.GrepResult
	grepErr  error

	globReq  ssh.GlobRequest
	globResp ssh.GlobResult
	globErr  error

	patchReq  ssh.ApplyPatchRequest
	patchResp ssh.ApplyPatchResult
	patchErr  error
}

func (m *structuredExecutorMock) View(_ context.Context, req ssh.ViewRequest) (ssh.ViewResult, error) {
	m.viewReq = req
	return m.viewResp, m.viewErr
}

func (m *structuredExecutorMock) GrepFiles(_ context.Context, req ssh.GrepRequest) (ssh.GrepResult, error) {
	m.grepReq = req
	return m.grepResp, m.grepErr
}

func (m *structuredExecutorMock) GlobFiles(_ context.Context, req ssh.GlobRequest) (ssh.GlobResult, error) {
	m.globReq = req
	return m.globResp, m.globErr
}

func (m *structuredExecutorMock) ApplyPatch(_ context.Context, req ssh.ApplyPatchRequest) (ssh.ApplyPatchResult, error) {
	m.patchReq = req
	return m.patchResp, m.patchErr
}

func testRegWithExecutor(exec ssh.Executor) *registry.Registry {
	reg := registry.New()
	reg.Register(&registry.ManagedCodespace{
		Alias:    "test",
		Name:     "test-cs",
		Executor: exec,
	})
	return reg
}

func jsonMap(t *testing.T, value any) map[string]any {
	t.Helper()

	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	var out map[string]any
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	return out
}

func hasImageContent(contents []mcpsdk.Content, data, mimeType string) bool {
	for _, content := range contents {
		image, ok := mcpsdk.AsImageContent(content)
		if ok && image.Data == data && image.MIMEType == mimeType {
			return true
		}
	}
	return false
}

func TestViewHandler_ValidatesViewRange(t *testing.T) {
	handler := viewHandler(testRegWithExecutor(&mockExecutor{viewFileResult: "1. hello\n"}))

	res, err := handler(context.Background(), makeReq(map[string]any{
		"path":       "internal/mcp/server.go",
		"view_range": []any{float64(7)},
	}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected tool error, got success")
	}
	if !strings.Contains(resultText(res), "view_range must contain exactly 2 integers") {
		t.Fatalf("result text = %q", resultText(res))
	}
}

func TestViewHandler_UsesStructuredDirectoryResults(t *testing.T) {
	mock := &structuredExecutorMock{
		mockExecutor: &mockExecutor{},
		viewResp: ssh.ViewResult{
			Kind:      ssh.ViewKindDirectory,
			Content:   "internal/\ninternal/mcp/\n",
			Entries:   []string{"internal/mcp/server.go"},
			Truncated: true,
		},
	}

	handler := viewHandler(testRegWithExecutor(mock))
	res, err := handler(context.Background(), makeReq(map[string]any{
		"path":                "internal",
		"view_range":          []any{float64(1), float64(-1)},
		"forceReadLargeFiles": true,
	}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got tool error: %s", resultText(res))
	}
	if !reflect.DeepEqual(mock.viewReq.ViewRange, []int{1, -1}) {
		t.Fatalf("view range = %v, want [1 -1]", mock.viewReq.ViewRange)
	}
	if mock.viewReq.Path != "internal" || !mock.viewReq.ForceReadLargeFiles {
		t.Fatalf("view request = %+v", mock.viewReq)
	}
	if res.StructuredContent == nil {
		t.Fatal("expected structured content")
	}
	got, ok := res.StructuredContent.(ssh.ViewResult)
	if !ok || got.Kind != ssh.ViewKindDirectory || len(got.Entries) != 1 {
		t.Fatalf("structured content = %#v", res.StructuredContent)
	}
	if !strings.Contains(resultText(res), "internal/mcp/") {
		t.Fatalf("result text = %q", resultText(res))
	}
}

func TestViewHandler_UsesImageContentWithRedactedStructuredMetadata(t *testing.T) {
	mock := &structuredExecutorMock{
		mockExecutor: &mockExecutor{},
		viewResp: ssh.ViewResult{
			Kind:       ssh.ViewKindImage,
			Content:    "assets/logo.png",
			MimeType:   "image/png",
			Base64Data: "aGVsbG8=",
			Truncated:  true,
		},
	}

	handler := viewHandler(testRegWithExecutor(mock))
	res, err := handler(context.Background(), makeReq(map[string]any{
		"path": "assets/logo.png",
	}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got tool error: %s", resultText(res))
	}
	if !hasImageContent(res.Content, "aGVsbG8=", "image/png") {
		t.Fatalf("content = %#v, want MCP image content", res.Content)
	}
	metadata, ok := res.StructuredContent.(ssh.ViewResult)
	if !ok {
		t.Fatalf("structured content = %#v, want ssh.ViewResult", res.StructuredContent)
	}
	if metadata.Base64Data != "" || metadata.Kind != ssh.ViewKindImage ||
		metadata.MimeType != "image/png" || !metadata.Truncated {
		t.Fatalf("structured metadata = %#v, want image metadata without base64", metadata)
	}
}

func TestToolRuntimeCall_SerializesImageOnceWithSmallMetadata(t *testing.T) {
	imageData := strings.Repeat("QUJD", 256*1024)
	mock := &structuredExecutorMock{
		mockExecutor: &mockExecutor{},
		viewResp: ssh.ViewResult{
			Kind:       ssh.ViewKindImage,
			Content:    "assets/logo.png",
			MimeType:   "image/png",
			Base64Data: imageData,
			Truncated:  false,
		},
	}

	runtime := NewToolRuntime(testRegWithExecutor(mock), LifecycleConfig{})
	result, err := runtime.Call(context.Background(), "remote_view", map[string]any{
		"path":                "assets/logo.png",
		"forceReadLargeFiles": true,
	})
	if err != nil {
		t.Fatalf("runtime.Call() error = %v", err)
	}
	if result.ResultType != "success" {
		t.Fatalf("result type = %q, want success", result.ResultType)
	}
	if !mock.viewReq.ForceReadLargeFiles {
		t.Fatalf("forceReadLargeFiles = false, want true")
	}

	serialized, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if got := bytes.Count(serialized, []byte(imageData)); got != 1 {
		t.Fatalf("serialized image payload count = %d, want 1", got)
	}

	raw := jsonMap(t, result)
	if _, ok := raw["contents"]; ok {
		t.Fatalf("runtime result emitted unsupported contents: %#v", raw)
	}
	binaryResults, ok := raw["binaryResultsForLlm"].([]any)
	if !ok || len(binaryResults) != 1 {
		t.Fatalf("binaryResultsForLlm = %#v, want one image", raw["binaryResultsForLlm"])
	}
	image, ok := binaryResults[0].(map[string]any)
	if !ok || image["data"] != imageData || image["mimeType"] != "image/png" || image["type"] != "image" {
		t.Fatalf("binary image result = %#v", binaryResults[0])
	}
	if strings.Contains(result.TextResultForLlm, imageData) || strings.Contains(result.TextResultForLlm, "base64_data") {
		t.Fatalf("text result repeats image payload: %s", result.TextResultForLlm)
	}
	if result.TextResultForLlm != "assets/logo.png" {
		t.Fatalf("result text = %q, want legacy image description", result.TextResultForLlm)
	}
	metadata, ok := raw["structuredContent"].(map[string]any)
	if !ok || metadata["kind"] != string(ssh.ViewKindImage) ||
		metadata["mime_type"] != "image/png" {
		t.Fatalf("structured content = %#v, want image metadata", raw["structuredContent"])
	}
	if _, ok := metadata["truncated"]; ok {
		t.Fatalf("structured content marks complete image truncated: %#v", metadata)
	}
	if _, ok := metadata["content"]; ok {
		t.Fatalf("structured content repeats text payload: %#v", metadata)
	}
	if _, ok := metadata["base64_data"]; ok {
		t.Fatalf("structured content repeats image payload: %#v", metadata)
	}
}

func TestToolRuntimeCall_RejectsTruncatedViewImage(t *testing.T) {
	mock := &structuredExecutorMock{
		mockExecutor: &mockExecutor{},
		viewResp: ssh.ViewResult{
			Kind:       ssh.ViewKindImage,
			Content:    "assets/logo.png",
			MimeType:   "image/png",
			Base64Data: "aW5jb21wbGV0ZQ==",
			Truncated:  true,
		},
	}

	runtime := NewToolRuntime(testRegWithExecutor(mock), LifecycleConfig{})
	result, err := runtime.Call(context.Background(), "remote_view", map[string]any{
		"path": "assets/logo.png",
	})
	if err != nil {
		t.Fatalf("runtime.Call() error = %v", err)
	}
	if result.ResultType != "failure" {
		t.Fatalf("result type = %q, want failure", result.ResultType)
	}
	if len(result.BinaryResultsForLlm) != 0 {
		t.Fatalf("binary results = %#v, want none", result.BinaryResultsForLlm)
	}
	for _, want := range []string{"assets/logo.png", "truncated", "forceReadLargeFiles", "narrower"} {
		if !strings.Contains(result.TextResultForLlm, want) {
			t.Fatalf("text result = %q, want %q", result.TextResultForLlm, want)
		}
	}
}

func TestToolRuntimeCall_WarnsOnTruncatedFilesystemText(t *testing.T) {
	mock := &structuredExecutorMock{
		mockExecutor: &mockExecutor{},
		viewResp: ssh.ViewResult{
			Kind:      ssh.ViewKindDirectory,
			Content:   "internal/\ninternal/mcp/\n",
			Entries:   []string{"internal", "internal/mcp"},
			Truncated: true,
		},
		grepResp: ssh.GrepResult{
			Output:     "one.go:1:match\n",
			OutputMode: ssh.GrepOutputModeContent,
			Paths:      []string{"."},
			Truncated:  true,
		},
		globResp: ssh.GlobResult{
			Output:    "one.go\ntwo.go\n",
			Paths:     []string{"."},
			Truncated: true,
			Limit:     2,
		},
	}
	runtime := NewToolRuntime(testRegWithExecutor(mock), LifecycleConfig{})

	tests := []struct {
		name        string
		tool        string
		args        map[string]any
		wantContent string
	}{
		{
			name:        "directory",
			tool:        "remote_view",
			args:        map[string]any{"path": "internal"},
			wantContent: "internal/mcp/",
		},
		{
			name:        "glob",
			tool:        "remote_glob",
			args:        map[string]any{"pattern": "*.go"},
			wantContent: "two.go",
		},
		{
			name:        "grep",
			tool:        "remote_grep",
			args:        map[string]any{"pattern": "match"},
			wantContent: "one.go:1:match",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := runtime.Call(context.Background(), tt.tool, tt.args)
			if err != nil {
				t.Fatalf("runtime.Call() error = %v", err)
			}
			if result.ResultType != "success" {
				t.Fatalf("result type = %q, want success", result.ResultType)
			}
			for _, want := range []string{tt.wantContent, "WARNING", "truncated", "narrower"} {
				if !strings.Contains(result.TextResultForLlm, want) {
					t.Fatalf("text result = %q, want %q", result.TextResultForLlm, want)
				}
			}
		})
	}
}

func TestToolRuntimeCall_PreservesLegacyTextAndSeparatesStructuredMetadata(t *testing.T) {
	mock := &structuredExecutorMock{
		mockExecutor: &mockExecutor{},
		viewResp: ssh.ViewResult{
			Kind:     ssh.ViewKindFile,
			Content:  "1. one\n2. two\n",
			MimeType: "text/plain",
		},
		grepResp: ssh.GrepResult{
			Output:     "one.go:1:match\n",
			OutputMode: ssh.GrepOutputModeContent,
			Paths:      []string{"."},
		},
		globResp: ssh.GlobResult{
			Output: "one.go\ntwo.go\n",
			Paths:  []string{"."},
		},
	}
	runtime := NewToolRuntime(testRegWithExecutor(mock), LifecycleConfig{})

	tests := []struct {
		name     string
		tool     string
		args     map[string]any
		wantText string
	}{
		{
			name:     "view",
			tool:     "remote_view",
			args:     map[string]any{"path": "notes.txt"},
			wantText: "1. one\n2. two\n",
		},
		{
			name:     "grep",
			tool:     "remote_grep",
			args:     map[string]any{"pattern": "match"},
			wantText: "one.go:1:match\n",
		},
		{
			name:     "glob",
			tool:     "remote_glob",
			args:     map[string]any{"pattern": "*.go"},
			wantText: "one.go\ntwo.go\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := runtime.Call(context.Background(), tt.tool, tt.args)
			if err != nil {
				t.Fatalf("runtime.Call() error = %v", err)
			}
			if result.TextResultForLlm != tt.wantText {
				t.Fatalf("text result = %q, want legacy text %q", result.TextResultForLlm, tt.wantText)
			}
			metadata, ok := jsonMap(t, result)["structuredContent"].(map[string]any)
			if !ok {
				t.Fatalf("structured content = %#v, want separate metadata", result.StructuredContent)
			}
			for _, payloadKey := range []string{"content", "output", "entries", "base64_data"} {
				if _, ok := metadata[payloadKey]; ok {
					t.Fatalf("structured content repeats %q payload: %#v", payloadKey, metadata)
				}
			}
		})
	}
}

func TestGrepHandler_SupportsLocalStyleOptions(t *testing.T) {
	mock := &structuredExecutorMock{
		mockExecutor: &mockExecutor{},
		grepResp: ssh.GrepResult{
			Output:     "cmd/main.go\ninternal/mcp/server.go\n",
			OutputMode: ssh.GrepOutputModeFilesWithMatches,
			Paths:      []string{"cmd", "internal"},
		},
	}

	handler := grepHandler(testRegWithExecutor(mock))
	res, err := handler(context.Background(), makeReq(map[string]any{
		"pattern":     "match",
		"paths":       []any{"cmd", "internal"},
		"output_mode": "files_with_matches",
		"glob":        "*.go",
		"type":        "go",
		"-i":          true,
		"-A":          float64(2),
		"-B":          float64(1),
		"-C":          float64(3),
		"-n":          false,
		"head_limit":  float64(5),
		"multiline":   true,
		"cwd":         "/workspaces/repo",
	}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got tool error: %s", resultText(res))
	}
	if !strings.Contains(resultText(res), "cmd/main.go") {
		t.Fatalf("result text = %q", resultText(res))
	}
	if mock.grepReq.Pattern != "match" || mock.grepReq.Glob != "*.go" || mock.grepReq.Type != "go" || !mock.grepReq.CaseInsensitive || mock.grepReq.Cwd != "/workspaces/repo" {
		t.Fatalf("grep request = %+v", mock.grepReq)
	}
	if !reflect.DeepEqual(mock.grepReq.Paths, []string{"cmd", "internal"}) {
		t.Fatalf("paths = %v, want [cmd internal]", mock.grepReq.Paths)
	}
	if mock.grepReq.OutputMode != ssh.GrepOutputModeFilesWithMatches || mock.grepReq.AfterContext != 2 || mock.grepReq.BeforeContext != 1 || mock.grepReq.Context != 3 || mock.grepReq.HeadLimit != 5 || !mock.grepReq.Multiline {
		t.Fatalf("grep request = %+v", mock.grepReq)
	}
	if mock.grepReq.LineNumbers == nil || *mock.grepReq.LineNumbers {
		t.Fatalf("line numbers = %v, want false", mock.grepReq.LineNumbers)
	}
}

func TestGlobHandler_SupportsLocalStylePaths(t *testing.T) {
	mock := &structuredExecutorMock{
		mockExecutor: &mockExecutor{},
		globResp: ssh.GlobResult{
			Output: "pkg/foo.go\ninternal/mcp/server.go\n",
			Paths:  []string{"pkg", "internal"},
		},
	}

	handler := globHandler(testRegWithExecutor(mock))
	res, err := handler(context.Background(), makeReq(map[string]any{
		"pattern": "**/*.go",
		"paths":   []any{"pkg", "internal"},
		"cwd":     "/workspaces/repo",
		"limit":   float64(25),
	}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got tool error: %s", resultText(res))
	}
	if !strings.Contains(resultText(res), "pkg/foo.go") {
		t.Fatalf("result text = %q", resultText(res))
	}
	if mock.globReq.Pattern != "**/*.go" || mock.globReq.Cwd != "/workspaces/repo" || mock.globReq.Limit != 25 {
		t.Fatalf("glob request = %+v", mock.globReq)
	}
	if !reflect.DeepEqual(mock.globReq.Paths, []string{"pkg", "internal"}) {
		t.Fatalf("paths = %v, want [pkg internal]", mock.globReq.Paths)
	}
}

func TestToolRuntime_RemoteApplyPatchSupportsStructuredExecutors(t *testing.T) {
	mock := &structuredExecutorMock{
		mockExecutor: &mockExecutor{},
		patchResp: ssh.ApplyPatchResult{
			Output:       "applied",
			FilesChanged: 2,
		},
	}

	runtime := NewToolRuntime(testRegWithExecutor(mock), LifecycleConfig{})

	var found bool
	for _, def := range runtime.Definitions() {
		if def.Name == "remote_apply_patch" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("remote_apply_patch definition not found")
	}

	result, err := runtime.Call(context.Background(), "remote_apply_patch", map[string]any{
		"patch": "*** Begin Patch\n*** End Patch\n",
		"cwd":   "/workspaces/repo",
	})
	if err != nil {
		t.Fatalf("runtime.Call() error = %v", err)
	}
	if result.ResultType != "success" || result.TextResultForLlm != "applied" {
		t.Fatalf("result = %+v", result)
	}
	metadata, ok := jsonMap(t, result)["structuredContent"].(map[string]any)
	if !ok || metadata["files_changed"] != float64(2) {
		t.Fatalf("structured content = %#v, want files_changed metadata", result.StructuredContent)
	}
	if mock.patchReq.Patch == "" || mock.patchReq.Cwd != "/workspaces/repo" {
		t.Fatalf("patch request = %+v", mock.patchReq)
	}
	if _, ok := jsonMap(t, result)["contents"]; ok {
		t.Fatalf("runtime result emitted unsupported contents: %+v", result)
	}
}

func TestToolRuntime_RemoteApplyPatchReturnsToolFailureWhenUnsupported(t *testing.T) {
	runtime := NewToolRuntime(testRegWithExecutor(&mockExecutor{}), LifecycleConfig{})

	result, err := runtime.Call(context.Background(), "remote_apply_patch", map[string]any{
		"patch": "*** Begin Patch\n*** End Patch\n",
	})
	if err != nil {
		t.Fatalf("runtime.Call() error = %v", err)
	}
	if result.ResultType != "failure" {
		t.Fatalf("result type = %q, want failure", result.ResultType)
	}
	if !strings.Contains(result.TextResultForLlm, ssh.ErrApplyPatchUnsupported.Error()) {
		t.Fatalf("result text = %q", result.TextResultForLlm)
	}
}
