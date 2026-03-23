package workspace

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// Workspace represents a local workspace session directory.
type Workspace struct {
	Name     string
	Dir      string
	Manifest *Manifest
}

// Manifest is the session state stored in workspace.json.
type Manifest struct {
	Created               time.Time                 `json:"created"`
	Codespaces            map[string]CodespaceEntry `json:"codespaces"`
	SelectedOnly          bool                      `json:"selectedOnly,omitempty"`
	AllowedCodespaceNames []string                  `json:"allowedCodespaceNames,omitempty"`
}

// SetAccessPolicy updates the persisted access policy fields.
func (m *Manifest) SetAccessPolicy(selectedOnly bool, allowedCodespaceNames []string) {
	if m == nil {
		return
	}

	m.SelectedOnly = selectedOnly
	m.AllowedCodespaceNames = normalizeAllowedCodespaceNames(allowedCodespaceNames)
}

// NormalizeAccessPolicy removes empty and duplicate codespace names.
func (m *Manifest) NormalizeAccessPolicy() {
	if m == nil {
		return
	}

	m.AllowedCodespaceNames = normalizeAllowedCodespaceNames(m.AllowedCodespaceNames)
}

// HasAllowedCodespaceName reports whether a codespace name is already allowlisted.
func (m *Manifest) HasAllowedCodespaceName(name string) bool {
	if m == nil || name == "" {
		return false
	}

	return slices.Contains(normalizeAllowedCodespaceNames(m.AllowedCodespaceNames), name)
}

// AddAllowedCodespaceName adds a codespace name to the allowlist if needed.
func (m *Manifest) AddAllowedCodespaceName(name string) bool {
	if m == nil || name == "" {
		return false
	}

	m.NormalizeAccessPolicy()
	if slices.Contains(m.AllowedCodespaceNames, name) {
		return false
	}

	m.AllowedCodespaceNames = append(m.AllowedCodespaceNames, name)
	return true
}

// RemoveAllowedCodespaceName removes a codespace name from the allowlist.
func (m *Manifest) RemoveAllowedCodespaceName(name string) bool {
	if m == nil || name == "" {
		return false
	}

	m.NormalizeAccessPolicy()
	idx := slices.Index(m.AllowedCodespaceNames, name)
	if idx < 0 {
		return false
	}

	m.AllowedCodespaceNames = slices.Delete(m.AllowedCodespaceNames, idx, idx+1)
	if len(m.AllowedCodespaceNames) == 0 {
		m.AllowedCodespaceNames = nil
	}
	return true
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
	Path           string
	Created        time.Time
	LastUsed       time.Time
	CodespaceCount int
	Repositories   []string
	CodespaceNames []string
	Branches       []string
	SelectedOnly   bool
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

	manifest.normalize()

	return &Workspace{
		Name:     name,
		Dir:      dir,
		Manifest: &manifest,
	}, nil
}

// Save writes the manifest to workspace.json.
func (w *Workspace) Save() error {
	w.Manifest.normalize()

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
		manifestInfo, err := os.Stat(filepath.Join(ws.Dir, "workspace.json"))
		if err != nil {
			continue
		}
		result = append(result, WorkspaceSummary{
			Name:           ws.Name,
			Path:           ws.Dir,
			Created:        ws.Manifest.Created,
			LastUsed:       manifestInfo.ModTime(),
			CodespaceCount: len(ws.Manifest.Codespaces),
			Repositories:   summarizeWorkspaceRepositories(ws.Manifest.Codespaces),
			CodespaceNames: summarizeWorkspaceCodespaceNames(ws.Manifest.Codespaces),
			Branches:       summarizeWorkspaceBranches(ws.Manifest.Codespaces),
			SelectedOnly:   ws.Manifest.SelectedOnly,
		})
	}

	sort.Slice(result, func(i, j int) bool {
		left := result[i].LastUsed
		if left.IsZero() {
			left = result[i].Created
		}
		right := result[j].LastUsed
		if right.IsZero() {
			right = result[j].Created
		}
		if left.Equal(right) {
			return result[i].Created.After(result[j].Created)
		}
		return left.After(right)
	})

	return result, nil
}

func generateName() string {
	ts := time.Now().Format("2006-01-02_150405")
	b := make([]byte, 3)
	rand.Read(b)
	return fmt.Sprintf("%s-%s", ts, hex.EncodeToString(b))
}

func (m *Manifest) normalize() {
	if m == nil {
		return
	}

	if m.Codespaces == nil {
		m.Codespaces = make(map[string]CodespaceEntry)
	}

	m.NormalizeAccessPolicy()
}

func normalizeAllowedCodespaceNames(names []string) []string {
	if len(names) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(names))
	normalized := make([]string, 0, len(names))
	for _, name := range names {
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		normalized = append(normalized, name)
	}

	if len(normalized) == 0 {
		return nil
	}
	return normalized
}

func summarizeWorkspaceRepositories(entries map[string]CodespaceEntry) []string {
	values := make([]string, 0, len(entries))
	for _, entry := range entries {
		values = append(values, entry.Repository)
	}
	return normalizeSummaryValues(values)
}

func summarizeWorkspaceCodespaceNames(entries map[string]CodespaceEntry) []string {
	values := make([]string, 0, len(entries))
	for _, entry := range entries {
		values = append(values, entry.Name)
	}
	return normalizeSummaryValues(values)
}

func summarizeWorkspaceBranches(entries map[string]CodespaceEntry) []string {
	values := make([]string, 0, len(entries))
	for _, entry := range entries {
		values = append(values, entry.Branch)
	}
	return normalizeSummaryValues(values)
}

func normalizeSummaryValues(values []string) []string {
	if len(values) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(values))
	normalized := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		normalized = append(normalized, value)
	}

	if len(normalized) == 0 {
		return nil
	}

	sort.Strings(normalized)
	return normalized
}
