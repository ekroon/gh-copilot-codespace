package workspace

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"time"

	"github.com/ekroon/gh-copilot-codespace/internal/codespaceenv"
)

// Workspace represents a local workspace session directory.
type Workspace struct {
	Name     string
	Dir      string
	Manifest *Manifest
}

// Manifest is the session state stored in workspace.json.
type Manifest struct {
	Created    time.Time                 `json:"created"`
	GitHubAuth string                    `json:"githubAuth,omitempty"`
	Codespaces map[string]CodespaceEntry `json:"codespaces"`
}

// CodespaceEntry records a codespace that is part of this workspace session.
type CodespaceEntry struct {
	Name       string `json:"name"`
	Repository string `json:"repository"`
	Branch     string `json:"branch"`
	Workdir    string `json:"workdir"`
}

// WorkspaceSummary is returned by List().
type WorkspaceSummary struct {
	Name           string
	Created        time.Time
	CodespaceCount int
}

// basePath returns the root directory for all workspaces.
func basePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".copilot", "workspaces")
}

// WorkspacePath returns the full path for a workspace by name.
func WorkspacePath(name string) string {
	return filepath.Join(basePath(), name)
}

// New creates a new workspace directory with a manifest.
// If name is empty, a timestamped name is generated.
func New(name string) (*Workspace, error) {
	if name == "" {
		name = generateName()
	}

	dir := WorkspacePath(name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating workspace dir: %w", err)
	}

	// Initialize git repo
	exec.Command("git", "-C", dir, "init", "-q").Run()

	ws := &Workspace{
		Name: name,
		Dir:  dir,
		Manifest: &Manifest{
			Created:    time.Now(),
			GitHubAuth: string(codespaceenv.GitHubAuthCodespace),
			Codespaces: make(map[string]CodespaceEntry),
		},
	}

	if err := ws.Save(); err != nil {
		return nil, fmt.Errorf("saving manifest: %w", err)
	}

	return ws, nil
}

// Load reads an existing workspace from disk.
func Load(name string) (*Workspace, error) {
	dir := WorkspacePath(name)
	manifestPath := filepath.Join(dir, "workspace.json")

	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("reading workspace %q: %w", name, err)
	}

	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("parsing workspace manifest: %w", err)
	}

	if manifest.Codespaces == nil {
		manifest.Codespaces = make(map[string]CodespaceEntry)
	}
	if manifest.GitHubAuth == "" {
		manifest.GitHubAuth = string(codespaceenv.GitHubAuthCodespace)
	}

	return &Workspace{
		Name:     name,
		Dir:      dir,
		Manifest: &manifest,
	}, nil
}

// Save writes the manifest to workspace.json.
func (w *Workspace) Save() error {
	data, err := json.MarshalIndent(w.Manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling manifest: %w", err)
	}
	return os.WriteFile(filepath.Join(w.Dir, "workspace.json"), data, 0o644)
}

// AddCodespace adds a codespace entry to the manifest.
func (w *Workspace) AddCodespace(alias string, entry CodespaceEntry) {
	w.Manifest.Codespaces[alias] = entry
}

// RemoveCodespace removes a codespace entry from the manifest.
func (w *Workspace) RemoveCodespace(alias string) {
	delete(w.Manifest.Codespaces, alias)
}

// List returns summaries of all available workspaces.
func List() ([]WorkspaceSummary, error) {
	base := basePath()
	entries, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("listing workspaces: %w", err)
	}

	var result []WorkspaceSummary
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		ws, err := Load(e.Name())
		if err != nil {
			continue
		}
		result = append(result, WorkspaceSummary{
			Name:           ws.Name,
			Created:        ws.Manifest.Created,
			CodespaceCount: len(ws.Manifest.Codespaces),
		})
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Created.After(result[j].Created)
	})

	return result, nil
}

func generateName() string {
	ts := time.Now().Format("2006-01-02_150405")
	b := make([]byte, 3)
	rand.Read(b)
	return fmt.Sprintf("%s-%s", ts, hex.EncodeToString(b))
}
