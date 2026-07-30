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
const extensionRuntimeDirName = "gh-copilot-codespace/extension-sessions"
const extensionSessionManifestPrefix = "extension-session-"
const extensionSessionManifestMaxAge = 24 * time.Hour

type extensionSessionManifest struct {
	SelfBinary string            `json:"selfBinary"`
	Env        map[string]string `json:"env"`
	Token      string            `json:"token"`
}

type extensionLaunch struct {
	Token        string
	ManifestPath string
	HostEnv      map[string]string
	ProcessEnv   map[string]string
}

func extensionSessionToken() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", os.Getpid())
	}
	return hex.EncodeToString(b[:])
}

func extensionHostEnv(reg *registry.Registry, lifecycleCfg mcp.LifecycleConfig, _ string) map[string]string {
	var entries []registryEntry
	for _, cs := range reg.All() {
		entries = append(entries, registryEntry{
			Alias:      cs.Alias,
			Name:       cs.Name,
			Repository: cs.Repository,
			Branch:     cs.Branch,
			Workdir:    cs.Workdir,
			HelperPath: cs.HelperPath,
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
	return env
}

func writeExtensionSessionManifest(_ string, selfBinary string, env map[string]string, token, _ string) (string, error) {
	dir, err := extensionRuntimeDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("creating extension manifest dir: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", fmt.Errorf("securing extension manifest dir: %w", err)
	}
	if err := cleanupStaleExtensionSessionManifests(dir, time.Now()); err != nil {
		return "", err
	}
	path := filepath.Join(dir, extensionSessionManifestPrefix+token+".json")
	data, err := json.MarshalIndent(extensionSessionManifest{
		SelfBinary: selfBinary,
		Env:        env,
		Token:      token,
	}, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshaling extension manifest: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("writing extension manifest: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		os.Remove(path)
		return "", fmt.Errorf("writing extension manifest: %w", err)
	}
	if err := file.Close(); err != nil {
		os.Remove(path)
		return "", fmt.Errorf("closing extension manifest: %w", err)
	}
	return path, nil
}

func extensionRuntimeDir() (string, error) {
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		var err error
		base, err = os.UserCacheDir()
		if err != nil {
			return "", fmt.Errorf("getting user cache dir: %w", err)
		}
	}
	return filepath.Join(base, filepath.FromSlash(extensionRuntimeDirName)), nil
}

func cleanupStaleExtensionSessionManifests(dir string, now time.Time) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("reading extension manifest dir: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), extensionSessionManifestPrefix) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("stat extension manifest %q: %w", entry.Name(), err)
		}
		if now.Sub(info.ModTime()) < extensionSessionManifestMaxAge {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
			return fmt.Errorf("removing stale extension manifest %q: %w", entry.Name(), err)
		}
	}
	return nil
}

func prepareExtensionLaunch(selfBinary string, reg *registry.Registry, lifecycleCfg mcp.LifecycleConfig) (extensionLaunch, error) {
	if err := installUserExtension(); err != nil {
		return extensionLaunch{}, err
	}
	token := extensionSessionToken()
	hostEnv := extensionHostEnv(reg, lifecycleCfg, "")
	manifestPath, err := writeExtensionSessionManifest("", selfBinary, hostEnv, token, "")
	if err != nil {
		return extensionLaunch{}, err
	}
	return extensionLaunch{
		Token:        token,
		ManifestPath: manifestPath,
		HostEnv:      hostEnv,
		ProcessEnv: map[string]string{
			codespaceExtensionTokenEnv:    token,
			codespaceExtensionManifestEnv: manifestPath,
		},
	}, nil
}

func generateProjectExtension(_ string) error {
	return installUserExtension()
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

function isToolResultContent(entry) {
  if (typeof entry !== "object" || entry === null || typeof entry.type !== "string") return false;
  switch (entry.type) {
    case "text":
    case "terminal":
      return typeof entry.text === "string";
    case "shell_exit":
      return typeof entry.shellId === "string" && typeof entry.exitCode === "number";
    case "image":
    case "audio":
      return typeof entry.data === "string" && typeof entry.mimeType === "string";
    case "resource_link":
      return typeof entry.uri === "string" && typeof entry.name === "string";
    case "resource": {
      const resource = entry.resource;
      if (typeof resource !== "object" || resource === null) return false;
      if (typeof resource.uri !== "string") return false;
      if ("mimeType" in resource && typeof resource.mimeType !== "string") return false;
      if ("text" in resource && typeof resource.text !== "string") return false;
      if ("blob" in resource && typeof resource.blob !== "string") return false;
      return "text" in resource || "blob" in resource;
    }
    default:
      return false;
  }
}

function isMcpCallToolResult(value) {
  if (typeof value !== "object" || value === null) return false;
  if (!Array.isArray(value.content)) return false;
  if ("isError" in value && typeof value.isError !== "boolean") return false;
  return value.content.every(isToolResultContent);
}

function normalizeBinaryResults(value) {
  if (!Array.isArray(value)) return [];
  const results = [];
  for (const entry of value) {
    if (typeof entry !== "object" || entry === null) continue;
    if (typeof entry.data !== "string" || !entry.data) continue;
    if (typeof entry.mimeType !== "string") continue;
    if (entry.type !== "image" && entry.type !== "resource") continue;
    const normalized = {
      data: entry.data,
      mimeType: entry.mimeType,
      type: entry.type,
    };
    if (typeof entry.description === "string") {
      normalized.description = entry.description;
    }
    results.push(normalized);
  }
  return results;
}

function isSDKToolResult(value) {
  if (typeof value !== "object" || value === null) return false;
  if (typeof value.textResultForLlm !== "string") return false;
  return ["success", "failure", "rejected", "denied", "timeout"].includes(value.resultType);
}

const truncatedResultWarning = "WARNING: The result is truncated and incomplete. Make a narrower request to retrieve complete output.";
const truncatedImageWarning = "ERROR: The image result is truncated or incomplete, so no binary image was returned. Retry with forceReadLargeFiles=true or make a narrower request.";

function normalizedStructuredKey(key) {
  return key.toLowerCase().replaceAll("_", "").replaceAll("-", "");
}

function structuredTruncation(value) {
  if (typeof value !== "object" || value === null || Array.isArray(value)) {
    return { truncated: false, image: false };
  }
  let truncated = false;
  let image = false;
  for (const [key, field] of Object.entries(value)) {
    switch (normalizedStructuredKey(key)) {
      case "truncated":
        truncated = field === true;
        break;
      case "kind":
      case "type":
        image = image || (typeof field === "string" && field.toLowerCase() === "image");
        break;
    }
  }
  return { truncated, image };
}

function prependResultWarning(text, warning) {
  if (!text) return warning;
  if (text.startsWith(warning)) return text;
  return warning + "\n\n" + text;
}

function applyStructuredTruncationPolicy(normalized, structuredContent) {
  const { truncated, image } = structuredTruncation(structuredContent);
  if (!truncated) return normalized;
  if (image) {
    normalized.resultType = "failure";
    delete normalized.binaryResultsForLlm;
    normalized.textResultForLlm = prependResultWarning(normalized.textResultForLlm, truncatedImageWarning);
    return normalized;
  }
  normalized.textResultForLlm = prependResultWarning(normalized.textResultForLlm, truncatedResultWarning);
  return normalized;
}

function normalizeSDKToolResult(result) {
  const normalized = {
    textResultForLlm: result.textResultForLlm,
    resultType: result.resultType,
  };
  const binaryResults = normalizeBinaryResults(result.binaryResultsForLlm);
  if (binaryResults.length > 0) {
    normalized.binaryResultsForLlm = binaryResults;
  }
  for (const field of ["error", "sessionLog"]) {
    if (typeof result[field] === "string") normalized[field] = result[field];
  }
  if (typeof result.toolTelemetry === "object" && result.toolTelemetry !== null && !Array.isArray(result.toolTelemetry)) {
    normalized.toolTelemetry = result.toolTelemetry;
  }
  if (Array.isArray(result.toolReferences) && result.toolReferences.every((entry) => typeof entry === "string")) {
    normalized.toolReferences = [...result.toolReferences];
  }
  return applyStructuredTruncationPolicy(normalized, result.structuredContent);
}

function convertMcpCallToolResult(result) {
  const textParts = [];
  const binaryResults = [];
  for (const entry of result.content) {
    switch (entry.type) {
      case "text":
        if (typeof entry.text === "string") {
          textParts.push(entry.text);
        }
        break;
      case "terminal":
        if (typeof entry.text === "string") {
          textParts.push(entry.text);
        }
        break;
      case "shell_exit":
      case "resource_link":
        textParts.push(JSON.stringify(entry));
        break;
      case "image":
        if (typeof entry.data === "string" && entry.data && typeof entry.mimeType === "string") {
          binaryResults.push({
            data: entry.data,
            mimeType: entry.mimeType,
            type: "image",
          });
        }
        break;
      case "audio":
        if (typeof entry.data === "string" && entry.data && typeof entry.mimeType === "string") {
          binaryResults.push({
            data: entry.data,
            mimeType: entry.mimeType,
            type: "resource",
            description: "audio",
          });
        }
        break;
      case "resource":
        if (entry.resource?.text) {
          textParts.push(entry.resource.text);
        }
        if (entry.resource?.blob) {
          const mimeType = entry.resource.mimeType;
          binaryResults.push({
            data: entry.resource.blob,
            mimeType: typeof mimeType === "string" && mimeType ? mimeType : "application/octet-stream",
            type: "resource",
            description: entry.resource.uri,
          });
        }
        break;
    }
  }
  return applyStructuredTruncationPolicy({
    textResultForLlm: textParts.join("\n"),
    resultType: result.isError ? "failure" : "success",
    ...(binaryResults.length > 0 ? { binaryResultsForLlm: binaryResults } : {}),
  }, result.structuredContent);
}

function normalizeToolResult(result) {
  if (isMcpCallToolResult(result)) {
    return convertMcpCallToolResult(result);
  }
  if (isSDKToolResult(result)) {
    return normalizeSDKToolResult(result);
  }
  return result;
}

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
        return normalizeToolResult(await request("call_tool", { tool: definition.name, args }));
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
