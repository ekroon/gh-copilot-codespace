package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const remoteUIConfigTimeout = 15 * time.Second

// This is a UI settings snapshot, not a clean-room profile: host MCP, plugins,
// credentials/state, instructions, hooks and skills retain their existing
// behavior. Native session options separately disable ambient skill discovery.
func prepareRemoteNativeProfile(self, hostDir, profileDir, statusPath string) error {
	original := os.Getenv("COPILOT_HOME")
	if original == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("getting original Copilot home: %w", err)
		}
		original = filepath.Join(home, ".copilot")
	}
	original, err := filepath.Abs(original)
	if err != nil {
		return fmt.Errorf("resolving original Copilot home: %w", err)
	}
	if err := os.Mkdir(profileDir, 0o700); err != nil {
		return fmt.Errorf("creating private Copilot profile: %w", err)
	}
	data, err := os.ReadFile(filepath.Join(original, "settings.json"))
	if os.IsNotExist(err) {
		data = []byte("{}")
	} else if err != nil {
		return fmt.Errorf("reading original Copilot settings: %w", err)
	}
	settingsPath := filepath.Join(profileDir, "settings.json")
	// O_EXCL guarantees a new regular file, never a link to original settings.
	settings, err := os.OpenFile(settingsPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("creating private Copilot settings: %w", err)
	}
	_, writeErr := settings.Write(data)
	closeErr := settings.Close()
	if writeErr != nil {
		return fmt.Errorf("copying private Copilot settings: %w", writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("closing private Copilot settings: %w", closeErr)
	}
	entries, err := os.ReadDir(original)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reading original Copilot profile: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		// MCP writer ownership is bound to the profile directory's identity.
		// Each private profile must create its own binding and matching lock.
		if name == "settings.json" || name == "extensions" || name == ".mcp-writer.binding" || name == ".mcp-writer.lock" {
			continue
		}
		if err := os.Symlink(filepath.Join(original, name), filepath.Join(profileDir, name)); err != nil {
			return fmt.Errorf("linking host Copilot profile entry %q: %w", name, err)
		}
	}
	extensionRoot := filepath.Join(profileDir, "extensions")
	if err := os.Mkdir(extensionRoot, 0o700); err != nil {
		return fmt.Errorf("creating private extension root: %w", err)
	}
	originalExtensions := filepath.Join(original, "extensions")
	entries, err = os.ReadDir(originalExtensions)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reading original Copilot extensions: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == userExtensionName || strings.HasPrefix(name, legacyGeneratedUserExtensionPrefix) {
			continue
		}
		if err := os.Symlink(filepath.Join(originalExtensions, name), filepath.Join(extensionRoot, name)); err != nil {
			return fmt.Errorf("linking host Copilot extension %q: %w", name, err)
		}
	}
	if err := installExtensionAtRoot(extensionRoot); err != nil {
		return err
	}
	if err := canonicalizeRemoteUISettings(hostDir, profileDir); err != nil {
		return err
	}
	return overlayRemoteUISettings(settingsPath, self, statusPath)
}

// Let the supported CLI parse JSONC (including trailing commas) on the private
// copy only, then use strict JSON to preserve unrelated root and nested fields.
func canonicalizeRemoteUISettings(hostDir, profileDir string) error {
	executable, err := exec.LookPath("copilot")
	args := []string{"config", "--global", "footer.showDirectory", "false"}
	if err != nil {
		executable, err = exec.LookPath("gh")
		if err != nil {
			return fmt.Errorf("canonicalizing private Copilot settings: neither 'copilot' nor 'gh' found in PATH")
		}
		args = append([]string{"copilot", "--"}, args...)
	}
	ctx, cancel := context.WithTimeout(context.Background(), remoteUIConfigTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.Dir = hostDir
	// os/exec applies the last value for a duplicate environment key.
	cmd.Env = append(os.Environ(), "COPILOT_HOME="+profileDir)
	// Do not echo CLI output: errors may include user settings or credentials.
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("canonicalizing private Copilot settings: %w", ctx.Err())
		}
		return fmt.Errorf("canonicalizing private Copilot settings: %w", err)
	}
	return nil
}

func overlayRemoteUISettings(settingsPath, self, statusPath string) error {
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		return fmt.Errorf("reading canonical private Copilot settings: %w", err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		// Avoid including parser diagnostics containing configuration content.
		return fmt.Errorf("private Copilot settings must be a valid JSON object")
	}
	if root == nil {
		return fmt.Errorf("private Copilot settings must be a JSON object")
	}
	overlays := map[string]map[string]bool{
		"footer": {
			"showDirectory": false, "showBranch": false, "showPullRequest": false,
			"showCodeChanges": false, "showCiStatus": false, "showCustom": true,
		},
		"tabs": {"enabled": false},
		"ide":  {"autoConnect": false},
	}
	for group, fields := range overlays {
		nested := make(map[string]json.RawMessage)
		if current, ok := root[group]; ok {
			if err := json.Unmarshal(current, &nested); err != nil || nested == nil {
				return fmt.Errorf("private Copilot settings %s must be a JSON object", group)
			}
		}
		for key, value := range fields {
			nested[key], _ = json.Marshal(value)
		}
		root[group], err = json.Marshal(nested)
		if err != nil {
			return fmt.Errorf("encoding private Copilot settings %s: %w", group, err)
		}
	}
	root["statusLine"], err = json.Marshal(struct {
		Type            string `json:"type"`
		Command         string `json:"command"`
		RefreshInterval int    `json:"refreshInterval"`
		Padding         int    `json:"padding"`
	}{
		Type: "command", Command: shellQuote(self) + " native-status " + shellQuote(statusPath),
		RefreshInterval: 1, Padding: 0,
	})
	if err != nil {
		return fmt.Errorf("encoding private Copilot statusLine settings: %w", err)
	}
	data, err = json.MarshalIndent(root, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding private Copilot settings: %w", err)
	}
	// The CLI may replace its output file; reassert privacy after normalization.
	if err := os.Chmod(settingsPath, 0o600); err != nil {
		return fmt.Errorf("securing private Copilot settings: %w", err)
	}
	if err := os.WriteFile(settingsPath, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("writing private Copilot settings: %w", err)
	}
	return nil
}
