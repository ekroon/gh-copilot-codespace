//go:build integration

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ekroon/gh-copilot-codespace/internal/mcp"
	"github.com/ekroon/gh-copilot-codespace/internal/registry"
	"github.com/ekroon/gh-copilot-codespace/internal/ssh"
)

// These tests require a running codespace with test fixtures.
// Run: TEST_CODESPACE=<name> go test -tags integration -v ./cmd/gh-copilot-codespace/

func testCodespace(t *testing.T) string {
	t.Helper()
	cs := os.Getenv("TEST_CODESPACE")
	if cs == "" {
		t.Skip("TEST_CODESPACE not set")
	}
	return cs
}

func testWorkdir(t *testing.T) string {
	t.Helper()
	wd := os.Getenv("TEST_WORKDIR")
	if wd == "" {
		return "/workspaces/ekroon"
	}
	return wd
}

func testSSHClient(t *testing.T, cs string) *ssh.Client {
	t.Helper()
	client := ssh.NewClient(cs)
	ctx := context.Background()
	if err := client.SetupMultiplexing(ctx); err != nil {
		t.Logf("SSH multiplexing not available, using fallback: %v", err)
	}
	return client
}

// testFetchInstructionFiles wraps fetchInstructionFiles with SSH client setup.
// On first call, it provisions test fixtures on the codespace.
func testFetchInstructionFiles(t *testing.T, cs, wd string) (string, map[string]any, error) {
	t.Helper()
	setupTestFixturesOnce(t, cs, wd)
	client := testSSHClient(t, cs)
	return fetchInstructionFiles(client, cs, wd, "")
}

var fixturesReady bool

func setupTestFixturesOnce(t *testing.T, cs, wd string) {
	t.Helper()
	if fixturesReady {
		return
	}
	setupTestFixtures(t, cs, wd)
	fixturesReady = true
}

func setupMirrorDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	// Initialize as git repo like the real code does
	exec.Command("git", "-C", dir, "init", "-q").Run()
	return dir
}

func TestIntegration_RootInstructionFiles(t *testing.T) {
	cs := testCodespace(t)
	wd := testWorkdir(t)

	// Run fetchInstructionFiles against the real codespace
	dir, _, err := testFetchInstructionFiles(t, cs, wd)
	if err != nil {
		t.Fatalf("fetchInstructionFiles: %v", err)
	}
	defer os.RemoveAll(dir)

	// Root-level files should be fetched
	expectFile(t, dir, ".github/copilot-instructions.md")
	expectFile(t, dir, "AGENTS.md")
	expectFile(t, dir, "CLAUDE.md")
	expectFile(t, dir, "GEMINI.md")
}

func TestIntegration_ScopedInstructionFiles(t *testing.T) {
	cs := testCodespace(t)
	wd := testWorkdir(t)

	dir, _, err := testFetchInstructionFiles(t, cs, wd)
	if err != nil {
		t.Fatalf("fetchInstructionFiles: %v", err)
	}
	defer os.RemoveAll(dir)

	// Scoped instruction files (including nested) should be fetched
	expectFile(t, dir, ".github/instructions/ruby.instructions.md")
	expectFile(t, dir, ".github/instructions/frontend/react.instructions.md")
	expectFile(t, dir, ".github/instructions/backend/api/rest.instructions.md")
}

func TestIntegration_HierarchicalAgentFiles(t *testing.T) {
	cs := testCodespace(t)
	wd := testWorkdir(t)

	dir, _, err := testFetchInstructionFiles(t, cs, wd)
	if err != nil {
		t.Fatalf("fetchInstructionFiles: %v", err)
	}
	defer os.RemoveAll(dir)

	// Root-level agents should be present
	expectFile(t, dir, "AGENTS.md")
	expectFile(t, dir, "CLAUDE.md")

	// Hierarchical (subdirectory) agent files should also be fetched
	expectFile(t, dir, "docs/AGENTS.md")
	expectFile(t, dir, "docs/CLAUDE.md")
	expectFile(t, dir, "teams/backend/AGENTS.md")
}

func TestIntegration_HierarchicalFileContent(t *testing.T) {
	cs := testCodespace(t)
	wd := testWorkdir(t)

	dir, _, err := testFetchInstructionFiles(t, cs, wd)
	if err != nil {
		t.Fatalf("fetchInstructionFiles: %v", err)
	}
	defer os.RemoveAll(dir)

	// Verify content to ensure we got the right files (not duplicates)
	rootContent := readFileContent(t, filepath.Join(dir, "AGENTS.md"))
	docsContent := readFileContent(t, filepath.Join(dir, "docs/AGENTS.md"))

	if rootContent == docsContent {
		t.Error("root AGENTS.md and docs/AGENTS.md should have different content")
	}
	if !strings.Contains(rootContent, "Root") {
		t.Errorf("root AGENTS.md should contain 'Root', got: %s", rootContent)
	}
	if !strings.Contains(docsContent, "Docs") {
		t.Errorf("docs/AGENTS.md should contain 'Docs', got: %s", docsContent)
	}
}

func TestIntegration_MCPConfigRewriting(t *testing.T) {
	cs := testCodespace(t)
	wd := testWorkdir(t)

	_, remoteMCP, err := testFetchInstructionFiles(t, cs, wd)
	if err != nil {
		t.Fatalf("fetchInstructionFiles: %v", err)
	}

	if remoteMCP == nil {
		t.Fatal("remoteMCPServers should not be nil (test codespace has .copilot/mcp-config.json)")
	}

	// remoteMCP contains the raw (unrewritten) server configs from the codespace
	testServer, ok := remoteMCP["test-server"]
	if !ok {
		t.Fatal("missing test-server in raw MCP config")
	}

	server, ok := testServer.(map[string]any)
	if !ok {
		t.Fatal("test-server should be a map")
	}

	// Raw config should have the original command (python3)
	if cmd, _ := server["command"].(string); cmd != "python3" {
		t.Errorf("raw command = %q, want 'python3'", cmd)
	}

	// Verify buildMCPConfig rewrites it to use gh
	mcpConfig := buildMCPConfig("/usr/local/bin/self", cs, wd, remoteMCP, "", false)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(mcpConfig), &parsed); err != nil {
		t.Fatalf("invalid merged MCP config JSON: %v", err)
	}

	servers := parsed["mcpServers"].(map[string]any)
	if _, ok := servers["codespace"]; !ok {
		t.Error("merged config should contain 'codespace' server")
	}
	rewrittenServer, ok := servers["test-server"].(map[string]any)
	if !ok {
		t.Fatal("merged config should contain 'test-server'")
	}
	if cmd, _ := rewrittenServer["command"].(string); cmd != "gh" {
		t.Errorf("rewritten command = %q, want 'gh'", cmd)
	}
}

func TestIntegration_MCPForwardingEndToEnd(t *testing.T) {
	cs := testCodespace(t)
	wd := testWorkdir(t)

	// Test that the rewritten MCP server config actually works by sending
	// an initialize request through SSH to the test MCP server
	initReq := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"0.1"}}}`

	// Run the MCP server command via SSH (simulating what the rewritten config does)
	cmd := exec.Command("gh", "codespace", "ssh", "-c", cs, "--",
		"python3", filepath.Join(wd, ".copilot/test-mcp-server.py"))
	cmd.Stdin = strings.NewReader(initReq + "\n")

	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("MCP server via SSH failed: %v", err)
	}

	var resp map[string]any
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("invalid JSON-RPC response: %v (raw: %s)", err, string(out))
	}

	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("missing result in response: %v", resp)
	}

	serverInfo, ok := result["serverInfo"].(map[string]any)
	if !ok {
		t.Fatalf("missing serverInfo in result: %v", result)
	}

	if name, _ := serverInfo["name"].(string); name != "test-mcp" {
		t.Errorf("serverInfo.name = %q, want 'test-mcp'", name)
	}
}

func TestIntegration_StaleFileCleanup(t *testing.T) {
	cs := testCodespace(t)
	wd := testWorkdir(t)

	// First fetch
	dir, _, err := testFetchInstructionFiles(t, cs, wd)
	if err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	defer os.RemoveAll(dir)

	// Initialize git repo (normally done in runLauncher)
	exec.Command("git", "-C", dir, "init", "-q").Run()

	// Plant a stale file that shouldn't survive re-fetch
	staleDir := filepath.Join(dir, "old-subdir")
	os.MkdirAll(staleDir, 0o755)
	os.WriteFile(filepath.Join(staleDir, "AGENTS.md"), []byte("stale"), 0o644)

	// Also plant a stale file in .github that doesn't exist on remote
	os.WriteFile(filepath.Join(dir, ".github", "stale-file.md"), []byte("stale"), 0o644)

	// Re-fetch (the function creates a deterministic dir, so we need to
	// simulate by calling cleanMirrorDir + the fetch logic again)
	cleanMirrorDir(dir)

	// Stale files should be gone
	if _, err := os.Stat(filepath.Join(staleDir, "AGENTS.md")); err == nil {
		t.Error("stale old-subdir/AGENTS.md should have been removed")
	}
	if _, err := os.Stat(filepath.Join(dir, ".github", "stale-file.md")); err == nil {
		t.Error("stale .github/stale-file.md should have been removed")
	}

	// .git should survive
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		t.Error(".git should survive cleanup")
	}
}

func TestIntegration_ScopedInstructionFrontmatter(t *testing.T) {
	cs := testCodespace(t)
	wd := testWorkdir(t)

	dir, _, err := testFetchInstructionFiles(t, cs, wd)
	if err != nil {
		t.Fatalf("fetchInstructionFiles: %v", err)
	}
	defer os.RemoveAll(dir)

	// Verify that scoped instruction files have their frontmatter preserved
	content := readFileContent(t, filepath.Join(dir, ".github/instructions/ruby.instructions.md"))

	if !strings.Contains(content, "applyTo") {
		t.Error("ruby.instructions.md should contain applyTo frontmatter")
	}
	if !strings.Contains(content, "**/*.rb") {
		t.Error("ruby.instructions.md should contain the glob pattern **/*.rb")
	}
}

func TestIntegration_ApplyToWorksWithCopilotCLI(t *testing.T) {
	cs := testCodespace(t)
	wd := testWorkdir(t)

	// Set up the mirror directory
	dir, _, err := testFetchInstructionFiles(t, cs, wd)
	if err != nil {
		t.Fatalf("fetchInstructionFiles: %v", err)
	}
	defer os.RemoveAll(dir)

	// Initialize as git repo (like runLauncher does)
	exec.Command("git", "-C", dir, "init", "-q").Run()

	// Check that copilot is available
	copilotPath, err := exec.LookPath("copilot")
	if err != nil {
		t.Skip("copilot not found in PATH")
	}

	// Run copilot -p from the mirror dir asking it to list loaded instructions
	cmd := exec.Command(copilotPath,
		"-p", "List the file paths of ALL custom instruction files you have loaded. Just list the paths, one per line, nothing else.",
		"--allow-all-tools",
		"--quiet",
	)
	cmd.Dir = dir

	out, err := cmd.Output()
	if err != nil {
		// --quiet might not be supported, try without
		cmd = exec.Command(copilotPath,
			"-p", "List the file paths of ALL custom instruction files you have loaded. Just list the paths, one per line, nothing else.",
			"--allow-all-tools",
		)
		cmd.Dir = dir
		out, err = cmd.Output()
		if err != nil {
			t.Fatalf("copilot -p failed: %v", err)
		}
	}

	output := string(out)

	// Copilot should have loaded the scoped instruction files with applyTo patterns
	if !strings.Contains(output, "react.instructions.md") {
		t.Errorf("copilot should have loaded react.instructions.md (applyTo: **/*.tsx,**/*.jsx)\nOutput: %s", output)
	}

	// Root instruction files should also be loaded
	if !strings.Contains(output, "AGENTS.md") {
		t.Errorf("copilot should have loaded AGENTS.md\nOutput: %s", output)
	}
}

func TestIntegration_SkillFiles(t *testing.T) {
	cs := testCodespace(t)
	wd := testWorkdir(t)

	dir, _, err := testFetchInstructionFiles(t, cs, wd)
	if err != nil {
		t.Fatalf("fetchInstructionFiles: %v", err)
	}
	defer os.RemoveAll(dir)

	// Skills should be fetched from all supported locations
	expectFile(t, dir, ".github/skills/test-skill/SKILL.md")
	expectFile(t, dir, ".claude/skills/deploy/SKILL.md")
}

func TestIntegration_SkillContent(t *testing.T) {
	cs := testCodespace(t)
	wd := testWorkdir(t)

	dir, _, err := testFetchInstructionFiles(t, cs, wd)
	if err != nil {
		t.Fatalf("fetchInstructionFiles: %v", err)
	}
	defer os.RemoveAll(dir)

	content := readFileContent(t, filepath.Join(dir, ".github/skills/test-skill/SKILL.md"))
	if !strings.Contains(content, "name:") {
		t.Error("SKILL.md should contain frontmatter with name field")
	}
}

func TestIntegration_SkillSupportingFiles(t *testing.T) {
	cs := testCodespace(t)
	wd := testWorkdir(t)

	dir, _, err := testFetchInstructionFiles(t, cs, wd)
	if err != nil {
		t.Fatalf("fetchInstructionFiles: %v", err)
	}
	defer os.RemoveAll(dir)

	// Skills with supporting scripts should have those scripts mirrored
	expectFile(t, dir, ".github/skills/test-skill/scripts/helper.sh")
}

func TestIntegration_CustomAgents(t *testing.T) {
	cs := testCodespace(t)
	wd := testWorkdir(t)

	dir, _, err := testFetchInstructionFiles(t, cs, wd)
	if err != nil {
		t.Fatalf("fetchInstructionFiles: %v", err)
	}
	defer os.RemoveAll(dir)

	// Custom agents should be fetched
	expectFile(t, dir, ".github/agents/reviewer.agent.md")
	expectFile(t, dir, ".claude/agents/helper.agent.md")
}

func TestIntegration_CustomAgentContent(t *testing.T) {
	cs := testCodespace(t)
	wd := testWorkdir(t)

	dir, _, err := testFetchInstructionFiles(t, cs, wd)
	if err != nil {
		t.Fatalf("fetchInstructionFiles: %v", err)
	}
	defer os.RemoveAll(dir)

	content := readFileContent(t, filepath.Join(dir, ".github/agents/reviewer.agent.md"))
	if !strings.Contains(content, "name:") {
		t.Error("agent.md should contain frontmatter with name field")
	}
}

func TestIntegration_Commands(t *testing.T) {
	cs := testCodespace(t)
	wd := testWorkdir(t)

	dir, _, err := testFetchInstructionFiles(t, cs, wd)
	if err != nil {
		t.Fatalf("fetchInstructionFiles: %v", err)
	}
	defer os.RemoveAll(dir)

	// Commands should be fetched
	expectFile(t, dir, ".claude/commands/test-command.md")
}

func TestIntegration_HooksForwarding(t *testing.T) {
	cs := testCodespace(t)
	wd := testWorkdir(t)

	dir, _, err := testFetchInstructionFiles(t, cs, wd)
	if err != nil {
		t.Fatalf("fetchInstructionFiles: %v", err)
	}
	defer os.RemoveAll(dir)

	// Hooks should be fetched and rewritten for SSH
	hooksPath := filepath.Join(dir, ".github/hooks/test-hooks.json")
	expectFile(t, dir, ".github/hooks/test-hooks.json")

	content := readFileContent(t, hooksPath)

	// The hook bash commands should have been rewritten to use SSH
	if !strings.Contains(content, "gh codespace ssh") {
		t.Errorf("hooks should be rewritten for SSH forwarding, got: %s", content)
	}
	if !strings.Contains(content, cs) {
		t.Errorf("hooks should reference the codespace name %q, got: %s", cs, content)
	}
}

func TestIntegration_HooksForwardingEndToEnd(t *testing.T) {
	cs := testCodespace(t)
	wd := testWorkdir(t)

	dir, _, err := testFetchInstructionFiles(t, cs, wd)
	if err != nil {
		t.Fatalf("fetchInstructionFiles: %v", err)
	}
	defer os.RemoveAll(dir)

	// Parse the rewritten hooks JSON to extract the actual bash command
	hooksContent := readFileContent(t, filepath.Join(dir, ".github/hooks/test-hooks.json"))
	var hooksConfig map[string]any
	if err := json.Unmarshal([]byte(hooksContent), &hooksConfig); err != nil {
		t.Fatalf("invalid hooks JSON: %v", err)
	}

	hooks := hooksConfig["hooks"].(map[string]any)
	preToolUse := hooks["preToolUse"].([]any)
	hook := preToolUse[0].(map[string]any)
	bashCmd := hook["bash"].(string)

	// Execute the rewritten hook command, piping a preToolUse event via stdin.
	// The test-hook.sh script on the codespace should read stdin and respond
	// with a JSON allow decision.
	event := `{"event":"preToolUse","toolName":"bash","toolInput":{"command":"echo hello"}}`
	cmd := exec.Command("bash", "-c", bashCmd)
	cmd.Stdin = strings.NewReader(event + "\n")

	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			t.Fatalf("hook command failed: %v\nstderr: %s", err, string(exitErr.Stderr))
		}
		t.Fatalf("hook command failed: %v", err)
	}

	// The test hook script should return a JSON response
	var resp map[string]any
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("invalid JSON response from hook: %v (raw: %s)", err, string(out))
	}

	decision, ok := resp["permissionDecision"].(string)
	if !ok {
		t.Fatalf("missing permissionDecision in hook response: %v", resp)
	}
	if decision != "allow" {
		t.Errorf("permissionDecision = %q, want 'allow'", decision)
	}
}

func TestIntegration_MCPForwardingEndToEnd_VSCode(t *testing.T) {
	cs := testCodespace(t)
	wd := testWorkdir(t)

	// Fetch and get remote MCP configs (includes .vscode/mcp.json)
	_, remoteMCP, err := testFetchInstructionFiles(t, cs, wd)
	if err != nil {
		t.Fatalf("fetchInstructionFiles: %v", err)
	}

	if remoteMCP == nil {
		t.Fatal("remoteMCPServers should not be nil")
	}

	vscodeServer, ok := remoteMCP["vscode-test-server"]
	if !ok {
		t.Fatal("missing vscode-test-server from .vscode/mcp.json")
	}

	// Rewrite for SSH (same as buildMCPConfig does)
	serverConfig, ok := vscodeServer.(map[string]any)
	if !ok {
		t.Fatal("vscode-test-server config should be a map")
	}
	rewritten := rewriteMCPServerForSSH(serverConfig, cs, wd, "")
	if rewritten == nil {
		t.Fatal("rewriteMCPServerForSSH returned nil for vscode-test-server")
	}

	// Execute the rewritten MCP server command and send an initialize request
	initReq := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"0.1"}}}`

	command := rewritten["command"].(string)
	args := rewritten["args"].([]string)
	cmd := exec.Command(command, args...)
	cmd.Stdin = strings.NewReader(initReq + "\n")

	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			t.Fatalf("MCP server via SSH failed: %v\nstderr: %s", err, string(exitErr.Stderr))
		}
		t.Fatalf("MCP server via SSH failed: %v", err)
	}

	var resp map[string]any
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("invalid JSON-RPC response: %v (raw: %s)", err, string(out))
	}

	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("missing result in response: %v", resp)
	}

	serverInfo, ok := result["serverInfo"].(map[string]any)
	if !ok {
		t.Fatalf("missing serverInfo in result: %v", result)
	}

	if name, _ := serverInfo["name"].(string); name != "test-mcp" {
		t.Errorf("serverInfo.name = %q, want 'test-mcp'", name)
	}
}

// TestIntegration_HookTriggeredByCopilot verifies the full hook pipeline:
// Copilot CLI discovers hooks from the mirror directory → triggers sessionStart →
// executes the rewritten bash command (which SSHs into the codespace) →
// hook script runs on codespace and writes a marker file.
func TestIntegration_HookTriggeredByCopilot(t *testing.T) {
	cs := testCodespace(t)
	wd := testWorkdir(t)

	dir, _, err := testFetchInstructionFiles(t, cs, wd)
	if err != nil {
		t.Fatalf("fetchInstructionFiles: %v", err)
	}
	defer os.RemoveAll(dir)

	exec.Command("git", "-C", dir, "init", "-q").Run()

	copilotPath, err := exec.LookPath("copilot")
	if err != nil {
		t.Skip("copilot not found in PATH")
	}

	// Clean marker files on codespace before test
	markers := []string{
		"/tmp/copilot-hook-e2e-session-start",
		"/tmp/copilot-hook-e2e-pre-tool-use",
	}
	for _, m := range markers {
		exec.Command("gh", "codespace", "ssh", "-c", cs, "--", "rm", "-f", m).Run()
	}
	defer func() {
		for _, m := range markers {
			exec.Command("gh", "codespace", "ssh", "-c", cs, "--", "rm", "-f", m).Run()
		}
	}()

	// Run copilot with a minimal prompt. The sessionStart hook should fire
	// on startup and write a marker file on the codespace via SSH.
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, copilotPath,
		"-p", "Say the word hello and nothing else",
		"--allow-all-tools",
	)
	cmd.Dir = dir

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("copilot -p failed: %v\nOutput: %s", err, string(out))
	}

	// sessionStart hook should have fired and created the marker on the codespace
	if exec.Command("gh", "codespace", "ssh", "-c", cs, "--",
		"test", "-f", "/tmp/copilot-hook-e2e-session-start").Run() != nil {
		t.Error("sessionStart hook was not triggered on codespace (marker file not found)")
	}
}

func TestIntegration_AdditionalMCPConfigs(t *testing.T) {
	cs := testCodespace(t)
	wd := testWorkdir(t)

	_, remoteMCP, err := testFetchInstructionFiles(t, cs, wd)
	if err != nil {
		t.Fatalf("fetchInstructionFiles: %v", err)
	}

	if remoteMCP == nil {
		t.Fatal("remoteMCPServers should not be nil")
	}

	// The test codespace should have the test-server from .copilot/mcp-config.json
	if _, ok := remoteMCP["test-server"]; !ok {
		t.Error("missing test-server from .copilot/mcp-config.json")
	}

	// And the vscode-test-server from .vscode/mcp.json
	if _, ok := remoteMCP["vscode-test-server"]; !ok {
		t.Error("missing vscode-test-server from .vscode/mcp.json")
	}
}

func TestIntegration_DeployAndExec(t *testing.T) {
	cs := testCodespace(t)

	client := testSSHClient(t, cs)

	// Deploy binary to codespace
	remotePath, err := deployBinary(client, cs)
	if err != nil {
		t.Fatalf("deployBinary: %v", err)
	}

	if remotePath == "" {
		t.Fatal("deployBinary returned empty path")
	}

	// Verify the binary exists and is executable
	out, err := exec.Command("gh", "codespace", "ssh", "-c", cs, "--",
		remotePath, "exec", "--workdir", "/tmp", "--", "echo", "hello-from-exec").CombinedOutput()
	if err != nil {
		t.Fatalf("exec on codespace failed: %v\nOutput: %s", err, string(out))
	}

	if !strings.Contains(string(out), "hello-from-exec") {
		t.Errorf("exec output should contain 'hello-from-exec', got: %s", string(out))
	}
}

func TestIntegration_DeployAndExecWithEnv(t *testing.T) {
	cs := testCodespace(t)

	client := testSSHClient(t, cs)

	remotePath, err := deployBinary(client, cs)
	if err != nil {
		t.Fatalf("deployBinary: %v", err)
	}

	// Test that --env properly sets environment variables
	out, err := exec.Command("gh", "codespace", "ssh", "-c", cs, "--",
		remotePath, "exec", "--env", "TEST_VAR=copilot-e2e", "--", "printenv", "TEST_VAR").CombinedOutput()
	if err != nil {
		t.Fatalf("exec with env failed: %v\nOutput: %s", err, string(out))
	}

	if !strings.Contains(string(out), "copilot-e2e") {
		t.Errorf("exec should output env var value, got: %s", string(out))
	}
}

func TestIntegration_DeployAndExecWithWorkdir(t *testing.T) {
	cs := testCodespace(t)
	wd := testWorkdir(t)

	client := testSSHClient(t, cs)

	remotePath, err := deployBinary(client, cs)
	if err != nil {
		t.Fatalf("deployBinary: %v", err)
	}

	// Test that --workdir properly changes directory
	out, err := exec.Command("gh", "codespace", "ssh", "-c", cs, "--",
		remotePath, "exec", "--workdir", wd, "--", "pwd").CombinedOutput()
	if err != nil {
		t.Fatalf("exec with workdir failed: %v\nOutput: %s", err, string(out))
	}

	if !strings.Contains(string(out), wd) {
		t.Errorf("exec should output workdir %q, got: %s", wd, string(out))
	}
}

// --- helpers ---

func expectFile(t *testing.T, dir, relPath string) {
	t.Helper()
	fullPath := filepath.Join(dir, relPath)
	info, err := os.Stat(fullPath)
	if err != nil {
		t.Errorf("expected file %s: %v", relPath, err)
		return
	}
	if info.Size() == 0 {
		t.Errorf("file %s exists but is empty", relPath)
	}
}

func readFileContent(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(data)
}

// setupTestFixtures creates all required test fixture files on the codespace
// via a single SSH call. This makes integration tests self-contained — no
// manual setup required. Existing fixture files are overwritten.
func setupTestFixtures(t *testing.T, cs, wd string) {
	t.Helper()

	script := fmt.Sprintf(`
set -e
WD=%s

# --- Instruction files (pre-existing fixtures) ---
mkdir -p "$WD/.github/instructions/frontend" "$WD/.github/instructions/backend/api"
mkdir -p "$WD/docs" "$WD/teams/backend"

test -f "$WD/.github/copilot-instructions.md" || echo '# Copilot Instructions' > "$WD/.github/copilot-instructions.md"
test -f "$WD/AGENTS.md" || echo '# Root AGENTS' > "$WD/AGENTS.md"
test -f "$WD/CLAUDE.md" || echo '# Root CLAUDE' > "$WD/CLAUDE.md"
test -f "$WD/GEMINI.md" || echo '# Root GEMINI' > "$WD/GEMINI.md"
test -f "$WD/docs/AGENTS.md" || echo '# Docs AGENTS' > "$WD/docs/AGENTS.md"
test -f "$WD/docs/CLAUDE.md" || echo '# Docs CLAUDE' > "$WD/docs/CLAUDE.md"
test -f "$WD/teams/backend/AGENTS.md" || echo '# Backend AGENTS' > "$WD/teams/backend/AGENTS.md"

cat > "$WD/.github/instructions/ruby.instructions.md" << 'FIXTURE'
---
applyTo: "**/*.rb"
---
Use Ruby best practices.
FIXTURE

test -f "$WD/.github/instructions/frontend/react.instructions.md" || cat > "$WD/.github/instructions/frontend/react.instructions.md" << 'FIXTURE'
---
applyTo: "**/*.tsx,**/*.jsx"
---
Use React best practices.
FIXTURE

test -f "$WD/.github/instructions/backend/api/rest.instructions.md" || cat > "$WD/.github/instructions/backend/api/rest.instructions.md" << 'FIXTURE'
---
applyTo: "**/*_controller.rb"
---
Follow REST conventions.
FIXTURE

# --- MCP configs ---
mkdir -p "$WD/.copilot" "$WD/.vscode"

test -f "$WD/.copilot/test-mcp-server.py" || cat > "$WD/.copilot/test-mcp-server.py" << 'FIXTURE'
import sys, json
req = json.loads(sys.stdin.readline())
resp = {"jsonrpc":"2.0","id":req["id"],"result":{"protocolVersion":"2024-11-05","capabilities":{},"serverInfo":{"name":"test-mcp","version":"0.1"}}}
print(json.dumps(resp))
FIXTURE

cat > "$WD/.copilot/mcp-config.json" << 'FIXTURE'
{"mcpServers":{"test-server":{"command":"python3","args":[".copilot/test-mcp-server.py"]}}}
FIXTURE

cat > "$WD/.vscode/mcp.json" << 'FIXTURE'
{"mcpServers":{"vscode-test-server":{"command":"python3","args":[".copilot/test-mcp-server.py"]}}}
FIXTURE

# --- Skills ---
mkdir -p "$WD/.github/skills/test-skill/scripts"
mkdir -p "$WD/.claude/skills/deploy"

cat > "$WD/.github/skills/test-skill/SKILL.md" << 'FIXTURE'
---
name: test-skill
description: A test skill for integration testing
---
Test skill content.
FIXTURE

cat > "$WD/.github/skills/test-skill/scripts/helper.sh" << 'FIXTURE'
#!/bin/bash
echo "helper"
FIXTURE
chmod +x "$WD/.github/skills/test-skill/scripts/helper.sh"

cat > "$WD/.claude/skills/deploy/SKILL.md" << 'FIXTURE'
---
name: deploy
description: Deploy skill for testing
---
Deploy skill content.
FIXTURE

# --- Custom agents ---
mkdir -p "$WD/.github/agents" "$WD/.claude/agents"

cat > "$WD/.github/agents/reviewer.agent.md" << 'FIXTURE'
---
name: reviewer
description: Code reviewer agent
tools: ["bash", "view"]
---
You are a code reviewer.
FIXTURE

cat > "$WD/.claude/agents/helper.agent.md" << 'FIXTURE'
---
name: helper
description: Helper agent
---
You are a helper.
FIXTURE

# --- Commands ---
mkdir -p "$WD/.claude/commands"

cat > "$WD/.claude/commands/test-command.md" << 'FIXTURE'
Test command content.
FIXTURE

# --- Hooks ---
mkdir -p "$WD/.github/hooks/scripts"

cat > "$WD/.github/hooks/test-hooks.json" << 'FIXTURE'
{"version":1,"hooks":{
  "sessionStart":[{"type":"command","bash":".github/hooks/scripts/test-hook.sh session-start","cwd":"."}],
  "preToolUse":[{"type":"command","bash":".github/hooks/scripts/test-hook.sh pre-tool-use","cwd":"."}]
}}
FIXTURE

cat > "$WD/.github/hooks/scripts/test-hook.sh" << 'FIXTURE'
#!/bin/bash
touch "/tmp/copilot-hook-e2e-${1:-unknown}"
cat > /dev/null 2>/dev/null || true
echo '{"permissionDecision":"allow"}'
FIXTURE
chmod +x "$WD/.github/hooks/scripts/test-hook.sh"

echo "fixtures-ok"
`, wd)

	out, err := exec.Command("gh", "codespace", "ssh", "-c", cs, "--", "bash", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("failed to set up test fixtures on codespace: %v\nOutput: %s", err, string(out))
	}
	if !strings.Contains(string(out), "fixtures-ok") {
		t.Fatalf("fixture setup did not complete successfully.\nOutput: %s", string(out))
	}
}

// --- Lifecycle integration tests ---

// TestIntegration_ListAvailableCodespaces verifies that list_available_codespaces
// runs gh cs list locally and returns results.
func TestIntegration_ListAvailableCodespaces(t *testing.T) {
	_ = testCodespace(t) // skip if no codespace configured

	ctx := context.Background()
	runner := &mcp.RealGHRunner{}
	out, err := runner.Run(ctx, "codespace", "list",
		"--json", "name,displayName,repository,state",
		"--limit", "50")
	if err != nil {
		t.Fatalf("gh codespace list failed: %v", err)
	}

	var codespaces []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(out), &codespaces); err != nil {
		t.Fatalf("parsing output: %v", err)
	}
	if len(codespaces) == 0 {
		t.Fatal("expected at least one codespace")
	}
	t.Logf("Found %d codespace(s)", len(codespaces))
}

// TestIntegration_ConnectCodespace verifies that connecting to an existing
// codespace sets up SSH multiplexing and registers in the registry.
func TestIntegration_ConnectCodespace(t *testing.T) {
	cs := testCodespace(t)

	ctx := context.Background()
	sshClient := ssh.NewClient(cs)
	if err := sshClient.SetupMultiplexing(ctx); err != nil {
		t.Logf("SSH multiplexing warning: %v", err)
	}

	// Verify we can run a command
	stdout, _, exitCode, err := sshClient.Exec(ctx, "echo connected-ok")
	if err != nil {
		t.Fatalf("exec failed: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("exit code %d", exitCode)
	}
	if !strings.Contains(stdout, "connected-ok") {
		t.Errorf("expected 'connected-ok', got %q", stdout)
	}

	// Register in registry
	reg := registry.New()
	if err := reg.Register(&registry.ManagedCodespace{
		Alias:    "test",
		Name:     cs,
		Executor: sshClient,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	// Verify resolution
	resolved, err := reg.Resolve("test")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.Name != cs {
		t.Errorf("resolved name %q, want %q", resolved.Name, cs)
	}
}

// TestIntegration_RemoteExplorerAgent verifies the generated remote-explorer
// agent file is created and contains the correct tools configuration.
func TestIntegration_RemoteExplorerAgent(t *testing.T) {
	_ = testCodespace(t)

	dir := t.TempDir()
	generateRemoteExplorerAgent(dir)

	agentPath := filepath.Join(dir, ".github", "agents", "remote-explorer.agent.md")
	data, err := os.ReadFile(agentPath)
	if err != nil {
		t.Fatalf("agent file not created: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, "codespace/*") {
		t.Error("agent should have codespace/* tools")
	}
	if !strings.Contains(content, "model: claude-haiku-4.5") {
		t.Error("agent should use haiku model")
	}
	if !strings.Contains(content, "remote_grep") {
		t.Error("agent instructions should mention remote_grep")
	}
}
