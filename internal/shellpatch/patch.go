package shellpatch

import (
	"fmt"
	"os"
	"path/filepath"
)

// patchJS is the CJS module that monkey-patches child_process.spawn to redirect
// the Copilot CLI "!" shell escape commands over SSH to the remote codespace.
//
// Detection heuristic: the "!" shell escape is the ONLY spawn call in Copilot CLI
// that uses both shell: true AND stdio: "pipe". All other internal spawns use
// "inherit" or "ignore".
//
// Required env vars (set by copilot-codespace launcher):
//
//	COPILOT_SSH_CONFIG  — path to the SSH config with ControlMaster
//	COPILOT_SSH_HOST    — the SSH host alias (e.g., "cs.develop-xxx.main")
//	CODESPACE_WORKDIR   — working directory on the codespace
const patchJS = `"use strict";

// --- Keytar mock ---
// When COPILOT_GITHUB_TOKEN is set, intercept native keytar addon loading
// to prevent macOS keychain popups (the keychain ACL only trusts the
// native copilot binary, not node).
if (process.env.COPILOT_GITHUB_TOKEN) {
  const Module = require("module");
  const _load = Module._load;
  Module._load = function(request, parent, isMain) {
    // Intercept any require of a keytar.node native addon
    if (request.endsWith("keytar.node") || request.includes("/keytar.node")) {
      return {
        getPassword: () => Promise.resolve(null),
        setPassword: () => Promise.resolve(),
        deletePassword: () => Promise.resolve(false),
        findPassword: () => Promise.resolve(null),
        findCredentials: () => Promise.resolve([]),
      };
    }
    return _load.call(this, request, parent, isMain);
  };
}

// --- Spawn redirect ---
const cp = require("child_process");
const _spawn = cp.spawn;

const sshConfig = process.env.COPILOT_SSH_CONFIG;
const sshHost = process.env.COPILOT_SSH_HOST;
const workdir = process.env.CODESPACE_WORKDIR || "/workspaces";
const mirrorDir = process.env.CODESPACE_MIRROR_DIR;

if (sshConfig && sshHost) {
  cp.spawn = function patchedSpawn(command, argsOrOpts, maybeOpts) {
    // spawn(cmd, [args], [options]) — when called without an args array,
    // the second parameter IS the options object (not an array).
    let opts = maybeOpts;
    if (argsOrOpts && !Array.isArray(argsOrOpts) && typeof argsOrOpts === "object") {
      opts = argsOrOpts;
    }

    // Detect the "!" shell escape: shell === true AND stdio === "pipe"
    if (
      opts &&
      opts.shell === true &&
      typeof command === "string"
    ) {
      const stdio = opts.stdio;
      const isPipe = stdio === "pipe" ||
        (Array.isArray(stdio) && stdio[0] === "pipe" && stdio[1] === "pipe");
      if (isPipe) {
        // Build remote command: load codespace secrets, cd to workdir, then run the user's command
        const q = (s) => "'" + s.replace(/'/g, "'\\''") + "'";
        const envLoader = "if [ -f /workspaces/.codespaces/shared/.env-secrets ]; then while IFS='=' read -r key value; do export \"$key=$(echo \"$value\" | base64 -d)\"; done < /workspaces/.codespaces/shared/.env-secrets; fi";
        const remoteCmd = envLoader + " && cd " + q(workdir) + " && " + command;

        // Replace with SSH exec — reuse the multiplexed connection
        const sshArgs = ["-F", sshConfig, "-o", "BatchMode=yes", sshHost, remoteCmd];
        const newOpts = Object.assign({}, opts, { shell: false });
        delete newOpts.cwd; // cwd is on the remote side now

        const child = _spawn.call(this, "ssh", sshArgs, newOpts);

        // Sync local branch after the shell command completes
        if (mirrorDir) {
          child.on("close", () => {
            try {
              const { execFileSync } = require("child_process");
              const branch = execFileSync("ssh", ["-F", sshConfig, "-o", "BatchMode=yes", sshHost,
                "git -C " + q(workdir) + " rev-parse --abbrev-ref HEAD"], { encoding: "utf8", timeout: 5000 }).trim();
              if (branch) {
                execFileSync("git", ["-C", mirrorDir, "symbolic-ref", "HEAD", "refs/heads/" + branch],
                  { timeout: 2000 });
              }
            } catch (_) {}
          });
        }

        return child;
      }
    }

    return _spawn.apply(this, arguments);
  };
}
`

// WritePatch writes the CJS patch to a temporary file and returns its path.
// The caller should clean up the file when done (e.g., defer os.Remove).
func WritePatch() (string, error) {
	dir, err := os.MkdirTemp("", "copilot-shell-patch-*")
	if err != nil {
		return "", fmt.Errorf("creating temp dir: %w", err)
	}

	path := filepath.Join(dir, "patch.cjs")
	if err := os.WriteFile(path, []byte(patchJS), 0o644); err != nil {
		return "", fmt.Errorf("writing patch: %w", err)
	}

	return path, nil
}
