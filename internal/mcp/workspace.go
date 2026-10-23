package mcp

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ekroon/gh-copilot-codespace/internal/registry"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

// WorkspaceConfig explicitly selects remote authority and optional local output.
// SkillRoots are remote paths, never local or ambient skill search directories.
type WorkspaceConfig struct {
	Alias       string   `json:"alias"`
	ArtifactDir string   `json:"artifactDir"`
	SkillRoots  []string `json:"skillRoots,omitempty"`
}

// WorkspaceRuntime adapts ordinary tool names to the existing remote handlers.
// Path checks prevent accidental target/cwd confusion; they are not a remote
// shell sandbox. Host permission/sandbox policy is not automatically enforced
// inside a Codespace. Explicit remote_* tools retain their original semantics.
type WorkspaceRuntime struct {
	runtime  *ToolRuntime
	reg      *registry.Registry
	primary  *registry.ManagedCodespace
	alias    string
	workdir  string
	artifact string
	roots    []string
	mu       sync.Mutex
	sessions map[string]bool
}

var workspaceOrdinary = map[string]string{
	"view": "remote_view", "edit": "remote_edit", "create": "remote_create",
	"apply_patch": "remote_apply_patch", "bash": "remote_bash", "rg": "remote_grep",
	"glob": "remote_glob", "read_bash": "remote_read_bash", "write_bash": "remote_write_bash",
	"stop_bash": "remote_stop_bash", "list_bash": "remote_list_bash",
}

var workspaceIdentifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

var workspaceToolDescriptions = strings.NewReplacer(
	"remote_read_bash", "read_bash", "remote_write_bash", "write_bash",
	"remote_stop_bash", "stop_bash", "remote_list_bash", "list_bash",
	"remote_apply_patch", "apply_patch", "remote_bash", "bash",
	"remote_view", "view", "remote_edit", "edit", "remote_create", "create",
	"remote_grep", "rg", "remote_glob", "glob",
)

// NewWorkspaceRuntime pins the selected connection and its startup workdir.
// Removing/replacing it fails closed, even if another target remains connected.
// An empty ArtifactDir disables all local file access; it never means local cwd.
func NewWorkspaceRuntime(reg *registry.Registry, cfg LifecycleConfig, workspace WorkspaceConfig) (*WorkspaceRuntime, error) {
	if reg == nil || !registry.ValidAlias(workspace.Alias) {
		return nil, fmt.Errorf("workspace requires an explicit valid codespace alias")
	}
	primary, err := reg.Resolve(workspace.Alias)
	if err != nil {
		return nil, err
	}
	if primary.Executor == nil || !path.IsAbs(primary.Workdir) || path.Clean(primary.Workdir) == "/" {
		return nil, fmt.Errorf("workspace requires a connected executor and an absolute non-root remote workdir")
	}
	r := &WorkspaceRuntime{
		reg: reg, primary: primary, alias: workspace.Alias, workdir: path.Clean(primary.Workdir),
		sessions: make(map[string]bool),
	}
	for _, root := range workspace.SkillRoots {
		if !path.IsAbs(root) || path.Clean(root) == "/" || invalidWorkspacePath(root) {
			return nil, fmt.Errorf("skill root %q must be an absolute bounded remote directory without traversal", root)
		}
		r.roots = append(r.roots, path.Clean(root))
	}
	if workspace.ArtifactDir != "" {
		if !filepath.IsAbs(workspace.ArtifactDir) || filepath.Clean(workspace.ArtifactDir) == string(filepath.Separator) {
			return nil, fmt.Errorf("artifactDir must be an absolute non-root local output directory")
		}
		root, err := resolveLocalRoot(workspace.ArtifactDir)
		if err != nil {
			return nil, err
		}
		info, err := os.Stat(root)
		if err != nil || !info.IsDir() {
			return nil, fmt.Errorf("artifactDir must be an existing directory")
		}
		localRoot, err := resolveLocalRoot(cfg.LocalWorkdir)
		if err != nil {
			return nil, err
		}
		if root == string(filepath.Separator) || localPathsOverlap(root, localRoot) {
			return nil, fmt.Errorf("artifactDir must not overlap the local checkout %q", localRoot)
		}
		r.artifact = root
	}
	// The existing copy bridge is rooted ONLY in explicit artifacts in this mode.
	cfg.LocalWorkdir = r.artifact
	r.runtime = NewToolRuntime(reg, cfg)
	r.installWorkspaceTools()
	return r, nil
}

func localPathsOverlap(a, b string) bool {
	within := func(root, value string) bool {
		rel, err := filepath.Rel(root, value)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
	}
	return within(a, b) || within(b, a)
}

func (r *WorkspaceRuntime) Definitions() []ToolDefinition {
	defs := r.runtime.Definitions()
	for i := range defs {
		_, defs[i].OverridesBuiltInTool = workspaceOrdinary[defs[i].Name]
	}
	return defs
}

func workspaceFailure(err error) (RuntimeCallResult, error) {
	return RuntimeCallResult{ResultType: "failure", TextResultForLlm: toolErrorMessage(err)}, nil
}

func (r *WorkspaceRuntime) connectedPrimary() error {
	primary, err := r.reg.Resolve(r.alias)
	if err != nil {
		return fmt.Errorf("pinned workspace unavailable: %w", err)
	}
	if primary != r.primary || primary.Executor == nil {
		return fmt.Errorf("pinned workspace %q was replaced; start a new workspace session to select a target", r.alias)
	}
	return nil
}

func (r *WorkspaceRuntime) Call(ctx context.Context, name string, args map[string]any) (RuntimeCallResult, error) {
	remoteName, ordinary := workspaceOrdinary[name]
	if !ordinary {
		return r.runtime.Call(ctx, name, args)
	}
	if err := r.connectedPrimary(); err != nil {
		return workspaceFailure(err)
	}
	normalized, err := r.ordinaryArgs(name, args)
	if err != nil {
		return workspaceFailure(err)
	}
	result, err := r.runtime.Call(ctx, remoteName, normalized)
	if name == "bash" && err == nil && result.ResultType == "success" && normalized["mode"] != "async" {
		// Rewrite only the handler's exact trailing continuation envelope for
		// this call's shellId, never occurrences in command output or other tools.
		id := normalized["shellId"].(string)
		suffix := fmt.Sprintf("\n\n[shellId: %s — use remote_read_bash to check for more output]", id)
		if strings.HasSuffix(result.TextResultForLlm, suffix) {
			result.TextResultForLlm = strings.TrimSuffix(result.TextResultForLlm, suffix) +
				fmt.Sprintf("\n\n[shellId: %s — use read_bash to check for more output]", id)
		}
	}
	if name == "stop_bash" && err == nil && result.ResultType == "success" {
		r.mu.Lock()
		delete(r.sessions, normalized["shellId"].(string))
		r.mu.Unlock()
	}
	return result, err
}

func invalidWorkspacePath(value string) bool {
	if strings.ContainsAny(value, "\x00\r\n\\") || strings.Contains(value, "://") {
		return true
	}
	for _, part := range strings.Split(value, "/") {
		if part == ".." {
			return true
		}
	}
	return false
}

func (r *WorkspaceRuntime) remotePath(value, base string) (string, error) {
	if strings.TrimSpace(value) == "" || invalidWorkspacePath(value) {
		return "", fmt.Errorf("invalid remote workspace path %q", value)
	}
	resolved := path.Clean(value)
	if !path.IsAbs(resolved) {
		resolved = path.Join(base, resolved)
	}
	if resolved != r.workdir && !strings.HasPrefix(resolved, r.workdir+"/") {
		return "", fmt.Errorf("path %q is outside pinned remote workdir %q; use explicit remote_* tools for other remote paths", value, r.workdir)
	}
	return resolved, nil
}

func (r *WorkspaceRuntime) ordinaryArgs(name string, args map[string]any) (map[string]any, error) {
	out := make(map[string]any, len(args)+2)
	for key, value := range args {
		out[key] = value
	}
	if value, ok := out["codespace"]; ok && value != r.alias {
		return nil, fmt.Errorf("ordinary tools are pinned to codespace %q; use remote_* for explicit secondary targets", r.alias)
	}
	out["codespace"] = r.alias
	cwd := r.workdir
	if value, ok := out["cwd"]; ok {
		text, valid := value.(string)
		if !valid {
			return nil, fmt.Errorf("cwd must be a remote path string")
		}
		var err error
		cwd, err = r.remotePath(text, r.workdir)
		if err != nil {
			return nil, err
		}
	}
	out["cwd"] = cwd // Never inherit remote_cd's mutable executor cwd.
	if value, ok := out["path"]; ok {
		text, valid := value.(string)
		if !valid {
			return nil, fmt.Errorf("path must be a remote path string")
		}
		resolved, err := r.remotePath(text, cwd)
		if err != nil {
			return nil, err
		}
		out["path"] = resolved
	}
	if name == "rg" || name == "glob" {
		if value, ok := out["paths"]; ok {
			if value == nil {
				return nil, fmt.Errorf("paths must be a remote path or array, not null")
			}
			paths, err := optionalPathsArg(makeWorkspaceRequest(out), "paths")
			if err != nil {
				return nil, err
			}
			normalized := make([]string, 0, len(paths))
			for _, value := range paths {
				resolved, err := r.remotePath(value, cwd)
				if err != nil {
					return nil, err
				}
				normalized = append(normalized, resolved)
			}
			out["paths"] = normalized
		}
	}
	if name == "apply_patch" {
		patchText, ok := out["patch"].(string)
		if !ok {
			return nil, fmt.Errorf("patch must be a canonical apply_patch string")
		}
		for _, line := range strings.Split(strings.ReplaceAll(patchText, "\r\n", "\n"), "\n") {
			for _, prefix := range []string{"*** Add File: ", "*** Update File: ", "*** Delete File: ", "*** Move to: "} {
				if strings.HasPrefix(line, prefix) {
					if _, err := r.remotePath(strings.TrimSpace(strings.TrimPrefix(line, prefix)), cwd); err != nil {
						return nil, err
					}
				}
			}
		}
	}
	if name == "bash" {
		if mode, ok := out["mode"]; ok && mode != "sync" && mode != "async" {
			return nil, fmt.Errorf("mode must be sync or async")
		}
		if detach, ok := out["detach"]; ok && detach != false {
			return nil, fmt.Errorf("remote detached sessions are not supported by this tool")
		}
		id := fmt.Sprintf("workspace-%d-%d", time.Now().UnixMilli(), remoteShellIDSequence.Add(1))
		if value, ok := out["shellId"]; ok {
			var valid bool
			id, valid = value.(string)
			if !valid || !workspaceIdentifier.MatchString(id) {
				return nil, fmt.Errorf("invalid workspace shellId")
			}
		}
		r.mu.Lock()
		exists := r.sessions[id]
		if !exists {
			r.sessions[id] = true
		}
		r.mu.Unlock()
		if exists {
			return nil, fmt.Errorf("workspace shellId %q is already reserved; use read_bash/write_bash", id)
		}
		out["shellId"] = id
	}
	if name == "read_bash" || name == "write_bash" || name == "stop_bash" {
		id, ok := out["shellId"].(string)
		if !ok || !workspaceIdentifier.MatchString(id) {
			return nil, fmt.Errorf("invalid workspace shellId")
		}
		r.mu.Lock()
		owned := r.sessions[id]
		r.mu.Unlock()
		if !owned {
			return nil, fmt.Errorf("shellId %q was not started by this workspace's ordinary bash tool; use explicit remote_* tools for other sessions", id)
		}
	}
	return out, nil
}

func makeWorkspaceRequest(args map[string]any) mcpsdk.CallToolRequest {
	return mcpsdk.CallToolRequest{Params: mcpsdk.CallToolParams{Arguments: args}}
}

func (r *WorkspaceRuntime) installWorkspaceTools() {
	original := append([]runtimeTool(nil), r.runtime.tools...)
	// Stable order follows existing remote tool definitions, not map iteration.
	for _, t := range original {
		for name, remoteName := range workspaceOrdinary {
			if t.tool.Name != remoteName {
				continue
			}
			tool := t.tool
			tool.Name = name
			tool.Description = fmt.Sprintf("Remote-native %s on pinned Codespace %q, default cwd %s. Paths must be within that remote workdir. No local fallback. Host sandbox restrictions do not automatically apply to remote shell commands. %s", name, r.alias, r.workdir, workspaceToolDescriptions.Replace(t.tool.Description))
			tool.InputSchema.Properties = make(map[string]any, len(t.tool.InputSchema.Properties))
			for key, value := range t.tool.InputSchema.Properties {
				if key != "codespace" {
					tool.InputSchema.Properties[key] = workspaceSchemaDescriptions(value)
				}
			}
			if name == "bash" {
				tool.Description = fmt.Sprintf(
					"Ordinary model-facing bash shell tool implemented by this extension. "+
						"Executes the supplied command on the pinned Codespace alias %q, target %q, not on the local host. "+
						"Default cwd is %s; an explicit cwd must remain within that remote workspace. "+
						"Use this ordinary tool name for the selected workspace; no explicit remote-tool substitution is required. "+
						"This tool runs arbitrary shell commands: they may read, write, or delete remote files, run Git, access the network, "+
						"and use the remote process environment and credentials available on the target. "+
						"Normal permission, trust, and sandbox controls remain unchanged; host filesystem sandbox restrictions do not automatically enforce remote shell restrictions. "+
						"Default sync mode starts one non-PTY process, waits up to initial_wait, and retains the same process under a shellId if still running. "+
						"Successful mode 'async' supports stdin, PTY interaction and always returns a shellId. "+
						"Use read_bash/write_bash to continue sessions, stop_bash to stop them, and list_bash to inspect sessions.",
					r.alias, r.primary.Name, r.workdir)
				tool.InputSchema.Properties["cwd"] = map[string]any{
					"type": "string",
					"description": fmt.Sprintf("Remote working directory within %s on Codespace alias %q (target %q); defaults to %s, never the local host cwd.",
						r.workdir, r.alias, r.primary.Name, r.workdir),
				}
			}
			r.runtime.tools = append(r.runtime.tools, runtimeTool{tool: tool})
		}
	}
	for _, t := range r.artifactTools() {
		r.runtime.tools = append(r.runtime.tools, t)
		r.runtime.handlers[t.tool.Name] = t.handler
	}
	skill := runtimeTool{tool: r.skillTool(), handler: r.skillHandler()}
	r.runtime.tools = append(r.runtime.tools, skill)
	r.runtime.handlers[skill.tool.Name] = skill.handler
	for i := range r.runtime.tools {
		if r.runtime.tools[i].tool.Name == "remote_copy" {
			r.runtime.tools[i].tool.Description = "Explicit one-file copy between a cs://<alias>/<path> remote endpoint and ArtifactDir ONLY, never the local checkout. Destination overwrite requires overwrite=true."
		}
	}
	r.runtime.handlers["remote_copy"] = r.artifactCopyHandler()
}

// Clone schema metadata before adapting descriptions so explicit remote tool
// schemas keep their original follow-up names. Non-description strings (such as
// enum values) are left untouched.
func workspaceSchemaDescriptions(value any) any {
	switch value := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(value))
		for key, item := range value {
			if description, ok := item.(string); key == "description" && ok {
				out[key] = workspaceToolDescriptions.Replace(description)
			} else {
				out[key] = workspaceSchemaDescriptions(item)
			}
		}
		return out
	case []any:
		out := make([]any, len(value))
		for i, item := range value {
			out[i] = workspaceSchemaDescriptions(item)
		}
		return out
	default:
		return value
	}
}
