package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ekroon/gh-copilot-codespace/internal/registry"
)

func setBoolFlag(value bool) optionalBool {
	return optionalBool{set: true, value: value}
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
			name: "preserves launcher and copilot arguments",
			args: []string{"--no-codespace", "--selected-only=false", "--model", "claude-sonnet-4.5"},
			want: launcherOptions{
				noCodespace:  true,
				selectedOnly: setBoolFlag(false),
				copilotArgs:  []string{"--model", "claude-sonnet-4.5"},
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
			name: "resume is forwarded with its value",
			args: []string{"-c", "cs-1", "--resume", "copilot-session", "--model", "claude-sonnet-4.5"},
			want: launcherOptions{
				codespaceNames: []string{"cs-1"},
				copilotArgs:    []string{"--resume", "copilot-session", "--model", "claude-sonnet-4.5"},
			},
		},
		{
			name: "bare separator is not forwarded",
			args: []string{"--no-codespace", "--", "--resume", "copilot-session"},
			want: launcherOptions{
				noCodespace: true,
				copilotArgs: []string{"--resume", "copilot-session"},
			},
		},
		{
			name:    "no-codespace conflicts with explicit codespace",
			args:    []string{"--no-codespace", "--codespace", "cs-1"},
			wantErr: "--no-codespace and --codespace are mutually exclusive",
		},
		{
			name:    "rejects removed launcher flags",
			args:    []string{"--here"},
			wantErr: "--here is no longer supported",
		},
		{
			name:    "rejects workdir",
			args:    []string{"--workdir", "/workspaces/repo"},
			wantErr: "--workdir is no longer supported",
		},
		{
			name:    "rejects short workdir",
			args:    []string{"-w", "/workspaces/repo"},
			wantErr: "-w is no longer supported",
		},
		{
			name:    "rejects local tools boolean form",
			args:    []string{"--local-tools=false"},
			wantErr: "--local-tools is no longer supported",
		},
		{
			name:    "rejects extension tools",
			args:    []string{"--extension-tools"},
			wantErr: "--extension-tools is no longer supported",
		},
		{
			name:    "rejects name",
			args:    []string{"--name", "saved-session"},
			wantErr: "--name is no longer supported",
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

func TestResolveLaunchContext(t *testing.T) {
	got := resolveLaunchContext("/local/repo")
	if got.copilotCWD != "/local/repo" {
		t.Fatalf("copilotCWD = %q, want /local/repo", got.copilotCWD)
	}
}

func TestBuildCopilotArgs(t *testing.T) {
	extraArgs := []string{"--model", "claude-sonnet-4.5"}
	got := buildCopilotArgs(extraArgs)
	if !reflect.DeepEqual(got, extraArgs) {
		t.Fatalf("buildCopilotArgs() = %v, want passthrough %v", got, extraArgs)
	}
	for _, obsolete := range []string{"--excluded-tools", "--additional-mcp-config"} {
		if slicesContain(got, obsolete) {
			t.Fatalf("buildCopilotArgs() unexpectedly contains %q: %v", obsolete, got)
		}
	}
}

func TestPrepareLaunchDirectoryLeavesCheckoutUntouched(t *testing.T) {
	checkout := t.TempDir()
	marker := filepath.Join(checkout, "tracked.txt")
	if err := os.WriteFile(marker, []byte("unchanged"), 0o644); err != nil {
		t.Fatal(err)
	}
	originalCWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(originalCWD) })

	if err := prepareLaunchDirectory(checkout); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "unchanged" {
		t.Fatalf("checkout marker changed to %q", content)
	}
	entries, err := os.ReadDir(checkout)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "tracked.txt" {
		t.Fatalf("helper setup changed checkout contents: %v", entries)
	}
}

func slicesContain(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
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

func TestSelectedCodespaceNames(t *testing.T) {
	selected := []codespace{
		{Name: "cs-1"},
		{Name: "cs-1"},
		{Name: ""},
		{Name: "cs-2"},
	}

	if got := selectedCodespaceNames(selected); !reflect.DeepEqual(got, []string{"cs-1", "cs-2"}) {
		t.Fatalf("selectedCodespaceNames() = %v, want [cs-1 cs-2]", got)
	}
}

func TestExtensionSourceIncludesHostBridgeAndTokenGate(t *testing.T) {
	source := extensionSource()

	for _, want := range []string{
		`joinSession`,
		`readFileSync(manifestPath, "utf8")`,
		`spawn(manifest.selfBinary, ["extension-host"]`,
		codespaceExtensionTokenEnv,
		codespaceExtensionManifestEnv,
		`manifest?.token !== token`,
		`function isMcpCallToolResult`,
		`function convertMcpCallToolResult`,
		`binaryResultsForLlm`,
		`case "image":`,
		`return convertMcpCallToolResult(result);`,
		`list_tools`,
		`call_tool`,
		`resultType: "failure"`,
		`Array.isArray(bootstrap)`,
		`sessionConfig.systemMessage`,
		`sessionConfig.customAgents`,
	} {
		if !strings.Contains(source, want) {
			t.Fatalf("generated extension source missing %q:\n%s", want, source)
		}
	}
	for _, notWant := range []string{
		`CODESPACE_REGISTRY`,
		`/usr/local/bin/self`,
	} {
		if strings.Contains(source, notWant) {
			t.Fatalf("generated extension source should not embed %q:\n%s", notWant, source)
		}
	}
}

func TestCleanupGeneratedUserExtensionsRemovesOnlyStaleGeneratedDirs(t *testing.T) {
	root := t.TempDir()
	staleGenerated := filepath.Join(root, legacyGeneratedUserExtensionPrefix+"stale")
	freshGenerated := filepath.Join(root, legacyGeneratedUserExtensionPrefix+"fresh")
	stableUserExtension := filepath.Join(root, userExtensionName)
	otherExtension := filepath.Join(root, "other-extension")
	for _, dir := range []string{staleGenerated, freshGenerated, stableUserExtension, otherExtension} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	now := time.Now()
	staleTime := now.Add(-(generatedUserExtensionMaxAge + time.Hour))
	freshTime := now.Add(-time.Hour)
	if err := os.Chtimes(staleGenerated, staleTime, staleTime); err != nil {
		t.Fatalf("chtimes stale: %v", err)
	}
	if err := os.Chtimes(freshGenerated, freshTime, freshTime); err != nil {
		t.Fatalf("chtimes fresh: %v", err)
	}
	if err := os.Chtimes(stableUserExtension, staleTime, staleTime); err != nil {
		t.Fatalf("chtimes stable: %v", err)
	}
	if err := os.Chtimes(otherExtension, staleTime, staleTime); err != nil {
		t.Fatalf("chtimes other: %v", err)
	}

	if err := cleanupLegacyGeneratedUserExtensions(root, now); err != nil {
		t.Fatalf("cleanupLegacyGeneratedUserExtensions: %v", err)
	}

	if _, err := os.Stat(staleGenerated); !os.IsNotExist(err) {
		t.Fatalf("stale generated extension still exists or unexpected err: %v", err)
	}
	for _, dir := range []string{freshGenerated, stableUserExtension, otherExtension} {
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("%s should remain: %v", dir, err)
		}
	}
}

func TestWriteExtensionSessionManifest(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", root)
	env := map[string]string{"CODESPACE_REGISTRY": "[]"}
	path, err := writeExtensionSessionManifest(root, "/usr/local/bin/self", env, "token-123", "here")
	if err != nil {
		t.Fatalf("writeExtensionSessionManifest: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat manifest: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("manifest mode = %v, want 0600", got)
	}
	var manifest extensionSessionManifest
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}
	if manifest.SelfBinary != "/usr/local/bin/self" || manifest.Token != "token-123" {
		t.Fatalf("manifest = %+v, want self/token", manifest)
	}
	if got := manifest.Env["CODESPACE_REGISTRY"]; got != "[]" {
		t.Fatalf("CODESPACE_REGISTRY = %q, want []", got)
	}
	if !strings.Contains(filepath.Base(path), "token-123") {
		t.Fatalf("manifest path %q should include token to avoid same-workspace races", path)
	}

	otherPath, err := writeExtensionSessionManifest(root, "/usr/local/bin/self", env, "token-456", "here")
	if err != nil {
		t.Fatalf("write second manifest: %v", err)
	}
	if otherPath == path {
		t.Fatal("expected distinct sessions to use distinct manifest paths")
	}
}

func TestInstallUserExtensionWritesStableWrapper(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := installUserExtension(); err != nil {
		t.Fatalf("installUserExtension: %v", err)
	}
	path := filepath.Join(home, ".copilot", "extensions", userExtensionName, "extension.mjs")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read extension: %v", err)
	}
	source := string(data)
	if !strings.Contains(source, codespaceExtensionManifestEnv) {
		t.Fatalf("extension source missing manifest env:\n%s", source)
	}
	if strings.Contains(source, "CODESPACE_REGISTRY") {
		t.Fatalf("stable user extension should not embed session env:\n%s", source)
	}
}

func TestRunExtensionHostIOListAndCall(t *testing.T) {
	t.Setenv("CODESPACE_REGISTRY", "[]")
	t.Setenv(codespaceLifecycleConfigEnv, "")
	t.Setenv(codespaceLocalWorkdirEnv, "")

	input := strings.NewReader(
		`{"id":1,"method":"list_tools"}` + "\n" +
			`{"id":2,"method":"call_tool","tool":"list_codespaces","args":{}}` + "\n")
	var output bytes.Buffer
	if err := runExtensionHostIO(input, &output); err != nil {
		t.Fatalf("runExtensionHostIO: %v", err)
	}

	dec := json.NewDecoder(&output)
	var listResp extensionHostResponse
	if err := dec.Decode(&listResp); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if listResp.Error != nil {
		t.Fatalf("list response error: %v", listResp.Error)
	}
	payload, ok := listResp.Result.(map[string]any)
	if !ok {
		t.Fatalf("list response result = %#v, want bootstrap payload object", listResp.Result)
	}
	defs, ok := payload["tools"].([]any)
	if !ok || len(defs) == 0 {
		t.Fatalf("bootstrap tools = %#v, want non-empty definitions", payload["tools"])
	}
	haveRemoteApplyPatch := false
	for _, def := range defs {
		tool, _ := def.(map[string]any)
		if tool["name"] == "remote_apply_patch" {
			haveRemoteApplyPatch = true
			break
		}
	}
	if !haveRemoteApplyPatch {
		t.Fatalf("bootstrap tools missing remote_apply_patch: %#v", defs)
	}
	sysMsg, ok := payload["systemMessage"].(map[string]any)
	if !ok {
		t.Fatalf("bootstrap systemMessage = %#v, want object", payload["systemMessage"])
	}
	if got := sysMsg["mode"]; got != "append" {
		t.Fatalf("systemMessage mode = %v, want append", got)
	}
	content, _ := sysMsg["content"].(string)
	if !strings.Contains(content, "list_available_codespaces") {
		t.Fatalf("systemMessage content missing zero-codespace guidance: %q", content)
	}

	var callResp extensionHostResponse
	if err := dec.Decode(&callResp); err != nil {
		t.Fatalf("decode call response: %v", err)
	}
	if callResp.Error != nil {
		t.Fatalf("call response error: %v", callResp.Error)
	}
	result, ok := callResp.Result.(map[string]any)
	if !ok {
		t.Fatalf("call response result = %#v, want object", callResp.Result)
	}
	if got := result["resultType"]; got != "success" {
		t.Fatalf("resultType = %v, want success", got)
	}
	text, _ := result["textResultForLlm"].(string)
	if !strings.Contains(text, "No codespaces connected") {
		t.Fatalf("textResultForLlm = %v, want no-codespaces message", result["textResultForLlm"])
	}

	// Zero codespaces ⇒ the inline remote-explorer agent must not be advertised
	// (its tools wouldn't work anyway).
	if got, ok := payload["customAgents"]; ok && got != nil {
		if agents, _ := got.([]any); len(agents) > 0 {
			t.Fatalf("customAgents with zero codespaces = %#v, want none", agents)
		}
	}
}

func TestRunExtensionHostIOAdvertisesRemoteExplorerWhenCodespaceConnected(t *testing.T) {
	t.Setenv("CODESPACE_REGISTRY", `[{"alias":"github","name":"cs-abc","workdir":"/workspaces/github"}]`)
	t.Setenv(codespaceLifecycleConfigEnv, "")
	t.Setenv(codespaceLocalWorkdirEnv, "")
	t.Setenv(codespaceExtensionModeEnv, "mirror")

	input := strings.NewReader(`{"id":1,"method":"list_tools"}` + "\n")
	var output bytes.Buffer
	if err := runExtensionHostIO(input, &output); err != nil {
		t.Fatalf("runExtensionHostIO: %v", err)
	}

	var listResp extensionHostResponse
	if err := json.NewDecoder(&output).Decode(&listResp); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	payload, ok := listResp.Result.(map[string]any)
	if !ok {
		t.Fatalf("list response result = %#v, want bootstrap payload object", listResp.Result)
	}

	agentsAny, ok := payload["customAgents"].([]any)
	if !ok || len(agentsAny) == 0 {
		t.Fatalf("customAgents = %#v, want one entry", payload["customAgents"])
	}
	agent, ok := agentsAny[0].(map[string]any)
	if !ok {
		t.Fatalf("customAgent entry = %#v, want object", agentsAny[0])
	}
	if got := agent["name"]; got != remoteExplorerAgentName {
		t.Fatalf("custom agent name = %v, want %s", got, remoteExplorerAgentName)
	}
	if prompt, _ := agent["prompt"].(string); !strings.Contains(prompt, "remote_grep") {
		t.Fatalf("custom agent prompt missing remote_grep guidance: %q", prompt)
	}
	toolsAny, _ := agent["tools"].([]any)
	if len(toolsAny) == 0 {
		t.Fatalf("custom agent tools empty, want allow-list")
	}
	var tools []string
	for _, tname := range toolsAny {
		s, _ := tname.(string)
		if strings.HasPrefix(s, "codespace/") {
			t.Fatalf("custom agent tool %q uses MCP namespace; expected bare extension tool name", s)
		}
		tools = append(tools, s)
	}
	if !reflect.DeepEqual(tools, remoteExplorerReadOnlyExtensionTools) {
		t.Fatalf("custom agent tools = %v, want %v", tools, remoteExplorerReadOnlyExtensionTools)
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
