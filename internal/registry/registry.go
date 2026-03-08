package registry

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/ekroon/gh-copilot-codespace/internal/ssh"
)

// ManagedCodespace represents a connected codespace with its SSH client and metadata.
type ManagedCodespace struct {
	Alias      string
	Name       string // gh codespace name (e.g., "fluffy-spoon-abc123")
	Repository string // e.g., "github/github"
	Branch     string
	Workdir    string       // detected workspace directory on the codespace
	Executor   ssh.Executor // SSH client for this codespace
	ExecAgent  string       // remote path to deployed binary (may be empty)
}

// Registry manages multiple codespace connections keyed by alias.
type Registry struct {
	mu         sync.RWMutex
	codespaces map[string]*ManagedCodespace
}

// New creates an empty registry.
func New() *Registry {
	return &Registry{
		codespaces: make(map[string]*ManagedCodespace),
	}
}

// Register adds a codespace to the registry. Returns an error if the alias is already taken.
func (r *Registry) Register(cs *ManagedCodespace) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.codespaces[cs.Alias]; exists {
		return fmt.Errorf("alias %q already registered", cs.Alias)
	}
	if existing := r.findByNameLocked(cs.Name); existing != nil {
		return fmt.Errorf("codespace %q already connected as alias %q", cs.Name, existing.Alias)
	}
	r.codespaces[cs.Alias] = cs
	return nil
}

// Deregister removes a codespace from the registry.
func (r *Registry) Deregister(alias string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.codespaces, alias)
}

// UpdateBranch updates the tracked branch for an already-registered codespace.
func (r *Registry) UpdateBranch(alias, branch string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	cs, ok := r.codespaces[alias]
	if !ok {
		return fmt.Errorf("codespace %q not found", alias)
	}
	cs.Branch = branch
	return nil
}

// Resolve finds a codespace by alias. When alias is empty and exactly one
// codespace is registered, it returns that one (single-codespace convenience).
func (r *Registry) Resolve(alias string) (*ManagedCodespace, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if alias == "" {
		if len(r.codespaces) == 0 {
			return nil, fmt.Errorf("no codespaces connected; use create_codespace or connect_codespace first")
		}
		if len(r.codespaces) == 1 {
			for _, cs := range r.codespaces {
				return cs, nil
			}
		}
		return nil, fmt.Errorf("multiple codespaces connected — specify which one: %s",
			strings.Join(r.aliasesLocked(), ", "))
	}

	cs, ok := r.codespaces[alias]
	if !ok {
		return nil, fmt.Errorf("codespace %q not found; available: %s",
			alias, strings.Join(r.aliasesLocked(), ", "))
	}
	return cs, nil
}

// Aliases returns a sorted list of registered aliases.
func (r *Registry) Aliases() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.aliasesLocked()
}

func (r *Registry) aliasesLocked() []string {
	aliases := make([]string, 0, len(r.codespaces))
	for alias := range r.codespaces {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	return aliases
}

// FindByName returns the registered codespace with the given codespace name.
func (r *Registry) FindByName(name string) *ManagedCodespace {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.findByNameLocked(name)
}

func (r *Registry) findByNameLocked(name string) *ManagedCodespace {
	if name == "" {
		return nil
	}
	for _, cs := range r.codespaces {
		if cs.Name == name {
			return cs
		}
	}
	return nil
}

// All returns all registered codespaces (sorted by alias).
func (r *Registry) All() []*ManagedCodespace {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]*ManagedCodespace, 0, len(r.codespaces))
	aliases := r.aliasesLocked()
	for _, alias := range aliases {
		result = append(result, r.codespaces[alias])
	}
	return result
}

// Len returns the number of registered codespaces.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.codespaces)
}

// DefaultAlias derives an alias from a repository name (e.g., "github/github" → "github").
// If the derived alias conflicts with existing aliases, a numeric suffix is appended.
func DefaultAlias(repository string, existing []string) string {
	base := repository
	if i := strings.LastIndex(repository, "/"); i >= 0 {
		base = repository[i+1:]
	}

	// Check for conflicts
	taken := make(map[string]bool, len(existing))
	for _, a := range existing {
		taken[a] = true
	}

	if !taken[base] {
		return base
	}

	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s-%d", base, i)
		if !taken[candidate] {
			return candidate
		}
	}
}
