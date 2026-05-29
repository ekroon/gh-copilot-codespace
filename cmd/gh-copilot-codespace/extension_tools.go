package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ekroon/gh-copilot-codespace/internal/mcp"
	"github.com/ekroon/gh-copilot-codespace/internal/registry"
)

const codespaceExtensionTokenEnv = "COPILOT_CODESPACE_EXTENSION_TOKEN"
const codespaceExtensionManifestEnv = "COPILOT_CODESPACE_EXTENSION_MANIFEST"
const codespaceExtensionModeEnv = "COPILOT_CODESPACE_EXTENSION_MODE"
const userExtensionName = "copilot-codespace"
const legacyGeneratedUserExtensionPrefix = "copilot-codespace-"
const generatedUserExtensionMaxAge = 24 * time.Hour

type extensionSessionManifest struct {
	SelfBinary string            `json:"selfBinary"`
	Env        map[string]string `json:"env"`
	Token      string            `json:"token"`
	Mode       string            `json:"mode,omitempty"`
}

func extensionSessionToken() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", os.Getpid())
	}
	return hex.EncodeToString(b[:])
}

func extensionHostEnv(reg *registry.Registry, lifecycleCfg mcp.LifecycleConfig, mode string) map[string]string {
	var entries []registryEntry
	for _, cs := range reg.All() {
		entries = append(entries, registryEntry{
			Alias:      cs.Alias,
			Name:       cs.Name,
			Repository: cs.Repository,
			Branch:     cs.Branch,
			Workdir:    cs.Workdir,
		})
	}
	registryJSON, _ := json.Marshal(entries)
	env := map[string]string{
		"CODESPACE_REGISTRY": string(registryJSON),
	}
	if lifecycleCfg.LocalWorkdir != "" {
		env[codespaceLocalWorkdirEnv] = lifecycleCfg.LocalWorkdir
	}
	if lifecycleJSON := lifecycleConfigEnvJSON(lifecycleCfg); lifecycleJSON != "" {
		env[codespaceLifecycleConfigEnv] = lifecycleJSON
	}
	if mode != "" {
		env[codespaceExtensionModeEnv] = mode
	}
	return env
}

func writeExtensionSessionManifest(root, selfBinary string, env map[string]string, token, mode string) (string, error) {
	dir := filepath.Join(root, ".copilot-codespace")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("creating extension manifest dir: %w", err)
	}
	path := filepath.Join(dir, "extension-session-"+token+".json")
	data, err := json.MarshalIndent(extensionSessionManifest{
		SelfBinary: selfBinary,
		Env:        env,
		Token:      token,
		Mode:       mode,
	}, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshaling extension manifest: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", fmt.Errorf("writing extension manifest: %w", err)
	}
	return path, nil
}

func generateProjectExtension(root string) error {
	extDir := filepath.Join(root, ".github", "extensions", "copilot-codespace")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		return fmt.Errorf("creating project extension dir: %w", err)
	}
	return os.WriteFile(filepath.Join(extDir, "extension.mjs"), []byte(extensionSource()), 0o644)
}

func installUserExtension() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("getting home dir: %w", err)
	}
	root := filepath.Join(home, ".copilot", "extensions")
	if err := cleanupLegacyGeneratedUserExtensions(root, time.Now()); err != nil {
		return err
	}
	extDir := filepath.Join(root, userExtensionName)
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		return fmt.Errorf("creating user extension dir: %w", err)
	}
	return os.WriteFile(filepath.Join(extDir, "extension.mjs"), []byte(extensionSource()), 0o644)
}

func cleanupLegacyGeneratedUserExtensions(root string, now time.Time) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading user extension dir: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, legacyGeneratedUserExtensionPrefix) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("stat generated user extension %q: %w", name, err)
		}
		if now.Sub(info.ModTime()) < generatedUserExtensionMaxAge {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, name)); err != nil {
			return fmt.Errorf("removing stale generated user extension %q: %w", name, err)
		}
	}
	return nil
}

func extensionSource() string {
	return fmt.Sprintf(`import { joinSession } from "@github/copilot-sdk/extension";
import { spawn } from "node:child_process";
import { readFileSync } from "node:fs";

const token = process.env.%s;
const manifestPath = process.env.%s;

function loadManifest() {
  if (!token || !manifestPath) return undefined;
  try {
    const manifest = JSON.parse(readFileSync(manifestPath, "utf8"));
    if (manifest?.token !== token) return undefined;
    if (typeof manifest?.selfBinary !== "string" || !manifest.selfBinary) return undefined;
    if (typeof manifest?.env !== "object" || manifest.env === null) return undefined;
    return manifest;
  } catch {
    return undefined;
  }
}

const manifest = loadManifest();

if (!manifest) {
  await joinSession({ tools: [] });
} else {
  const host = spawn(manifest.selfBinary, ["extension-host"], {
    stdio: ["pipe", "pipe", "inherit"],
    env: { ...process.env, ...manifest.env },
  });

  let nextId = 1;
  let buffer = "";
  const pending = new Map();

  host.stdout.setEncoding("utf8");
  host.stdout.on("data", (chunk) => {
    buffer += chunk;
    for (;;) {
      const newline = buffer.indexOf("\n");
      if (newline < 0) break;
      const line = buffer.slice(0, newline).trim();
      buffer = buffer.slice(newline + 1);
      if (!line) continue;
      let message;
      try {
        message = JSON.parse(line);
      } catch (error) {
        continue;
      }
      const waiter = pending.get(message.id);
      if (!waiter) continue;
      pending.delete(message.id);
      if (message.error) {
        waiter.reject(new Error(String(message.error)));
      } else {
        waiter.resolve(message.result);
      }
    }
  });

  host.on("exit", (code, signal) => {
    const error = new Error("extension host exited (" + (code ?? signal ?? "unknown") + ")");
    for (const waiter of pending.values()) {
      waiter.reject(error);
    }
    pending.clear();
  });

  function request(method, payload = {}) {
    const id = nextId++;
    const message = { id, method, ...payload };
    return new Promise((resolve, reject) => {
      pending.set(id, { resolve, reject });
      host.stdin.write(JSON.stringify(message) + "\n", (error) => {
        if (error) {
          pending.delete(id);
          reject(error);
        }
      });
    });
  }

  const bootstrap = await request("list_tools");
  // The Go side returns { tools, systemMessage?, customAgents? }. Older builds
  // returned a bare array of tool definitions; tolerate that shape too so a
  // mid-rollout binary skew never breaks the extension.
  const definitions = Array.isArray(bootstrap)
    ? bootstrap
    : Array.isArray(bootstrap?.tools)
      ? bootstrap.tools
      : [];
  const systemMessage = Array.isArray(bootstrap) ? undefined : bootstrap?.systemMessage;
  const customAgents = Array.isArray(bootstrap) ? undefined : bootstrap?.customAgents;
  const tools = definitions.map((definition) => ({
    name: definition.name,
    description: definition.description,
    parameters: definition.parameters,
    handler: async (args) => {
      try {
        return await request("call_tool", { tool: definition.name, args });
      } catch (error) {
        return {
          textResultForLlm: String(error?.message || error),
          resultType: "failure",
        };
      }
    },
  }));

  const sessionConfig = { tools };
  if (systemMessage && typeof systemMessage.content === "string" && systemMessage.content.length > 0) {
    sessionConfig.systemMessage = systemMessage;
  }
  if (Array.isArray(customAgents) && customAgents.length > 0) {
    sessionConfig.customAgents = customAgents;
  }

  await joinSession(sessionConfig);
}
`, codespaceExtensionTokenEnv, codespaceExtensionManifestEnv)
}
