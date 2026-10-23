package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ekroon/gh-copilot-codespace/internal/registry"
	"github.com/ekroon/gh-copilot-codespace/internal/ssh"
)

// Optional integration check against the already-deployed, unmodified helper.
// The entire test (including these temporary fixtures) runs in the Codespace.
type workspaceHelperExecutor struct {
	*mockExecutor
	helper    string
	forceGlob string
}

func (e *workspaceHelperExecutor) invoke(ctx context.Context, op string, req, result any) error {
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, e.helper, "filesystem", op)
	cmd.Stdin = bytes.NewReader(data)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", op, err, stderr.String())
	}
	return json.Unmarshal(output, result)
}

func (e *workspaceHelperExecutor) GlobFiles(ctx context.Context, req ssh.GlobRequest) (ssh.GlobResult, error) {
	if e.forceGlob != "" {
		return ssh.GlobResult{Output: e.forceGlob}, nil
	}
	var result ssh.GlobResult
	err := e.invoke(ctx, "glob", req, &result)
	return result, err
}

func (e *workspaceHelperExecutor) ReadFileRooted(ctx context.Context, req ssh.RootedReadRequest) ([]byte, error) {
	var result struct {
		Data []byte `json:"data"`
	}
	err := e.invoke(ctx, "read", req, &result)
	return result.Data, err
}

func TestWorkspaceDeployedHelperSkills(t *testing.T) {
	helper := os.Getenv("CODESPACE_WORKSPACE_TEST_HELPER")
	if helper == "" {
		t.Skip("set CODESPACE_WORKSPACE_TEST_HELPER to an existing deployed helper to run integration checks")
	}
	root := t.TempDir()
	skillDir := filepath.Join(root, "explicit")
	if err := os.Mkdir(skillDir, 0o700); err != nil {
		t.Fatal(err)
	}
	skill := []byte("---\nname: live-proof\ndescription: Remote explicit skill\n---\nEXPLICIT_ONLY_PROOF\n")
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), skill, 0o600); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "SKILL.md"), []byte("---\nname: ambient\ndescription: Must not load\n---\nOUTSIDE_PROOF\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	executor := &workspaceHelperExecutor{mockExecutor: &mockExecutor{}, helper: helper}
	reg := registry.New()
	if err := reg.Register(&registry.ManagedCodespace{Alias: "primary", Name: "live", Workdir: "/workspaces/gh-copilot-codespace", Executor: executor}); err != nil {
		t.Fatal(err)
	}
	for _, configured := range []string{root, skillDir} {
		r, err := NewWorkspaceRuntime(reg, LifecycleConfig{}, WorkspaceConfig{Alias: "primary", SkillRoots: []string{configured}})
		if err != nil {
			t.Fatal(err)
		}
		result := workspaceCall(t, r, "workspace_skill", map[string]any{}, true)
		if !strings.Contains(result.TextResultForLlm, "live-proof") || strings.Contains(result.TextResultForLlm, "ambient") {
			t.Fatalf("explicit helper listing = %+v", result)
		}
		loaded := workspaceCall(t, r, "workspace_skill", map[string]any{"name": "live-proof"}, true)
		if !strings.Contains(loaded.TextResultForLlm, "EXPLICIT_ONLY_PROOF") || !strings.Contains(loaded.TextResultForLlm, skillDir) {
			t.Fatalf("explicit helper load = %+v", loaded)
		}
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	// Even if discovery reports a symlink path, the real rooted read must reject it.
	executor.forceGlob = filepath.Join(root, "escape", "SKILL.md") + "\n"
	r, err := NewWorkspaceRuntime(reg, LifecycleConfig{}, WorkspaceConfig{Alias: "primary", SkillRoots: []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	failed := workspaceCall(t, r, "workspace_skill", map[string]any{}, false)
	if strings.Contains(failed.TextResultForLlm, "OUTSIDE_PROOF") {
		t.Fatal("skill symlink leaked outside content")
	}
}
