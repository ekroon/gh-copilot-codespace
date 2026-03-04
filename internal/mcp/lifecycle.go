package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/ekroon/gh-copilot-codespace/internal/registry"
	"github.com/ekroon/gh-copilot-codespace/internal/ssh"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// --- create_codespace ---

func createCodespaceTool() mcpsdk.Tool {
	return mcpsdk.Tool{
		Name:        "create_codespace",
		Description: "Create a new GitHub Codespace, wait for it to be ready, and connect to it. The new codespace becomes available for all remote_* tools.",
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
					"description": "Machine type (e.g., 'standardLinux32gb', 'xLargePremiumLinux'). Defaults to repository's default.",
				},
				"display_name": map[string]any{
					"type":        "string",
					"description": "Display name for the codespace (optional, defaults to branch name)",
				},
				"devcontainer_path": map[string]any{
					"type":        "string",
					"description": "Path to devcontainer.json (e.g., '.devcontainer/devcontainer.json'). Required for repos with multiple devcontainer configs.",
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

func createCodespaceHandler(reg *registry.Registry, ghRunner GHRunner) server.ToolHandlerFunc {
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

		if alias == "" {
			alias = registry.DefaultAlias(repo, reg.Aliases())
		}

		// Check alias isn't already taken
		if _, err := reg.Resolve(alias); err == nil {
			return toolError(fmt.Sprintf("alias %q already in use; specify a different alias", alias)), nil
		}

		// Build gh cs create command
		args := []string{"codespace", "create", "-R", repo, "--default-permissions"}
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
		output, err := ghRunner.Run(ctx, args...)
		if err != nil {
			return toolError(fmt.Sprintf("failed to create codespace: %v\n%s", err, output)), nil
		}

		csName := strings.TrimSpace(output)
		if csName == "" {
			return toolError("codespace creation returned empty name"), nil
		}

		// Wait for SSH readiness
		for i := 0; i < 30; i++ {
			checkOut, err := ghRunner.Run(ctx, "codespace", "ssh", "-c", csName, "--", "echo ready")
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

		// Detect workdir
		workdir := detectCSWorkdir(ctx, sshClient, repo)

		// Checkout branch if specified
		if branch != "" {
			sshClient.SetWorkdir(workdir)
			sshClient.RunBash(ctx, "git fetch origin")
			sshClient.RunBash(ctx, fmt.Sprintf("git checkout %s 2>/dev/null || git checkout -b %s",
				shellQuote(branch), shellQuote(branch)))
		}

		// Register
		cs := &registry.ManagedCodespace{
			Alias:      alias,
			Name:       csName,
			Repository: repo,
			Branch:     branch,
			Workdir:    workdir,
			Executor:   sshClient,
		}
		if err := reg.Register(cs); err != nil {
			return toolError(fmt.Sprintf("registration failed: %v", err)), nil
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
		Description: "Connect to an existing GitHub Codespace that is not yet in the current session.",
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

func connectCodespaceHandler(reg *registry.Registry) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		csName, err := requiredString(req, "name")
		if err != nil {
			return toolError(err.Error()), nil
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

		workdir := detectCSWorkdir(ctx, sshClient, repoInfo)

		cs := &registry.ManagedCodespace{
			Alias:      alias,
			Name:       csName,
			Repository: repoInfo,
			Workdir:    workdir,
			Executor:   sshClient,
		}
		if err := reg.Register(cs); err != nil {
			return toolError(fmt.Sprintf("registration failed: %v", err)), nil
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
