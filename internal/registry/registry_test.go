package registry

import (
	"testing"
)

func newStubCS(alias, name, repo string) *ManagedCodespace {
	return &ManagedCodespace{
		Alias:      alias,
		Name:       name,
		Repository: repo,
	}
}

func TestResolve_SingleCodespace_NoAlias(t *testing.T) {
	reg := New()
	reg.Register(newStubCS("github", "cs-abc", "github/github"))

	cs, err := reg.Resolve("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cs.Alias != "github" {
		t.Errorf("got alias %q, want %q", cs.Alias, "github")
	}
}

func TestResolve_SingleCodespace_WithAlias(t *testing.T) {
	reg := New()
	reg.Register(newStubCS("github", "cs-abc", "github/github"))

	cs, err := reg.Resolve("github")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cs.Name != "cs-abc" {
		t.Errorf("got name %q, want %q", cs.Name, "cs-abc")
	}
}

func TestResolve_SingleCodespace_WrongAlias(t *testing.T) {
	reg := New()
	reg.Register(newStubCS("github", "cs-abc", "github/github"))

	_, err := reg.Resolve("nonexistent")
	if err == nil {
		t.Fatal("expected error for wrong alias")
	}
}

func TestResolve_MultipleCodespaces_NoAlias(t *testing.T) {
	reg := New()
	reg.Register(newStubCS("github", "cs-abc", "github/github"))
	reg.Register(newStubCS("web", "cs-def", "ekroon/web-app"))

	_, err := reg.Resolve("")
	if err == nil {
		t.Fatal("expected error when multiple codespaces and no alias")
	}
}

func TestResolve_MultipleCodespaces_WithAlias(t *testing.T) {
	reg := New()
	reg.Register(newStubCS("github", "cs-abc", "github/github"))
	reg.Register(newStubCS("web", "cs-def", "ekroon/web-app"))

	cs, err := reg.Resolve("web")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cs.Name != "cs-def" {
		t.Errorf("got name %q, want %q", cs.Name, "cs-def")
	}
}

func TestResolve_EmptyRegistry(t *testing.T) {
	reg := New()
	_, err := reg.Resolve("")
	if err == nil {
		t.Fatal("expected error for empty registry")
	}
}

func TestRegister_DuplicateAlias(t *testing.T) {
	reg := New()
	if err := reg.Register(newStubCS("github", "cs-abc", "github/github")); err != nil {
		t.Fatalf("first register: %v", err)
	}
	err := reg.Register(newStubCS("github", "cs-def", "github/github"))
	if err == nil {
		t.Fatal("expected error for duplicate alias")
	}
}

func TestDeregister(t *testing.T) {
	reg := New()
	reg.Register(newStubCS("github", "cs-abc", "github/github"))
	reg.Deregister("github")

	_, err := reg.Resolve("github")
	if err == nil {
		t.Fatal("expected error after deregister")
	}
}

func TestAliases(t *testing.T) {
	reg := New()
	reg.Register(newStubCS("web", "cs-def", "ekroon/web-app"))
	reg.Register(newStubCS("api", "cs-ghi", "ekroon/api"))

	aliases := reg.Aliases()
	if len(aliases) != 2 {
		t.Fatalf("got %d aliases, want 2", len(aliases))
	}
	// Should be sorted
	if aliases[0] != "api" || aliases[1] != "web" {
		t.Errorf("got aliases %v, want [api web]", aliases)
	}
}

func TestAll(t *testing.T) {
	reg := New()
	reg.Register(newStubCS("web", "cs-def", "ekroon/web-app"))
	reg.Register(newStubCS("api", "cs-ghi", "ekroon/api"))

	all := reg.All()
	if len(all) != 2 {
		t.Fatalf("got %d codespaces, want 2", len(all))
	}
}

func TestDefaultAlias_FromRepository(t *testing.T) {
	tests := []struct {
		repo string
		want string
	}{
		{"github/github", "github"},
		{"ekroon/web-app", "web-app"},
		{"org/my-service", "my-service"},
		{"single", "single"},
	}
	for _, tt := range tests {
		t.Run(tt.repo, func(t *testing.T) {
			got := DefaultAlias(tt.repo, nil)
			if got != tt.want {
				t.Errorf("DefaultAlias(%q) = %q, want %q", tt.repo, got, tt.want)
			}
		})
	}
}

func TestDefaultAlias_Conflict(t *testing.T) {
	existing := []string{"github"}
	got := DefaultAlias("github/github", existing)
	if got == "github" {
		t.Errorf("should not return %q when it already exists", got)
	}
	if got != "github-2" {
		t.Errorf("got %q, want %q", got, "github-2")
	}
}

func TestDefaultAlias_MultipleConflicts(t *testing.T) {
	existing := []string{"github", "github-2", "github-3"}
	got := DefaultAlias("github/github", existing)
	if got != "github-4" {
		t.Errorf("got %q, want %q", got, "github-4")
	}
}
