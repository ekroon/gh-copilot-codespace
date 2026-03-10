package workspace

import (
	"os"
	"path/filepath"
	"testing"
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
	if ws.Manifest.GitHubAuth != "codespace" {
		t.Errorf("GitHubAuth = %q, want codespace", ws.Manifest.GitHubAuth)
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
	if err := ws.Save(); err != nil {
		t.Fatalf("Save: %v", err)
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
	if loaded.Manifest.GitHubAuth != "codespace" {
		t.Errorf("GitHubAuth = %q, want codespace", loaded.Manifest.GitHubAuth)
	}
}

func TestLoadWorkspaceBackfillsDefaultGitHubAuth(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	dir := WorkspacePath("legacy")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	data := `{"created":"2026-03-10T00:00:00Z","codespaces":{"github":{"name":"cs-abc","repository":"github/github"}}}`
	if err := os.WriteFile(filepath.Join(dir, "workspace.json"), []byte(data), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	ws, err := Load("legacy")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if ws.Manifest.GitHubAuth != "codespace" {
		t.Fatalf("GitHubAuth = %q, want codespace", ws.Manifest.GitHubAuth)
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
