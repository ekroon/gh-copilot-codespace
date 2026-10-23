package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ekroon/gh-copilot-codespace/internal/mcp"
)

// The fake CLI canonicalizes only the copied profile. No test invokes a real
// Copilot executable or reads the developer's configuration.
func installRemoteUIConfigWriter(t *testing.T, mode string) {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	script := "#!/bin/sh\nexec " + shellQuote(binary) + " -test.run=^TestRemoteUIConfigWriterProcess$ -- \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "copilot"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("GO_WANT_REMOTE_UI_CONFIG_WRITER", "1")
	t.Setenv("REMOTE_UI_WRITER_MODE", mode)
}

func TestRemoteUIConfigWriterProcess(t *testing.T) {
	if os.Getenv("GO_WANT_REMOTE_UI_CONFIG_WRITER") != "1" {
		return
	}
	fail := func(message string) {
		fmt.Fprintln(os.Stderr, message)
		os.Exit(7)
	}
	profile := os.Getenv("COPILOT_HOME")
	cwd, _ := os.Getwd()
	if !filepath.IsAbs(profile) || filepath.Base(profile) != "profile" || filepath.Base(cwd) != "host" || filepath.Dir(profile) != filepath.Dir(cwd) {
		fail("config writer outside private host/profile")
	}
	var args []string
	for i, arg := range os.Args {
		if arg == "--" {
			args = os.Args[i+1:]
			break
		}
	}
	if os.Getenv("REMOTE_UI_WRITER_MODE") == "gh" {
		if len(args) < 2 || !reflect.DeepEqual(args[:2], []string{"copilot", "--"}) {
			fail("missing gh copilot separator")
		}
		args = args[2:]
	}
	if !reflect.DeepEqual(args, []string{"config", "--global", "footer.showDirectory", "false"}) {
		fail("unexpected config writer arguments")
	}
	settings := filepath.Join(profile, "settings.json")
	data, err := os.ReadFile(settings)
	if err != nil {
		fail("missing private settings")
	}
	if expected := os.Getenv("REMOTE_UI_EXPECTED_SETTINGS"); expected != "" && string(data) != expected {
		fail("settings were not copied byte-for-byte before canonicalization")
	}
	switch os.Getenv("REMOTE_UI_WRITER_MODE") {
	case "timeout":
		time.Sleep(2 * remoteUIConfigTimeout)
	case "fail":
		fail("private fixture credential must not appear in error output")
	case "malformed":
		_ = os.WriteFile(settings, []byte("{broken"), 0o600)
		os.Exit(0)
	}
	text := strings.ReplaceAll(string(data), "// profile fixture\n", "")
	text = strings.ReplaceAll(text, ",}", "}")
	var root map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &root); err != nil || root == nil {
		fail("malformed fixture settings")
	}
	canonical, _ := json.Marshal(root)
	if err := os.WriteFile(settings, canonical, 0o600); err != nil {
		fail("cannot write private settings")
	}
	os.Exit(0)
}

func TestRemoteUIProfilePreservesHostConfiguration(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	original := filepath.Join(t.TempDir(), "configured profile")
	t.Setenv("COPILOT_HOME", original)
	installRemoteUIConfigWriter(t, "")
	settings := "// profile fixture\n" + `{"model":"keep","modelEffort":"high","theme":"keep","permissions":{"allow":["fixture-only"]},"auth":{"fixture":"not-a-secret"},"footer":{"showModelEffort":true,"showContextWindow":true,"showQuota":false,"showAiUsed":true,"showAgent":true,"showUsername":false,"showSandbox":true,"showYolo":true,"showSchedules":false,"customPreference":{"keep":1}},"tabs":{"custom":"keep"},"ide":{"custom":"keep"},"statusLine":{"command":"old"},}`
	t.Setenv("REMOTE_UI_EXPECTED_SETTINGS", settings)
	files := map[string]string{
		"settings.json":                  settings,
		"config.json":                    `{"keep":"config"}`,
		"mcp-config.json":                `{"mcpServers":{"keep":{"command":"host-command"}}}`,
		"plugins/plugin/config":          "preserve plugin",
		"auth/state":                     "fixture host state",
		"instructions/user.md":           "preserve instructions",
		"hooks/hook":                     "preserve hook",
		"skills/user/SKILL.md":           "preserve skill",
		"extensions/other/extension.mjs": "preserve other extension",
		"extensions/copilot-codespace/extension.mjs":     "unchanged original stable extension",
		"extensions/copilot-codespace-old/extension.mjs": "unchanged generated extension",
	}
	for name, content := range files {
		full := filepath.Join(original, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	self := "/launcher with ' quote"
	launch, err := prepareRemoteNativeLaunch(self, nativeTestRegistry(t), mcp.LifecycleConfig{}, launcherOptions{remoteNative: true})
	if err != nil {
		t.Fatal(err)
	}
	profile := launch.ProcessEnv["COPILOT_HOME"]
	if !filepath.IsAbs(profile) || filepath.Base(profile) != "profile" {
		t.Fatalf("missing absolute private profile: %q", profile)
	}
	for _, name := range []string{"config.json", "mcp-config.json", "plugins", "auth", "instructions", "hooks", "skills", "extensions/other"} {
		target, err := os.Readlink(filepath.Join(profile, name))
		if err != nil || target != filepath.Join(original, name) || !filepath.IsAbs(target) {
			t.Fatalf("host entry %s not preserved by absolute symlink: %q, %v", name, target, err)
		}
	}
	for name, expected := range files {
		data, err := os.ReadFile(filepath.Join(original, name))
		if err != nil || string(data) != expected {
			t.Fatalf("original fixture changed: %s (%v)", name, err)
		}
	}
	for name, mode := range map[string]os.FileMode{"": 0o700, "extensions": 0o700, "settings.json": 0o600} {
		info, err := os.Lstat(filepath.Join(profile, name))
		if err != nil || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != mode {
			t.Fatalf("private profile entry %s has wrong mode: %v, %v", name, info, err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(profile, "extensions"))
	if err != nil || len(entries) != 2 {
		t.Fatalf("private extension discovery = %v, %v", entries, err)
	}
	data, err := os.ReadFile(filepath.Join(profile, "extensions", userExtensionName, "extension.mjs"))
	if err != nil || string(data) != extensionSource() {
		t.Fatalf("private owned extension not installed: %v", err)
	}
	data, err = os.ReadFile(filepath.Join(profile, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal("private settings were not canonical strict JSON")
	}
	var want map[string]json.RawMessage
	strict := strings.TrimPrefix(strings.TrimSuffix(settings, ",}")+"}", "// profile fixture\n")
	if err := json.Unmarshal([]byte(strict), &want); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"model", "modelEffort", "theme", "permissions", "auth"} {
		var a, b any
		_ = json.Unmarshal(got[key], &a)
		_ = json.Unmarshal(want[key], &b)
		if !reflect.DeepEqual(a, b) {
			t.Errorf("unrelated root setting %s changed", key)
		}
	}
	for group, fields := range map[string]map[string]any{
		"footer": {"showDirectory": false, "showBranch": false, "showPullRequest": false, "showCodeChanges": false, "showCiStatus": false, "showCustom": true, "showModelEffort": true, "showContextWindow": true, "showQuota": false, "showAiUsed": true, "showAgent": true, "showUsername": false, "showSandbox": true, "showYolo": true, "showSchedules": false, "customPreference": map[string]any{"keep": float64(1)}},
		"tabs":   {"enabled": false, "custom": "keep"},
		"ide":    {"autoConnect": false, "custom": "keep"},
	} {
		var values map[string]any
		if err := json.Unmarshal(got[group], &values); err != nil || !reflect.DeepEqual(values, fields) {
			t.Errorf("UI overlay %s differs from contract", group)
		}
	}
	statusPath := filepath.Join(filepath.Dir(profile), "status.json")
	var status map[string]any
	_ = json.Unmarshal(got["statusLine"], &status)
	if !reflect.DeepEqual(status, map[string]any{"type": "command", "command": shellQuote(self) + " native-status " + shellQuote(statusPath), "refreshInterval": float64(1), "padding": float64(0)}) {
		t.Error("statusLine command differs from contract")
	}
	const statusEnv = "COPILOT_CODESPACE_STATUS_FILE"
	if launch.HostEnv[statusEnv] != statusPath || launch.HostEnv["COPILOT_HOME"] != profile || launch.ProcessEnv["COPILOT_HOME"] != profile {
		t.Fatal("private profile/status environment missing")
	}
	data, err = os.ReadFile(launch.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest extensionSessionManifest
	if err := json.Unmarshal(data, &manifest); err != nil || manifest.Env[statusEnv] != statusPath || manifest.Env["COPILOT_HOME"] != profile {
		t.Fatal("manifest dropped profile/status environment")
	}
}

func TestRemoteUIProfileFailuresSurface(t *testing.T) {
	for _, mode := range []string{"fail", "malformed", "invalid-original", "invalid-group"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
			root := t.TempDir()
			t.Setenv("COPILOT_HOME", root)
			installRemoteUIConfigWriter(t, mode)
			original := "{}"
			if mode == "invalid-original" {
				original = "{broken"
			}
			if mode == "invalid-group" {
				original = `{"footer":false}`
			}
			if err := os.WriteFile(filepath.Join(root, "settings.json"), []byte(original), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := prepareRemoteNativeLaunch("/self", nativeTestRegistry(t), mcp.LifecycleConfig{}, launcherOptions{remoteNative: true})
			if err == nil {
				t.Fatal("invalid configuration silently replaced with defaults")
			}
			if !strings.Contains(err.Error(), "settings") || strings.Contains(err.Error(), "private fixture credential") {
				t.Fatalf("error must identify settings failure without exposing writer output: %v", err)
			}
			data, readErr := os.ReadFile(filepath.Join(root, "settings.json"))
			if readErr != nil || string(data) != original {
				t.Fatal("failure changed original settings")
			}
		})
	}
}

func TestRemoteUIProfileUsesGhFallback(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("COPILOT_HOME", "")
	installRemoteUIConfigWriter(t, "gh")
	bin := os.Getenv("PATH")
	if err := os.Rename(filepath.Join(bin, "copilot"), filepath.Join(bin, "gh")); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareRemoteNativeLaunch("/self", nativeTestRegistry(t), mcp.LifecycleConfig{}, launcherOptions{remoteNative: true}); err != nil {
		t.Fatal(err)
	}
}

func TestRemoteUIProfileSnapshotsSymlinkedSettingsFromRelativeHome(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	root := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(cwd, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("COPILOT_HOME", relative)
	installRemoteUIConfigWriter(t, "")
	externalSettings := filepath.Join(t.TempDir(), "original-settings.json")
	original := `{"theme":"unchanged"}`
	if err := os.WriteFile(externalSettings, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(externalSettings, filepath.Join(root, "settings.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "mcp-config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	launch, err := prepareRemoteNativeLaunch("/self", nativeTestRegistry(t), mcp.LifecycleConfig{}, launcherOptions{remoteNative: true})
	if err != nil {
		t.Fatal(err)
	}
	profile := launch.ProcessEnv["COPILOT_HOME"]
	info, err := os.Lstat(filepath.Join(profile, "settings.json"))
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("settings snapshot is not a private regular file: %v, %v", info, err)
	}
	target, err := os.Readlink(filepath.Join(profile, "mcp-config.json"))
	if err != nil || target != filepath.Join(root, "mcp-config.json") {
		t.Fatalf("relative original root did not become an absolute target: %q, %v", target, err)
	}
	data, err := os.ReadFile(externalSettings)
	if err != nil || string(data) != original {
		t.Fatal("original symlink target changed")
	}
}

func TestRemoteUIProfileConfigTimeout(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("COPILOT_HOME", "")
	installRemoteUIConfigWriter(t, "timeout")
	_, err := prepareRemoteNativeLaunch("/self", nativeTestRegistry(t), mcp.LifecycleConfig{}, launcherOptions{remoteNative: true})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("config writer did not surface its bounded timeout: %v", err)
	}
}
