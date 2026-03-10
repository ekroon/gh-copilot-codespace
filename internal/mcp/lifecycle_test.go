package mcp

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/ekroon/gh-copilot-codespace/internal/codespaceenv"
	"github.com/ekroon/gh-copilot-codespace/internal/registry"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

// mockGHRunner simulates gh CLI calls for testing.
type mockGHRunner struct {
	results map[string]mockGHResult // key: first arg (e.g., "codespace")
	calls   [][]string
}

type mockGHResult struct {
	output string
	err    error
}

func (m *mockGHRunner) Run(_ context.Context, args ...string) (string, error) {
	m.calls = append(m.calls, args)
	// Match on the command pattern
	key := strings.Join(args[:2], " ")
	if r, ok := m.results[key]; ok {
		return r.output, r.err
	}
	// Default: match on first two args for codespace commands
	if len(args) >= 2 {
		if r, ok := m.results[args[0]+" "+args[1]]; ok {
			return r.output, r.err
		}
	}
	return "", nil
}

func TestCreateCodespaceHandler_MissingRepo(t *testing.T) {
	reg := registry.New()
	gh := &mockGHRunner{}
	handler := createCodespaceHandler(reg, LifecycleConfig{GHRunner: gh})

	res, _ := handler(context.Background(), makeReq(map[string]any{}))
	if !res.IsError {
		t.Fatal("expected error for missing repository")
	}
	if !strings.Contains(resultText(res), "missing required parameter") {
		t.Errorf("expected 'missing required parameter', got %q", resultText(res))
	}
}

func TestCreateCodespaceHandler_AliasConflict(t *testing.T) {
	reg := registry.New()
	reg.Register(&registry.ManagedCodespace{Alias: "github", Name: "cs-old", Executor: &mockExecutor{}})

	gh := &mockGHRunner{}
	handler := createCodespaceHandler(reg, LifecycleConfig{GHRunner: gh})

	res, _ := handler(context.Background(), makeReq(map[string]any{
		"repository": "github/github",
		"alias":      "github",
	}))
	if !res.IsError {
		t.Fatal("expected error for alias conflict")
	}
	if !strings.Contains(resultText(res), "already in use") {
		t.Errorf("expected 'already in use', got %q", resultText(res))
	}
}

func TestCreateCodespaceHandler_CreateFails(t *testing.T) {
	reg := registry.New()
	gh := &mockGHRunner{
		results: map[string]mockGHResult{
			"codespace create": {output: "error", err: fmt.Errorf("quota exceeded")},
		},
	}
	handler := createCodespaceHandler(reg, LifecycleConfig{GHRunner: gh})

	res, _ := handler(context.Background(), makeReq(map[string]any{
		"repository": "github/github",
	}))
	if !res.IsError {
		t.Fatal("expected error when creation fails")
	}
	if !strings.Contains(resultText(res), "quota exceeded") {
		t.Errorf("expected quota error, got %q", resultText(res))
	}
}

func TestDeleteCodespaceHandler_Disconnect(t *testing.T) {
	reg := registry.New()
	reg.Register(&registry.ManagedCodespace{Alias: "github", Name: "cs-abc", Executor: &mockExecutor{}})

	gh := &mockGHRunner{}
	handler := deleteCodespaceHandler(reg, gh)

	res, _ := handler(context.Background(), makeReq(map[string]any{
		"codespace": "github",
	}))
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultText(res))
	}
	if !strings.Contains(resultText(res), "Disconnected") {
		t.Errorf("expected disconnect message, got %q", resultText(res))
	}
	if !strings.Contains(resultText(res), "still running") {
		t.Errorf("expected 'still running' message, got %q", resultText(res))
	}

	// Verify deregistered
	if reg.Len() != 0 {
		t.Error("expected registry to be empty after disconnect")
	}
}

func TestDeleteCodespaceHandler_DeleteFromGitHub(t *testing.T) {
	reg := registry.New()
	reg.Register(&registry.ManagedCodespace{Alias: "github", Name: "cs-abc", Executor: &mockExecutor{}})

	gh := &mockGHRunner{
		results: map[string]mockGHResult{
			"codespace delete": {output: "deleted"},
		},
	}
	handler := deleteCodespaceHandler(reg, gh)

	res, _ := handler(context.Background(), makeReq(map[string]any{
		"codespace": "github",
		"delete":    true,
	}))
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultText(res))
	}
	if !strings.Contains(resultText(res), "deleted") {
		t.Errorf("expected delete message, got %q", resultText(res))
	}
}

func TestDeleteCodespaceHandler_NotFound(t *testing.T) {
	reg := registry.New()
	gh := &mockGHRunner{}
	handler := deleteCodespaceHandler(reg, gh)

	res, _ := handler(context.Background(), makeReq(map[string]any{
		"codespace": "nonexistent",
	}))
	if !res.IsError {
		t.Fatal("expected error for nonexistent codespace")
	}
}

func TestConnectCodespaceHandler_MissingName(t *testing.T) {
	reg := registry.New()
	handler := connectCodespaceHandler(reg, LifecycleConfig{})

	res, _ := handler(context.Background(), makeReq(map[string]any{}))
	if !res.IsError {
		t.Fatal("expected error for missing name")
	}
}

func TestConnectCodespaceHandler_DuplicateCodespaceName(t *testing.T) {
	t.Setenv("PATH", "")

	reg := registry.New()
	if err := reg.Register(&registry.ManagedCodespace{
		Alias:    "graph-hopper",
		Name:     "graph-hopper-pre-prod-97pxr4rj4cpg79",
		Executor: &mockExecutor{},
	}); err != nil {
		t.Fatalf("register existing codespace: %v", err)
	}

	handler := connectCodespaceHandler(reg, LifecycleConfig{})
	res, _ := handler(context.Background(), makeReq(map[string]any{
		"name": "graph-hopper-pre-prod-97pxr4rj4cpg79",
	}))
	if !res.IsError {
		t.Fatal("expected duplicate codespace error")
	}
	if !strings.Contains(resultText(res), `already connected as alias "graph-hopper"`) {
		t.Fatalf("expected existing alias in error, got %q", resultText(res))
	}
}

func TestNewLifecycleSSHClientAppliesGitHubAuthMode(t *testing.T) {
	client := newLifecycleSSHClient("cs-abc", codespaceenv.GitHubAuthLocal)
	if got := client.GitHubAuthMode(); got != codespaceenv.GitHubAuthLocal {
		t.Fatalf("GitHubAuthMode() = %q, want %q", got, codespaceenv.GitHubAuthLocal)
	}
}

// Helper for lifecycle tests
func makeLifecycleReq(args map[string]any) mcpsdk.CallToolRequest {
	return mcpsdk.CallToolRequest{
		Params: mcpsdk.CallToolParams{
			Arguments: args,
		},
	}
}

func TestExtractURL(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "permissions error with URL",
			input: "You must authorize or deny additional permissions requested by this codespace before continuing. https://github.com/codespaces/some-codespace/permissions",
			want:  "https://github.com/codespaces/some-codespace/permissions",
		},
		{
			name:  "no URL",
			input: "You must authorize or deny additional permissions",
			want:  "",
		},
		{
			name:  "URL with trailing period",
			input: "Visit https://github.com/settings/codespaces.",
			want:  "https://github.com/settings/codespaces",
		},
		{
			name:  "empty string",
			input: "",
			want:  "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractURL(tt.input)
			if got != tt.want {
				t.Errorf("extractURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCreateCodespaceHandler_PermissionsError(t *testing.T) {
	reg := registry.New()
	gh := &mockGHRunner{
		results: map[string]mockGHResult{
			"codespace create": {
				output: "",
				err:    fmt.Errorf("exit status 1\nYou must authorize or deny additional permissions https://github.com/codespaces/test/permissions"),
			},
		},
	}
	handler := createCodespaceHandler(reg, LifecycleConfig{GHRunner: gh})

	res, _ := handler(context.Background(), makeReq(map[string]any{
		"repository": "github/github",
	}))
	if !res.IsError {
		t.Fatal("expected error for permissions")
	}
	text := resultText(res)
	if !strings.Contains(text, "https://github.com/codespaces/test/permissions") {
		t.Errorf("expected URL in error, got %q", text)
	}
	if !strings.Contains(text, "default_permissions=true") {
		t.Errorf("expected default_permissions hint, got %q", text)
	}
}

func TestCreateCodespaceHandler_DefaultPermissions(t *testing.T) {
	reg := registry.New()
	gh := &mockGHRunner{
		results: map[string]mockGHResult{
			"codespace create": {output: "test-cs-name", err: nil},
			"codespace ssh":    {output: "ready", err: nil},
		},
	}
	handler := createCodespaceHandler(reg, LifecycleConfig{GHRunner: gh})

	// With default_permissions=true, --default-permissions should be in args
	handler(context.Background(), makeReq(map[string]any{
		"repository":          "github/github",
		"default_permissions": true,
	}))

	// Check that --default-permissions was passed
	found := false
	for _, call := range gh.calls {
		for _, arg := range call {
			if arg == "--default-permissions" {
				found = true
			}
		}
	}
	if !found {
		t.Error("expected --default-permissions in gh args")
	}
}

func TestGetCodespaceOptions_MissingRepo(t *testing.T) {
	gh := &mockGHRunner{}
	handler := getCodespaceOptionsHandler(gh)

	res, _ := handler(context.Background(), makeReq(map[string]any{}))
	if !res.IsError {
		t.Fatal("expected error for missing repository")
	}
}
