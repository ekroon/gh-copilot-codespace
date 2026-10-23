package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/ekroon/gh-copilot-codespace/internal/mcp"
	"github.com/ekroon/gh-copilot-codespace/internal/registry"
)

const codespaceWorkspaceConfigEnv = "CODESPACE_WORKSPACE_CONFIG"
const codespaceRemoteNativeEnv = "COPILOT_CODESPACE_REMOTE_NATIVE"

const nativeHostUIWarning = "REMOTE-NATIVE LIMITATION: /diff, /cwd, /ide, ! shell and @attachment UI operate on the LOCAL HOST, NOT the Codespace. UI /resume, /agent, /add-dir, /worktree, /fork and workspace switching are unsupported; relaunch instead. Tool routing is not a global shell sandbox. Configured MCP/custom tools retain their own behavior and normal permissions."

func launcherPathArg(args []string, index *int, flag string) (string, error) {
	arg := args[*index]
	if strings.HasPrefix(arg, flag+"=") {
		value := strings.TrimPrefix(arg, flag+"=")
		if value != "" {
			return value, nil
		}
	} else if *index+1 < len(args) && !strings.HasPrefix(args[*index+1], "-") {
		*index++
		return args[*index], nil
	}
	return "", fmt.Errorf("%s requires an absolute path", flag)
}

func validateRemoteNativeOptions(opts launcherOptions) error {
	if !opts.remoteNative {
		if len(opts.skillRoots) > 0 || opts.artifactDir != "" {
			return fmt.Errorf("--skill-root and --artifact-dir require --remote-native")
		}
		return nil
	}
	if opts.noCodespace || len(opts.codespaceNames) > 1 {
		return fmt.Errorf("--remote-native requires exactly one initially selected codespace")
	}
	for _, root := range opts.skillRoots {
		if !path.IsAbs(root) || strings.ContainsRune(root, 0) {
			return fmt.Errorf("--skill-root requires an absolute remote path: %q", root)
		}
	}
	if opts.artifactDir != "" && (!filepath.IsAbs(opts.artifactDir) || strings.ContainsRune(opts.artifactDir, 0)) {
		return fmt.Errorf("--artifact-dir requires an absolute local path: %q", opts.artifactDir)
	}
	for i := 0; i < len(opts.copilotArgs); i++ {
		arg := opts.copilotArgs[i]
		name, value, equals := strings.Cut(arg, "=")
		if name != "--model" && name != "--reasoning-effort" {
			return fmt.Errorf("--remote-native does not support forwarded argument %q; only --model and --reasoning-effort are supported", arg)
		}
		if !equals {
			if i+1 >= len(opts.copilotArgs) || strings.HasPrefix(opts.copilotArgs[i+1], "-") {
				return fmt.Errorf("%s requires a value", name)
			}
			i++
			value = opts.copilotArgs[i]
		}
		if value == "" {
			return fmt.Errorf("%s requires a value", name)
		}
	}
	return nil
}

func validateRemoteNativeSelection(opts launcherOptions, selected []codespace) error {
	if opts.remoteNative && len(selected) != 1 {
		return fmt.Errorf("--remote-native requires exactly one initially selected codespace (got %d)", len(selected))
	}
	return nil
}

// Set the source-aware allowlist before extension discovery. A missing or dead
// extension cannot expose built-in file/search/shell tools as a fallback.
// MCP/custom tools are deliberately retained, not claimed to be sandboxed.
func remoteNativeCopilotArgs(extra []string) []string {
	return append([]string{"--available-tools", "custom:*", "mcp:*", "builtin:task", "builtin:ask_user", "--no-custom-instructions"}, extra...)
}

func prepareRemoteNativeLaunch(self string, reg *registry.Registry, cfg mcp.LifecycleConfig, opts launcherOptions) (extensionLaunch, error) {
	if reg.Len() != 1 {
		return extensionLaunch{}, fmt.Errorf("--remote-native requires exactly one initially connected codespace")
	}
	base, err := extensionRuntimeDir()
	if err != nil {
		return extensionLaunch{}, err
	}
	base, err = filepath.Abs(base)
	if err != nil {
		return extensionLaunch{}, err
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return extensionLaunch{}, err
	}
	if err := os.Chmod(base, 0o700); err != nil {
		return extensionLaunch{}, err
	}
	base, err = filepath.EvalSymlinks(base)
	if err != nil {
		return extensionLaunch{}, err
	}
	for dir := base; ; dir = filepath.Dir(dir) {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return extensionLaunch{}, fmt.Errorf("native host directory must not be beneath a Git checkout: %s", dir)
		} else if !os.IsNotExist(err) {
			return extensionLaunch{}, err
		}
		if filepath.Dir(dir) == dir {
			break
		}
	}
	sessionDir, err := os.MkdirTemp(base, "native-session-")
	if err != nil {
		return extensionLaunch{}, err
	}
	hostDir := filepath.Join(sessionDir, "host")
	artifactDir := opts.artifactDir
	if artifactDir == "" {
		artifactDir = filepath.Join(sessionDir, "artifacts")
	}
	for _, dir := range []string{hostDir, artifactDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return extensionLaunch{}, err
		}
	}
	cfg.LocalWorkdir = hostDir
	workspace := mcp.WorkspaceConfig{
		Alias: reg.All()[0].Alias, ArtifactDir: artifactDir,
		SkillRoots: append([]string(nil), opts.skillRoots...),
	}
	// Validate before starting Copilot; never launch a native-looking legacy session.
	if _, err := mcp.NewWorkspaceRuntime(reg, cfg, workspace); err != nil {
		return extensionLaunch{}, fmt.Errorf("validating native workspace: %w", err)
	}
	data, err := json.Marshal(workspace)
	if err != nil {
		return extensionLaunch{}, err
	}
	// Snapshot UI settings in a session-specific user profile. Other host
	// configuration/state stays linked, but the owned extension has one origin
	// and the original stable extension is never rewritten by native launch.
	profileDir := filepath.Join(sessionDir, "profile")
	statusPath := filepath.Join(sessionDir, "status.json")
	if err := prepareRemoteNativeProfile(self, hostDir, profileDir, statusPath); err != nil {
		return extensionLaunch{}, err
	}
	hostEnv := extensionHostEnv(reg, cfg, "")
	hostEnv[codespaceWorkspaceConfigEnv] = string(data)
	hostEnv[codespaceRemoteNativeEnv] = "1"
	hostEnv["COPILOT_HOME"] = profileDir
	hostEnv[codespaceNativeStatusEnv] = statusPath
	token := extensionSessionToken()
	manifest, err := writeExtensionSessionManifest("", self, hostEnv, token, "")
	if err != nil {
		return extensionLaunch{}, err
	}
	return extensionLaunch{
		Token: token, ManifestPath: manifest, HostEnv: hostEnv,
		LocalWorkdir: hostDir, ArtifactDir: artifactDir,
		ProcessEnv: map[string]string{
			codespaceExtensionTokenEnv: token, codespaceExtensionManifestEnv: manifest,
			codespaceRemoteNativeEnv: "1",
			"COPILOT_HOME":           profileDir,
		},
	}, nil
}

type nativeWorkspaceWire struct {
	Alias        string   `json:"alias"`
	Repository   string   `json:"repository"`
	Branch       string   `json:"branch"`
	Workdir      string   `json:"workdir"`
	LocalWorkdir string   `json:"localWorkdir"`
	ArtifactDir  string   `json:"artifactDir"`
	SkillRoots   []string `json:"skillRoots,omitempty"`
}

func nativeBootstrap(reg *registry.Registry, cfg mcp.LifecycleConfig, workspace mcp.WorkspaceConfig) *bootstrapPayload {
	var selected *registry.ManagedCodespace
	for _, cs := range reg.All() {
		if cs.Alias == workspace.Alias {
			selected = cs
			break
		}
	}
	// NewWorkspaceRuntime validates the pinned alias before this is called.
	metadata := &nativeWorkspaceWire{
		Alias: workspace.Alias, Repository: selected.Repository, Branch: selected.Branch,
		Workdir: selected.Workdir, LocalWorkdir: cfg.LocalWorkdir,
		ArtifactDir: workspace.ArtifactDir, SkillRoots: workspace.SkillRoots,
	}
	content := fmt.Sprintf(`# Remote-native Codespace workspace

Repository source: alias %q, repository %q, branch at connection %q, remote workdir %q.
The pinned alias never changes when another Codespace is explicitly connected.
LOCAL HOST folder %q is private session plumbing, NOT a source checkout.
Local artifact root: %q. Never treat cs:// URIs as local paths.

Use ordinary view, edit, create, apply_patch, rg, glob, bash, write_bash,
read_bash, stop_bash and list_bash tools for the pinned remote source.
For another explicitly connected Codespace or paths outside the pinned workdir,
use explicit remote_* tools with the codespace alias and remote paths.
Ordinary tools do not accept cs:// URIs or switch the pinned target.
Use workspace_skill to list/load skills ONLY from the explicit remote skill roots.
No ambient local or built-in skills are enabled; an empty /skills UI is expected.
Use artifact_view, artifact_write, artifact_import and artifact_export for explicit
local artifact operations, never source fallback.
Use list_codespaces for remote metadata and @remote-explorer for source exploration.
Do not read host instructions, agents, hooks or source as remote project context.
Normal permission, trust and sandbox settings still apply. Never bypass approval.
If the extension is unavailable, STOP source work; do not fall back to local tools.

%s
`, selected.Alias, selected.Repository, selected.Branch, selected.Workdir, cfg.LocalWorkdir, workspace.ArtifactDir, nativeHostUIWarning)
	agent := customAgentWire{
		Name:        remoteExplorerAgentName,
		Description: "Explore pinned remote Codespace source using ordinary workspace tools.",
		Prompt:      content + "\nExplore and diagnose only. Read focused ranges and return concise findings.",
		Tools:       []string{"view", "rg", "glob", "bash", "write_bash", "read_bash", "stop_bash", "list_bash", "list_codespaces", "workspace_skill"},
	}
	return &bootstrapPayload{
		SystemMessage: &systemMessageWire{Mode: "append", Content: content},
		CustomAgents:  []customAgentWire{agent}, Workspace: metadata,
	}
}
