package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ekroon/gh-copilot-codespace/internal/registry"
	"github.com/ekroon/gh-copilot-codespace/internal/ssh"
)

func TestBuildMCPConfig(t *testing.T) {
	result := buildMCPConfig("/usr/local/bin/self", "my-codespace", "/workspaces/repo", nil, "", false)

	var parsed map[string]any
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("buildMCPConfig returned invalid JSON: %v", err)
	}

	servers, ok := parsed["mcpServers"].(map[string]any)
	if !ok {
		t.Fatal("missing mcpServers key")
	}
	cs, ok := servers["codespace"].(map[string]any)
	if !ok {
		t.Fatal("missing mcpServers.codespace key")
	}

	if got := cs["command"]; got != "/usr/local/bin/self" {
		t.Errorf("command = %v, want /usr/local/bin/self", got)
	}

	args, ok := cs["args"].([]any)
	if !ok || len(args) != 1 || args[0] != "mcp" {
		t.Errorf("args = %v, want [mcp]", cs["args"])
	}

	env, ok := cs["env"].(map[string]any)
	if !ok {
		t.Fatal("missing env key")
	}
	if got := env["CODESPACE_NAME"]; got != "my-codespace" {
		t.Errorf("CODESPACE_NAME = %v, want my-codespace", got)
	}
	if got := env["CODESPACE_WORKDIR"]; got != "/workspaces/repo" {
		t.Errorf("CODESPACE_WORKDIR = %v, want /workspaces/repo", got)
	}
}

func TestBuildMCPConfig_HeadlessDelegate(t *testing.T) {
	result := buildMCPConfig("/usr/local/bin/self", "my-codespace", "/workspaces/repo", nil, "", true)

	var parsed map[string]any
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("buildMCPConfig returned invalid JSON: %v", err)
	}

	servers := parsed["mcpServers"].(map[string]any)
	cs := servers["codespace"].(map[string]any)
	env := cs["env"].(map[string]any)

	if got := env["CODESPACE_ENABLE_HEADLESS_DELEGATE"]; got != "1" {
		t.Fatalf("CODESPACE_ENABLE_HEADLESS_DELEGATE = %v, want 1", got)
	}
}

func TestBuildMCPConfigWithRemoteServers(t *testing.T) {
	remoteMCP := map[string]any{
		"my-tool": map[string]any{
			"type":    "local",
			"command": "gh",
			"args":    []string{"codespace", "ssh", "-c", "cs", "--", "my-tool"},
		},
	}

	result := buildMCPConfig("/usr/local/bin/self", "cs", "/workspaces/repo", remoteMCP, "", false)

	var parsed map[string]any
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	servers := parsed["mcpServers"].(map[string]any)

	// Our server should still be present
	if _, ok := servers["codespace"]; !ok {
		t.Error("missing codespace server")
	}
	// Remote server should be merged
	if _, ok := servers["my-tool"]; !ok {
		t.Error("missing remote my-tool server")
	}
}

func TestBuildMCPConfigRemoteCannotOverrideCodespace(t *testing.T) {
	remoteMCP := map[string]any{
		"codespace": map[string]any{
			"type":    "local",
			"command": "evil",
		},
	}

	result := buildMCPConfig("/usr/local/bin/self", "cs", "/workspaces/repo", remoteMCP, "", false)

	var parsed map[string]any
	json.Unmarshal([]byte(result), &parsed)

	servers := parsed["mcpServers"].(map[string]any)
	cs := servers["codespace"].(map[string]any)

	// Should still be our binary, not the overridden one
	if got := cs["command"]; got != "/usr/local/bin/self" {
		t.Errorf("codespace command = %v, should not be overridden", got)
	}
}

func TestRewriteMCPServerForSSH(t *testing.T) {
	server := map[string]any{
		"type":    "stdio",
		"command": "/usr/local/bin/test-mcp",
		"args":    []any{"--mode", "dev"},
		"env": map[string]any{
			"API_KEY": "secret",
		},
	}

	result := rewriteMCPServerForSSH(server, "my-cs", "/workspaces/repo", "")

	if result == nil {
		t.Fatal("rewriteMCPServerForSSH returned nil")
	}

	if got := result["command"]; got != "gh" {
		t.Errorf("command = %v, want gh", got)
	}

	args, ok := result["args"].([]string)
	if !ok {
		t.Fatal("args not []string")
	}

	// Should contain: codespace ssh -c my-cs -- bash -c <remote-cmd>
	if len(args) < 6 {
		t.Fatalf("args too short: %v", args)
	}
	if args[0] != "codespace" || args[1] != "ssh" {
		t.Errorf("args should start with [codespace ssh], got %v", args[:2])
	}

	// The remote command should contain the original command
	remoteCmd := args[len(args)-1]
	if !contains(remoteCmd, "/usr/local/bin/test-mcp") {
		t.Errorf("remote command should contain original command, got %q", remoteCmd)
	}
	if !contains(remoteCmd, "--mode") {
		t.Errorf("remote command should contain args, got %q", remoteCmd)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestCleanMirrorDir(t *testing.T) {
	dir := t.TempDir()

	// Create some files and directories including .git, files/, workspace.json
	os.MkdirAll(filepath.Join(dir, ".git", "objects"), 0o755)
	os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref"), 0o644)
	os.MkdirAll(filepath.Join(dir, ".github"), 0o755)
	os.WriteFile(filepath.Join(dir, ".github", "copilot-instructions.md"), []byte("hi"), 0o644)
	os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("agents"), 0o644)
	os.MkdirAll(filepath.Join(dir, "docs"), 0o755)
	os.WriteFile(filepath.Join(dir, "docs", "AGENTS.md"), []byte("docs agents"), 0o644)
	os.MkdirAll(filepath.Join(dir, "files"), 0o755)
	os.WriteFile(filepath.Join(dir, "files", "plan.md"), []byte("my plan"), 0o644)
	os.WriteFile(filepath.Join(dir, "workspace.json"), []byte("{}"), 0o644)

	cleanMirrorDir(dir)

	// .git, files/, workspace.json should survive
	for _, name := range []string{".git/HEAD", "files/plan.md", "workspace.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s should survive cleanup", name)
		}
	}

	// Everything else should be gone
	for _, name := range []string{".github", "AGENTS.md", "docs"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Errorf("%s should have been removed", name)
		}
	}
}

func TestGenerateBranchSyncHookTracksSessionTools(t *testing.T) {
	dir := t.TempDir()
	client := ssh.NewClient("demo")

	generateBranchSyncHook(dir, "demo", "/workspaces/repo", client)

	path := filepath.Join(dir, ".github", "hooks", "branch-sync.json")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read hook file: %v", err)
	}

	if !strings.Contains(string(content), "remote_(bash|write_bash|read_bash|stop_bash)") {
		t.Fatalf("hook file does not watch session tools: %s", content)
	}
}

func TestEnsureTrustedFolder(t *testing.T) {
	// Point HOME to a temp dir so ensureTrustedFolder writes there
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	configDir := filepath.Join(tmpHome, ".copilot")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(configDir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"trusted_folders":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	dir := "/some/trusted/dir"

	// First call: should add the folder
	if err := ensureTrustedFolder(dir); err != nil {
		t.Fatalf("first call: %v", err)
	}
	assertTrustedFolders(t, configPath, []string{dir})

	// Second call: should not duplicate
	if err := ensureTrustedFolder(dir); err != nil {
		t.Fatalf("second call: %v", err)
	}
	assertTrustedFolders(t, configPath, []string{dir})
}

func assertTrustedFolders(t *testing.T, configPath string, want []string) {
	t.Helper()
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	raw, _ := config["trusted_folders"].([]any)
	var got []string
	for _, v := range raw {
		if s, ok := v.(string); ok {
			got = append(got, s)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("trusted_folders = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("trusted_folders[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestParseMCPConfigJSON_MultipleLocations(t *testing.T) {
	// Verify that parseMCPConfigJSON works for configs from all supported locations
	tests := []struct {
		name    string
		content string
		want    []string // expected server names
	}{
		{
			name:    "copilot-mcp-config",
			content: `{"mcpServers":{"db-tool":{"command":"db-mcp"}}}`,
			want:    []string{"db-tool"},
		},
		{
			name:    "vscode-mcp",
			content: `{"mcpServers":{"vscode-server":{"command":"vscode-mcp"}}}`,
			want:    []string{"vscode-server"},
		},
		{
			name:    "root-mcp",
			content: `{"mcpServers":{"root-server":{"command":"root-mcp"}}}`,
			want:    []string{"root-server"},
		},
		{
			name:    "empty-servers",
			content: `{"mcpServers":{}}`,
			want:    nil,
		},
		{
			name:    "invalid-json",
			content: `{invalid`,
			want:    nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := parseMCPConfigJSON([]byte(tc.content))
			if tc.want == nil {
				if result != nil {
					t.Errorf("expected nil, got %v", result)
				}
				return
			}
			if result == nil {
				t.Fatal("expected non-nil result")
			}
			for _, name := range tc.want {
				if _, ok := result[name]; !ok {
					t.Errorf("missing server %q", name)
				}
			}
		})
	}
}

func TestRewriteHooksForSSH(t *testing.T) {
	hooksJSON := `{
		"version": 1,
		"hooks": {
			"sessionStart": [
				{
					"type": "command",
					"bash": "echo 'started'",
					"cwd": ".",
					"timeoutSec": 10
				}
			],
			"preToolUse": [
				{
					"type": "command",
					"bash": "./scripts/policy-check.sh",
					"cwd": "scripts",
					"env": {"LOG_LEVEL": "INFO"}
				}
			]
		}
	}`

	result := rewriteHooksForSSH([]byte(hooksJSON), "my-cs", "/workspaces/repo", "")
	if result == nil {
		t.Fatal("rewriteHooksForSSH returned nil")
	}

	var parsed map[string]any
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	hooks := parsed["hooks"].(map[string]any)

	// Check sessionStart hook was rewritten
	sessionStart := hooks["sessionStart"].([]any)
	if len(sessionStart) != 1 {
		t.Fatalf("expected 1 sessionStart hook, got %d", len(sessionStart))
	}
	hook0 := sessionStart[0].(map[string]any)
	bash0 := hook0["bash"].(string)
	if !contains(bash0, "gh codespace ssh") {
		t.Errorf("sessionStart bash should contain 'gh codespace ssh', got %q", bash0)
	}
	if !contains(bash0, "my-cs") {
		t.Errorf("sessionStart bash should contain codespace name, got %q", bash0)
	}
	if !contains(bash0, "echo") {
		t.Errorf("sessionStart bash should contain original command, got %q", bash0)
	}
	// cwd should be removed (baked into SSH command)
	if _, ok := hook0["cwd"]; ok {
		t.Error("cwd should be removed from rewritten hook")
	}

	// Check preToolUse hook was rewritten
	preToolUse := hooks["preToolUse"].([]any)
	hook1 := preToolUse[0].(map[string]any)
	bash1 := hook1["bash"].(string)
	if !contains(bash1, "./scripts/policy-check.sh") {
		t.Errorf("preToolUse bash should contain original command, got %q", bash1)
	}
	// Env should be removed (baked into SSH command)
	if _, ok := hook1["env"]; ok {
		t.Error("env should be removed from rewritten hook")
	}
	// Verify env was baked into the command
	if !contains(bash1, "LOG_LEVEL") {
		t.Errorf("env vars should be baked into SSH command, got %q", bash1)
	}
}

func TestRepoBaseName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"github/github", "github"},
		{"owner/repo", "repo"},
		{"repo-only", "repo-only"},
		{"org/sub/repo", "repo"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := repoBaseName(tc.input); got != tc.want {
			t.Errorf("repoBaseName(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestChooseWorkdir(t *testing.T) {
	tests := []struct {
		name     string
		dirs     []string
		repoName string
		want     string
	}{
		{
			name:     "single dir",
			dirs:     []string{"/workspaces/github"},
			repoName: "github",
			want:     "/workspaces/github",
		},
		{
			name:     "single dir no match needed",
			dirs:     []string{"/workspaces/other"},
			repoName: "github",
			want:     "/workspaces/other",
		},
		{
			name:     "multiple dirs with match",
			dirs:     []string{"/workspaces/github-ui", "/workspaces/github"},
			repoName: "github",
			want:     "/workspaces/github",
		},
		{
			name:     "multiple dirs no match",
			dirs:     []string{"/workspaces/foo", "/workspaces/bar"},
			repoName: "github",
			want:     "",
		},
		{
			name:     "empty repo name",
			dirs:     []string{"/workspaces/foo", "/workspaces/bar"},
			repoName: "",
			want:     "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := chooseWorkdir(tc.dirs, tc.repoName)
			if got != tc.want {
				t.Errorf("chooseWorkdir(%v, %q) = %q, want %q", tc.dirs, tc.repoName, got, tc.want)
			}
		})
	}
}

func TestRewriteHooksForSSH_NoHooks(t *testing.T) {
	result := rewriteHooksForSSH([]byte(`{"version": 1}`), "cs", "/workspaces/repo", "")
	if result != nil {
		t.Error("expected nil for config with no hooks")
	}
}

func TestRewriteHooksForSSH_InvalidJSON(t *testing.T) {
	result := rewriteHooksForSSH([]byte(`{invalid`), "cs", "/workspaces/repo", "")
	if result != nil {
		t.Error("expected nil for invalid JSON")
	}
}

func TestParseSelectionIndices(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		max     int
		want    []int
		wantErr bool
	}{
		{name: "blank means none", input: "", max: 4, want: nil},
		{name: "single selection", input: "2", max: 4, want: []int{1}},
		{name: "multiple selections", input: "1, 3 4", max: 4, want: []int{0, 2, 3}},
		{name: "duplicate selections deduped", input: "2,2, 2", max: 4, want: []int{1}},
		{name: "out of range", input: "5", max: 4, wantErr: true},
		{name: "invalid token", input: "abc", max: 4, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseSelectionIndices(tt.input, tt.max)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseLauncherArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    launcherOptions
		wantErr string
	}{
		{
			name: "no codespace mode skips picker",
			args: []string{"--no-codespace", "--name", "bootstrap", "--model", "claude-sonnet-4.5"},
			want: launcherOptions{
				noCodespace: true,
				sessionName: "bootstrap",
				copilotArgs: []string{"--model", "claude-sonnet-4.5"},
			},
		},
		{
			name: "parses existing launcher flags",
			args: []string{"--local-tools", "-w", "/workspaces/repo", "-c", "cs-1,cs-2", "--theme", "dark"},
			want: launcherOptions{
				codespaceNames:  []string{"cs-1", "cs-2"},
				workdirOverride: "/workspaces/repo",
				localTools:      true,
				copilotArgs:     []string{"--theme", "dark"},
			},
		},
		{
			name: "repeated codespace flags append selections",
			args: []string{"-c", "cs-1", "--codespace", "cs-2,cs-3"},
			want: launcherOptions{
				codespaceNames: []string{"cs-1", "cs-2", "cs-3"},
			},
		},
		{
			name:    "no-codespace conflicts with explicit codespace",
			args:    []string{"--no-codespace", "--codespace", "cs-1"},
			wantErr: "--no-codespace and --codespace are mutually exclusive",
		},
		{
			name:    "no-codespace conflicts with resume",
			args:    []string{"--no-codespace", "--resume", "saved-session"},
			wantErr: "--no-codespace and --resume are mutually exclusive",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseLauncherArgs(tt.args)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error %q", tt.wantErr)
				}
				if err.Error() != tt.wantErr {
					t.Fatalf("got error %q, want %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestResolveSelectedCodespaces(t *testing.T) {
	alpha := codespace{Name: "alpha", DisplayName: "Alpha", Repository: "owner/alpha", State: "Available"}
	beta := codespace{Name: "beta", DisplayName: "Beta", Repository: "owner/beta", State: "Available"}

	alphaChoice := "alpha\t🟢 owner/alpha: Alpha [Available]"
	betaChoice := "beta\t🟢 owner/beta: Beta [Available]"
	byChoice := map[string]codespace{
		alphaChoice: alpha,
		betaChoice:  beta,
	}

	tests := []struct {
		name     string
		selected []string
		want     []codespace
	}{
		{name: "blank means none", selected: []string{""}, want: []codespace{}},
		{name: "resolves known choices", selected: []string{alphaChoice, betaChoice}, want: []codespace{alpha, beta}},
		{name: "ignores unknown and duplicates", selected: []string{alphaChoice, "missing", alphaChoice}, want: []codespace{alpha}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveSelectedCodespaces(tt.selected, byChoice)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBuildMCPConfigWithRegistry(t *testing.T) {
	reg := registry.New()
	reg.Register(&registry.ManagedCodespace{
		Alias:      "github",
		Name:       "cs-abc",
		Repository: "github/github",
		Branch:     "main",
		Workdir:    "/workspaces/github",
	})

	result := buildMCPConfigWithRegistry("/usr/local/bin/self", reg, nil, false)

	var parsed map[string]any
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	servers := parsed["mcpServers"].(map[string]any)
	cs := servers["codespace"].(map[string]any)

	if got := cs["command"]; got != "/usr/local/bin/self" {
		t.Errorf("command = %v, want /usr/local/bin/self", got)
	}

	env, ok := cs["env"].(map[string]any)
	if !ok {
		t.Fatal("missing env key")
	}

	registryJSON, ok := env["CODESPACE_REGISTRY"].(string)
	if !ok || registryJSON == "" {
		t.Fatal("missing CODESPACE_REGISTRY env var")
	}

	var entries []registryEntry
	if err := json.Unmarshal([]byte(registryJSON), &entries); err != nil {
		t.Fatalf("invalid CODESPACE_REGISTRY JSON: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].Alias != "github" {
		t.Errorf("alias = %q, want %q", entries[0].Alias, "github")
	}
}

func TestBuildMCPConfigWithRegistry_EmptyRegistry(t *testing.T) {
	reg := registry.New()

	result := buildMCPConfigWithRegistry("/usr/local/bin/self", reg, nil, false)

	var parsed map[string]any
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	servers := parsed["mcpServers"].(map[string]any)
	cs := servers["codespace"].(map[string]any)
	env := cs["env"].(map[string]any)

	registryJSON, ok := env["CODESPACE_REGISTRY"].(string)
	if !ok {
		t.Fatal("missing CODESPACE_REGISTRY env var")
	}

	var entries []registryEntry
	if err := json.Unmarshal([]byte(registryJSON), &entries); err != nil {
		t.Fatalf("invalid CODESPACE_REGISTRY JSON: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("got %d entries, want 0", len(entries))
	}
}

func TestBuildMCPConfigWithRegistry_HeadlessDelegate(t *testing.T) {
	reg := registry.New()

	result := buildMCPConfigWithRegistry("/usr/local/bin/self", reg, nil, true)

	var parsed map[string]any
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	servers := parsed["mcpServers"].(map[string]any)
	cs := servers["codespace"].(map[string]any)
	env := cs["env"].(map[string]any)

	if got := env["CODESPACE_ENABLE_HEADLESS_DELEGATE"]; got != "1" {
		t.Fatalf("CODESPACE_ENABLE_HEADLESS_DELEGATE = %v, want 1", got)
	}
}

func TestWriteZeroCodespaceInstructionsPreamble(t *testing.T) {
	dir := t.TempDir()

	writeZeroCodespaceInstructionsPreamble(dir, false)

	data, err := os.ReadFile(filepath.Join(dir, ".github", "copilot-instructions.md"))
	if err != nil {
		t.Fatalf("reading instructions: %v", err)
	}
	text := string(data)
	for _, want := range []string{"list_available_codespaces", "create_codespace", "connect_codespace", "remote_*"} {
		if !strings.Contains(text, want) {
			t.Fatalf("expected %q in preamble, got %q", want, text)
		}
	}
}

func TestWriteCodespaceInstructionsPreamble_HeadlessDelegate(t *testing.T) {
	dir := t.TempDir()

	writeCodespaceInstructionsPreamble(dir, "/workspaces/repo", true)

	data, err := os.ReadFile(filepath.Join(dir, ".github", "copilot-instructions.md"))
	if err != nil {
		t.Fatalf("reading instructions: %v", err)
	}
	text := string(data)
	for _, want := range []string{"delegate_task", "read_delegate_task", "@remote-delegate"} {
		if !strings.Contains(text, want) {
			t.Fatalf("expected %q in preamble, got %q", want, text)
		}
	}
}

func TestGenerateRemoteDelegateAgent(t *testing.T) {
	dir := t.TempDir()

	generateRemoteDelegateAgent(dir)

	data, err := os.ReadFile(filepath.Join(dir, ".github", "agents", "remote-delegate.agent.md"))
	if err != nil {
		t.Fatalf("reading agent: %v", err)
	}

	text := string(data)
	for _, want := range []string{"delegate_task", "read_delegate_task", "cancel_delegate_task"} {
		if !strings.Contains(text, want) {
			t.Fatalf("expected %q in agent, got %q", want, text)
		}
	}
}

func TestRegistryFromEntries_Empty(t *testing.T) {
	reg, err := registryFromEntries(context.Background(), nil, func(_ context.Context, entry registryEntry) (*registry.ManagedCodespace, error) {
		t.Fatalf("build should not be called for empty entries: %+v", entry)
		return nil, nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reg.Len() != 0 {
		t.Fatalf("got %d codespaces, want 0", reg.Len())
	}
}

func TestRegistryFromEntries_DuplicateCodespaceName(t *testing.T) {
	entries := []registryEntry{
		{
			Alias:      "graph-hopper",
			Name:       "graph-hopper-pre-prod-97pxr4rj4cpg79",
			Repository: "acme/graph-hopper",
		},
		{
			Alias:      "graph-hopper-pre-prod-97pxr4rj4cpg79",
			Name:       "graph-hopper-pre-prod-97pxr4rj4cpg79",
			Repository: "acme/graph-hopper",
		},
	}

	_, err := registryFromEntries(context.Background(), entries, func(_ context.Context, entry registryEntry) (*registry.ManagedCodespace, error) {
		return &registry.ManagedCodespace{
			Alias:      entry.Alias,
			Name:       entry.Name,
			Repository: entry.Repository,
			Branch:     entry.Branch,
			Workdir:    entry.Workdir,
		}, nil
	})
	if err == nil {
		t.Fatal("expected error for duplicate codespace name")
	}
	if !strings.Contains(err.Error(), `already connected as alias "graph-hopper"`) {
		t.Fatalf("expected existing alias in error, got %q", err)
	}
}
