package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ekroon/gh-copilot-codespace/internal/registry"
	"github.com/ekroon/gh-copilot-codespace/internal/workspace"
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

func newTestWorkspace(t *testing.T, name string) *workspace.Workspace {
	t.Helper()

	home := t.TempDir()
	t.Setenv("HOME", home)

	ws, err := workspace.New(name)
	if err != nil {
		t.Fatalf("workspace.New(%q): %v", name, err)
	}
	return ws
}

func installFakeCodespaceCLI(t *testing.T, listJSON string, workdir string) {
	t.Helper()

	if listJSON == "" {
		listJSON = "[]"
	}
	if workdir == "" {
		workdir = "/workspaces/repo/"
	}

	binDir := t.TempDir()
	ghPath := filepath.Join(binDir, "gh")
	sshPath := filepath.Join(binDir, "ssh")

	const ghScript = `#!/bin/sh
set -eu

case "${1:-} ${2:-}" in
  "codespace list")
    printf '%s\n' "${FAKE_GH_CODESPACE_LIST_JSON:-[]}"
    ;;
  "codespace ssh")
    if [ "${3:-}" = "--config" ]; then
      cat <<'EOF'
Host cs.test
  HostName example.com
  User git
EOF
      exit 0
    fi

    cmd=""
    found=0
    for arg in "$@"; do
      if [ "$found" -eq 1 ]; then
        if [ -n "$cmd" ]; then
          cmd="$cmd $arg"
        else
          cmd="$arg"
        fi
      elif [ "$arg" = "--" ]; then
        found=1
      fi
    done

    case "$cmd" in
      *"ls -d /workspaces/*/ 2>/dev/null"*)
        printf '%s\n' "${FAKE_GH_WORKDIR:-/workspaces/repo/}"
        ;;
      *)
        printf '%s\n' "${FAKE_GH_REMOTE_STDOUT:-ok}"
        ;;
    esac
    ;;
  *)
    echo "unexpected gh args: $*" >&2
    exit 1
    ;;
esac
`

	const sshScript = `#!/bin/sh
echo "fake ssh unavailable" >&2
exit 1
`

	if err := os.WriteFile(ghPath, []byte(ghScript), 0o755); err != nil {
		t.Fatalf("WriteFile gh: %v", err)
	}
	if err := os.WriteFile(sshPath, []byte(sshScript), 0o755); err != nil {
		t.Fatalf("WriteFile ssh: %v", err)
	}

	path := binDir
	if existing := os.Getenv("PATH"); existing != "" {
		path += string(os.PathListSeparator) + existing
	}
	t.Setenv("PATH", path)
	t.Setenv("FAKE_GH_CODESPACE_LIST_JSON", listJSON)
	t.Setenv("FAKE_GH_WORKDIR", workdir)
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

func TestCreateCodespaceHandler_PersistsWorkspaceManifestAndAllowlist(t *testing.T) {
	ws := newTestWorkspace(t, "create-manifest")
	installFakeCodespaceCLI(t, `[{"name":"cs-created","repository":"owner/repo"}]`, "/workspaces/repo/")

	reg := registry.New()
	gh := &mockGHRunner{
		results: map[string]mockGHResult{
			"codespace create": {output: "cs-created"},
			"codespace ssh":    {output: "ready"},
		},
	}
	handler := createCodespaceHandler(reg, LifecycleConfig{
		GHRunner: gh,
		AccessPolicy: CodespaceAccessPolicy{
			SelectedOnly:          true,
			AllowedCodespaceNames: []string{"cs-selected"},
		},
		Workspace: WorkspaceSessionContext{
			Name: ws.Name,
			Dir:  ws.Dir,
		},
	})

	res, _ := handler(context.Background(), makeReq(map[string]any{
		"repository": "owner/repo",
	}))
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultText(res))
	}

	loaded, err := workspace.Load(ws.Name)
	if err != nil {
		t.Fatalf("workspace.Load: %v", err)
	}

	entry, ok := loaded.Manifest.Codespaces["repo"]
	if !ok {
		t.Fatalf("expected persisted alias %q, got %v", "repo", loaded.Manifest.Codespaces)
	}
	if entry.Name != "cs-created" {
		t.Fatalf("entry.Name = %q, want %q", entry.Name, "cs-created")
	}
	if entry.Repository != "owner/repo" {
		t.Fatalf("entry.Repository = %q, want %q", entry.Repository, "owner/repo")
	}
	if entry.Workdir != "/workspaces/repo" {
		t.Fatalf("entry.Workdir = %q, want %q", entry.Workdir, "/workspaces/repo")
	}
	if !reflect.DeepEqual(loaded.Manifest.AllowedCodespaceNames, []string{"cs-selected", "cs-created"}) {
		t.Fatalf("allowed codespace names = %v, want [cs-selected cs-created]", loaded.Manifest.AllowedCodespaceNames)
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

func TestDeleteCodespaceHandler_DisconnectKeepsAllowlistInWorkspaceManifest(t *testing.T) {
	ws := newTestWorkspace(t, "disconnect-manifest")
	ws.AddCodespace("repo", workspace.CodespaceEntry{
		Name:       "cs-created",
		Repository: "owner/repo",
		Workdir:    "/workspaces/repo",
	})
	ws.Manifest.SetAccessPolicy(true, []string{"cs-created"})
	if err := ws.Save(); err != nil {
		t.Fatalf("workspace.Save: %v", err)
	}

	reg := registry.New()
	if err := reg.Register(&registry.ManagedCodespace{Alias: "repo", Name: "cs-created", Executor: &mockExecutor{}}); err != nil {
		t.Fatalf("register: %v", err)
	}

	handler := deleteCodespaceHandlerWithState(reg, newLifecycleState(LifecycleConfig{
		GHRunner: &mockGHRunner{},
		AccessPolicy: CodespaceAccessPolicy{
			SelectedOnly:          true,
			AllowedCodespaceNames: []string{"cs-created"},
		},
		Workspace: WorkspaceSessionContext{
			Name: ws.Name,
			Dir:  ws.Dir,
		},
	}))

	res, _ := handler(context.Background(), makeReq(map[string]any{
		"codespace": "repo",
	}))
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultText(res))
	}

	loaded, err := workspace.Load(ws.Name)
	if err != nil {
		t.Fatalf("workspace.Load: %v", err)
	}
	if len(loaded.Manifest.Codespaces) != 0 {
		t.Fatalf("codespaces = %v, want empty", loaded.Manifest.Codespaces)
	}
	if !reflect.DeepEqual(loaded.Manifest.AllowedCodespaceNames, []string{"cs-created"}) {
		t.Fatalf("allowed codespace names = %v, want [cs-created]", loaded.Manifest.AllowedCodespaceNames)
	}
}

func TestDeleteCodespaceHandler_DeleteRemovesAllowlistFromWorkspaceManifest(t *testing.T) {
	ws := newTestWorkspace(t, "delete-manifest")
	ws.AddCodespace("repo", workspace.CodespaceEntry{
		Name:       "cs-created",
		Repository: "owner/repo",
		Workdir:    "/workspaces/repo",
	})
	ws.Manifest.SetAccessPolicy(true, []string{"cs-created"})
	if err := ws.Save(); err != nil {
		t.Fatalf("workspace.Save: %v", err)
	}

	reg := registry.New()
	if err := reg.Register(&registry.ManagedCodespace{Alias: "repo", Name: "cs-created", Executor: &mockExecutor{}}); err != nil {
		t.Fatalf("register: %v", err)
	}

	gh := &mockGHRunner{
		results: map[string]mockGHResult{
			"codespace delete": {output: "deleted"},
		},
	}
	handler := deleteCodespaceHandlerWithState(reg, newLifecycleState(LifecycleConfig{
		GHRunner: gh,
		AccessPolicy: CodespaceAccessPolicy{
			SelectedOnly:          true,
			AllowedCodespaceNames: []string{"cs-created"},
		},
		Workspace: WorkspaceSessionContext{
			Name: ws.Name,
			Dir:  ws.Dir,
		},
	}))

	res, _ := handler(context.Background(), makeReq(map[string]any{
		"codespace": "repo",
		"delete":    true,
	}))
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultText(res))
	}

	loaded, err := workspace.Load(ws.Name)
	if err != nil {
		t.Fatalf("workspace.Load: %v", err)
	}
	if len(loaded.Manifest.Codespaces) != 0 {
		t.Fatalf("codespaces = %v, want empty", loaded.Manifest.Codespaces)
	}
	if len(loaded.Manifest.AllowedCodespaceNames) != 0 {
		t.Fatalf("allowed codespace names = %v, want empty", loaded.Manifest.AllowedCodespaceNames)
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

func TestConnectCodespaceHandler_PersistsWorkspaceManifestWithoutExpandingAllowlist(t *testing.T) {
	ws := newTestWorkspace(t, "connect-manifest")
	ws.Manifest.SetAccessPolicy(true, []string{"cs-selected"})
	if err := ws.Save(); err != nil {
		t.Fatalf("workspace.Save: %v", err)
	}
	installFakeCodespaceCLI(t, `[{"name":"cs-selected","repository":"owner/repo"}]`, "/workspaces/repo/")

	reg := registry.New()
	handler := connectCodespaceHandler(reg, LifecycleConfig{
		AccessPolicy: CodespaceAccessPolicy{
			SelectedOnly:          true,
			AllowedCodespaceNames: []string{"cs-selected"},
		},
		Workspace: WorkspaceSessionContext{
			Name: ws.Name,
			Dir:  ws.Dir,
		},
	})

	res, _ := handler(context.Background(), makeReq(map[string]any{
		"name": "cs-selected",
	}))
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultText(res))
	}

	loaded, err := workspace.Load(ws.Name)
	if err != nil {
		t.Fatalf("workspace.Load: %v", err)
	}
	entry, ok := loaded.Manifest.Codespaces["cs-selected"]
	if !ok {
		t.Fatalf("expected persisted alias %q, got %v", "cs-selected", loaded.Manifest.Codespaces)
	}
	if entry.Name != "cs-selected" {
		t.Fatalf("entry.Name = %q, want %q", entry.Name, "cs-selected")
	}
	if entry.Repository != "owner/repo" {
		t.Fatalf("entry.Repository = %q, want %q", entry.Repository, "owner/repo")
	}
	if entry.Workdir != "/workspaces/repo" {
		t.Fatalf("entry.Workdir = %q, want %q", entry.Workdir, "/workspaces/repo")
	}
	if !reflect.DeepEqual(loaded.Manifest.AllowedCodespaceNames, []string{"cs-selected"}) {
		t.Fatalf("allowed codespace names = %v, want [cs-selected]", loaded.Manifest.AllowedCodespaceNames)
	}
}

func TestCodespaceAccessPolicy_AllowsExistingCodespace(t *testing.T) {
	tests := []struct {
		name      string
		policy    CodespaceAccessPolicy
		codespace string
		want      bool
	}{
		{
			name:      "unrestricted allows any codespace",
			policy:    CodespaceAccessPolicy{},
			codespace: "cs-any",
			want:      true,
		},
		{
			name: "startup selected only allows listed codespace",
			policy: CodespaceAccessPolicy{
				SelectedOnly:          true,
				AllowedCodespaceNames: []string{"cs-allowed"},
			},
			codespace: "cs-allowed",
			want:      true,
		},
		{
			name: "startup selected only blocks unlisted codespace",
			policy: CodespaceAccessPolicy{
				SelectedOnly:          true,
				AllowedCodespaceNames: []string{"cs-allowed"},
			},
			codespace: "cs-blocked",
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.policy.allowsExistingCodespace(tt.codespace); got != tt.want {
				t.Fatalf("allowsExistingCodespace(%q) = %v, want %v", tt.codespace, got, tt.want)
			}
		})
	}
}

func TestConnectCodespaceHandler_SelectedOnlyRejectsUnlistedCodespace(t *testing.T) {
	t.Setenv("PATH", "")

	reg := registry.New()
	handler := connectCodespaceHandler(reg, LifecycleConfig{
		AccessPolicy: CodespaceAccessPolicy{
			SelectedOnly: true,
		},
	})

	res, _ := handler(context.Background(), makeReq(map[string]any{
		"name": "graph-hopper-pre-prod-97pxr4rj4cpg79",
	}))
	if !res.IsError {
		t.Fatal("expected selected-only rejection")
	}
	text := resultText(res)
	if !strings.Contains(text, "no existing codespaces were selected at startup") {
		t.Fatalf("expected selected-only guidance, got %q", text)
	}
	if !strings.Contains(text, "create_codespace") {
		t.Fatalf("expected create_codespace guidance, got %q", text)
	}
}

func TestConnectCodespaceHandler_SelectedOnlySuggestsAllowlistWhenPresent(t *testing.T) {
	t.Setenv("PATH", "")

	reg := registry.New()
	handler := connectCodespaceHandler(reg, LifecycleConfig{
		AccessPolicy: CodespaceAccessPolicy{
			SelectedOnly:          true,
			AllowedCodespaceNames: []string{"cs-selected", "cs-created"},
		},
	})

	res, _ := handler(context.Background(), makeReq(map[string]any{
		"name": "cs-blocked",
	}))
	if !res.IsError {
		t.Fatal("expected selected-only rejection")
	}
	text := resultText(res)
	if !strings.Contains(text, "wasn't selected at startup for this selected-only session") {
		t.Fatalf("expected selected-only guidance, got %q", text)
	}
	if !strings.Contains(text, "list_available_codespaces") {
		t.Fatalf("expected list_available_codespaces guidance, got %q", text)
	}
	if !strings.Contains(text, "create_codespace") {
		t.Fatalf("expected create_codespace guidance, got %q", text)
	}
}

func TestListAvailableCodespacesHandler_FiltersDisallowedCodespaces(t *testing.T) {
	gh := &mockGHRunner{
		results: map[string]mockGHResult{
			"codespace list": {
				output: `[{"name":"cs-allowed","displayName":"Allowed","repository":"owner/repo","state":"Available"},{"name":"cs-blocked","displayName":"Blocked","repository":"owner/repo","state":"Shutdown"}]`,
			},
		},
	}

	handler := listAvailableCodespacesHandler(gh, CodespaceAccessPolicy{
		SelectedOnly:          true,
		AllowedCodespaceNames: []string{"cs-allowed"},
	})

	res, _ := handler(context.Background(), makeReq(map[string]any{}))
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultText(res))
	}
	text := resultText(res)
	if !strings.Contains(text, "cs-allowed") {
		t.Fatalf("expected allowed codespace to be listed, got %q", text)
	}
	if strings.Contains(text, "cs-blocked") {
		t.Fatalf("expected disallowed codespace to be hidden, got %q", text)
	}
	if !strings.Contains(text, `connect_codespace(name="<codespace-name>")`) {
		t.Fatalf("expected connect hint, got %q", text)
	}
}

func TestListAvailableCodespacesHandler_RestrictedZeroCodespaceSuggestsCreate(t *testing.T) {
	gh := &mockGHRunner{
		results: map[string]mockGHResult{
			"codespace list": {
				output: `[{"name":"cs-blocked","displayName":"Blocked","repository":"owner/repo","state":"Available"}]`,
			},
		},
	}

	handler := listAvailableCodespacesHandler(gh, CodespaceAccessPolicy{
		SelectedOnly: true,
	})

	res, _ := handler(context.Background(), makeReq(map[string]any{}))
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultText(res))
	}
	text := resultText(res)
	if !strings.Contains(text, "No codespaces selected at startup are available to connect in this selected-only session.") {
		t.Fatalf("expected restricted empty-list guidance, got %q", text)
	}
	if !strings.Contains(text, "create_codespace") {
		t.Fatalf("expected create_codespace guidance, got %q", text)
	}
	if strings.Contains(text, "connect_codespace") {
		t.Fatalf("did not expect connect guidance when no codespaces selected at startup are available, got %q", text)
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
	t.Setenv("HOME", t.TempDir())
	installFakeCodespaceCLI(t, `[{"name":"test-cs-name","repository":"github/github"}]`, "/workspaces/github/")

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

func TestCreateCodespaceHandler_WorkspacePersistenceFailureRollsBackRegistry(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	installFakeCodespaceCLI(t, `[{"name":"cs-created","repository":"owner/repo"}]`, "/workspaces/repo/")

	reg := registry.New()
	gh := &mockGHRunner{
		results: map[string]mockGHResult{
			"codespace create": {output: "cs-created"},
			"codespace ssh":    {output: "ready"},
		},
	}
	handler := createCodespaceHandler(reg, LifecycleConfig{
		GHRunner: gh,
		Workspace: WorkspaceSessionContext{
			Name: "missing-workspace",
		},
	})

	res, _ := handler(context.Background(), makeReq(map[string]any{
		"repository": "owner/repo",
	}))
	if !res.IsError {
		t.Fatal("expected workspace persistence failure")
	}
	text := resultText(res)
	if !strings.Contains(text, "failed to persist workspace state") {
		t.Fatalf("expected persistence failure message, got %q", text)
	}
	if reg.Len() != 0 {
		t.Fatalf("expected registry rollback, got %d entries", reg.Len())
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
