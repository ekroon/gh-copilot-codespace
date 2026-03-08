package mcp

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/ekroon/gh-copilot-codespace/internal/delegate"
	"github.com/ekroon/gh-copilot-codespace/internal/registry"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

func makeReq(args map[string]any) mcpsdk.CallToolRequest {
	return mcpsdk.CallToolRequest{
		Params: mcpsdk.CallToolParams{
			Arguments: args,
		},
	}
}

func TestRequiredString(t *testing.T) {
	tests := []struct {
		name    string
		args    map[string]any
		key     string
		want    string
		wantErr string
	}{
		{
			name: "key present with string value",
			args: map[string]any{"key": "hello"},
			key:  "key",
			want: "hello",
		},
		{
			name:    "key missing",
			args:    map[string]any{},
			key:     "key",
			wantErr: "missing required parameter",
		},
		{
			name:    "key present with non-string value",
			args:    map[string]any{"key": float64(42)},
			key:     "key",
			wantErr: "must be a string",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := requiredString(makeReq(tt.args), tt.key)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErr)
				}
				if !contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected error containing %q, got %q", tt.wantErr, err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestOptionalString(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
		key  string
		want string
	}{
		{
			name: "key present with string value",
			args: map[string]any{"key": "hello"},
			key:  "key",
			want: "hello",
		},
		{
			name: "key missing",
			args: map[string]any{},
			key:  "key",
			want: "",
		},
		{
			name: "key present with non-string value",
			args: map[string]any{"key": float64(42)},
			key:  "key",
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := optionalString(makeReq(tt.args), tt.key)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestOptionalFloat(t *testing.T) {
	tests := []struct {
		name       string
		args       map[string]any
		key        string
		defaultVal float64
		want       float64
	}{
		{
			name:       "key present with float64 value",
			args:       map[string]any{"key": float64(3.14)},
			key:        "key",
			defaultVal: 1.0,
			want:       3.14,
		},
		{
			name:       "key missing",
			args:       map[string]any{},
			key:        "key",
			defaultVal: 1.0,
			want:       1.0,
		},
		{
			name:       "key present with non-float64 value",
			args:       map[string]any{"key": "notfloat"},
			key:        "key",
			defaultVal: 1.0,
			want:       1.0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := optionalFloat(makeReq(tt.args), tt.key, tt.defaultVal)
			if got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestToInt(t *testing.T) {
	tests := []struct {
		name   string
		input  any
		want   int
		wantOK bool
	}{
		{name: "float64", input: float64(42), want: 42, wantOK: true},
		{name: "int", input: int(42), want: 42, wantOK: true},
		{name: "string", input: "42", want: 0, wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := toInt(tt.input)
			if ok != tt.wantOK {
				t.Errorf("ok = %v, want %v", ok, tt.wantOK)
			}
			if got != tt.want {
				t.Errorf("got %d, want %d", got, tt.want)
			}
		})
	}
}

func TestToolSuccess(t *testing.T) {
	result := toolSuccess("ok")
	if result.IsError {
		t.Error("expected IsError to be false")
	}
	if len(result.Content) != 1 {
		t.Fatalf("expected 1 content item, got %d", len(result.Content))
	}
	tc, ok := result.Content[0].(mcpsdk.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", result.Content[0])
	}
	if tc.Text != "ok" {
		t.Errorf("got text %q, want %q", tc.Text, "ok")
	}
}

func TestToolError(t *testing.T) {
	result := toolError("fail")
	if !result.IsError {
		t.Error("expected IsError to be true")
	}
	if len(result.Content) != 1 {
		t.Fatalf("expected 1 content item, got %d", len(result.Content))
	}
	tc, ok := result.Content[0].(mcpsdk.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", result.Content[0])
	}
	if tc.Text != "fail" {
		t.Errorf("got text %q, want %q", tc.Text, "fail")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchSubstring(s, substr)
}

func searchSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// --- Mock Executor ---

type mockExecutor struct {
	viewFileResult      string
	viewFileErr         error
	editFileErr         error
	createFileErr       error
	runBashCalls        int
	lastRunBashCommand  string
	lastRunBashCwd      string
	runBashStdout       string
	runBashStderr       string
	runBashExit         int
	runBashErr          error
	lastGrepPattern     string
	lastGrepPath        string
	lastGrepGlob        string
	lastGrepCwd         string
	grepResult          string
	grepErr             error
	lastGlobPattern     string
	lastGlobPath        string
	lastGlobCwd         string
	globResult          string
	globErr             error
	startSessionCalls   int
	lastSessionID       string
	lastCommand         string
	lastStartSessionCwd string
	startSessionErr     error
	writeSessionErr     error
	readSessionCalls    int
	readSessionResults  []string
	readSessionResult   string
	readSessionErr      error
	stopSessionCalls    int
	stopSessionErr      error
	listSessionsResult  string
	listSessionsErr     error
	workdir             string
}

func (m *mockExecutor) ViewFile(_ context.Context, _ string, _ []int) (string, error) {
	return m.viewFileResult, m.viewFileErr
}

func (m *mockExecutor) EditFile(_ context.Context, _, _, _ string) error {
	return m.editFileErr
}

func (m *mockExecutor) CreateFile(_ context.Context, _, _ string) error {
	return m.createFileErr
}

func (m *mockExecutor) RunBash(_ context.Context, command, cwd string) (string, string, int, error) {
	m.runBashCalls++
	m.lastRunBashCommand = command
	m.lastRunBashCwd = cwd
	return m.runBashStdout, m.runBashStderr, m.runBashExit, m.runBashErr
}

func (m *mockExecutor) Grep(_ context.Context, pattern, path, glob, cwd string) (string, error) {
	m.lastGrepPattern = pattern
	m.lastGrepPath = path
	m.lastGrepGlob = glob
	m.lastGrepCwd = cwd
	return m.grepResult, m.grepErr
}

func (m *mockExecutor) Glob(_ context.Context, pattern, path, cwd string) (string, error) {
	m.lastGlobPattern = pattern
	m.lastGlobPath = path
	m.lastGlobCwd = cwd
	return m.globResult, m.globErr
}

func (m *mockExecutor) StartSession(_ context.Context, sessionID, command, cwd string) error {
	m.startSessionCalls++
	m.lastSessionID = sessionID
	m.lastCommand = command
	m.lastStartSessionCwd = cwd
	return m.startSessionErr
}

func (m *mockExecutor) WriteSession(_ context.Context, _, _ string) error {
	return m.writeSessionErr
}

func (m *mockExecutor) ReadSession(_ context.Context, _ string) (string, error) {
	m.readSessionCalls++
	if len(m.readSessionResults) > 0 {
		result := m.readSessionResults[0]
		m.readSessionResults = m.readSessionResults[1:]
		return result, m.readSessionErr
	}
	return m.readSessionResult, m.readSessionErr
}

func (m *mockExecutor) StopSession(_ context.Context, _ string) error {
	m.stopSessionCalls++
	return m.stopSessionErr
}

func (m *mockExecutor) ListSessions(_ context.Context) (string, error) {
	return m.listSessionsResult, m.listSessionsErr
}

func (m *mockExecutor) SetWorkdir(dir string) {
	m.workdir = dir
}

func (m *mockExecutor) GetWorkdir() string {
	if m.workdir == "" {
		return "/workspaces"
	}
	return m.workdir
}

// helper to extract text from a CallToolResult
func resultText(r *mcpsdk.CallToolResult) string {
	if len(r.Content) == 0 {
		return ""
	}
	tc, ok := r.Content[0].(mcpsdk.TextContent)
	if !ok {
		return ""
	}
	return tc.Text
}

// testReg wraps a mockExecutor in a single-codespace registry for handler testing.
func testReg(mock *mockExecutor) *registry.Registry {
	reg := registry.New()
	reg.Register(&registry.ManagedCodespace{
		Alias:    "test",
		Name:     "test-cs",
		Executor: mock,
	})
	return reg
}

type fakeTaskManager struct {
	lastStartOptions delegate.StartOptions
	startTaskID      string
	startErr         error
	readSnapshot     delegate.TaskSnapshot
	readErr          error
	readTaskID       string
	cancelTaskID     string
	cancelErr        error
}

func (f *fakeTaskManager) StartTask(opts delegate.StartOptions) (string, error) {
	f.lastStartOptions = opts
	if f.startTaskID == "" {
		f.startTaskID = "delegate-1"
	}
	return f.startTaskID, f.startErr
}

func (f *fakeTaskManager) GetTask(id string) (delegate.TaskSnapshot, error) {
	f.readTaskID = id
	return f.readSnapshot, f.readErr
}

func (f *fakeTaskManager) CancelTask(id string) error {
	f.cancelTaskID = id
	return f.cancelErr
}

// --- Handler Tests ---

func TestViewHandler(t *testing.T) {
	tests := []struct {
		name     string
		mock     *mockExecutor
		args     map[string]any
		wantErr  bool
		wantText string
	}{
		{
			name:     "success",
			mock:     &mockExecutor{viewFileResult: "1. hello\n2. world\n"},
			args:     map[string]any{"path": "/tmp/test.txt"},
			wantText: "1. hello\n2. world\n",
		},
		{
			name:     "error from executor",
			mock:     &mockExecutor{viewFileErr: fmt.Errorf("no such file")},
			args:     map[string]any{"path": "/tmp/missing.txt"},
			wantErr:  true,
			wantText: "no such file",
		},
		{
			name:     "missing path arg",
			mock:     &mockExecutor{},
			args:     map[string]any{},
			wantErr:  true,
			wantText: "missing required parameter",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := viewHandler(testReg(tt.mock))
			res, err := handler(context.Background(), makeReq(tt.args))
			if err != nil {
				t.Fatalf("unexpected Go error: %v", err)
			}
			if tt.wantErr && !res.IsError {
				t.Fatal("expected tool error, got success")
			}
			if !tt.wantErr && res.IsError {
				t.Fatalf("expected success, got tool error: %s", resultText(res))
			}
			if !strings.Contains(resultText(res), tt.wantText) {
				t.Errorf("result text %q does not contain %q", resultText(res), tt.wantText)
			}
		})
	}
}

func TestEditHandler(t *testing.T) {
	tests := []struct {
		name     string
		mock     *mockExecutor
		args     map[string]any
		wantErr  bool
		wantText string
	}{
		{
			name:     "success",
			mock:     &mockExecutor{},
			args:     map[string]any{"path": "/tmp/f.txt", "old_str": "a", "new_str": "b"},
			wantText: "Successfully edited",
		},
		{
			name:     "executor error",
			mock:     &mockExecutor{editFileErr: fmt.Errorf("old_str not found")},
			args:     map[string]any{"path": "/tmp/f.txt", "old_str": "x", "new_str": "y"},
			wantErr:  true,
			wantText: "old_str not found",
		},
		{
			name:     "missing old_str arg",
			mock:     &mockExecutor{},
			args:     map[string]any{"path": "/tmp/f.txt", "new_str": "b"},
			wantErr:  true,
			wantText: "missing required parameter",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := editHandler(testReg(tt.mock))
			res, err := handler(context.Background(), makeReq(tt.args))
			if err != nil {
				t.Fatalf("unexpected Go error: %v", err)
			}
			if tt.wantErr && !res.IsError {
				t.Fatal("expected tool error, got success")
			}
			if !tt.wantErr && res.IsError {
				t.Fatalf("expected success, got tool error: %s", resultText(res))
			}
			if !strings.Contains(resultText(res), tt.wantText) {
				t.Errorf("result text %q does not contain %q", resultText(res), tt.wantText)
			}
		})
	}
}

func TestCreateHandler(t *testing.T) {
	tests := []struct {
		name     string
		mock     *mockExecutor
		args     map[string]any
		wantErr  bool
		wantText string
	}{
		{
			name:     "success",
			mock:     &mockExecutor{},
			args:     map[string]any{"path": "/tmp/new.txt", "file_text": "content"},
			wantText: "Created /tmp/new.txt",
		},
		{
			name:     "executor error",
			mock:     &mockExecutor{createFileErr: fmt.Errorf("permission denied")},
			args:     map[string]any{"path": "/root/f.txt", "file_text": "x"},
			wantErr:  true,
			wantText: "permission denied",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := createHandler(testReg(tt.mock))
			res, err := handler(context.Background(), makeReq(tt.args))
			if err != nil {
				t.Fatalf("unexpected Go error: %v", err)
			}
			if tt.wantErr && !res.IsError {
				t.Fatal("expected tool error, got success")
			}
			if !tt.wantErr && res.IsError {
				t.Fatalf("expected success, got tool error: %s", resultText(res))
			}
			if !strings.Contains(resultText(res), tt.wantText) {
				t.Errorf("result text %q does not contain %q", resultText(res), tt.wantText)
			}
		})
	}
}

func TestBashHandler_DefaultReturnsCompletedSessionOutput(t *testing.T) {
	mock := &mockExecutor{
		readSessionResult: "hello world\n[session exited]",
	}

	handler := bashHandler(testReg(mock))
	res, err := handler(context.Background(), makeReq(map[string]any{
		"command":      "echo hello world",
		"shellId":      "s1",
		"initial_wait": 0.001,
	}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got tool error: %s", resultText(res))
	}
	if got := resultText(res); got != "hello world" {
		t.Fatalf("result text = %q, want %q", got, "hello world")
	}
	if mock.startSessionCalls != 1 {
		t.Fatalf("startSessionCalls = %d, want 1", mock.startSessionCalls)
	}
	if mock.stopSessionCalls != 1 {
		t.Fatalf("stopSessionCalls = %d, want 1", mock.stopSessionCalls)
	}
	if mock.lastStartSessionCwd != "" {
		t.Fatalf("lastStartSessionCwd = %q, want empty fallback cwd", mock.lastStartSessionCwd)
	}
	if mock.runBashCalls != 0 {
		t.Fatalf("runBashCalls = %d, want 0", mock.runBashCalls)
	}
}

func TestBashHandler_DefaultReturnsShellIDForRunningCommand(t *testing.T) {
	mock := &mockExecutor{
		readSessionResult: "still running",
	}

	handler := bashHandler(testReg(mock))
	res, err := handler(context.Background(), makeReq(map[string]any{
		"command":      "go test ./...",
		"shellId":      "s2",
		"initial_wait": 0.001,
	}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got tool error: %s", resultText(res))
	}
	text := resultText(res)
	if !strings.Contains(text, "still running") || !strings.Contains(text, "[shellId: s2") {
		t.Fatalf("unexpected result text: %q", text)
	}
	if mock.stopSessionCalls != 0 {
		t.Fatalf("stopSessionCalls = %d, want 0", mock.stopSessionCalls)
	}
}

func TestBashHandler_DefaultPreservesOutputWhenCleanupFails(t *testing.T) {
	mock := &mockExecutor{
		readSessionResult: "done\n[session exited]",
		stopSessionErr:    fmt.Errorf("session not found"),
	}

	handler := bashHandler(testReg(mock))
	res, err := handler(context.Background(), makeReq(map[string]any{
		"command":      "echo done",
		"shellId":      "s2b",
		"initial_wait": 0.001,
	}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got tool error: %s", resultText(res))
	}
	text := resultText(res)
	if !strings.Contains(text, "done") || !strings.Contains(text, "[cleanup warning: failed to stop completed session s2b: session not found]") {
		t.Fatalf("unexpected result text: %q", text)
	}
}

func TestBashHandler_DefaultFallsBackToRunBashWhenSessionStartFails(t *testing.T) {
	mock := &mockExecutor{
		startSessionErr: fmt.Errorf("tmux unavailable"),
		runBashStdout:   "fallback output\n",
	}

	handler := bashHandler(testReg(mock))
	res, err := handler(context.Background(), makeReq(map[string]any{
		"command": "echo hi",
		"shellId": "s3",
	}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got tool error: %s", resultText(res))
	}
	if !strings.Contains(resultText(res), "fallback output") {
		t.Fatalf("unexpected result text: %q", resultText(res))
	}
	if mock.runBashCalls != 1 {
		t.Fatalf("runBashCalls = %d, want 1", mock.runBashCalls)
	}
}

func TestBashHandler_AsyncStartsSession(t *testing.T) {
	mock := &mockExecutor{
		readSessionResult: "server booting",
	}

	handler := bashHandler(testReg(mock))
	res, err := handler(context.Background(), makeReq(map[string]any{
		"command": "npm run dev",
		"mode":    "async",
		"shellId": "s4",
		"cwd":     "/workspaces/repo/web",
	}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got tool error: %s", resultText(res))
	}
	text := resultText(res)
	if !strings.Contains(text, "Started async session: s4") || !strings.Contains(text, "server booting") {
		t.Fatalf("unexpected result text: %q", text)
	}
	if mock.stopSessionCalls != 0 {
		t.Fatalf("stopSessionCalls = %d, want 0", mock.stopSessionCalls)
	}
	if mock.lastStartSessionCwd != "/workspaces/repo/web" {
		t.Fatalf("lastStartSessionCwd = %q, want %q", mock.lastStartSessionCwd, "/workspaces/repo/web")
	}
}

func TestGrepHandler(t *testing.T) {
	tests := []struct {
		name     string
		mock     *mockExecutor
		args     map[string]any
		wantErr  bool
		wantText string
	}{
		{
			name:     "success with results",
			mock:     &mockExecutor{grepResult: "file.go:10:match\n"},
			args:     map[string]any{"pattern": "match"},
			wantText: "file.go:10:match",
		},
		{
			name:     "no matches",
			mock:     &mockExecutor{grepResult: ""},
			args:     map[string]any{"pattern": "nope"},
			wantText: "No matches found.",
		},
		{
			name:     "executor error",
			mock:     &mockExecutor{grepErr: fmt.Errorf("grep failed with exit code 2")},
			args:     map[string]any{"pattern": "bad["},
			wantErr:  true,
			wantText: "grep failed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := grepHandler(testReg(tt.mock))
			res, err := handler(context.Background(), makeReq(tt.args))
			if err != nil {
				t.Fatalf("unexpected Go error: %v", err)
			}
			if tt.wantErr && !res.IsError {
				t.Fatal("expected tool error, got success")
			}
			if !tt.wantErr && res.IsError {
				t.Fatalf("expected success, got tool error: %s", resultText(res))
			}
			if !strings.Contains(resultText(res), tt.wantText) {
				t.Errorf("result text %q does not contain %q", resultText(res), tt.wantText)
			}
		})
	}
}

func TestGrepHandler_PassesExplicitCwd(t *testing.T) {
	mock := &mockExecutor{grepResult: "cmd/main.go:12:match\n"}

	handler := grepHandler(testReg(mock))
	res, err := handler(context.Background(), makeReq(map[string]any{
		"pattern": "match",
		"path":    "cmd",
		"glob":    "*.go",
		"cwd":     "/workspaces/repo",
	}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got tool error: %s", resultText(res))
	}
	if mock.lastGrepPattern != "match" || mock.lastGrepPath != "cmd" || mock.lastGrepGlob != "*.go" || mock.lastGrepCwd != "/workspaces/repo" {
		t.Fatalf("grep args = pattern:%q path:%q glob:%q cwd:%q", mock.lastGrepPattern, mock.lastGrepPath, mock.lastGrepGlob, mock.lastGrepCwd)
	}
}

func TestGlobHandler(t *testing.T) {
	tests := []struct {
		name     string
		mock     *mockExecutor
		args     map[string]any
		wantErr  bool
		wantText string
	}{
		{
			name:     "success with results",
			mock:     &mockExecutor{globResult: "src/main.go\nsrc/util.go\n"},
			args:     map[string]any{"pattern": "**/*.go"},
			wantText: "src/main.go",
		},
		{
			name:     "no matches",
			mock:     &mockExecutor{globResult: ""},
			args:     map[string]any{"pattern": "**/*.xyz"},
			wantText: "No matches found.",
		},
		{
			name:     "executor error",
			mock:     &mockExecutor{globErr: fmt.Errorf("glob failed with exit code 2")},
			args:     map[string]any{"pattern": "**/*"},
			wantErr:  true,
			wantText: "glob failed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := globHandler(testReg(tt.mock))
			res, err := handler(context.Background(), makeReq(tt.args))
			if err != nil {
				t.Fatalf("unexpected Go error: %v", err)
			}
			if tt.wantErr && !res.IsError {
				t.Fatal("expected tool error, got success")
			}
			if !tt.wantErr && res.IsError {
				t.Fatalf("expected success, got tool error: %s", resultText(res))
			}
			if !strings.Contains(resultText(res), tt.wantText) {
				t.Errorf("result text %q does not contain %q", resultText(res), tt.wantText)
			}
		})
	}
}

func TestGlobHandler_PassesExplicitCwd(t *testing.T) {
	mock := &mockExecutor{globResult: "pkg/foo.go\n"}

	handler := globHandler(testReg(mock))
	res, err := handler(context.Background(), makeReq(map[string]any{
		"pattern": "**/*.go",
		"path":    "pkg",
		"cwd":     "/workspaces/repo",
	}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got tool error: %s", resultText(res))
	}
	if mock.lastGlobPattern != "**/*.go" || mock.lastGlobPath != "pkg" || mock.lastGlobCwd != "/workspaces/repo" {
		t.Fatalf("glob args = pattern:%q path:%q cwd:%q", mock.lastGlobPattern, mock.lastGlobPath, mock.lastGlobCwd)
	}
}

func TestStopBashHandler(t *testing.T) {
	tests := []struct {
		name     string
		mock     *mockExecutor
		args     map[string]any
		wantErr  bool
		wantText string
	}{
		{
			name:     "success",
			mock:     &mockExecutor{},
			args:     map[string]any{"shellId": "s1"},
			wantText: "stopped",
		},
		{
			name:     "executor error",
			mock:     &mockExecutor{stopSessionErr: fmt.Errorf("session not found")},
			args:     map[string]any{"shellId": "bad"},
			wantErr:  true,
			wantText: "session not found",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := stopBashHandler(testReg(tt.mock))
			res, err := handler(context.Background(), makeReq(tt.args))
			if err != nil {
				t.Fatalf("unexpected Go error: %v", err)
			}
			if tt.wantErr && !res.IsError {
				t.Fatal("expected tool error, got success")
			}
			if !tt.wantErr && res.IsError {
				t.Fatalf("expected success, got tool error: %s", resultText(res))
			}
			if !strings.Contains(resultText(res), tt.wantText) {
				t.Errorf("result text %q does not contain %q", resultText(res), tt.wantText)
			}
		})
	}
}

func TestListBashHandler(t *testing.T) {
	tests := []struct {
		name     string
		mock     *mockExecutor
		wantErr  bool
		wantText string
	}{
		{
			name:     "success with sessions",
			mock:     &mockExecutor{listSessionsResult: "copilot-s1 123 456\n"},
			wantText: "copilot-s1",
		},
		{
			name:     "empty returns no active",
			mock:     &mockExecutor{listSessionsResult: ""},
			wantText: "No active sessions.",
		},
		{
			name:     "executor error",
			mock:     &mockExecutor{listSessionsErr: fmt.Errorf("list sessions failed with exit code 2")},
			wantErr:  true,
			wantText: "list sessions failed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := listBashHandler(testReg(tt.mock))
			res, err := handler(context.Background(), makeReq(map[string]any{}))
			if err != nil {
				t.Fatalf("unexpected Go error: %v", err)
			}
			if tt.wantErr && !res.IsError {
				t.Fatal("expected tool error, got success")
			}
			if !tt.wantErr && res.IsError {
				t.Fatalf("expected success, got tool error: %s", resultText(res))
			}
			if !strings.Contains(resultText(res), tt.wantText) {
				t.Errorf("result text %q does not contain %q", resultText(res), tt.wantText)
			}
		})
	}
}

func TestCdHandler(t *testing.T) {
	tests := []struct {
		name     string
		args     map[string]any
		mock     *mockExecutor
		wantErr  bool
		wantText string
		wantDir  string
	}{
		{
			name:     "missing path",
			args:     map[string]any{},
			mock:     &mockExecutor{},
			wantErr:  true,
			wantText: "missing required parameter",
		},
		{
			name:     "directory exists",
			args:     map[string]any{"path": "/workspaces/myproject/src"},
			mock:     &mockExecutor{runBashStdout: "/workspaces/myproject/src\n", runBashExit: 0},
			wantText: "Changed working directory",
			wantDir:  "/workspaces/myproject/src",
		},
		{
			name:     "directory does not exist",
			args:     map[string]any{"path": "/nonexistent"},
			mock:     &mockExecutor{runBashExit: 1},
			wantErr:  true,
			wantText: "directory does not exist",
		},
		{
			name:     "executor error",
			args:     map[string]any{"path": "/workspaces"},
			mock:     &mockExecutor{runBashErr: fmt.Errorf("connection failed")},
			wantErr:  true,
			wantText: "failed to change directory",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := cdHandler(testReg(tt.mock))
			res, err := handler(context.Background(), makeReq(tt.args))
			if err != nil {
				t.Fatalf("unexpected Go error: %v", err)
			}
			if tt.wantErr && !res.IsError {
				t.Fatal("expected tool error, got success")
			}
			if !tt.wantErr && res.IsError {
				t.Fatalf("expected success, got tool error: %s", resultText(res))
			}
			if !strings.Contains(resultText(res), tt.wantText) {
				t.Errorf("result text %q does not contain %q", resultText(res), tt.wantText)
			}
			if tt.wantDir != "" && tt.mock.workdir != tt.wantDir {
				t.Errorf("expected workdir %q, got %q", tt.wantDir, tt.mock.workdir)
			}
		})
	}
}

func TestCdHandler_ValidatesRelativePathAgainstCurrentDefaultCwd(t *testing.T) {
	mock := &mockExecutor{
		workdir:       "/workspaces/repo",
		runBashStdout: "/workspaces/repo/src\n",
		runBashExit:   0,
	}

	handler := cdHandler(testReg(mock))
	res, err := handler(context.Background(), makeReq(map[string]any{"path": "src"}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got tool error: %s", resultText(res))
	}
	if mock.lastRunBashCwd != "/workspaces/repo" {
		t.Fatalf("lastRunBashCwd = %q, want %q", mock.lastRunBashCwd, "/workspaces/repo")
	}
	if mock.workdir != "/workspaces/repo/src" {
		t.Fatalf("workdir = %q, want %q", mock.workdir, "/workspaces/repo/src")
	}
}

func TestCwdHandler(t *testing.T) {
	mock := &mockExecutor{workdir: "/workspaces/myproject"}
	handler := cwdHandler(testReg(mock))
	res, err := handler(context.Background(), makeReq(map[string]any{}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got tool error: %s", resultText(res))
	}
	if !strings.Contains(resultText(res), "/workspaces/myproject") {
		t.Errorf("expected workdir in result, got %q", resultText(res))
	}
}

func TestCwdHandlerDefault(t *testing.T) {
	mock := &mockExecutor{}
	handler := cwdHandler(testReg(mock))
	res, err := handler(context.Background(), makeReq(map[string]any{}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if !strings.Contains(resultText(res), "/workspaces") {
		t.Errorf("expected default workdir, got %q", resultText(res))
	}
}

func TestListCodespacesHandler(t *testing.T) {
	reg := registry.New()
	reg.Register(&registry.ManagedCodespace{
		Alias:      "github",
		Name:       "cs-abc",
		Repository: "github/github",
		Branch:     "main",
		Workdir:    "/workspaces/github",
		Executor:   &mockExecutor{},
	})

	handler := listCodespacesHandler(reg)
	res, err := handler(context.Background(), makeReq(map[string]any{}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	text := resultText(res)
	if !strings.Contains(text, "github") {
		t.Errorf("expected 'github' alias in output, got %q", text)
	}
	if !strings.Contains(text, "github/github") {
		t.Errorf("expected repository in output, got %q", text)
	}
}

func TestListCodespacesHandler_Empty(t *testing.T) {
	reg := registry.New()
	handler := listCodespacesHandler(reg)
	res, _ := handler(context.Background(), makeReq(map[string]any{}))
	if !strings.Contains(resultText(res), "No codespaces") {
		t.Errorf("expected 'No codespaces' message, got %q", resultText(res))
	}
}

func TestDelegateTaskHandler(t *testing.T) {
	mock := &mockExecutor{}
	reg := testReg(mock)
	cs, err := reg.Resolve("test")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	cs.Workdir = "/workspaces/repo"
	cs.ExecAgent = "/tmp/gh-copilot-codespace"

	manager := &fakeTaskManager{startTaskID: "delegate-42"}
	handler := delegateTaskHandler(reg, manager)

	result, err := handler(context.Background(), makeReq(map[string]any{
		"codespace": "test",
		"prompt":    "Implement the feature",
		"cwd":       "/workspaces/repo/subdir",
		"model":     "gpt-5",
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected tool error: %s", resultText(result))
	}
	if manager.lastStartOptions.CodespaceName != "test-cs" {
		t.Fatalf("codespace = %q, want test-cs", manager.lastStartOptions.CodespaceName)
	}
	if manager.lastStartOptions.Cwd != "/workspaces/repo/subdir" {
		t.Fatalf("cwd = %q", manager.lastStartOptions.Cwd)
	}
	if manager.lastStartOptions.ExecAgent != "/tmp/gh-copilot-codespace" {
		t.Fatalf("exec agent = %q", manager.lastStartOptions.ExecAgent)
	}
	if !strings.Contains(resultText(result), "delegate-42") {
		t.Fatalf("result text = %q", resultText(result))
	}
}

func TestReadDelegateTaskHandler(t *testing.T) {
	manager := &fakeTaskManager{
		readSnapshot: delegate.TaskSnapshot{
			ID:            "delegate-7",
			Status:        delegate.StatusCompleted,
			CodespaceName: "test-cs",
			Cwd:           "/workspaces/repo",
			Result:        "Done.",
			Log:           "Created delegate session.",
		},
	}
	handler := readDelegateTaskHandler(manager)

	result, err := handler(context.Background(), makeReq(map[string]any{"task_id": "delegate-7"}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	text := resultText(result)
	for _, want := range []string{"Task ID: delegate-7", "Status: completed", "Result:", "Done."} {
		if !strings.Contains(text, want) {
			t.Fatalf("expected %q in %q", want, text)
		}
	}
}

func TestCancelDelegateTaskHandler(t *testing.T) {
	manager := &fakeTaskManager{}
	handler := cancelDelegateTaskHandler(manager)

	result, err := handler(context.Background(), makeReq(map[string]any{"task_id": "delegate-9"}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected tool error: %s", resultText(result))
	}
	if manager.cancelTaskID != "delegate-9" {
		t.Fatalf("cancel task id = %q, want delegate-9", manager.cancelTaskID)
	}
}

func TestResolveExecutor_MultiCS_NoAlias(t *testing.T) {
	reg := registry.New()
	reg.Register(&registry.ManagedCodespace{Alias: "a", Name: "cs-a", Executor: &mockExecutor{}})
	reg.Register(&registry.ManagedCodespace{Alias: "b", Name: "cs-b", Executor: &mockExecutor{}})

	handler := viewHandler(reg)
	res, _ := handler(context.Background(), makeReq(map[string]any{"path": "/tmp/f.txt"}))
	if !res.IsError {
		t.Fatal("expected error when multiple codespaces and no alias")
	}
	if !strings.Contains(resultText(res), "multiple codespaces") {
		t.Errorf("expected disambiguation error, got %q", resultText(res))
	}
}

func TestResolveExecutor_MultiCS_WithAlias(t *testing.T) {
	mock := &mockExecutor{viewFileResult: "hello from b"}
	reg := registry.New()
	reg.Register(&registry.ManagedCodespace{Alias: "a", Name: "cs-a", Executor: &mockExecutor{}})
	reg.Register(&registry.ManagedCodespace{Alias: "b", Name: "cs-b", Executor: mock})

	handler := viewHandler(reg)
	res, _ := handler(context.Background(), makeReq(map[string]any{"path": "/tmp/f.txt", "codespace": "b"}))
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultText(res))
	}
	if !strings.Contains(resultText(res), "hello from b") {
		t.Errorf("expected result from codespace b, got %q", resultText(res))
	}
}
