package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ekroon/gh-copilot-codespace/internal/mcp"
	"github.com/ekroon/gh-copilot-codespace/internal/shellpatch"
	"github.com/ekroon/gh-copilot-codespace/internal/ssh"
	"github.com/mark3labs/mcp-go/server"
)

type codespace struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Repository  string `json:"repository"`
	State       string `json:"state"`
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `Usage: gh copilot-codespace [flags] [-- copilot-args...]

Run Copilot CLI against a remote GitHub Codespace via SSH.

Flags:
  -c, --codespace NAME   Use a specific codespace (skip interactive picker)
  -w, --workdir PATH     Override workspace directory on the codespace
      --local-shell      Keep shell commands running locally

Subcommands:
  mcp                    Run as MCP server (used internally by Copilot)
  exec                   Execute a command on the codespace (used internally).
`)
}

func main() {
	// Handle --help / -h before anything else
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--help", "-h":
			printUsage()
			return
		}
	}

	// If first arg is "mcp", run as MCP server (called by copilot via --additional-mcp-config)
	if len(os.Args) > 1 && os.Args[1] == "mcp" {
		runMCPServer()
		return
	}

	// If first arg is "exec", run a command with workdir/env setup (used on codespace)
	if len(os.Args) > 1 && os.Args[1] == "exec" {
		if err := runExec(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "exec: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// Otherwise, run as interactive launcher
	if err := runLauncher(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func runMCPServer() {
	codespaceName := os.Getenv("CODESPACE_NAME")
	if codespaceName == "" {
		fmt.Fprintln(os.Stderr, "CODESPACE_NAME environment variable is required")
		os.Exit(1)
	}

	sshClient := ssh.NewClient(codespaceName)

	// Establish SSH multiplexing for fast command execution
	ctx := context.Background()
	if err := sshClient.SetupMultiplexing(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "codespace-mcp: multiplexing setup warning: %v\n", err)
	}

	mcpServer := mcp.NewServerSingle(sshClient, codespaceName)

	log.SetOutput(os.Stderr)
	log.Printf("codespace-mcp: starting for codespace %q", codespaceName)

	if err := server.ServeStdio(mcpServer); err != nil {
		log.Fatalf("codespace-mcp: server error: %v", err)
	}
}

func runLauncher(args []string) error {
	// Parse our flags (consume them, don't pass to copilot)
	localShell := false
	codespaceName := ""
	workdirOverride := ""
	var copilotArgs []string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--local-shell":
			localShell = true
		case (args[i] == "--codespace" || args[i] == "-c") && i+1 < len(args):
			codespaceName = args[i+1]
			i++
		case (args[i] == "--workdir" || args[i] == "-w") && i+1 < len(args):
			workdirOverride = args[i+1]
			i++
		default:
			copilotArgs = append(copilotArgs, args[i])
		}
	}

	// The binary serves as both launcher and MCP server
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("finding executable: %w", err)
	}

	// Select codespace: use --codespace flag or interactive picker
	var selected codespace
	if codespaceName != "" {
		selected, err = lookupCodespace(codespaceName)
	} else {
		selected, err = selectCodespace()
	}
	if err != nil {
		return err
	}
	fmt.Printf("Selected: %s (%s)\n", selected.DisplayName, selected.Repository)

	// Start codespace if needed
	if selected.State != "Available" {
		if err := startCodespace(selected.Name); err != nil {
			return err
		}
	}

	// Detect workspace directory
	var workdir string
	if workdirOverride != "" {
		workdir = workdirOverride
	} else {
		workdir, err = detectWorkdir(selected.Name, selected.Repository)
		if err != nil {
			return err
		}
	}
	fmt.Printf("Workspace: %s\n", workdir)

	// Set up SSH multiplexing early for fast file fetching (~0.1s vs ~3s per call)
	sshClient := ssh.NewClient(selected.Name)
	ctx := context.Background()
	if err := sshClient.SetupMultiplexing(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: SSH multiplexing failed, fetching will be slower: %v\n", err)
	}

	// Deploy exec agent binary to codespace for structured remote execution
	remoteBinary, err := deployBinary(sshClient, selected.Name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not deploy exec agent, using shell fallback: %v\n", err)
	}

	// Fetch instruction files into a deterministic dir that acts as the cwd
	instructionsDir, remoteMCPServers, err := fetchInstructionFiles(sshClient, selected.Name, workdir, remoteBinary)
	if err != nil {
		return fmt.Errorf("fetching instructions: %w", err)
	}

	// Prepend codespace context to copilot-instructions.md so the agent knows
	// how to route between local tools (session state) and remote tools (codespace)
	writeCodespaceInstructionsPreamble(instructionsDir, workdir)

	// Ensure the directory is trusted by copilot so it doesn't prompt each time
	if err := ensureTrustedFolder(instructionsDir); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not auto-trust directory: %v\n", err)
	}

	// Initialize as git repo so copilot treats it as a repo root and loads instructions
	exec.Command("git", "-C", instructionsDir, "init", "-q").Run()

	// Set local branch to match the codespace's current branch
	branch := detectRemoteBranch(sshClient, selected.Name, workdir)
	if branch != "" {
		exec.Command("git", "-C", instructionsDir, "symbolic-ref", "HEAD", "refs/heads/"+branch).Run()
	}

	// Generate a postToolUse hook to keep the branch in sync when the agent switches branches
	generateBranchSyncHook(instructionsDir, selected.Name, workdir, sshClient)

	// Change to the instructions dir so copilot finds the instruction files
	if err := os.Chdir(instructionsDir); err != nil {
		return fmt.Errorf("changing to instructions dir: %w", err)
	}

	// Build MCP config — points to this same binary with "mcp" subcommand,
	// plus any MCP servers from the codespace's .copilot/mcp-config.json
	mcpConfig := buildMCPConfig(self, selected.Name, workdir, remoteMCPServers, remoteBinary)

	// Excluded tools — local shell/search tools that have remote equivalents.
	// Keep edit, create, view enabled so the agent can manage local session
	// state files (plan.md, etc.) while using remote_* for codespace files.
	excludedTools := []string{
		"bash", "write_bash", "read_bash",
		"stop_bash", "list_bash", "grep", "glob",
	}

	// Forward IDE connections from codespace so copilot can auto-connect
	_, err = forwardIDEConnections(sshClient, selected.Name, instructionsDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: IDE forwarding failed: %v\n", err)
	}

	fmt.Printf("\nLaunching Copilot CLI with remote codespace tools...\n")
	fmt.Printf("  Codespace: %s\n", selected.Name)
	fmt.Printf("  Workspace: %s\n", workdir)
	if branch != "" {
		fmt.Printf("  Branch:    %s\n", branch)
	}
	fmt.Printf("  Excluded:  %d local tools\n", len(excludedTools))
	if localShell {
		fmt.Printf("  Shell:     ! commands execute locally (--local-shell)\n")
	} else {
		fmt.Printf("  Shell:     ! commands execute on codespace\n")
	}
	fmt.Printf("\n  Shell access (from another terminal):\n")
	fmt.Printf("    gh codespace ssh -c %s\n\n", selected.Name)

	// Exec copilot from the instructions dir (cwd is already set)
	if localShell {
		return execCopilot(excludedTools, mcpConfig, copilotArgs)
	}
	// Default: use shell patch so "!" commands run on the codespace
	return execCopilotWithShellPatch(excludedTools, mcpConfig, copilotArgs, sshClient, workdir, instructionsDir)
}

// lookupCodespace finds a codespace by name (exact or prefix match).
func lookupCodespace(name string) (codespace, error) {
	out, err := exec.Command("gh", "codespace", "list",
		"--json", "name,displayName,repository,state",
		"--limit", "50").Output()
	if err != nil {
		return codespace{}, fmt.Errorf("listing codespaces: %w", err)
	}

	var codespaces []codespace
	if err := json.Unmarshal(out, &codespaces); err != nil {
		return codespace{}, fmt.Errorf("parsing codespace list: %w", err)
	}

	// Try exact match first, then prefix match
	for _, cs := range codespaces {
		if cs.Name == name {
			return cs, nil
		}
	}
	for _, cs := range codespaces {
		if strings.HasPrefix(cs.Name, name) || strings.HasPrefix(cs.DisplayName, name) {
			return cs, nil
		}
	}
	return codespace{}, fmt.Errorf("codespace %q not found", name)
}

// selectCodespace lets the user pick a codespace interactively.
// Uses gum filter for fuzzy search if available, otherwise falls back to a numbered list.
func selectCodespace() (codespace, error) {
	out, err := exec.Command("gh", "codespace", "list",
		"--json", "name,displayName,repository,state",
		"--limit", "50").Output()
	if err != nil {
		return codespace{}, fmt.Errorf("listing codespaces: %w", err)
	}

	var codespaces []codespace
	if err := json.Unmarshal(out, &codespaces); err != nil {
		return codespace{}, fmt.Errorf("parsing codespace list: %w", err)
	}
	if len(codespaces) == 0 {
		return codespace{}, fmt.Errorf("no codespaces found")
	}

	// Sort: available first, then by display name
	sort.Slice(codespaces, func(i, j int) bool {
		if (codespaces[i].State == "Available") != (codespaces[j].State == "Available") {
			return codespaces[i].State == "Available"
		}
		return codespaces[i].DisplayName < codespaces[j].DisplayName
	})

	// Build display lines: "name\ticon repo: display [state]"
	lines := make([]string, len(codespaces))
	for i, cs := range codespaces {
		icon := "🟢"
		if cs.State != "Available" {
			icon = "⏸️"
		}
		lines[i] = fmt.Sprintf("%s\t%s %s: %s [%s]", cs.Name, icon, cs.Repository, cs.DisplayName, cs.State)
	}

	// Try gum filter for fuzzy interactive picker
	if gumPath, err := exec.LookPath("gum"); err == nil {
		displayLines := make([]string, len(lines))
		for i, l := range lines {
			// Show only the display part (after tab) in the picker
			parts := strings.SplitN(l, "\t", 2)
			displayLines[i] = parts[1]
		}

		cmd := exec.Command(gumPath, "filter", "--placeholder", "Choose codespace...")
		cmd.Stdin = strings.NewReader(strings.Join(displayLines, "\n"))
		cmd.Stderr = os.Stderr
		selected, err := cmd.Output()
		if err == nil {
			choice := strings.TrimSpace(string(selected))
			for i, dl := range displayLines {
				if dl == choice {
					return codespaces[i], nil
				}
			}
		}
		// gum failed (e.g., no TTY), fall through to numbered list
	}

	// Fallback: numbered list
	for i, l := range lines {
		parts := strings.SplitN(l, "\t", 2)
		fmt.Printf("  %2d) %s\n", i+1, parts[1])
	}

	fmt.Printf("\nSelect [1-%d]: ", len(codespaces))
	reader := bufio.NewReader(os.Stdin)
	input, err := reader.ReadString('\n')
	if err != nil {
		return codespace{}, fmt.Errorf("reading input: %w", err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(input))
	if err != nil || n < 1 || n > len(codespaces) {
		return codespace{}, fmt.Errorf("invalid selection")
	}
	return codespaces[n-1], nil
}

func startCodespace(name string) error {
	fmt.Println("Starting codespace (this may take a moment)...")
	time.Sleep(3 * time.Second)

	for i := 0; i < 30; i++ {
		if exec.Command("gh", "codespace", "ssh", "-c", name, "--", "echo ready").Run() == nil {
			fmt.Println("Codespace is ready!")
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("timed out waiting for codespace SSH")
}

func detectWorkdir(codespaceName, repository string) (string, error) {
	out, err := exec.Command("gh", "codespace", "ssh", "-c", codespaceName,
		"--", "ls -d /workspaces/*/ 2>/dev/null",
	).Output()
	if err != nil {
		return "/workspaces", nil
	}

	raw := strings.TrimSpace(string(out))
	if raw == "" {
		return "/workspaces", nil
	}

	// Parse directory list and strip trailing slashes
	var dirs []string
	for _, line := range strings.Split(raw, "\n") {
		d := strings.TrimRight(strings.TrimSpace(line), "/")
		if d != "" {
			dirs = append(dirs, d)
		}
	}
	if len(dirs) == 0 {
		return "/workspaces", nil
	}

	// Try automatic selection based on repository name
	repoName := repoBaseName(repository)
	chosen := chooseWorkdir(dirs, repoName)
	if chosen != "" {
		return chosen, nil
	}

	// Multiple dirs, no repo match — interactive selection
	return selectWorkdir(dirs)
}

func sshCommand(codespaceName, command string) (string, error) {
	out, err := exec.Command("gh", "codespace", "ssh", "-c", codespaceName,
		"--", command,
	).Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// execSSH runs a command on the codespace using the multiplexed SSH client.
// Falls back to gh codespace ssh if the client has no multiplexing.
func execSSH(sshClient *ssh.Client, codespaceName, command string) (string, error) {
	if sshClient != nil {
		ctx := context.Background()
		stdout, stderr, exitCode, err := sshClient.Exec(ctx, command)
		if err != nil {
			return "", err
		}
		if exitCode != 0 {
			return "", fmt.Errorf("exit %d: %s", exitCode, strings.TrimSpace(stderr))
		}
		return stdout, nil
	}
	return sshCommand(codespaceName, command)
}

func fetchInstructionFiles(sshClient *ssh.Client, codespaceName, workdir, remoteBinary string) (string, map[string]any, error) {
	// Use a deterministic directory so copilot only needs to trust it once per codespace
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", nil, fmt.Errorf("getting home dir: %w", err)
	}
	baseDir := filepath.Join(homeDir, ".copilot", "codespace-workdirs", codespaceName)
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return "", nil, fmt.Errorf("creating workdir: %w", err)
	}
	// Clean all contents except .git/ so stale instruction files don't persist
	cleanMirrorDir(baseDir)

	fmt.Println("Fetching instruction files from codespace...")

	// Discover and fetch ALL instruction files, skills, agents, commands,
	// hooks, and MCP configs in a single SSH call.
	// Each file is output as: ===FILE_BOUNDARY===\n<relpath>\n<base64-content>
	batchScript := fmt.Sprintf(`
WD=%s
SEP="===FILE_BOUNDARY==="
files=(
  $(test -f "$WD/.github/copilot-instructions.md" && echo "$WD/.github/copilot-instructions.md")
  $(find "$WD/.github/instructions" -name '*.instructions.md' 2>/dev/null)
  $(find "$WD" \( -name 'AGENTS.md' -o -name 'CLAUDE.md' -o -name 'GEMINI.md' \) 2>/dev/null | grep -v '/\.git/')
  $(test -f "$WD/.copilot/mcp-config.json" && echo "$WD/.copilot/mcp-config.json")
  $(find "$WD/.github/agents" -name '*.agent.md' 2>/dev/null)
  $(find "$WD/.claude/agents" -name '*.agent.md' 2>/dev/null)
  $(find "$WD/.github/skills" -type f 2>/dev/null)
  $(find "$WD/.agents/skills" -type f 2>/dev/null)
  $(find "$WD/.claude/skills" -type f 2>/dev/null)
  $(test -f "$WD/.vscode/mcp.json" && echo "$WD/.vscode/mcp.json")
  $(test -f "$WD/.mcp.json" && echo "$WD/.mcp.json")
  $(test -f "$WD/.github/mcp.json" && echo "$WD/.github/mcp.json")
  $(find "$WD/.claude/commands" -type f 2>/dev/null)
  $(find "$WD/.github/hooks" -name '*.json' 2>/dev/null)
)
for f in "${files[@]}"; do
  echo "$SEP"
  echo "${f#$WD/}"
  base64 < "$f"
done
echo "$SEP"
`, shellQuote(workdir))

	output, err := execSSH(sshClient, codespaceName, batchScript)
	if err != nil {
		// Non-fatal: continue with empty mirror
		fmt.Fprintf(os.Stderr, "Warning: failed to fetch instruction files: %v\n", err)
		return baseDir, nil, nil
	}

	// Parse batched output and write files
	var remoteMCPConfig map[string]any
	files := parseBatchedOutput(output, workdir)

	// MCP config locations to parse (not written to mirror)
	mcpConfigPaths := map[string]bool{
		".copilot/mcp-config.json": true,
		".vscode/mcp.json":        true,
		".mcp.json":               true,
		".github/mcp.json":        true,
	}

	for relPath, content := range files {
		if mcpConfigPaths[relPath] {
			// Parse MCP config for server rewriting instead of writing to mirror
			parsed := parseMCPConfigJSON(content)
			if parsed != nil {
				if remoteMCPConfig == nil {
					remoteMCPConfig = make(map[string]any)
				}
				for name, server := range parsed {
					if _, exists := remoteMCPConfig[name]; !exists {
						remoteMCPConfig[name] = server
						fmt.Printf("  ✓ MCP server: %s (from %s, forwarded over SSH)\n", name, relPath)
					}
				}
			}
			continue
		}
		if strings.HasPrefix(relPath, ".github/hooks/") && strings.HasSuffix(relPath, ".json") {
			// Rewrite hook commands to execute on the codespace via SSH.
			// If rewriting fails, skip the file — writing the original would
			// leave hooks that try to run scripts locally (which don't exist).
			rewritten := rewriteHooksForSSH(content, codespaceName, workdir, remoteBinary)
			if rewritten != nil {
				content = rewritten
				fmt.Printf("  ✓ %s (hooks forwarded over SSH)\n", relPath)
			} else {
				fmt.Fprintf(os.Stderr, "  ⚠ %s (skipped: could not rewrite for SSH)\n", relPath)
				continue
			}
		} else {
			fmt.Printf("  ✓ %s\n", relPath)
		}
		localPath := filepath.Join(baseDir, relPath)
		if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
			continue
		}
		if err := os.WriteFile(localPath, content, 0o644); err != nil {
			continue
		}
	}

	return baseDir, remoteMCPConfig, nil
}

const fileBoundary = "===FILE_BOUNDARY==="

// parseBatchedOutput parses the boundary-delimited output from the batch fetch script.
// Returns a map of relative paths to file contents (decoded from base64).
func parseBatchedOutput(output, workdir string) map[string][]byte {
	files := make(map[string][]byte)
	parts := strings.Split(output, fileBoundary)

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		// First line is the relative path, rest is base64-encoded content
		lines := strings.SplitN(part, "\n", 2)
		if len(lines) < 2 {
			continue
		}
		relPath := strings.TrimSpace(lines[0])
		b64Content := strings.TrimSpace(lines[1])
		if relPath == "" || b64Content == "" {
			continue
		}

		decoded, err := base64.StdEncoding.DecodeString(b64Content)
		if err != nil {
			continue
		}
		files[relPath] = decoded
	}

	return files
}

// parseMCPConfigJSON parses .copilot/mcp-config.json content and rewrites servers for SSH forwarding.
func parseMCPConfigJSON(content []byte) map[string]any {
	var config map[string]any
	if err := json.Unmarshal(content, &config); err != nil {
		return nil
	}
	servers, ok := config["mcpServers"].(map[string]any)
	if !ok {
		return nil
	}
	// The caller (buildMCPConfig) handles the rewriting via rewriteMCPServerForSSH
	rewritten := make(map[string]any)
	for name, raw := range servers {
		if server, ok := raw.(map[string]any); ok {
			rewritten[name] = server
		}
	}
	if len(rewritten) == 0 {
		return nil
	}
	return rewritten
}

// writeCodespaceInstructionsPreamble prepends a codespace-context section to the
// copilot-instructions.md in the mirror dir. If the file doesn't exist, it creates it.
// This tells the agent how to route between local and remote tools.
func writeCodespaceInstructionsPreamble(mirrorDir, workdir string) {
	preamble := fmt.Sprintf(`# Codespace Remote Development

You are working on a remote GitHub Codespace. Source code lives on the codespace at %s, NOT locally.

## Tool routing

- **Source code** (viewing, editing, creating, searching files in the project): use remote_* tools (remote_view, remote_edit, remote_create, remote_bash, remote_grep, remote_glob)
- **Local session files** (plan.md, session state, notes under ~/.copilot/): use the built-in local tools (view, edit, create)
- **Shell commands**: use remote_bash (runs on the codespace), NOT the local bash

`, workdir)

	instructionsPath := filepath.Join(mirrorDir, ".github", "copilot-instructions.md")
	if err := os.MkdirAll(filepath.Dir(instructionsPath), 0o755); err != nil {
		return
	}

	existing, err := os.ReadFile(instructionsPath)
	if err != nil {
		// File doesn't exist; create with just the preamble
		os.WriteFile(instructionsPath, []byte(preamble), 0o644)
		return
	}
	// Prepend preamble to existing content
	combined := preamble + string(existing)
	os.WriteFile(instructionsPath, []byte(combined), 0o644)
}

// cleanMirrorDir removes all contents of the mirror directory except .git/,
// ensuring stale instruction files don't persist across fetches.
func cleanMirrorDir(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.Name() == ".git" {
			continue
		}
		os.RemoveAll(filepath.Join(dir, e.Name()))
	}
}

// ensureTrustedFolder adds the directory to copilot's trusted_folders config if not already present.
func ensureTrustedFolder(dir string) error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	configPath := filepath.Join(homeDir, ".copilot", "config.json")

	data, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}

	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		return err
	}

	// Check if already trusted
	trusted, _ := config["trusted_folders"].([]any)
	for _, f := range trusted {
		if s, ok := f.(string); ok && s == dir {
			return nil // already trusted
		}
	}

	// Add to trusted folders
	trusted = append(trusted, dir)
	config["trusted_folders"] = trusted

	out, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath, out, 0o644)
}

func buildMCPConfig(selfBinary, codespaceName, workdir string, remoteMCPServers map[string]any, remoteBinary string) string {
	servers := map[string]any{
		"codespace": map[string]any{
			"type":    "local",
			"command": selfBinary,
			"args":    []string{"mcp"},
			"env": map[string]string{
				"CODESPACE_NAME":    codespaceName,
				"CODESPACE_WORKDIR": workdir,
			},
			"tools": []string{"*"},
		},
	}

	// Merge remote MCP servers (rewritten to forward over SSH)
	for name, serverConfig := range remoteMCPServers {
		if name == "codespace" {
			continue // don't override our own server
		}
		if server, ok := serverConfig.(map[string]any); ok {
			rewritten := rewriteMCPServerForSSH(server, codespaceName, workdir, remoteBinary)
			if rewritten != nil {
				servers[name] = rewritten
			}
		}
	}

	config := map[string]any{
		"mcpServers": servers,
	}
	b, _ := json.Marshal(config)
	return string(b)
}

// rewriteMCPServerForSSH rewrites an MCP server config to forward its stdio over SSH.
// When remoteBinary is available, uses structured exec args instead of shell assembly.
func rewriteMCPServerForSSH(server map[string]any, codespaceName, workdir, remoteBinary string) map[string]any {
	command, _ := server["command"].(string)
	if command == "" {
		return nil
	}

	// When remote binary is deployed, use structured exec (no shell escaping needed)
	if remoteBinary != "" {
		args := []string{"codespace", "ssh", "-c", codespaceName, "--",
			remoteBinary, "exec", "--workdir", workdir}

		// Add env vars as structured flags
		if env, ok := server["env"].(map[string]any); ok {
			for k, v := range env {
				if s, ok := v.(string); ok {
					args = append(args, "--env", k+"="+s)
				}
			}
		}

		// Add command and its args after --
		args = append(args, "--", command)
		if cmdArgs, ok := server["args"].([]any); ok {
			for _, arg := range cmdArgs {
				if s, ok := arg.(string); ok {
					args = append(args, s)
				}
			}
		}

		return map[string]any{
			"type":    "local",
			"command": "gh",
			"args":    args,
		}
	}

	// Fallback: shell assembly (when remote binary not available)
	remoteCmd := command
	if args, ok := server["args"].([]any); ok {
		for _, arg := range args {
			if s, ok := arg.(string); ok {
				remoteCmd += " " + s
			}
		}
	}

	envPrefix := fmt.Sprintf("cd %s", workdir)
	if env, ok := server["env"].(map[string]any); ok {
		for k, v := range env {
			if s, ok := v.(string); ok {
				envPrefix += fmt.Sprintf(" && export %s=%s", k, s)
			}
		}
	}
	remoteCmd = envPrefix + " && exec " + remoteCmd

	return map[string]any{
		"type":    "local",
		"command": "gh",
		"args":    []string{"codespace", "ssh", "-c", codespaceName, "--", "bash", "-c", shellQuote(remoteCmd)},
	}
}

// rewriteHooksForSSH rewrites hook commands in a hooks JSON file to execute
// on the codespace via SSH. When remoteBinary is available, uses structured
// exec args. Otherwise falls back to shell assembly.
func rewriteHooksForSSH(content []byte, codespaceName, workdir, remoteBinary string) []byte {
	var config map[string]any
	if err := json.Unmarshal(content, &config); err != nil {
		return nil
	}

	hooks, ok := config["hooks"].(map[string]any)
	if !ok {
		return nil
	}

	modified := false
	for event, handlers := range hooks {
		handlerList, ok := handlers.([]any)
		if !ok {
			continue
		}
		for i, handler := range handlerList {
			h, ok := handler.(map[string]any)
			if !ok {
				continue
			}
			bashCmd, ok := h["bash"].(string)
			if !ok || bashCmd == "" {
				continue
			}

			// Build remote command with cd to workdir (+ optional cwd)
			remoteCwd := workdir
			if cwd, ok := h["cwd"].(string); ok && cwd != "" && cwd != "." {
				remoteCwd = workdir + "/" + cwd
			}

			if remoteBinary != "" {
				// Structured exec via remote binary (no shell escaping)
				// Double-quote the bash command: once for local shell (which consumes
				// the hook's bash field), once for the remote shell (SSH).
				execArgs := remoteBinary + " exec --workdir " + shellQuote(remoteCwd)
				if env, ok := h["env"].(map[string]any); ok {
					for k, v := range env {
						if s, ok := v.(string); ok {
							execArgs += " --env " + shellQuote(k+"="+s)
						}
					}
				}
				execArgs += " -- bash -c " + shellQuote(shellQuote(bashCmd))
				h["bash"] = fmt.Sprintf("gh codespace ssh -c %s -- %s", codespaceName, execArgs)
			} else {
				// Fallback: shell assembly
				envPrefix := ""
				if env, ok := h["env"].(map[string]any); ok {
					for k, v := range env {
						if s, ok := v.(string); ok {
							envPrefix += fmt.Sprintf("export %s=%s && ", k, shellQuote(s))
						}
					}
				}
				remoteCmd := fmt.Sprintf("cd %s && %s%s", shellQuote(remoteCwd), envPrefix, bashCmd)
				h["bash"] = fmt.Sprintf("gh codespace ssh -c %s -- bash -c %s", codespaceName, shellQuote(shellQuote(remoteCmd)))
			}

			// Clear cwd and env since they're baked into the SSH command
			delete(h, "cwd")
			delete(h, "env")
			handlerList[i] = h
			modified = true
		}
		hooks[event] = handlerList
	}

	if !modified {
		return nil
	}

	config["hooks"] = hooks
	out, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return nil
	}
	return out
}

// repoBaseName extracts the repository name from an "owner/repo" string.
func repoBaseName(repository string) string {
	if i := strings.LastIndex(repository, "/"); i >= 0 {
		return repository[i+1:]
	}
	return repository
}

// chooseWorkdir picks the best workspace directory from a list given the repo name.
// Returns the best match or "" if interactive selection is needed.
func chooseWorkdir(dirs []string, repoName string) string {
	if len(dirs) == 1 {
		return dirs[0]
	}
	if repoName == "" {
		return ""
	}
	for _, d := range dirs {
		base := filepath.Base(d)
		if base == repoName {
			return d
		}
	}
	return ""
}

// selectWorkdir lets the user pick a workspace directory interactively.
func selectWorkdir(dirs []string) (string, error) {
	if len(dirs) == 0 {
		return "/workspaces", nil
	}

	// Try gum filter for fuzzy interactive picker
	if gumPath, err := exec.LookPath("gum"); err == nil {
		cmd := exec.Command(gumPath, "filter", "--placeholder", "Choose workspace directory...")
		cmd.Stdin = strings.NewReader(strings.Join(dirs, "\n"))
		cmd.Stderr = os.Stderr
		selected, err := cmd.Output()
		if err == nil {
			choice := strings.TrimSpace(string(selected))
			if choice != "" {
				return choice, nil
			}
		}
	}

	// Fallback: numbered list
	fmt.Println("Multiple workspace directories found:")
	for i, d := range dirs {
		fmt.Printf("  %2d) %s\n", i+1, d)
	}
	fmt.Printf("\nSelect [1-%d]: ", len(dirs))
	reader := bufio.NewReader(os.Stdin)
	input, err := reader.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("reading input: %w", err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(input))
	if err != nil || n < 1 || n > len(dirs) {
		return "", fmt.Errorf("invalid selection")
	}
	return dirs[n-1], nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

func execCopilot(excludedTools []string, mcpConfig string, extraArgs []string) error {
	copilotArgs := []string{
		"--excluded-tools",
	}
	copilotArgs = append(copilotArgs, excludedTools...)
	copilotArgs = append(copilotArgs, "--additional-mcp-config", mcpConfig)
	copilotArgs = append(copilotArgs, extraArgs...)

	// Try standalone copilot binary first
	if copilotPath, err := exec.LookPath("copilot"); err == nil {
		args := append([]string{"copilot"}, copilotArgs...)
		return syscall.Exec(copilotPath, args, os.Environ())
	}

	// Fall back to gh copilot (gh manages the copilot binary installation)
	ghPath, err := exec.LookPath("gh")
	if err != nil {
		return fmt.Errorf("neither 'copilot' nor 'gh' found in PATH; install copilot CLI or gh CLI")
	}

	// Use "--" so gh doesn't interpret copilot's flags
	args := append([]string{"gh", "copilot", "--"}, copilotArgs...)
	return syscall.Exec(ghPath, args, os.Environ())
}

// execCopilotWithShellPatch runs copilot's JS bundle via node with a require
// patch that intercepts the "!" shell escape and redirects it over SSH.
// This bypasses the native binary so the CJS patch can monkey-patch spawn.
// If the copilot binary is not in PATH (e.g. using gh copilot), falls back
// to execCopilot with a warning since the shell patch requires index.js.
func execCopilotWithShellPatch(excludedTools []string, mcpConfig string, extraArgs []string, sshClient *ssh.Client, workdir, mirrorDir string) error {
	// Find copilot's index.js (the JS bundle) — requires copilot in PATH
	indexJS, err := findCopilotIndexJS()
	if err != nil {
		// Shell patch needs the copilot JS bundle which isn't available via gh copilot.
		// Fall back to execCopilot (which handles gh copilot fallback).
		fmt.Fprintf(os.Stderr, "Warning: shell patch unavailable (%v); '!' commands will run locally\n", err)
		return execCopilot(excludedTools, mcpConfig, extraArgs)
	}

	// Write the CJS patch to a temp file
	patchPath, err := shellpatch.WritePatch()
	if err != nil {
		return fmt.Errorf("writing shell patch: %w", err)
	}
	defer os.RemoveAll(filepath.Dir(patchPath))

	// Find node binary
	nodePath, err := exec.LookPath("node")
	if err != nil {
		return fmt.Errorf("node not found in PATH: %w", err)
	}

	// Build args: node --require <patch.cjs> <index.js> <copilot-args...>
	args := []string{"node", "--require", patchPath, indexJS,
		"--excluded-tools",
	}
	args = append(args, excludedTools...)
	args = append(args, "--additional-mcp-config", mcpConfig)
	args = append(args, extraArgs...)

	// Add SSH connection info and workdir to env for the patch script
	env := os.Environ()
	if sshClient.SSHConfigPath() != "" {
		env = append(env, "COPILOT_SSH_CONFIG="+sshClient.SSHConfigPath())
		env = append(env, "COPILOT_SSH_HOST="+sshClient.SSHHost())
	}
	env = append(env, "CODESPACE_WORKDIR="+workdir)
	env = append(env, "CODESPACE_MIRROR_DIR="+mirrorDir)

	// Pre-fetch the auth token from keychain so node doesn't trigger a
	// macOS keychain prompt (the keychain ACL only trusts the native binary).
	if token := readCopilotToken(); token != "" {
		env = append(env, "COPILOT_GITHUB_TOKEN="+token)
	}

	return syscall.Exec(nodePath, args, env)
}

// findCopilotIndexJS locates copilot's index.js by following the symlink chain
// from the `copilot` binary → npm-loader.js → index.js in the same directory.
func findCopilotIndexJS() (string, error) {
	copilotPath, err := exec.LookPath("copilot")
	if err != nil {
		return "", fmt.Errorf("copilot not found in PATH: %w", err)
	}

	// Resolve symlinks to get the actual npm-loader.js path
	realPath, err := filepath.EvalSymlinks(copilotPath)
	if err != nil {
		return "", fmt.Errorf("resolving copilot path: %w", err)
	}

	// index.js is in the same directory as npm-loader.js
	dir := filepath.Dir(realPath)
	indexJS := filepath.Join(dir, "index.js")

	if _, err := os.Stat(indexJS); err != nil {
		return "", fmt.Errorf("copilot index.js not found at %s", indexJS)
	}

	return indexJS, nil
}

// readCopilotToken obtains a GitHub token for copilot auth.
// Uses `gh auth token` to avoid macOS keychain popups (the keychain ACL
// only trusts the native copilot binary, not node).
// Returns empty string on any failure.
func readCopilotToken() string {
	// Skip if already set via env
	for _, key := range []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"} {
		if os.Getenv(key) != "" {
			return ""
		}
	}

	out, err := exec.Command("gh", "auth", "token").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// detectRemoteBranch reads the current git branch from the codespace via SSH.
func detectRemoteBranch(sshClient *ssh.Client, codespaceName, workdir string) string {
	cmd := fmt.Sprintf("git -C %s rev-parse --abbrev-ref HEAD 2>/dev/null", shellQuote(workdir))
	out, err := execSSH(sshClient, codespaceName, cmd)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// generateBranchSyncHook writes a postToolUse hook that keeps the local mirror's
// git branch in sync with the codespace after remote_bash commands.
func generateBranchSyncHook(mirrorDir, codespaceName, workdir string, sshClient *ssh.Client) {
	hooksDir := filepath.Join(mirrorDir, ".github", "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return
	}

	// Build SSH command: prefer multiplexed SSH for speed (~0.1s vs ~3s)
	sshCmd := fmt.Sprintf("gh codespace ssh -c %s -- git -C %s rev-parse --abbrev-ref HEAD", shellQuote(codespaceName), shellQuote(workdir))
	if sshClient.SSHConfigPath() != "" {
		sshCmd = fmt.Sprintf("ssh -F %s %s git -C %s rev-parse --abbrev-ref HEAD",
			shellQuote(sshClient.SSHConfigPath()), shellQuote(sshClient.SSHHost()), shellQuote(workdir))
	}

	// Use lenient matching: MCP tools may be namespaced (e.g., mcp__codespace__remote_bash)
	script := fmt.Sprintf(
		`INPUT=$(cat); echo "$INPUT" | grep -q 'remote_bash' || exit 0; branch=$(%s 2>/dev/null); [ -n "$branch" ] && git -C %s symbolic-ref HEAD "refs/heads/$branch" 2>/dev/null; exit 0`,
		sshCmd, shellQuote(mirrorDir),
	)

	hook := map[string]any{
		"version": 1,
		"hooks": map[string]any{
			"postToolUse": []any{
				map[string]any{
					"type":       "command",
					"bash":       script,
					"timeoutSec": 10,
				},
			},
		},
	}

	data, err := json.MarshalIndent(hook, "", "  ")
	if err != nil {
		return
	}
	os.WriteFile(filepath.Join(hooksDir, "branch-sync.json"), data, 0o644)
}
