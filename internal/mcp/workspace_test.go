package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ekroon/gh-copilot-codespace/internal/registry"
	"github.com/ekroon/gh-copilot-codespace/internal/ssh"
)

func workspaceFixture(t *testing.T, artifacts string) (*WorkspaceRuntime, *registry.Registry, *structuredExecutorMock) {
	t.Helper()
	exec := &structuredExecutorMock{mockExecutor: &mockExecutor{}}
	reg := registry.New()
	if err := reg.Register(&registry.ManagedCodespace{Alias: "primary", Name: "primary-cs", Workdir: "/workspaces/project", Executor: exec}); err != nil {
		t.Fatal(err)
	}
	r, err := NewWorkspaceRuntime(reg, LifecycleConfig{}, WorkspaceConfig{Alias: "primary", ArtifactDir: artifacts})
	if err != nil {
		t.Fatal(err)
	}
	return r, reg, exec
}

func workspaceCall(t *testing.T, r *WorkspaceRuntime, name string, args map[string]any, success bool) RuntimeCallResult {
	t.Helper()
	result, err := r.Call(context.Background(), name, args)
	if err != nil {
		t.Fatal(err)
	}
	if (result.ResultType == "success") != success {
		t.Fatalf("%s(%v): %+v", name, args, result)
	}
	return result
}

func TestWorkspaceDefinitionsAndPinnedTarget(t *testing.T) {
	r, reg, exec := workspaceFixture(t, "")
	defs := map[string]ToolDefinition{}
	for _, d := range r.Definitions() {
		defs[d.Name] = d
		if _, ok := d.Parameters.Properties["skipPermission"]; ok {
			t.Fatalf("permission bypass in %s", d.Name)
		}
	}
	for _, name := range []string{"view", "edit", "create", "apply_patch", "bash", "rg", "glob", "read_bash", "write_bash", "stop_bash", "list_bash"} {
		if !defs[name].OverridesBuiltInTool {
			t.Fatalf("%s does not override built-in", name)
		}
		if _, ok := defs[name].Parameters.Properties["codespace"]; ok {
			t.Fatalf("%s exposes target selection", name)
		}
	}
	for _, name := range []string{"remote_view", "remote_apply_patch", "remote_copy", "connect_codespace", "workspace_skill", "artifact_view", "artifact_write", "artifact_import", "artifact_export"} {
		if defs[name].Name == "" || defs[name].OverridesBuiltInTool {
			t.Fatalf("missing or overriding explicit tool %s", name)
		}
	}
	secondary := &mockExecutor{}
	if err := reg.Register(&registry.ManagedCodespace{Alias: "secondary", Name: "secondary-cs", Workdir: "/workspaces/other", Executor: secondary}); err != nil {
		t.Fatal(err)
	}
	workspaceCall(t, r, "view", map[string]any{"path": "README.md"}, true)
	if exec.viewReq.Path != "/workspaces/project/README.md" {
		t.Fatalf("wrong path: %+v", exec.viewReq)
	}
	workspaceCall(t, r, "glob", map[string]any{"pattern": "*.go"}, true)
	if exec.globReq.Cwd != "/workspaces/project" {
		t.Fatalf("cwd = %q", exec.globReq.Cwd)
	}
	exec.SetWorkdir("/workspaces/other")
	workspaceCall(t, r, "view", map[string]any{"path": "README.md"}, true)
	workspaceCall(t, r, "rg", map[string]any{"pattern": "test"}, true)
	if exec.viewReq.Path != "/workspaces/project/README.md" || exec.grepReq.Cwd != "/workspaces/project" {
		t.Fatal("ordinary calls inherited sticky remote_cd cwd")
	}
	for _, args := range []map[string]any{
		{"path": "/Users/me/local/README.md"},
		{"path": "../other/README.md"},
		{"path": "cs://secondary/README.md"},
		{"path": "README.md", "codespace": "secondary"},
		{"path": "README.md", "codespace": 1},
	} {
		workspaceCall(t, r, "view", args, false)
	}
	for _, args := range []map[string]any{
		{"pattern": "x", "cwd": "/Users/me/local"},
		{"pattern": "x", "paths": []any{".", "../other"}},
		{"pattern": "x", "paths": nil},
	} {
		workspaceCall(t, r, "rg", args, false)
	}
	workspaceCall(t, r, "remote_create", map[string]any{"codespace": "secondary", "path": "explicit.txt", "file_text": "ok"}, true)
	if secondary.lastCreatePath != "explicit.txt" {
		t.Fatal("explicit remote tools no longer support secondary targets")
	}
	reg.Deregister("primary")
	workspaceCall(t, r, "view", map[string]any{"path": "README.md"}, false)
	if err := reg.Register(&registry.ManagedCodespace{Alias: "primary", Name: "replacement", Workdir: "/workspaces/project", Executor: secondary}); err != nil {
		t.Fatal(err)
	}
	workspaceCall(t, r, "view", map[string]any{"path": "README.md"}, false)
}

func TestWorkspaceConfigurationFailsClosed(t *testing.T) {
	r, reg, _ := workspaceFixture(t, "")
	_ = r
	for _, cfg := range []WorkspaceConfig{
		{}, {Alias: "missing"}, {Alias: "primary", ArtifactDir: "relative"},
		{Alias: "primary", ArtifactDir: "/"},
		{Alias: "primary", SkillRoots: []string{"relative"}},
		{Alias: "primary", SkillRoots: []string{"/skills/../ambient"}},
	} {
		if _, err := NewWorkspaceRuntime(reg, LifecycleConfig{}, cfg); err == nil {
			t.Fatalf("accepted config %+v", cfg)
		}
	}
	local := t.TempDir()
	artifacts := filepath.Join(local, "artifacts")
	if err := os.Mkdir(artifacts, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := NewWorkspaceRuntime(reg, LifecycleConfig{LocalWorkdir: local}, WorkspaceConfig{Alias: "primary", ArtifactDir: artifacts}); err == nil {
		t.Fatal("artifact root overlaps local checkout")
	}
	reg.Register(&registry.ManagedCodespace{Alias: "other", Name: "other-cs", Workdir: "/remote", Executor: &mockExecutor{}})
	if _, err := NewWorkspaceRuntime(reg, LifecycleConfig{}, WorkspaceConfig{}); err == nil {
		t.Fatal("ambiguous target implicitly selected")
	}
}

func TestWorkspaceGeneratedAliases(t *testing.T) {
	for _, repo := range []string{"org/.github", "org/_project", "org/-project", "org/" + strings.Repeat("a", 128)} {
		t.Run(repo, func(t *testing.T) {
			reg := registry.New()
			for _, name := range []string{"first", "second"} {
				exec := &structuredExecutorMock{mockExecutor: &mockExecutor{}}
				alias := registry.DefaultAlias(repo, reg.Aliases())
				if err := reg.Register(&registry.ManagedCodespace{
					Alias: alias, Name: name, Repository: repo,
					Workdir: "/workspaces/project", Executor: exec,
				}); err != nil {
					t.Fatal(err)
				}
				r, err := NewWorkspaceRuntime(reg, LifecycleConfig{}, WorkspaceConfig{Alias: alias})
				if err != nil {
					t.Fatalf("generated alias %q rejected: %v", alias, err)
				}
				workspaceCall(t, r, "view", map[string]any{"path": "README.md"}, true)
				if exec.viewReq.Path != "/workspaces/project/README.md" {
					t.Fatalf("generated alias %q did not route to its executor: %+v", alias, exec.viewReq)
				}
			}
		})
	}
}

func TestWorkspacePatchAndSessionRouting(t *testing.T) {
	r, reg, exec := workspaceFixture(t, "")
	patch := "*** Begin Patch\n*** Add File: file.txt\n+hello\n*** End Patch\n"
	workspaceCall(t, r, "apply_patch", map[string]any{"patch": patch}, true)
	if exec.patchReq.Patch != patch || exec.patchReq.Cwd != "/workspaces/project" {
		t.Fatalf("patch request = %+v", exec.patchReq)
	}
	for _, bad := range []string{
		strings.Replace(patch, "file.txt", "../outside", 1),
		strings.Replace(patch, "file.txt", "/Users/local", 1),
		"*** Begin Patch\n*** Update File: file.txt\n*** Move to: ../outside\n*** End Patch\n",
	} {
		workspaceCall(t, r, "apply_patch", map[string]any{"patch": bad}, false)
	}
	workspaceCall(t, r, "bash", map[string]any{"command": "true", "cwd": "/Users/local"}, false)
	workspaceCall(t, r, "bash", map[string]any{"command": "true", "shellId": "../escape"}, false)
	workspaceCall(t, r, "read_bash", map[string]any{"shellId": "secondary-session", "delay": 0.0}, false)
	workspaceCall(t, r, "bash", map[string]any{"command": "cat", "mode": "async", "shellId": "owned"}, true)
	if exec.lastStartSessionCwd != "/workspaces/project" || exec.lastSessionID != "owned" {
		t.Fatalf("session routed incorrectly: %+v", exec.mockExecutor)
	}
	secondary := &mockExecutor{readSessionResult: "secondary session output"}
	if err := reg.Register(&registry.ManagedCodespace{Alias: "secondary", Name: "secondary-cs", Workdir: "/workspaces/other", Executor: secondary}); err != nil {
		t.Fatal(err)
	}
	exec.readSessionResult = "pinned primary output"
	read := workspaceCall(t, r, "read_bash", map[string]any{"shellId": "owned", "delay": 0.0}, true)
	if read.TextResultForLlm != "pinned primary output" || secondary.readSessionCalls != 0 {
		t.Fatalf("read escaped primary: %+v; secondary reads %d", read, secondary.readSessionCalls)
	}
	workspaceCall(t, r, "write_bash", map[string]any{"shellId": "owned", "input": "hello", "delay": 0.0}, true)
	workspaceCall(t, r, "stop_bash", map[string]any{"shellId": "owned"}, true)
	workspaceCall(t, r, "read_bash", map[string]any{"shellId": "owned", "delay": 0.0}, false)
	exec.viewErr = errors.New("disconnected")
	result := workspaceCall(t, r, "view", map[string]any{"path": "README.md"}, false)
	if !strings.Contains(result.TextResultForLlm, "disconnected") {
		t.Fatalf("lost remote failure: %+v", result)
	}
}

func TestWorkspaceBashContinuationHintAndContent(t *testing.T) {
	r, reg, executor := workspaceFixture(t, "")
	stdout := "stdout mentions remote_read_bash and remote_write_bash unchanged"
	executor.readSessionResult = stdout
	continued := workspaceCall(t, r, "bash", map[string]any{
		"command": "long-running-command", "shellId": "hint-proof", "initial_wait": 0.0,
	}, true)
	want := stdout + "\n\n[shellId: hint-proof — use read_bash to check for more output]"
	if continued.TextResultForLlm != want {
		t.Fatalf("ordinary continuation = %q, want %q", continued.TextResultForLlm, want)
	}
	if err := reg.Register(&registry.ManagedCodespace{Alias: "secondary", Name: "secondary-cs", Workdir: "/workspaces/other", Executor: &mockExecutor{}}); err != nil {
		t.Fatal(err)
	}
	workspaceCall(t, r, "read_bash", map[string]any{"shellId": "hint-proof", "delay": 0.0}, true)
	explicit := workspaceCall(t, r, "remote_bash", map[string]any{
		"codespace": "primary", "command": "long-running-command", "shellId": "explicit-proof", "initial_wait": 0.0,
	}, true)
	if explicit.TextResultForLlm != stdout+"\n\n[shellId: explicit-proof — use remote_read_bash to check for more output]" {
		t.Fatalf("explicit remote continuation changed: %+v", explicit)
	}
	// A completed command's output and a viewed source file are not metadata.
	executor.readSessionResult = stdout + "\n[session exited]"
	completed := workspaceCall(t, r, "bash", map[string]any{
		"command": "completed-command", "shellId": "completed-proof", "initial_wait": 0.0,
	}, true)
	if completed.TextResultForLlm != stdout {
		t.Fatalf("completed command stdout changed: %+v", completed)
	}
	executor.viewResp = ssh.ViewResult{Content: stdout}
	viewed := workspaceCall(t, r, "view", map[string]any{"path": "source.txt"}, true)
	if viewed.TextResultForLlm != stdout {
		t.Fatalf("source content changed: %+v", viewed)
	}
	// Async mode has no handler-generated continuation suffix. Even exact-looking
	// output is command content and must remain unchanged.
	asyncStdout := "\n\n[shellId: async-proof — use remote_read_bash to check for more output]"
	executor.readSessionResult = asyncStdout
	async := workspaceCall(t, r, "bash", map[string]any{
		"command": "interactive-command", "shellId": "async-proof", "mode": "async",
	}, true)
	if async.TextResultForLlm != "Started async session: async-proof\n\n"+asyncStdout {
		t.Fatalf("async command stdout changed: %+v", async)
	}
}

func TestWorkspaceOrdinaryContinuationMetadata(t *testing.T) {
	r, _, _ := workspaceFixture(t, "")
	defs := map[string]ToolDefinition{}
	for _, definition := range r.Definitions() {
		defs[definition.Name] = definition
	}
	for _, name := range []string{"bash", "read_bash", "write_bash", "stop_bash", "list_bash"} {
		data, err := json.Marshal(defs[name])
		if err != nil {
			t.Fatal(err)
		}
		for _, explicit := range []string{"remote_bash", "remote_read_bash", "remote_write_bash", "remote_stop_bash", "remote_list_bash"} {
			if strings.Contains(string(data), explicit) {
				t.Fatalf("ordinary %s metadata still directs to %s: %s", name, explicit, data)
			}
		}
	}
	for _, name := range []string{"remote_bash", "remote_read_bash", "remote_write_bash"} {
		data, err := json.Marshal(defs[name])
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), "remote_read_bash") && name == "remote_bash" {
			t.Fatalf("explicit continuation metadata changed: %s", data)
		}
		if !strings.Contains(string(data), "remote_bash") {
			t.Fatalf("explicit metadata lost remote names: %s", data)
		}
	}
}

func TestWorkspaceBashDescription(t *testing.T) {
	r, _, _ := workspaceFixture(t, "")
	defs := map[string]ToolDefinition{}
	for _, definition := range r.Definitions() {
		defs[definition.Name] = definition
	}
	bash := defs["bash"]
	for _, clause := range []string{
		"Ordinary model-facing bash shell tool implemented by this extension",
		`alias "primary", target "primary-cs"`,
		"not on the local host",
		"Default cwd is /workspaces/project",
		"an explicit cwd must remain within that remote workspace",
		"no explicit remote-tool substitution is required",
		"arbitrary shell commands",
		"read, write, or delete remote files",
		"run Git",
		"access the network",
		"remote process environment and credentials available on the target",
		"Normal permission, trust, and sandbox controls remain unchanged",
		"host filesystem sandbox restrictions do not automatically enforce remote shell restrictions",
		"sync mode",
		"non-PTY process",
		"retains the same process under a shellId",
		"initial_wait",
		"mode 'async'",
		"stdin, PTY",
		"always returns a shellId",
		"read_bash/write_bash",
		"stop_bash",
		"list_bash",
	} {
		if !strings.Contains(bash.Description, clause) {
			t.Errorf("ordinary bash description missing %q: %s", clause, bash.Description)
		}
	}
	for _, misleading := range []string{"Replaces the local", "read-only", "safe"} {
		if strings.Contains(bash.Description, misleading) {
			t.Errorf("ordinary bash description contains %q: %s", misleading, bash.Description)
		}
	}
	if !bash.OverridesBuiltInTool {
		t.Fatal("ordinary bash lost its override classification")
	}
	if _, ok := bash.Parameters.Properties["skipPermission"]; ok {
		t.Fatal("ordinary bash exposes a permission bypass")
	}
	if _, ok := bash.Parameters.Properties["codespace"]; ok {
		t.Fatal("ordinary bash exposes target selection")
	}
	cwd, ok := bash.Parameters.Properties["cwd"].(map[string]any)
	if !ok {
		t.Fatalf("cwd schema = %#v", bash.Parameters.Properties["cwd"])
	}
	cwdDescription, _ := cwd["description"].(string)
	for _, clause := range []string{
		"within /workspaces/project",
		`alias "primary" (target "primary-cs")`,
		"defaults to /workspaces/project",
		"never the local host cwd",
	} {
		if !strings.Contains(cwdDescription, clause) {
			t.Errorf("ordinary bash cwd description missing %q: %s", clause, cwdDescription)
		}
	}
	legacy := bashTool()
	if defs["remote_bash"].Description != legacy.Description {
		t.Fatal("explicit remote_bash description changed")
	}
	gotSchema, err := json.Marshal(defs["remote_bash"].Parameters)
	if err != nil {
		t.Fatal(err)
	}
	wantSchema, err := json.Marshal(legacy.InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotSchema) != string(wantSchema) {
		t.Fatal("explicit remote_bash schema changed")
	}
}

func TestWorkspaceBashCwdRouting(t *testing.T) {
	for _, tc := range []struct {
		name string
		cwd  string
		want string
	}{
		{name: "default", want: "/workspaces/project"},
		{name: "relative", cwd: "nested", want: "/workspaces/project/nested"},
		{name: "absolute", cwd: "/workspaces/project/nested", want: "/workspaces/project/nested"},
	} {
		for _, mode := range []string{"", "sync", "async"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				r, _, exec := workspaceFixture(t, "")
				exec.SetWorkdir("/workspaces/other")
				exec.readSessionResult = "remote output\n[session exited]"
				const command = "printf 'remote cwd routing\\n'"
				args := map[string]any{
					"command": command, "shellId": "routing-proof", "initial_wait": 0.0,
				}
				if tc.cwd != "" {
					args["cwd"] = tc.cwd
				}
				if mode != "" {
					args["mode"] = mode
				}
				workspaceCall(t, r, "bash", args, true)
				if exec.startSessionCalls != 1 || exec.lastCommand != command ||
					exec.lastStartSessionCwd != tc.want || exec.lastSessionID != "routing-proof" {
					t.Fatalf("ordinary bash request routed incorrectly: calls=%d command=%q cwd=%q shellId=%q",
						exec.startSessionCalls, exec.lastCommand, exec.lastStartSessionCwd, exec.lastSessionID)
				}
			})
		}
	}
}

func TestWorkspaceArtifactsConfined(t *testing.T) {
	root := t.TempDir()
	r, _, exec := workspaceFixture(t, root)
	workspaceCall(t, r, "artifact_write", map[string]any{"path": "result.txt", "content": "first"}, true)
	got := workspaceCall(t, r, "artifact_view", map[string]any{"path": "result.txt"}, true)
	if got.TextResultForLlm != "first" {
		t.Fatalf("artifact content = %q", got.TextResultForLlm)
	}
	workspaceCall(t, r, "artifact_write", map[string]any{"path": "result.txt", "content": "second"}, false)
	workspaceCall(t, r, "artifact_write", map[string]any{"path": "result.txt", "content": "second", "overwrite": true}, true)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"../secret", filepath.Join(outside, "secret"), "link/secret"} {
		workspaceCall(t, r, "artifact_view", map[string]any{"path": path}, false)
		workspaceCall(t, r, "artifact_write", map[string]any{"path": path, "content": "bad", "overwrite": true}, false)
		workspaceCall(t, r, "remote_copy", map[string]any{"source": path, "destination": "cs://primary/file"}, false)
	}
	exec.rootedReadData = []byte("remote artifact")
	workspaceCall(t, r, "artifact_import", map[string]any{"remote_path": "build/out.txt", "path": "download.txt"}, true)
	if exec.lastRootedRead.Root != "/workspaces/project" || exec.lastRootedRead.Path != "/workspaces/project/build/out.txt" {
		t.Fatalf("import read = %+v", exec.lastRootedRead)
	}
	exec.runBashExit = 1
	workspaceCall(t, r, "artifact_export", map[string]any{"path": "download.txt", "remote_path": "upload.txt"}, true)
	if string(exec.lastRootedWrite.Data) != "remote artifact" || exec.lastRootedWrite.Root != "/workspaces/project" {
		t.Fatalf("export write = %+v", exec.lastRootedWrite)
	}
	workspaceCall(t, r, "artifact_write", map[string]any{"path": "bad", "content": "x", "overwrite": "yes"}, false)
}

type workspaceSkillExecutor struct {
	*mockExecutor
	globs []ssh.GlobRequest
	reads []ssh.RootedReadRequest
}

func (e *workspaceSkillExecutor) GlobFiles(_ context.Context, req ssh.GlobRequest) (ssh.GlobResult, error) {
	e.globs = append(e.globs, req)
	if req.Pattern != "SKILL.md" {
		return ssh.GlobResult{}, errors.New("fd-backed executor requires basename SKILL.md for portable collection discovery")
	}
	if req.Cwd == "/skills" {
		return ssh.GlobResult{Output: "alpha/SKILL.md\nbeta/SKILL.md\n"}, nil
	}
	return ssh.GlobResult{Output: "SKILL.md\n"}, nil
}

func (e *workspaceSkillExecutor) ReadFileRooted(_ context.Context, req ssh.RootedReadRequest) ([]byte, error) {
	e.reads = append(e.reads, req)
	return []byte("---\nname: example\ndescription: Explicit remote instructions\n---\nUse companion.txt beside this skill.\n"), nil
}

func TestWorkspaceSkillsExplicitOnly(t *testing.T) {
	r, reg, _ := workspaceFixture(t, "")
	workspaceCall(t, r, "workspace_skill", map[string]any{}, true)
	workspaceCall(t, r, "workspace_skill", map[string]any{"name": "../ambient"}, false)
	workspaceCall(t, r, "workspace_skill", map[string]any{"path": "/ambient/SKILL.md"}, false)
	exec := &workspaceSkillExecutor{mockExecutor: &mockExecutor{}}
	reg.Deregister("primary")
	reg.Register(&registry.ManagedCodespace{Alias: "primary", Name: "primary", Workdir: "/workspaces/project", Executor: exec})
	r, err := NewWorkspaceRuntime(reg, LifecycleConfig{}, WorkspaceConfig{Alias: "primary", SkillRoots: []string{"/skills", "/single"}})
	if err != nil {
		t.Fatal(err)
	}
	list := workspaceCall(t, r, "workspace_skill", map[string]any{}, true)
	if !strings.Contains(list.TextResultForLlm, "example") || !strings.Contains(list.TextResultForLlm, "Explicit remote instructions") {
		t.Fatalf("skill list = %+v", list)
	}
	loaded := workspaceCall(t, r, "workspace_skill", map[string]any{"name": "example"}, true)
	if !strings.Contains(loaded.TextResultForLlm, "/skills/alpha") || strings.Contains(loaded.TextResultForLlm, "/single") {
		t.Fatalf("duplicate precedence/base = %+v", loaded)
	}
	for _, req := range exec.globs {
		if req.Cwd != "/skills" && req.Cwd != "/single" {
			t.Fatalf("ambient root scanned: %+v", req)
		}
	}
	for _, req := range exec.reads {
		if req.Root != "/skills" && req.Root != "/single" {
			t.Fatalf("unrooted skill read: %+v", req)
		}
	}
	workspaceCall(t, r, "workspace_skill", map[string]any{"name": "../example"}, false)
}
