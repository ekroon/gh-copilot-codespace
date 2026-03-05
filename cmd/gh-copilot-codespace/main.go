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
	"github.com/ekroon/gh-copilot-codespace/internal/registry"
	"github.com/ekroon/gh-copilot-codespace/internal/ssh"
	"github.com/ekroon/gh-copilot-codespace/internal/workspace"
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

Run Copilot CLI against remote GitHub Codespace(s) via SSH.

Flags:
  -c, --codespace NAME   Use a specific codespace (repeatable, or comma-separated)
  -w, --workdir PATH     Override workspace directory on the codespace
      --name SESSION      Name for the local workspace session
      --resume SESSION    Re-attach to a previous workspace session
      --local-tools       Keep all local tools (bash, grep, glob) enabled alongside remote_* tools

Subcommands:
  mcp                    Run as MCP server (used internally by Copilot)
  exec                   Execute a command on the codespace (used internally)
  workspaces             List available workspace sessions
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

	// If first arg is "workspaces", list/manage workspace sessions
	if len(os.Args) > 1 && os.Args[1] == "workspaces" {
		if err := runWorkspaces(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
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
	// Support multi-codespace via CODESPACE_REGISTRY env var (JSON)
	// Falls back to single CODESPACE_NAME for backward compatibility
	registryJSON := os.Getenv("CODESPACE_REGISTRY")

	var reg *registry.Registry
	if registryJSON != "" {
		var err error
		reg, err = registryFromJSON(registryJSON)
		if err != nil {
			fmt.Fprintf(os.Stderr, "codespace-mcp: invalid CODESPACE_REGISTRY: %v\n", err)
			os.Exit(1)
		}
	} else {
		codespaceName := os.Getenv("CODESPACE_NAME")
		if codespaceName == "" {
			fmt.Fprintln(os.Stderr, "CODESPACE_NAME or CODESPACE_REGISTRY environment variable is required")
			os.Exit(1)
		}
		sshClient := ssh.NewClient(codespaceName)
		ctx := context.Background()
		if err := sshClient.SetupMultiplexing(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "codespace-mcp: multiplexing setup warning: %v\n", err)
		}
		workdir := os.Getenv("CODESPACE_WORKDIR")
		reg = registry.New()
		reg.Register(&registry.ManagedCodespace{
			Alias:    registry.DefaultAlias(codespaceName, nil),
			Name:     codespaceName,
			Workdir:  workdir,
			Executor: sshClient,
		})
	}

	mcpServer := mcp.NewServer(reg)

	log.SetOutput(os.Stderr)
	log.Printf("codespace-mcp: starting with %d codespace(s)", reg.Len())

	if err := server.ServeStdio(mcpServer); err != nil {
		log.Fatalf("codespace-mcp: server error: %v", err)
	}
}

// registryEntry is the JSON-serializable form of a codespace for MCP config env.
type registryEntry struct {
	Alias      string `json:"alias"`
	Name       string `json:"name"`
	Repository string `json:"repository"`
	Branch     string `json:"branch"`
	Workdir    string `json:"workdir"`
}

// registryFromJSON deserializes CODESPACE_REGISTRY env var and creates SSH clients.
func registryFromJSON(data string) (*registry.Registry, error) {
	var entries []registryEntry
	if err := json.Unmarshal([]byte(data), &entries); err != nil {
		return nil, fmt.Errorf("parsing registry: %w", err)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("empty registry")
	}

	reg := registry.New()
	ctx := context.Background()
	for _, e := range entries {
		sshClient := ssh.NewClient(e.Name)
		if err := sshClient.SetupMultiplexing(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "codespace-mcp: multiplexing warning for %s: %v\n", e.Alias, err)
		}
		if e.Workdir != "" {
			sshClient.SetWorkdir(e.Workdir)
		}
		reg.Register(&registry.ManagedCodespace{
			Alias:      e.Alias,
			Name:       e.Name,
			Repository: e.Repository,
			Branch:     e.Branch,
			Workdir:    e.Workdir,
			Executor:   sshClient,
		})
	}
	return reg, nil
}

func runLauncher(args []string) error {
	// Parse our flags (consume them, don't pass to copilot)
	var codespaceNames []string
	workdirOverride := ""
	sessionName := ""
	resumeSession := ""
	localTools := false
	var copilotArgs []string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--local-tools":
			localTools = true
		case (args[i] == "--codespace" || args[i] == "-c") && i+1 < len(args):
			// Support comma-separated: -c cs1,cs2
			for _, name := range strings.Split(args[i+1], ",") {
				name = strings.TrimSpace(name)
				if name != "" {
					codespaceNames = append(codespaceNames, name)
				}
			}
			i++
		case (args[i] == "--workdir" || args[i] == "-w") && i+1 < len(args):
			workdirOverride = args[i+1]
			i++
		case args[i] == "--name" && i+1 < len(args):
			sessionName = args[i+1]
			i++
		case args[i] == "--resume" && i+1 < len(args):
			resumeSession = args[i+1]
			i++
		default:
			copilotArgs = append(copilotArgs, args[i])
		}
	}

	// Handle --resume: load workspace and reconnect to codespaces
	if resumeSession != "" {
		return runResume(resumeSession, copilotArgs)
	}

	// The binary serves as both launcher and MCP server
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("finding executable: %w", err)
	}

	// Select codespace(s): use --codespace flag(s) or interactive picker
	var selectedList []codespace
	if len(codespaceNames) > 0 {
		for _, name := range codespaceNames {
			cs, err := lookupCodespace(name)
			if err != nil {
				return fmt.Errorf("codespace %q: %w", name, err)
			}
			selectedList = append(selectedList, cs)
		}
	} else {
		selected, err := selectCodespace()
		if err != nil {
			return err
		}
		selectedList = append(selectedList, selected)
	}

	ctx := context.Background()
	reg := registry.New()
	var firstSSHClient *ssh.Client
	var firstWorkdir, firstRemoteBinary string
	var allRemoteMCPServers map[string]any

	for _, selected := range selectedList {
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
		fmt.Printf("  Workspace: %s\n", workdir)

		// Set up SSH multiplexing early for fast file fetching
		sshClient := ssh.NewClient(selected.Name)
		if err := sshClient.SetupMultiplexing(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: SSH multiplexing failed for %s: %v\n", selected.Name, err)
		}

		// Deploy exec agent binary
		remoteBinary, err := deployBinary(sshClient, selected.Name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not deploy exec agent for %s: %v\n", selected.Name, err)
		}

		// Detect branch
		branch := detectRemoteBranch(sshClient, selected.Name, workdir)

		alias := registry.DefaultAlias(selected.Repository, reg.Aliases())
		sshClient.SetWorkdir(workdir)
		reg.Register(&registry.ManagedCodespace{
			Alias:      alias,
			Name:       selected.Name,
			Repository: selected.Repository,
			Branch:     branch,
			Workdir:    workdir,
			Executor:   sshClient,
			ExecAgent:  remoteBinary,
		})

		if firstSSHClient == nil {
			firstSSHClient = sshClient
			firstWorkdir = workdir
			firstRemoteBinary = remoteBinary
		}
	}

	// Use the first codespace for instruction fetching (primary codespace)
	primary := selectedList[0]

	// Fetch instruction files into a deterministic dir that acts as the cwd
	instructionsDir, remoteMCPServers, err := fetchInstructionFiles(firstSSHClient, primary.Name, firstWorkdir, firstRemoteBinary)
	if err != nil {
		return fmt.Errorf("fetching instructions: %w", err)
	}
	allRemoteMCPServers = remoteMCPServers

	// Prepend codespace context to copilot-instructions.md
	if reg.Len() > 1 {
		writeMultiCodespaceInstructionsPreamble(instructionsDir, reg)
	} else {
		writeCodespaceInstructionsPreamble(instructionsDir, firstWorkdir)
	}

	// Ensure the directory is trusted by copilot so it doesn't prompt each time
	if err := ensureTrustedFolder(instructionsDir); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not auto-trust directory: %v\n", err)
	}

	// Initialize as git repo so copilot treats it as a repo root and loads instructions
	exec.Command("git", "-C", instructionsDir, "init", "-q").Run()

	// Set local branch to match the primary codespace's current branch
	if all := reg.All(); len(all) > 0 && all[0].Branch != "" {
		exec.Command("git", "-C", instructionsDir, "symbolic-ref", "HEAD", "refs/heads/"+all[0].Branch).Run()
	}

	// Generate a postToolUse hook to keep the branch in sync
	generateBranchSyncHook(instructionsDir, primary.Name, firstWorkdir, firstSSHClient)

	// Generate remote-explorer custom agent for codespace file exploration
	generateRemoteExplorerAgent(instructionsDir)

	// Change to the instructions dir so copilot finds the instruction files
	if err := os.Chdir(instructionsDir); err != nil {
		return fmt.Errorf("changing to instructions dir: %w", err)
	}

	// Build MCP config with registry serialization for multi-CS support
	mcpConfig := buildMCPConfigWithRegistry(self, reg, allRemoteMCPServers)

	// Excluded tools
	var excludedTools []string
	if !localTools {
		excludedTools = []string{
			"bash", "write_bash", "read_bash",
			"stop_bash", "list_bash", "grep", "glob",
		}
	}

	// Forward IDE connections from all connected codespaces
	for _, cs := range reg.All() {
		if sshClient, ok := cs.Executor.(*ssh.Client); ok && sshClient.SSHConfigPath() != "" {
			_, err = forwardIDEConnections(sshClient, cs.Name, instructionsDir, cs.Workdir)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: IDE forwarding failed for %s: %v\n", cs.Alias, err)
			}
		}
	}

	// Save workspace manifest for --resume
	ws, wsErr := workspace.New(sessionName)
	if wsErr == nil {
		for _, cs := range reg.All() {
			ws.AddCodespace(cs.Alias, workspace.CodespaceEntry{
				Name:       cs.Name,
				Repository: cs.Repository,
				Branch:     cs.Branch,
				Workdir:    cs.Workdir,
			})
		}
		ws.Save()
		fmt.Printf("  Session:   %s (resume with --resume %s)\n", ws.Name, ws.Name)
	}

	fmt.Printf("\nLaunching Copilot CLI with remote codespace tools...\n")
	for _, cs := range reg.All() {
		fmt.Printf("  Codespace: %s (alias: %s, repo: %s)\n", cs.Name, cs.Alias, cs.Repository)
	}
	fmt.Printf("  Excluded:  %d local tools\n", len(excludedTools))
	fmt.Printf("\n")

	// Exec copilot
	return execCopilot(excludedTools, mcpConfig, copilotArgs)
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
- **Exploring the codebase**: delegate to @remote-explorer instead of the built-in explore agent (the built-in explore agent cannot access remote files)

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
// files/ (user-created artifacts), and workspace.json (session manifest),
// ensuring stale instruction files don't persist across fetches.
func cleanMirrorDir(dir string) {
	preserve := map[string]bool{
		".git":           true,
		"files":          true,
		"workspace.json": true,
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if preserve[e.Name()] {
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

// buildMCPConfigWithRegistry creates the MCP config JSON using the full registry.
// Uses CODESPACE_REGISTRY env var (JSON array) for multi-codespace support.
func buildMCPConfigWithRegistry(selfBinary string, reg *registry.Registry, remoteMCPServers map[string]any) string {
	// Serialize registry entries for the MCP server process
	var entries []registryEntry
	for _, cs := range reg.All() {
		entries = append(entries, registryEntry{
			Alias:      cs.Alias,
			Name:       cs.Name,
			Repository: cs.Repository,
			Branch:     cs.Branch,
			Workdir:    cs.Workdir,
		})
	}
	registryJSON, _ := json.Marshal(entries)

	servers := map[string]any{
		"codespace": map[string]any{
			"type":    "local",
			"command": selfBinary,
			"args":    []string{"mcp"},
			"env": map[string]string{
				"CODESPACE_REGISTRY": string(registryJSON),
			},
			"tools": []string{"*"},
		},
	}

	// Merge remote MCP servers using the primary codespace for SSH forwarding
	if len(reg.All()) > 0 {
		primary := reg.All()[0]
		for name, serverConfig := range remoteMCPServers {
			if name == "codespace" {
				continue
			}
			if server, ok := serverConfig.(map[string]any); ok {
				rewritten := rewriteMCPServerForSSH(server, primary.Name, primary.Workdir, primary.ExecAgent)
				if rewritten != nil {
					servers[name] = rewritten
				}
			}
		}
	}

	config := map[string]any{
		"mcpServers": servers,
	}
	b, _ := json.Marshal(config)
	return string(b)
}

// writeMultiCodespaceInstructionsPreamble writes a preamble listing all connected codespaces.
func writeMultiCodespaceInstructionsPreamble(mirrorDir string, reg *registry.Registry) {
	var sb strings.Builder
	sb.WriteString("# Multi-Codespace Remote Development\n\n")
	sb.WriteString("You are connected to multiple remote GitHub Codespaces. Source code lives on the codespaces, NOT locally.\n\n")
	sb.WriteString("## Connected codespaces\n\n")
	sb.WriteString("| Alias | Repository | Branch | Workdir |\n")
	sb.WriteString("|-------|-----------|--------|--------|\n")
	for _, cs := range reg.All() {
		branch := cs.Branch
		if branch == "" {
			branch = "(default)"
		}
		sb.WriteString(fmt.Sprintf("| %s | %s | %s | %s |\n", cs.Alias, cs.Repository, branch, cs.Workdir))
	}
	sb.WriteString("\n## Tool routing\n\n")
	sb.WriteString("- **All remote_* tools** accept an optional `codespace` parameter. Use the alias name to target a specific codespace.\n")
	sb.WriteString("- Use `list_codespaces` to see connected codespaces.\n")
	sb.WriteString("- **Local session files** (plan.md, session state): use built-in local tools (view, edit, create)\n")
	sb.WriteString("- **Shell commands**: use `remote_bash` with the `codespace` parameter\n")
	sb.WriteString("- **Exploring the codebase**: delegate to @remote-explorer instead of the built-in explore agent\n\n")

	instructionsPath := filepath.Join(mirrorDir, ".github", "copilot-instructions.md")
	if err := os.MkdirAll(filepath.Dir(instructionsPath), 0o755); err != nil {
		return
	}

	existing, err := os.ReadFile(instructionsPath)
	if err != nil {
		os.WriteFile(instructionsPath, []byte(sb.String()), 0o644)
		return
	}
	os.WriteFile(instructionsPath, []byte(sb.String()+string(existing)), 0o644)
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

// generateRemoteExplorerAgent creates a custom agent that can explore codespace
// files using remote_* MCP tools. This replaces the built-in explore agent which
// can't access remote tools (its local grep/glob/view are excluded).
func generateRemoteExplorerAgent(mirrorDir string) {
	agentsDir := filepath.Join(mirrorDir, ".github", "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		return
	}

	agent := `---
name: remote-explorer
description: >-
  Explore and search codespace files using remote tools. Use this agent instead of the built-in
  explore agent when you need to search, read, or understand code on a remote codespace.
  Delegates to this agent are appropriate for: finding files, searching code patterns,
  understanding codebase structure, reading specific files, and answering questions about code.
model: claude-haiku-4.5
tools:
  - codespace/*
  - read
  - search
---

You are a fast code exploration agent for remote GitHub Codespaces.

## Available tools

Use these remote tools to explore the codespace:
- **remote_grep** — search for patterns in files (ripgrep)
- **remote_glob** — find files by name patterns
- **remote_view** — read file contents with line numbers
- **remote_bash** — run commands (e.g., find, wc, head, git log)
- **remote_cwd** — check current working directory

## Guidelines

- Be concise — return focused answers under 300 words
- Search broadly first, then narrow down
- Use remote_grep for content search, remote_glob for file discovery
- Read only the relevant portions of files (use view_range)
- When exploring structure, use remote_bash with find or ls
`

	os.WriteFile(filepath.Join(agentsDir, "remote-explorer.agent.md"), []byte(agent), 0o644)
}

// runResume loads a workspace session and reconnects to its codespaces.
func runResume(sessionName string, copilotArgs []string) error {
	ws, err := workspace.Load(sessionName)
	if err != nil {
		return fmt.Errorf("loading workspace %q: %w", sessionName, err)
	}

	if len(ws.Manifest.Codespaces) == 0 {
		return fmt.Errorf("workspace %q has no codespaces", sessionName)
	}

	fmt.Printf("Resuming workspace %q...\n", sessionName)

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("finding executable: %w", err)
	}

	ctx := context.Background()
	reg := registry.New()

	for alias, entry := range ws.Manifest.Codespaces {
		fmt.Printf("  Reconnecting %s (%s)...\n", alias, entry.Name)

		// Check if codespace still exists and start if needed
		if err := startCodespace(entry.Name); err != nil {
			fmt.Fprintf(os.Stderr, "  ⚠ Codespace %s unavailable: %v (skipping)\n", alias, err)
			continue
		}

		sshClient := ssh.NewClient(entry.Name)
		if err := sshClient.SetupMultiplexing(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "  ⚠ SSH failed for %s: %v (skipping)\n", alias, err)
			continue
		}

		sshClient.SetWorkdir(entry.Workdir)
		reg.Register(&registry.ManagedCodespace{
			Alias:      alias,
			Name:       entry.Name,
			Repository: entry.Repository,
			Branch:     entry.Branch,
			Workdir:    entry.Workdir,
			Executor:   sshClient,
		})
		fmt.Printf("  ✓ %s connected\n", alias)
	}

	if reg.Len() == 0 {
		return fmt.Errorf("no codespaces could be reconnected")
	}

	// Reuse the workspace directory (don't clean it — preserve local files)
	instructionsDir := ws.Dir

	// Re-fetch instructions (branches may have changed)
	if all := reg.All(); len(all) > 0 {
		primary := all[0]
		remoteBinary, _ := deployBinary(primary.Executor.(*ssh.Client), primary.Name)
		fetchInstructionFiles(primary.Executor.(*ssh.Client), primary.Name, primary.Workdir, remoteBinary)

		if reg.Len() > 1 {
			writeMultiCodespaceInstructionsPreamble(instructionsDir, reg)
		} else {
			writeCodespaceInstructionsPreamble(instructionsDir, primary.Workdir)
		}
	}

	if err := ensureTrustedFolder(instructionsDir); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not auto-trust directory: %v\n", err)
	}

	generateRemoteExplorerAgent(instructionsDir)

	if err := os.Chdir(instructionsDir); err != nil {
		return fmt.Errorf("changing to workspace dir: %w", err)
	}

	mcpConfig := buildMCPConfigWithRegistry(self, reg, nil)

	excludedTools := []string{
		"bash", "write_bash", "read_bash",
		"stop_bash", "list_bash", "grep", "glob",
	}

	fmt.Printf("\nResuming with %d codespace(s)...\n", reg.Len())
	for _, cs := range reg.All() {
		fmt.Printf("  %s: %s (%s)\n", cs.Alias, cs.Name, cs.Repository)
	}
	fmt.Printf("\n")

	return execCopilot(excludedTools, mcpConfig, copilotArgs)
}

// runWorkspaces lists or manages workspace sessions.
func runWorkspaces(args []string) error {
	// Parse subcommand args
	deleteTarget := ""
	for i := 0; i < len(args); i++ {
		if args[i] == "--delete" && i+1 < len(args) {
			deleteTarget = args[i+1]
			i++
		}
	}

	if deleteTarget != "" {
		dir := workspace.WorkspacePath(deleteTarget)
		if _, err := os.Stat(dir); err != nil {
			return fmt.Errorf("workspace %q not found", deleteTarget)
		}
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("deleting workspace: %w", err)
		}
		fmt.Printf("Deleted workspace %q\n", deleteTarget)
		return nil
	}

	// List workspaces
	list, err := workspace.List()
	if err != nil {
		return err
	}

	if len(list) == 0 {
		fmt.Println("No workspace sessions found.")
		return nil
	}

	fmt.Printf("%-30s %-20s %s\n", "Name", "Created", "Codespaces")
	fmt.Println(strings.Repeat("-", 60))
	for _, ws := range list {
		fmt.Printf("%-30s %-20s %d\n", ws.Name, ws.Created.Format("2006-01-02 15:04"), ws.CodespaceCount)
	}
	fmt.Printf("\nResume with: gh copilot-codespace --resume <name>\n")
	fmt.Printf("Delete with: gh copilot-codespace workspaces --delete <name>\n")
	return nil
}
