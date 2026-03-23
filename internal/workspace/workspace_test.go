package workspace

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestNewWorkspace(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	ws, err := New("")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer os.RemoveAll(ws.Dir)

	if ws.Name == "" {
		t.Error("expected non-empty name")
	}
	if ws.Manifest == nil {
		t.Error("expected non-nil manifest")
	}

	// Verify directory exists
	if _, err := os.Stat(ws.Dir); err != nil {
		t.Errorf("workspace dir not created: %v", err)
	}

	// Verify git init
	if _, err := os.Stat(filepath.Join(ws.Dir, ".git")); err != nil {
		t.Errorf("git not initialized: %v", err)
	}

	// Verify workspace.json written
	if _, err := os.Stat(filepath.Join(ws.Dir, "workspace.json")); err != nil {
		t.Errorf("workspace.json not created: %v", err)
	}
}

func TestNewWorkspace_CustomName(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	ws, err := New("my-session")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer os.RemoveAll(ws.Dir)

	if ws.Name != "my-session" {
		t.Errorf("got name %q, want %q", ws.Name, "my-session")
	}
}

func TestLoadWorkspace(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	// Create a workspace first
	ws, err := New("test-load")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Add a codespace entry
	ws.AddCodespace("github", CodespaceEntry{
		Name:       "cs-abc",
		Repository: "github/github",
		Branch:     "main",
		Workdir:    "/workspaces/github",
	})
	ws.Manifest.SetAccessPolicy(true, []string{"cs-abc"})
	if err := ws.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(ws.Dir, "workspace.json"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(data), `"selectedOnly": true`) {
		t.Fatalf("expected workspace.json to persist selectedOnly, got %q", string(data))
	}

	// Load it
	loaded, err := Load("test-load")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if loaded.Name != "test-load" {
		t.Errorf("got name %q, want %q", loaded.Name, "test-load")
	}
	if len(loaded.Manifest.Codespaces) != 1 {
		t.Fatalf("got %d codespaces, want 1", len(loaded.Manifest.Codespaces))
	}
	entry := loaded.Manifest.Codespaces["github"]
	if entry.Name != "cs-abc" {
		t.Errorf("got codespace name %q, want %q", entry.Name, "cs-abc")
	}
	if !loaded.Manifest.SelectedOnly {
		t.Error("expected selected-only policy to round trip")
	}
	if !reflect.DeepEqual(loaded.Manifest.AllowedCodespaceNames, []string{"cs-abc"}) {
		t.Errorf("allowed codespace names = %v, want [cs-abc]", loaded.Manifest.AllowedCodespaceNames)
	}
}

func TestLoadWorkspace_NotFound(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	_, err := Load("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent workspace")
	}
}

func TestWorkspacePath(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	path := WorkspacePath("my-session")
	want := filepath.Join(tmpDir, ".copilot", "workspaces", "my-session")
	if path != want {
		t.Errorf("got %q, want %q", path, want)
	}
}

func TestListWorkspaces(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	// Create two workspaces
	ws1, _ := New("session-a")
	ws1.AddCodespace("github", CodespaceEntry{Repository: "github/github"})
	ws1.Save()

	ws2, _ := New("session-b")
	ws2.Save()

	list, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("got %d workspaces, want 2", len(list))
	}
}

func TestListWorkspacesIncludesSummaryMetadata(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	ws, err := New("session-a")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ws.AddCodespace("repo", CodespaceEntry{
		Name:       "cs-abc",
		Repository: "github/github",
		Branch:     "main",
	})
	ws.AddCodespace("docs", CodespaceEntry{
		Name:       "cs-docs",
		Repository: "github/docs",
		Branch:     "feature/planning",
	})
	ws.Manifest.SetAccessPolicy(true, []string{"cs-abc"})
	if err := ws.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	manifestPath := filepath.Join(ws.Dir, "workspace.json")
	lastUsed := time.Date(2026, 3, 19, 12, 34, 0, 0, time.UTC)
	if err := os.Chtimes(manifestPath, lastUsed, lastUsed); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	list, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("got %d workspaces, want 1", len(list))
	}

	got := list[0]
	if got.Name != "session-a" {
		t.Fatalf("Name = %q, want session-a", got.Name)
	}
	if got.Path != ws.Dir {
		t.Fatalf("Path = %q, want %q", got.Path, ws.Dir)
	}
	if !got.LastUsed.Equal(lastUsed) {
		t.Fatalf("LastUsed = %v, want %v", got.LastUsed, lastUsed)
	}
	if !got.SelectedOnly {
		t.Fatal("expected SelectedOnly to be true")
	}
	if !reflect.DeepEqual(got.Repositories, []string{"github/docs", "github/github"}) {
		t.Fatalf("Repositories = %v, want [github/docs github/github]", got.Repositories)
	}
	if !reflect.DeepEqual(got.CodespaceNames, []string{"cs-abc", "cs-docs"}) {
		t.Fatalf("CodespaceNames = %v, want [cs-abc cs-docs]", got.CodespaceNames)
	}
	if !reflect.DeepEqual(got.Branches, []string{"feature/planning", "main"}) {
		t.Fatalf("Branches = %v, want [feature/planning main]", got.Branches)
	}
}

func TestAddRemoveCodespace(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	ws, _ := New("test-add-remove")

	ws.AddCodespace("github", CodespaceEntry{
		Name:       "cs-abc",
		Repository: "github/github",
	})
	if len(ws.Manifest.Codespaces) != 1 {
		t.Fatalf("got %d codespaces after add, want 1", len(ws.Manifest.Codespaces))
	}

	ws.RemoveCodespace("github")
	if len(ws.Manifest.Codespaces) != 0 {
		t.Fatalf("got %d codespaces after remove, want 0", len(ws.Manifest.Codespaces))
	}
}

func TestManifestSetAccessPolicy(t *testing.T) {
	manifest := &Manifest{}

	manifest.SetAccessPolicy(true, []string{"cs-1", "", "cs-1", "cs-2"})

	if !manifest.SelectedOnly {
		t.Fatal("expected selected-only policy to be enabled")
	}
	if !reflect.DeepEqual(manifest.AllowedCodespaceNames, []string{"cs-1", "cs-2"}) {
		t.Fatalf("allowed codespace names = %v, want [cs-1 cs-2]", manifest.AllowedCodespaceNames)
	}

	manifest.SetAccessPolicy(false, []string{"", ""})
	if manifest.SelectedOnly {
		t.Fatal("expected selected-only policy to be disabled")
	}
	if len(manifest.AllowedCodespaceNames) != 0 {
		t.Fatalf("allowed codespace names = %v, want empty", manifest.AllowedCodespaceNames)
	}
}

func TestManifestAllowedCodespaceHelpers(t *testing.T) {
	manifest := &Manifest{
		AllowedCodespaceNames: []string{"cs-1", "", "cs-1", "cs-2"},
	}

	manifest.NormalizeAccessPolicy()
	if !reflect.DeepEqual(manifest.AllowedCodespaceNames, []string{"cs-1", "cs-2"}) {
		t.Fatalf("allowed codespace names after normalize = %v, want [cs-1 cs-2]", manifest.AllowedCodespaceNames)
	}

	if !manifest.HasAllowedCodespaceName("cs-1") {
		t.Fatal("expected cs-1 to be allowlisted")
	}
	if manifest.HasAllowedCodespaceName("") {
		t.Fatal("did not expect empty codespace name to be allowlisted")
	}
	if manifest.AddAllowedCodespaceName("cs-2") {
		t.Fatal("did not expect duplicate codespace name to be added")
	}
	if !manifest.AddAllowedCodespaceName("cs-3") {
		t.Fatal("expected cs-3 to be added")
	}
	if !reflect.DeepEqual(manifest.AllowedCodespaceNames, []string{"cs-1", "cs-2", "cs-3"}) {
		t.Fatalf("allowed codespace names after add = %v, want [cs-1 cs-2 cs-3]", manifest.AllowedCodespaceNames)
	}

	if !manifest.RemoveAllowedCodespaceName("cs-1") {
		t.Fatal("expected cs-1 to be removed")
	}
	if manifest.RemoveAllowedCodespaceName("missing") {
		t.Fatal("did not expect missing codespace name to be removed")
	}
	if !reflect.DeepEqual(manifest.AllowedCodespaceNames, []string{"cs-2", "cs-3"}) {
		t.Fatalf("allowed codespace names after remove = %v, want [cs-2 cs-3]", manifest.AllowedCodespaceNames)
	}

	if !manifest.RemoveAllowedCodespaceName("cs-2") || !manifest.RemoveAllowedCodespaceName("cs-3") {
		t.Fatal("expected remaining codespace names to be removed")
	}
	if len(manifest.AllowedCodespaceNames) != 0 {
		t.Fatalf("allowed codespace names = %v, want empty", manifest.AllowedCodespaceNames)
	}
}
