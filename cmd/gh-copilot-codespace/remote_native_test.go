package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ekroon/gh-copilot-codespace/internal/mcp"
	"github.com/ekroon/gh-copilot-codespace/internal/registry"
	"github.com/ekroon/gh-copilot-codespace/internal/ssh"
)

func TestRemoteNativeArgs(t *testing.T) {
	opts, err := parseLauncherArgs([]string{"--remote-native", "-c", "one", "--skill-root", "/skills/team", "--skill-root=/skills/repo", "--artifact-dir=/tmp/artifacts", "--", "--model", "gpt-6.1-sol", "--reasoning-effort=high"})
	if err != nil {
		t.Fatal(err)
	}
	if !opts.remoteNative || !reflect.DeepEqual(opts.skillRoots, []string{"/skills/team", "/skills/repo"}) || opts.artifactDir != "/tmp/artifacts" {
		t.Fatalf("native options = %+v", opts)
	}
	for _, args := range [][]string{
		{"--skill-root", "/skills"},
		{"--artifact-dir", "/tmp/artifacts"},
		{"--create-repo", "github/graph-hopper"},
		{"--remote-native", "--create-repo", "github/graph-hopper"},
		{"--remote-native", "-c", "one", "--create-repo", "github/graph-hopper"},
		{"--remote-native", "--no-codespace", "--create-repo", "invalid"},
		{"--remote-native", "--no-codespace", "--create-repo"},
		{"--remote-native", "--skill-root", "relative"},
		{"--remote-native", "--artifact-dir", "relative"},
		{"--remote-native", "--skill-root"},
		{"--remote-native", "--artifact-dir"},
		{"--remote-native", "-c", "one,two"},
		{"--remote-native", "--", "-C", "/source"},
		{"--remote-native", "--", "-C/source"},
		{"--remote-native", "--", "--resume=old"},
		{"--remote-native", "--", "--continue"},
		{"--remote-native", "--", "--session-id", "old"},
		{"--remote-native", "--", "--add-dir=/source"},
		{"--remote-native", "--", "--available-tools", "builtin:*"},
		{"--remote-native", "--", "--allow-all-tools"},
		{"--remote-native", "--", "-p", "run"},
		{"--remote-native", "--", "--agent=local"},
		{"--remote-native", "--", "--plugin-dir=/local"},
		{"--remote-native", "--", "--model"},
	} {
		if _, err := parseLauncherArgs(args); err == nil {
			t.Errorf("accepted unsafe/invalid native args: %q", args)
		}
	}
	for _, n := range []int{0, 2} {
		if err := validateRemoteNativeSelection(launcherOptions{remoteNative: true}, make([]codespace, n)); err == nil {
			t.Errorf("accepted %d initial codespaces", n)
		}
	}
	if err := validateRemoteNativeSelection(launcherOptions{remoteNative: true}, []codespace{{Name: "one"}}); err != nil {
		t.Fatal(err)
	}
	if err := validateRemoteNativeSelection(launcherOptions{remoteNative: true, noCodespace: true}, nil); err == nil {
		t.Fatal("accepted a native session without a connected Codespace")
	}
	got := remoteNativeCopilotArgs([]string{"--model", "test"})
	want := []string{"--available-tools", "custom:*", "mcp:*", "builtin:task", "builtin:ask_user", "builtin:task_complete", "--no-custom-instructions", "--model", "test"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("native startup args = %q, want %q", got, want)
	}
}

func TestRemoteNativeNoCodespaceCreatesInitialTarget(t *testing.T) {
	bin := t.TempDir()
	log := filepath.Join(bin, "gh-args")
	t.Setenv("GH_TEST_LOG", log)
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(`#!/bin/sh
printf '%s\n' "$*" >> "$GH_TEST_LOG"
case "$*" in
  "repo view --json nameWithOwner --jq .nameWithOwner") echo owner/repo ;;
  "api repos/owner/repo --jq .default_branch") echo main ;;
  "api repos/owner/repo/codespaces/machines --jq "*) echo basicLinux32gb ;;
  "api repos/owner/repo/codespaces/new --jq .defaults.devcontainer_path") echo .devcontainer/devcontainer.json ;;
  "codespace create -R owner/repo -b main -m basicLinux32gb --devcontainer-path .devcontainer/devcontainer.json") echo newly-created ;;
  *) exit 1 ;;
esac
`), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	opts, err := parseLauncherArgs([]string{"--remote-native", "--no-codespace"})
	if err != nil {
		t.Fatal(err)
	}
	selected, err := selectLauncherCodespaces(opts)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(selected, []codespace{{Name: "newly-created", DisplayName: "newly-created", Repository: "owner/repo"}}) {
		t.Fatalf("selected = %+v", selected)
	}
	if err := validateRemoteNativeSelection(opts, selected); err != nil {
		t.Fatal(err)
	}
	calls, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(calls), "repo view --json nameWithOwner --jq .nameWithOwner\napi repos/owner/repo --jq .default_branch\napi repos/owner/repo/codespaces/machines --jq ") ||
		!strings.HasSuffix(string(calls), "\ncodespace create -R owner/repo -b main -m basicLinux32gb --devcontainer-path .devcontainer/devcontainer.json\n") {
		t.Fatalf("gh calls = %q", calls)
	}
	legacy, err := selectLauncherCodespaces(launcherOptions{noCodespace: true})
	if err != nil || len(legacy) != 0 {
		t.Fatalf("default no-codespace selected %v: %v", legacy, err)
	}
}

func TestRemoteNativeNoCodespaceExplicitRepository(t *testing.T) {
	bin := t.TempDir()
	log := filepath.Join(bin, "gh-args")
	t.Setenv("GH_TEST_LOG", log)
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(`#!/bin/sh
printf '%s\n' "$*" >> "$GH_TEST_LOG"
case "$*" in
  "api repos/github/graph-hopper --jq .default_branch") echo main ;;
  "api repos/github/graph-hopper/codespaces/machines --jq "*) echo premiumLinux32gb ;;
  "api repos/github/graph-hopper/codespaces/new --jq .defaults.devcontainer_path") echo .devcontainer/devcontainer.json ;;
  "codespace create -R github/graph-hopper -b main -m premiumLinux32gb --devcontainer-path .devcontainer/devcontainer.json") echo newly-created ;;
  *) exit 1 ;;
esac
`), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	opts, err := parseLauncherArgs([]string{"--remote-native", "--no-codespace", "--create-repo", "github/graph-hopper"})
	if err != nil {
		t.Fatal(err)
	}
	selected, err := selectLauncherCodespaces(opts)
	if err != nil || !reflect.DeepEqual(selected, []codespace{{Name: "newly-created", DisplayName: "newly-created", Repository: "github/graph-hopper"}}) {
		t.Fatalf("selected = %+v, error = %v", selected, err)
	}
	calls, err := os.ReadFile(log)
	if err != nil || !strings.HasPrefix(string(calls), "api repos/github/graph-hopper --jq .default_branch\napi repos/github/graph-hopper/codespaces/machines --jq ") ||
		!strings.HasSuffix(string(calls), "\ncodespace create -R github/graph-hopper -b main -m premiumLinux32gb --devcontainer-path .devcontainer/devcontainer.json\n") {
		t.Fatalf("gh calls = %q, error = %v", calls, err)
	}
}

func TestRemoteNativeNoCodespaceOpensPermissionsReview(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("fake browser launcher uses a shell script")
	}
	for _, tt := range []struct {
		name        string
		createError string
		openError   bool
		wantOpen    bool
		wantError   string
	}{
		{
			name:        "permissions required",
			createError: "You must authorize or deny additional permissions requested by this codespace before continuing. https://github.com/codespaces/test/permissions.",
			wantOpen:    true,
			wantError:   "review the permissions in your browser, then rerun the command",
		},
		{
			name:        "browser fails",
			createError: "You must authorize or deny additional permissions https://github.com/codespaces/test/permissions",
			openError:   true,
			wantOpen:    true,
			wantError:   "could not open permissions review",
		},
		{
			name:        "no URL",
			createError: "You must authorize or deny additional permissions",
			wantError:   "creating Codespace",
		},
		{
			name:        "untrusted URL",
			createError: "You must authorize or deny additional permissions https://github.com.evil.example/codespaces/test/permissions",
			wantError:   "creating Codespace",
		},
		{
			name:        "unrelated GitHub URL",
			createError: "You must authorize or deny additional permissions https://github.com/owner/repo/settings",
			wantError:   "creating Codespace",
		},
		{
			name:        "unrelated error",
			createError: "repository not found https://github.com/codespaces/test/permissions",
			wantError:   "repository not found",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			bin := t.TempDir()
			log := filepath.Join(bin, "browser-args")
			t.Setenv("BROWSER_TEST_LOG", log)
			t.Setenv("CREATE_TEST_ERROR", tt.createError)
			if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(`#!/bin/sh
case "$1 $2" in
  "api repos/owner/repo") echo main ;;
  *) case "$*" in
    "api repos/owner/repo/codespaces/machines --jq "*) echo basicLinux32gb ;;
    "api repos/owner/repo/codespaces/new --jq .defaults.devcontainer_path") echo .devcontainer/devcontainer.json ;;
    "codespace create -R owner/repo "*)
      printf '%s\n' "$CREATE_TEST_ERROR" >&2
      exit 1 ;;
    *) exit 1 ;;
  esac ;;
esac
`), 0o700); err != nil {
				t.Fatal(err)
			}
			browser := "open"
			if runtime.GOOS == "linux" {
				browser = "xdg-open"
			}
			if err := os.WriteFile(filepath.Join(bin, browser), []byte(`#!/bin/sh
printf '%s\n' "$*" >> "$BROWSER_TEST_LOG"
if [ "$BROWSER_TEST_FAIL" = "1" ]; then exit 1; fi
`), 0o700); err != nil {
				t.Fatal(err)
			}
			if tt.openError {
				t.Setenv("BROWSER_TEST_FAIL", "1")
			}
			t.Setenv("PATH", bin)
			_, err := selectLauncherCodespaces(launcherOptions{remoteNative: true, noCodespace: true, createRepo: "owner/repo"})
			if err == nil || !strings.Contains(err.Error(), tt.wantError) || !strings.Contains(err.Error(), tt.createError) {
				t.Fatalf("create error = %v", err)
			}
			opened, readErr := os.ReadFile(log)
			if tt.wantOpen {
				if readErr != nil || string(opened) != "https://github.com/codespaces/test/permissions\n" {
					t.Fatalf("browser calls = %q, read error = %v", opened, readErr)
				}
			} else if !os.IsNotExist(readErr) {
				t.Fatalf("unexpected browser calls = %q, read error = %v", opened, readErr)
			}
		})
	}
}

func TestRemoteNativeNoCodespaceSelectsDefaultDevcontainer(t *testing.T) {
	bin := t.TempDir()
	log := filepath.Join(bin, "gh-args")
	t.Setenv("GH_TEST_LOG", log)
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(`#!/bin/sh
printf '%s\n' "$*" >> "$GH_TEST_LOG"
case "$*" in
  "api repos/github/github --jq .default_branch") echo master ;;
  "api repos/github/github/codespaces/machines --jq "*) echo xLargePremiumLinux ;;
  "api repos/github/github/codespaces/new --jq .defaults.devcontainer_path") echo .devcontainer/devcontainer.json ;;
  "codespace create -R github/github -b master -m xLargePremiumLinux --devcontainer-path .devcontainer/devcontainer.json") echo newly-created ;;
  *) exit 1 ;;
esac
`), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	opts, err := parseLauncherArgs([]string{"--remote-native", "--autopilot", "--no-codespace", "--create-repo", "github/github", "--prompt", "Run to completion"})
	if err != nil {
		t.Fatal(err)
	}
	selected, err := selectLauncherCodespaces(opts)
	if err != nil || len(selected) != 1 || selected[0].Repository != "github/github" {
		t.Fatalf("selected = %+v, error = %v", selected, err)
	}
	calls, err := os.ReadFile(log)
	if err != nil || !strings.Contains(string(calls), "codespace create -R github/github -b master -m xLargePremiumLinux --devcontainer-path .devcontainer/devcontainer.json\n") {
		t.Fatalf("gh calls = %q, error = %v", calls, err)
	}
}

func TestRemoteNativeNoCodespaceWithoutDefaultDevcontainer(t *testing.T) {
	for _, tt := range []struct {
		name        string
		available   string
		createFlags string
	}{
		{name: "multiple configurations", available: ".devcontainer/first/devcontainer.json", createFlags: " --devcontainer-path .devcontainer/first/devcontainer.json"},
		{name: "no configurations"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			bin := t.TempDir()
			log := filepath.Join(bin, "gh-args")
			t.Setenv("GH_TEST_LOG", log)
			t.Setenv("GH_TEST_AVAILABLE", tt.available)
			if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(`#!/bin/sh
printf '%s\n' "$*" >> "$GH_TEST_LOG"
case "$*" in
  "api repos/owner/repo --jq .default_branch") echo main ;;
  "api repos/owner/repo/codespaces/machines --jq "*) echo basicLinux32gb ;;
  "api repos/owner/repo/codespaces/new --jq .defaults.devcontainer_path") echo null ;;
  "api repos/owner/repo/codespaces/devcontainers --jq "*) printf '%s\n' "$GH_TEST_AVAILABLE" ;;
  "codespace create -R owner/repo -b main -m basicLinux32gb"*) echo newly-created ;;
  *) exit 1 ;;
esac
`), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin)
			selected, err := selectLauncherCodespaces(launcherOptions{remoteNative: true, noCodespace: true, createRepo: "owner/repo"})
			if err != nil || len(selected) != 1 {
				t.Fatalf("selected = %+v, error = %v", selected, err)
			}
			calls, err := os.ReadFile(log)
			if err != nil || !strings.HasSuffix(string(calls), "\ncodespace create -R owner/repo -b main -m basicLinux32gb"+tt.createFlags+"\n") {
				t.Fatalf("gh calls = %q, error = %v", calls, err)
			}
		})
	}
}

func TestRemoteNativeNoCodespaceRequiresRepository(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\nprintf '\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	if _, err := selectLauncherCodespaces(launcherOptions{remoteNative: true, noCodespace: true}); err == nil || !strings.Contains(err.Error(), "current GitHub repository") {
		t.Fatalf("missing repository error = %v", err)
	}
}

func TestRemoteNativeAutopilotPrompt(t *testing.T) {
	opts, err := parseLauncherArgs([]string{"--remote-native", "-c", "one", "--prompt", "Inspect the remote workspace", "--autopilot", "--model", "auto"})
	if err != nil {
		t.Fatal(err)
	}
	got := remoteNativeCopilotArgs(launcherCopilotArgs(opts))
	want := []string{"--available-tools", "custom:*", "mcp:*", "builtin:task", "builtin:ask_user", "builtin:task_complete", "--no-custom-instructions", "--model", "auto", "--autopilot", "--interactive=Inspect the remote workspace"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("native startup args = %q, want %q", got, want)
	}
}

func TestRemoteNativeDeploymentModeSetBeforeInitialConnection(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\nprintf '[]'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv(codespaceRemoteNativeEnv, "")
	// The empty picker result stops before SSH or execCopilot, after launcher
	// initialization. Initial deployment must already be in native mode.
	if err := runLauncher([]string{"--remote-native", "-c", "missing"}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("launcher selection stop = %v", err)
	}
	if os.Getenv(codespaceRemoteNativeEnv) != "1" {
		t.Fatal("native deployment mode was not enabled before initial connection")
	}
}

func nativeTestRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	reg := registry.New()
	if err := reg.Register(&registry.ManagedCodespace{Alias: "pinned", Name: "one", Repository: "owner/repo", Branch: "topic", Workdir: "/workspaces/repo", Executor: ssh.NewClient("one")}); err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestRemoteNativeLaunchIsolation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("COPILOT_HOME", "")
	installRemoteUIConfigWriter(t, "")
	reg := nativeTestRegistry(t)
	launch, err := prepareRemoteNativeLaunch("/self", reg, mcp.LifecycleConfig{LocalWorkdir: "/original/source"}, launcherOptions{remoteNative: true, skillRoots: []string{"/remote/skills"}})
	if err != nil {
		t.Fatal(err)
	}
	root, err := extensionRuntimeDir()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(launch.LocalWorkdir, root+string(filepath.Separator)) || filepath.Base(launch.LocalWorkdir) != "host" {
		t.Fatalf("native cwd = %q; root = %q", launch.LocalWorkdir, root)
	}
	if filepath.Dir(launch.LocalWorkdir) != filepath.Dir(launch.ArtifactDir) || filepath.Base(launch.ArtifactDir) != "artifacts" {
		t.Fatalf("host/artifact roots not separate siblings: %+v", launch)
	}
	for _, dir := range []string{filepath.Dir(launch.LocalWorkdir), launch.LocalWorkdir, launch.ArtifactDir} {
		info, err := os.Stat(dir)
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("private directory %s: %v, %v", dir, info, err)
		}
	}
	var workspace mcp.WorkspaceConfig
	if err := json.Unmarshal([]byte(launch.HostEnv[codespaceWorkspaceConfigEnv]), &workspace); err != nil {
		t.Fatal(err)
	}
	if workspace.Alias != "pinned" || workspace.ArtifactDir != launch.ArtifactDir || !reflect.DeepEqual(workspace.SkillRoots, []string{"/remote/skills"}) {
		t.Fatalf("workspace config = %+v", workspace)
	}
	if launch.HostEnv[codespaceLocalWorkdirEnv] != launch.LocalWorkdir || launch.ProcessEnv[codespaceRemoteNativeEnv] != "1" || launch.HostEnv[codespaceRemoteNativeEnv] != "1" {
		t.Fatalf("native env = %+v / %+v", launch.HostEnv, launch.ProcessEnv)
	}
	if _, err := os.Stat(filepath.Join(launch.LocalWorkdir, ".github", "extensions")); !os.IsNotExist(err) {
		t.Fatalf("native project extension duplicates stable user extension: %v", err)
	}
	profile := launch.ProcessEnv["COPILOT_HOME"]
	if !filepath.IsAbs(profile) || filepath.Dir(profile) != filepath.Dir(launch.LocalWorkdir) {
		t.Fatalf("private profile is not an absolute session sibling: %q", profile)
	}
	if _, err := os.Stat(filepath.Join(profile, "extensions", userExtensionName, "extension.mjs")); err != nil {
		t.Fatalf("private user extension missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".copilot")); !os.IsNotExist(err) {
		t.Fatalf("native launch changed original configuration root: %v", err)
	}
	for _, config := range []string{"config.json", "mcp-config.json"} {
		if _, err := os.Stat(filepath.Join(home, ".copilot", config)); !os.IsNotExist(err) {
			t.Fatalf("native launch created user config %s: %v", config, err)
		}
	}
	data, err := os.ReadFile(launch.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest extensionSessionManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Env[codespaceWorkspaceConfigEnv] != launch.HostEnv[codespaceWorkspaceConfigEnv] || manifest.Env[codespaceRemoteNativeEnv] != "1" {
		t.Fatal("manifest dropped pinned workspace config or native deployment guard")
	}
}

func TestRemoteNativeUsesOnlyStableUserExtension(t *testing.T) {
	home := t.TempDir()
	runtimeBase := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_RUNTIME_DIR", runtimeBase)
	t.Setenv("COPILOT_HOME", "")
	installRemoteUIConfigWriter(t, "")
	stable := filepath.Join(home, ".copilot", "extensions", userExtensionName, "extension.mjs")
	legacyDir := filepath.Join(home, ".copilot", "extensions", legacyGeneratedUserExtensionPrefix+"preserve")
	oldProject := filepath.Join(runtimeBase, "old-native-session", "host", ".github", "extensions", userExtensionName, "extension.mjs")
	files := map[string]string{
		stable: "already installed baseline extension",
		filepath.Join(home, ".copilot", "config.json"):     `{"model":"keep-user-preference"}`,
		filepath.Join(home, ".copilot", "mcp-config.json"): `{"mcpServers":{"keep":{"command":"configured-local-server"}}}`,
		filepath.Join(legacyDir, "extension.mjs"):          "preserve legacy directory, even when stale",
		oldProject: "preserve existing runtime extension",
	}
	for name, content := range files {
		if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	stale := time.Now().Add(-2 * generatedUserExtensionMaxAge)
	if err := os.Chtimes(legacyDir, stale, stale); err != nil {
		t.Fatal(err)
	}
	launch, err := prepareRemoteNativeLaunch("/self", nativeTestRegistry(t), mcp.LifecycleConfig{}, launcherOptions{remoteNative: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(launch.LocalWorkdir, ".github", "extensions")); !os.IsNotExist(err) {
		t.Fatalf("new native host created a second discovery location: %v", err)
	}
	for name, original := range files {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("existing user/runtime file removed: %s: %v", name, err)
		}
		if string(data) != original {
			t.Errorf("unexpected file change: %s", name)
		}
	}
	profile := launch.ProcessEnv["COPILOT_HOME"]
	entries, err := os.ReadDir(filepath.Join(profile, "extensions"))
	if err != nil || len(entries) != 1 || entries[0].Name() != userExtensionName {
		t.Fatalf("private owned extension discovery = %v, %v", entries, err)
	}
}

func TestRemoteNativeRejectsGitRuntimeDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", root)
	if _, err := prepareRemoteNativeLaunch("/self", nativeTestRegistry(t), mcp.LifecycleConfig{}, launcherOptions{remoteNative: true}); err == nil {
		t.Fatal("accepted native host cwd beneath a Git checkout")
	}
}

func TestRemoteNativeBootstrapUsesRemoteContext(t *testing.T) {
	reg := nativeTestRegistry(t)
	workspace := mcp.WorkspaceConfig{Alias: "pinned", SkillRoots: []string{"/remote/skills"}}
	rt, bootstrap, err := newExtensionHostRuntime(reg, mcp.LifecycleConfig{LocalWorkdir: "/host/private"}, &workspace)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rt.(*mcp.WorkspaceRuntime); !ok {
		t.Fatalf("native runtime = %T", rt)
	}
	if bootstrap.Workspace == nil || bootstrap.Workspace.Alias != "pinned" {
		t.Fatalf("native bootstrap = %+v", bootstrap)
	}
	for _, text := range []string{bootstrap.SystemMessage.Content, bootstrap.CustomAgents[0].Prompt} {
		for _, want := range []string{"pinned", "/workspaces/repo", "workspace_skill"} {
			if !strings.Contains(text, want) {
				t.Errorf("native prompt missing %q: %s", want, text)
			}
		}
		if strings.Contains(text, "A local checkout") || strings.Contains(text, "remote_view") {
			t.Errorf("stale legacy native prompt: %s", text)
		}
	}
	if bootstrap.CustomAgents[0].Model != "" {
		t.Fatal("native launch forced a production subagent model")
	}
	legacy, legacyBootstrap, err := newExtensionHostRuntime(reg, mcp.LifecycleConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := legacy.(*mcp.ToolRuntime); !ok || legacyBootstrap.Workspace != nil {
		t.Fatalf("legacy changed: %T, %+v", legacy, legacyBootstrap)
	}
}

func TestRemoteNativeExtensionControls(t *testing.T) {
	source := extensionSource()
	for _, want := range []string{"enableSkills: false", "skipCustomInstructions: true", "enableHostGitOperations: false", "enableOnDemandInstructionDiscovery: false", "enableFileHooks: false", "session.log(", "LOCAL HOST", "/diff", "/cwd", "! shell", "@attachment", "overridesBuiltInTool", `typeof args === "string"`, `patch: args`, `host.on("error"`, "setTimeout(", "remote-native extension unavailable"} {
		if !strings.Contains(source, want) {
			t.Errorf("native extension missing %q", want)
		}
	}
	for _, forbidden := range []string{"skillProvider", "enableConfigDiscovery: false", "onPermissionRequest:", "requestedEnvironmentVariables:"} {
		if strings.Contains(source, forbidden) {
			t.Errorf("native extension contains unsupported/unsafe %q", forbidden)
		}
	}
}

func TestRemoteNativeJSBootstrap(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	source := extensionSource()
	start := strings.Index(source, "function nativeSessionOptions(")
	end := strings.Index(source, "if (!manifest)")
	if start < 0 || end <= start {
		t.Fatal("missing native session configuration helper")
	}
	script := source[start:end] + `
const options = nativeSessionOptions({alias:"pinned", localWorkdir:"/host", workdir:"/workspaces/repo"});
for (const key of ["workingDirectory", "cwd", "configDirectory", "configDir", "enableConfigDiscovery", "pluginDirectories"]) {
  if (key in options) throw new Error("native options must inherit the live session's context/discovery: " + key);
}
for (const key of ["enableSkills", "enableHostGitOperations", "enableFileHooks", "enableOnDemandInstructionDiscovery"]) {
  if (options[key] !== false) throw new Error(key + " not disabled");
}
if (options.skipCustomInstructions !== true) throw new Error("ambient instructions enabled");
if ("enableConfigDiscovery" in options || "onPermissionRequest" in options) throw new Error("MCP/permission settings changed");
if (JSON.stringify(normalizeToolArgs("apply_patch", "patch text")) !== '{"patch":"patch text"}') throw new Error("string patch not wrapped");
if (normalizeToolArgs("view", "/file") !== "/file") throw new Error("non-patch arguments changed");
if (!mayOverrideBuiltIn({name:"view",overridesBuiltInTool:true}, true)) throw new Error("view override lost");
if (mayOverrideBuiltIn({name:"workspace_skill",overridesBuiltInTool:true}, true)) throw new Error("custom override allowed");
if (mayOverrideBuiltIn({name:"view",overridesBuiltInTool:true}, false)) throw new Error("legacy override changed");
`
	cmd := exec.Command(node, "--input-type=module", "-")
	cmd.Stdin = strings.NewReader(script)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("native JS controls: %v\n%s", err, output)
	}
}
