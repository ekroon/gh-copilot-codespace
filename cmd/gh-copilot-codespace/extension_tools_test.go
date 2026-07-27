package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ekroon/gh-copilot-codespace/internal/mcp"
	"github.com/ekroon/gh-copilot-codespace/internal/registry"
)

func TestWriteExtensionSessionManifestUsesUserRuntimeDirAndCleansStaleFiles(t *testing.T) {
	runtimeBase := t.TempDir()
	repository := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtimeBase)

	runtimeDir := filepath.Join(runtimeBase, extensionRuntimeDirName)
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(runtimeDir, extensionSessionManifestPrefix+"stale.json")
	fresh := filepath.Join(runtimeDir, extensionSessionManifestPrefix+"fresh.json")
	unrelated := filepath.Join(runtimeDir, "keep.txt")
	for _, path := range []string{stale, fresh, unrelated} {
		if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	staleTime := now.Add(-(extensionSessionManifestMaxAge + time.Hour))
	if err := os.Chtimes(stale, staleTime, staleTime); err != nil {
		t.Fatal(err)
	}

	path, err := writeExtensionSessionManifest(repository, "/usr/local/bin/self", map[string]string{}, "token", "mirror")
	if err != nil {
		t.Fatalf("writeExtensionSessionManifest: %v", err)
	}
	if !strings.HasPrefix(path, runtimeDir+string(filepath.Separator)) {
		t.Fatalf("manifest path = %q, want under %q", path, runtimeDir)
	}
	if strings.HasPrefix(path, repository+string(filepath.Separator)) {
		t.Fatalf("manifest path = %q, must be outside repository %q", path, repository)
	}
	info, err := os.Stat(runtimeDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("runtime dir mode = %v, want 0700", got)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale manifest still exists or unexpected error: %v", err)
	}
	for _, remaining := range []string{fresh, unrelated} {
		if _, err := os.Stat(remaining); err != nil {
			t.Fatalf("%s should remain: %v", remaining, err)
		}
	}
}

func TestGenerateProjectExtensionInstallsOnlyUserScopedExtension(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	t.Setenv("HOME", home)

	if err := generateProjectExtension(project); err != nil {
		t.Fatalf("generateProjectExtension: %v", err)
	}
	if _, err := os.Stat(filepath.Join(project, ".github", "extensions", userExtensionName, "extension.mjs")); !os.IsNotExist(err) {
		t.Fatalf("project extension should not be written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".copilot", "extensions", userExtensionName, "extension.mjs")); err != nil {
		t.Fatalf("user extension not installed: %v", err)
	}
}

func TestPrepareExtensionLaunchUsesSingleCurrentDirectoryMode(t *testing.T) {
	home := t.TempDir()
	runtimeBase := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_RUNTIME_DIR", runtimeBase)

	launch, err := prepareExtensionLaunch("/usr/local/bin/self", registry.New(), mcp.LifecycleConfig{})
	if err != nil {
		t.Fatalf("prepareExtensionLaunch: %v", err)
	}
	if launch.Token == "" || launch.ManifestPath == "" {
		t.Fatalf("launch = %+v, want token and manifest path", launch)
	}
	if _, ok := launch.HostEnv[codespaceExtensionModeEnv]; ok {
		t.Fatalf("host env contains legacy mode: %+v", launch.HostEnv)
	}
	if launch.ProcessEnv[codespaceExtensionTokenEnv] != launch.Token {
		t.Fatalf("process token env = %q, want %q", launch.ProcessEnv[codespaceExtensionTokenEnv], launch.Token)
	}
	if launch.ProcessEnv[codespaceExtensionManifestEnv] != launch.ManifestPath {
		t.Fatalf("process manifest env = %q, want %q", launch.ProcessEnv[codespaceExtensionManifestEnv], launch.ManifestPath)
	}
}

func TestExtensionModeAlwaysUsesCurrentDirectory(t *testing.T) {
	reg := registry.New()
	env := extensionHostEnv(reg, mcp.LifecycleConfig{}, "mirror")
	if _, ok := env[codespaceExtensionModeEnv]; ok {
		t.Fatalf("host env contains legacy mode: %+v", env)
	}
	if got := preambleModeFromEnv("mirror"); got != PreambleModeHere {
		t.Fatalf("preamble mode = %v, want current-directory mode", got)
	}
}
