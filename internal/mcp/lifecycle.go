package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/ekroon/gh-copilot-codespace/internal/delegate"
	"github.com/ekroon/gh-copilot-codespace/internal/provisioner"
	"github.com/ekroon/gh-copilot-codespace/internal/registry"
	"github.com/ekroon/gh-copilot-codespace/internal/ssh"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// DeployFunc deploys the exec agent binary to a codespace.
// Returns the remote path to the deployed binary, or error.
type DeployFunc func(sshClient *ssh.Client, codespaceName string) (string, error)

// LifecycleConfig holds dependencies for lifecycle tool handlers.
type LifecycleConfig struct {
	GHRunner        GHRunner
	DeployFunc      DeployFunc                // optional: deploy exec agent after SSH setup
	Provisioners    []provisioner.Provisioner // optional: run after setup
	DelegateManager delegate.TaskManager      // optional: headless delegate task manager
}

// --- create_codespace ---

func createCodespaceTool() mcpsdk.Tool {
	return mcpsdk.Tool{
		Name:        "create_codespace",
		Description: "Create a new GitHub Codespace, wait for it to be ready, and connect to it. This operation may take 1-3 minutes. Use get_codespace_options first to see available machine types and devcontainer configs for the repository.",
		InputSchema: mcpsdk.ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"repository": map[string]any{
					"type":        "string",
					"description": "Repository in owner/repo format (e.g., 'github/github')",
				},
				"branch": map[string]any{
					"type":        "string",
					"description": "Branch to checkout (optional, uses default branch if omitted)",
				},
				"machine_type": map[string]any{
					"type":        "string",
					"description": "Machine type name from get_codespace_options (e.g., 'xLargePremiumLinux')",
				},
				"display_name": map[string]any{
					"type":        "string",
					"description": "Display name for the codespace (max 48 chars, defaults to branch name)",
				},
				"devcontainer_path": map[string]any{
					"type":        "string",
					"description": "Path to devcontainer.json from get_codespace_options (e.g., '.devcontainer/devcontainer.json')",
				},
				"default_permissions": map[string]any{
					"type":        "boolean",
					"description": "Use default permissions without authorization prompt (default: false). Set to true only if the user explicitly agrees to skip the permissions review.",
				},
				"alias": map[string]any{
					"type":        "string",
					"description": "Local alias for the codespace (optional, derived from repo name)",
				},
			},
			Required: []string{"repository"},
		},
	}
}

func createCodespaceHandler(reg *registry.Registry, cfg LifecycleConfig) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		repo, err := requiredString(req, "repository")
		if err != nil {
			return toolError(err.Error()), nil
		}

		branch := optionalString(req, "branch")
		machineType := optionalString(req, "machine_type")
		displayName := optionalString(req, "display_name")
		devcontainerPath := optionalString(req, "devcontainer_path")
		alias := optionalString(req, "alias")

		defaultPerms := false
		if raw, ok := req.GetArguments()["default_permissions"]; ok {
			if b, ok := raw.(bool); ok {
				defaultPerms = b
			}
		}

		if alias == "" {
			alias = registry.DefaultAlias(repo, reg.Aliases())
		}

		// Check alias isn't already taken
		if _, err := reg.Resolve(alias); err == nil {
			return toolError(fmt.Sprintf("alias %q already in use; specify a different alias", alias)), nil
		}

		// Build gh cs create command
		args := []string{"codespace", "create", "-R", repo}
		if defaultPerms {
			args = append(args, "--default-permissions")
		}
		if machineType != "" {
			args = append(args, "-m", machineType)
		}
		if devcontainerPath != "" {
			args = append(args, "--devcontainer-path", devcontainerPath)
		}
		if displayName != "" {
			args = append(args, "--display-name", displayName)
		} else if branch != "" {
			args = append(args, "--display-name", branch)
		}

		// Create the codespace
		output, err := cfg.GHRunner.Run(ctx, args...)
		if err != nil {
			errMsg := err.Error()
			// Check for permissions authorization required — extract URL for user
			if strings.Contains(errMsg, "authorize or deny additional permissions") ||
				strings.Contains(errMsg, "You must authorize") {
				authURL := extractURL(errMsg)
				msg := "Codespace creation requires additional permissions authorization.\n"
				if authURL != "" {
					msg += fmt.Sprintf("\nPlease open this URL to authorize: %s\n", authURL)
				}
				msg += "\nAfter authorizing, retry create_codespace. Or set default_permissions=true to skip the review."
				return toolError(msg), nil
			}
			return toolError(fmt.Sprintf("failed to create codespace: %v", err)), nil
		}

		csName := strings.TrimSpace(output)
		if csName == "" {
			return toolError("codespace creation returned empty name"), nil
		}

		// Wait for SSH readiness
		for i := 0; i < 30; i++ {
			checkOut, err := cfg.GHRunner.Run(ctx, "codespace", "ssh", "-c", csName, "--", "echo ready")
			if err == nil && strings.Contains(checkOut, "ready") {
				break
			}
			if i == 29 {
				return toolError(fmt.Sprintf("codespace %s created but SSH not ready after 30 attempts", csName)), nil
			}
			time.Sleep(3 * time.Second)
		}

		// Setup SSH multiplexing
		sshClient := ssh.NewClient(csName)
		if err := sshClient.SetupMultiplexing(ctx); err != nil {
			return toolError(fmt.Sprintf("SSH multiplexing failed: %v", err)), nil
		}

		// Deploy exec agent binary
		var execAgent string
		if cfg.DeployFunc != nil {
			remotePath, err := cfg.DeployFunc(sshClient, csName)
			if err != nil {
				fmt.Fprintf(os.Stderr, "  ⚠ exec agent deploy failed: %v\n", err)
			} else {
				execAgent = remotePath
			}
		}

		// Detect workdir
		workdir := detectCSWorkdir(ctx, sshClient, repo)

		// Checkout branch if specified
		if branch != "" {
			sshClient.SetWorkdir(workdir)
			sshClient.RunBash(ctx, "git fetch origin", workdir)
			sshClient.RunBash(ctx, fmt.Sprintf("git checkout %s 2>/dev/null || git checkout -b %s",
				shellQuote(branch), shellQuote(branch)), workdir)
		}

		// Register
		cs := &registry.ManagedCodespace{
			Alias:      alias,
			Name:       csName,
			Repository: repo,
			Branch:     branch,
			Workdir:    workdir,
			Executor:   sshClient,
			ExecAgent:  execAgent,
		}
		if err := reg.Register(cs); err != nil {
			return toolError(fmt.Sprintf("registration failed: %v", err)), nil
		}

		// Run provisioners
		if len(cfg.Provisioners) > 0 {
			target := &csTarget{name: csName, repo: repo, workdir: workdir, client: sshClient}
			rctx := provisioner.RunContext{
				Terminal:       os.Getenv("TERM"),
				Repository:     repo,
				IsNewCodespace: true,
			}
			provisioner.RunAll(ctx, cfg.Provisioners, rctx, target)
		}

		return toolSuccess(fmt.Sprintf("Created and connected codespace %q (alias: %s)\nRepository: %s\nWorkdir: %s",
			csName, alias, repo, workdir)), nil
	}
}

// detectCSWorkdir finds the workspace directory on a codespace.
func detectCSWorkdir(ctx context.Context, client *ssh.Client, repo string) string {
	stdout, _, _, err := client.Exec(ctx, "ls -d /workspaces/*/ 2>/dev/null")
	if err != nil {
		return "/workspaces"
	}

	var dirs []string
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		d := strings.TrimRight(strings.TrimSpace(line), "/")
		if d != "" {
			dirs = append(dirs, d)
		}
	}

	if len(dirs) == 0 {
		return "/workspaces"
	}
	if len(dirs) == 1 {
		return dirs[0]
	}

	// Try matching repo name
	repoName := repo
	if i := strings.LastIndex(repo, "/"); i >= 0 {
		repoName = repo[i+1:]
	}
	for _, d := range dirs {
		if strings.HasSuffix(d, "/"+repoName) || d == "/workspaces/"+repoName {
			return d
		}
	}
	return dirs[0]
}

// --- connect_codespace ---

func connectCodespaceTool() mcpsdk.Tool {
	return mcpsdk.Tool{
		Name:        "connect_codespace",
		Description: "Connect to an existing GitHub Codespace that is not yet in the current session. May take 30-60 seconds for SSH setup and exec agent deployment.",
		InputSchema: mcpsdk.ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"name": map[string]any{
					"type":        "string",
					"description": "Codespace name (from gh codespace list)",
				},
				"alias": map[string]any{
					"type":        "string",
					"description": "Local alias (optional, derived from codespace name)",
				},
			},
			Required: []string{"name"},
		},
	}
}

func connectCodespaceHandler(reg *registry.Registry, cfg LifecycleConfig) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		csName, err := requiredString(req, "name")
		if err != nil {
			return toolError(err.Error()), nil
		}
		if existing := reg.FindByName(csName); existing != nil {
			return toolError(fmt.Sprintf("codespace %q already connected as alias %q", csName, existing.Alias)), nil
		}
		alias := optionalString(req, "alias")
		if alias == "" {
			alias = registry.DefaultAlias(csName, reg.Aliases())
		}

		// Check alias isn't already taken
		if _, err := reg.Resolve(alias); err == nil {
			return toolError(fmt.Sprintf("alias %q already in use", alias)), nil
		}

		// Look up the codespace to get its repository
		repoInfo := lookupCSRepository(csName)

		// Setup SSH
		sshClient := ssh.NewClient(csName)
		if err := sshClient.SetupMultiplexing(ctx); err != nil {
			return toolError(fmt.Sprintf("SSH setup failed: %v", err)), nil
		}

		// Deploy exec agent binary
		var execAgent string
		if cfg.DeployFunc != nil {
			remotePath, err := cfg.DeployFunc(sshClient, csName)
			if err != nil {
				fmt.Fprintf(os.Stderr, "  ⚠ exec agent deploy failed for %s: %v\n", csName, err)
			} else {
				execAgent = remotePath
			}
		}

		workdir := detectCSWorkdir(ctx, sshClient, repoInfo)

		cs := &registry.ManagedCodespace{
			Alias:      alias,
			Name:       csName,
			Repository: repoInfo,
			Workdir:    workdir,
			Executor:   sshClient,
			ExecAgent:  execAgent,
		}
		if err := reg.Register(cs); err != nil {
			return toolError(fmt.Sprintf("registration failed: %v", err)), nil
		}

		// Run provisioners
		if len(cfg.Provisioners) > 0 {
			target := &csTarget{name: csName, repo: repoInfo, workdir: workdir, client: sshClient}
			rctx := provisioner.RunContext{
				Terminal:       os.Getenv("TERM"),
				Repository:     repoInfo,
				IsNewCodespace: false,
			}
			provisioner.RunAll(ctx, cfg.Provisioners, rctx, target)
		}

		return toolSuccess(fmt.Sprintf("Connected to codespace %q (alias: %s)\nWorkdir: %s", csName, alias, workdir)), nil
	}
}

// lookupCSRepository fetches the repository name for a codespace via gh CLI.
func lookupCSRepository(csName string) string {
	out, err := exec.Command("gh", "codespace", "list",
		"--json", "name,repository", "--limit", "50").Output()
	if err != nil {
		return ""
	}
	var csList []struct {
		Name       string `json:"name"`
		Repository string `json:"repository"`
	}
	json.Unmarshal(out, &csList)
	for _, cs := range csList {
		if cs.Name == csName {
			return cs.Repository
		}
	}
	return ""
}

// --- delete_codespace ---

func deleteCodespaceTool() mcpsdk.Tool {
	return mcpsdk.Tool{
		Name:        "delete_codespace",
		Description: "Disconnect and optionally delete a codespace from the current session.",
		InputSchema: mcpsdk.ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"codespace": map[string]any{
					"type":        "string",
					"description": "Codespace alias to disconnect",
				},
				"delete": map[string]any{
					"type":        "boolean",
					"description": "If true, also delete the codespace from GitHub (default: false, just disconnects)",
				},
			},
			Required: []string{"codespace"},
		},
	}
}

func deleteCodespaceHandler(reg *registry.Registry, ghRunner GHRunner) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		alias, err := requiredString(req, "codespace")
		if err != nil {
			return toolError(err.Error()), nil
		}

		cs, err := reg.Resolve(alias)
		if err != nil {
			return toolError(err.Error()), nil
		}

		csName := cs.Name
		shouldDelete := false
		if raw, ok := req.GetArguments()["delete"]; ok {
			if b, ok := raw.(bool); ok {
				shouldDelete = b
			}
		}

		reg.Deregister(alias)

		if shouldDelete {
			if _, err := ghRunner.Run(ctx, "codespace", "delete", "-c", csName, "--force"); err != nil {
				return toolError(fmt.Sprintf("disconnected %q but failed to delete: %v", alias, err)), nil
			}
			return toolSuccess(fmt.Sprintf("Disconnected and deleted codespace %q (%s)", alias, csName)), nil
		}

		return toolSuccess(fmt.Sprintf("Disconnected codespace %q (%s). The codespace is still running.", alias, csName)), nil
	}
}

// GHRunner abstracts gh CLI execution for testing.
type GHRunner interface {
	Run(ctx context.Context, args ...string) (string, error)
}

// RealGHRunner runs gh commands using exec.Command.
type RealGHRunner struct{}

func (r *RealGHRunner) Run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		// Include stderr in error message for diagnostics
		return "", fmt.Errorf("%w\n%s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

// csTarget adapts an ssh.Client to the provisioner.CodespaceTarget interface.
type csTarget struct {
	name    string
	repo    string
	workdir string
	client  *ssh.Client
}

func (t *csTarget) CodespaceName() string { return t.name }
func (t *csTarget) Repository() string    { return t.repo }
func (t *csTarget) Workdir() string       { return t.workdir }
func (t *csTarget) RunSSH(ctx context.Context, command string) (string, error) {
	stdout, stderr, exitCode, err := t.client.Exec(ctx, command)
	if err != nil {
		return "", err
	}
	if exitCode != 0 {
		return stdout, fmt.Errorf("exit %d: %s", exitCode, strings.TrimSpace(stderr))
	}
	return stdout, nil
}

// extractURL finds the first https://github.com/... URL in a string.
func extractURL(s string) string {
	for _, word := range strings.Fields(s) {
		if strings.HasPrefix(word, "https://github.com/") {
			return strings.TrimRight(word, ".,;:!?)")
		}
	}
	return ""
}

// --- get_codespace_options ---

func getCodespaceOptionsTool() mcpsdk.Tool {
	return mcpsdk.Tool{
		Name:        "get_codespace_options",
		Description: "Get available machine types and devcontainer configurations for a repository. Use this before create_codespace to see what options are available.",
		InputSchema: mcpsdk.ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"repository": map[string]any{
					"type":        "string",
					"description": "Repository in owner/repo format (e.g., 'github/github')",
				},
			},
			Required: []string{"repository"},
		},
	}
}

func getCodespaceOptionsHandler(ghRunner GHRunner) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		repo, err := requiredString(req, "repository")
		if err != nil {
			return toolError(err.Error()), nil
		}

		var sb strings.Builder

		// Fetch machine types
		machineOutput, err := ghRunner.Run(ctx, "api",
			fmt.Sprintf("/repos/%s/codespaces/machines", repo),
			"--jq", `.machines[] | "\(.name)\t\(.display_name)\t\(.cpus)\t\(.memory_in_bytes / 1073741824 | floor)GB\t\(.storage_in_bytes / 1073741824 | floor)GB"`)
		if err != nil {
			sb.WriteString(fmt.Sprintf("Machine types: could not fetch (%v)\n", err))
		} else {
			sb.WriteString("## Machine Types\n\n")
			sb.WriteString(fmt.Sprintf("%-30s %-40s %-6s %-8s %s\n", "Name", "Display Name", "CPUs", "RAM", "Storage"))
			sb.WriteString(strings.Repeat("-", 100) + "\n")
			for _, line := range strings.Split(strings.TrimSpace(machineOutput), "\n") {
				parts := strings.SplitN(line, "\t", 5)
				if len(parts) == 5 {
					sb.WriteString(fmt.Sprintf("%-30s %-40s %-6s %-8s %s\n",
						parts[0], parts[1], parts[2], parts[3], parts[4]))
				}
			}
		}

		// Fetch devcontainer configs
		dcOutput, err := ghRunner.Run(ctx, "api",
			fmt.Sprintf("/repos/%s/codespaces/devcontainers", repo),
			"--jq", `.devcontainers[] | "\(.path)\t\(.display_name // .name // "(default)")"`)
		if err != nil {
			sb.WriteString(fmt.Sprintf("\nDevcontainer configs: could not fetch (%v)\n", err))
		} else if strings.TrimSpace(dcOutput) != "" {
			sb.WriteString("\n## Devcontainer Configs\n\n")
			sb.WriteString(fmt.Sprintf("%-60s %s\n", "Path", "Display Name"))
			sb.WriteString(strings.Repeat("-", 80) + "\n")
			for _, line := range strings.Split(strings.TrimSpace(dcOutput), "\n") {
				parts := strings.SplitN(line, "\t", 2)
				if len(parts) == 2 {
					sb.WriteString(fmt.Sprintf("%-60s %s\n", parts[0], parts[1]))
				}
			}
		}

		sb.WriteString(fmt.Sprintf("\nUse these values with create_codespace(repository=%q, machine_type=..., devcontainer_path=...)", repo))

		return toolSuccess(sb.String()), nil
	}
}
