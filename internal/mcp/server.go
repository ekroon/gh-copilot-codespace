package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/ekroon/gh-copilot-codespace/internal/codespaceenv"
	"github.com/ekroon/gh-copilot-codespace/internal/registry"
	"github.com/ekroon/gh-copilot-codespace/internal/ssh"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// NewServer creates and configures the MCP server with all remote tools.
// Uses a registry to support multiple codespaces.
func NewServer(reg *registry.Registry, lcfg ...LifecycleConfig) *server.MCPServer {
	s := server.NewMCPServer("codespace-mcp", "0.2.0")

	// Default lifecycle config
	var cfg LifecycleConfig
	if len(lcfg) > 0 {
		cfg = lcfg[0]
	}
	if cfg.GHRunner == nil {
		cfg.GHRunner = &RealGHRunner{}
	}

	s.AddTool(viewTool(), viewHandler(reg))
	s.AddTool(editTool(), editHandler(reg))
	s.AddTool(createTool(), createHandler(reg))
	s.AddTool(bashTool(), bashHandler(reg))
	s.AddTool(grepTool(), grepHandler(reg))
	s.AddTool(globTool(), globHandler(reg))
	s.AddTool(writeBashTool(), writeBashHandler(reg))
	s.AddTool(readBashTool(), readBashHandler(reg))
	s.AddTool(stopBashTool(), stopBashHandler(reg))
	s.AddTool(listBashTool(), listBashHandler(reg))
	s.AddTool(openShellTool(), openShellHandler(reg))
	s.AddTool(cdTool(), cdHandler(reg))
	s.AddTool(cwdTool(), cwdHandler(reg))
	s.AddTool(listCodespacesTool(), listCodespacesHandler(reg))
	s.AddTool(listAvailableCodespacesTool(), listAvailableCodespacesHandler(cfg.GHRunner))
	s.AddTool(getCodespaceOptionsTool(), getCodespaceOptionsHandler(cfg.GHRunner))
	s.AddTool(createCodespaceTool(), createCodespaceHandler(reg, cfg))
	s.AddTool(connectCodespaceTool(), connectCodespaceHandler(reg, cfg))
	s.AddTool(deleteCodespaceTool(), deleteCodespaceHandler(reg, cfg.GHRunner))

	return s
}

// NewServerSingle creates an MCP server with a single codespace for backward compatibility.
func NewServerSingle(executor ssh.Executor, codespaceName string) *server.MCPServer {
	reg := registry.New()
	reg.Register(&registry.ManagedCodespace{
		Alias:    registry.DefaultAlias(codespaceName, nil),
		Name:     codespaceName,
		Executor: executor,
	})
	return NewServer(reg)
}

// resolveExecutor extracts the codespace alias from the request and resolves it via the registry.
func resolveExecutor(reg *registry.Registry, req mcpsdk.CallToolRequest) (ssh.Executor, error) {
	alias := optionalString(req, "codespace")
	cs, err := reg.Resolve(alias)
	if err != nil {
		return nil, err
	}
	return cs.Executor, nil
}

// codespaceParam is the common "codespace" parameter added to all remote tools.
var codespaceParam = map[string]any{
	"type":        "string",
	"description": "Codespace alias (optional if only one connected). Use list_codespaces to see available aliases.",
}

// --- remote_view ---

func viewTool() mcpsdk.Tool {
	return mcpsdk.Tool{
		Name:        "remote_view",
		Description: "View a file or directory on the remote codespace. Returns file contents with line numbers. Replaces the local 'view' tool.",
		InputSchema: mcpsdk.ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"codespace": codespaceParam,
				"path": map[string]any{
					"type":        "string",
					"description": "Path to the file to view",
				},
				"view_range": map[string]any{
					"type":        "array",
					"description": "Optional [start_line, end_line] range. Use -1 for end_line to read to end of file.",
					"items":       map[string]any{"type": "integer"},
				},
			},
			Required: []string{"path"},
		},
	}
}

func viewHandler(reg *registry.Registry) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		c, err := resolveExecutor(reg, req)
		if err != nil {
			return toolError(err.Error()), nil
		}
		path, err := requiredString(req, "path")
		if err != nil {
			return toolError(err.Error()), nil
		}

		var viewRange []int
		if raw, ok := req.GetArguments()["view_range"]; ok {
			if arr, ok := raw.([]any); ok && len(arr) == 2 {
				start, ok1 := toInt(arr[0])
				end, ok2 := toInt(arr[1])
				if ok1 && ok2 {
					viewRange = []int{start, end}
				}
			}
		}

		result, err := c.ViewFile(ctx, path, viewRange)
		if err != nil {
			return toolError(err.Error()), nil
		}
		return toolSuccess(result), nil
	}
}

// --- remote_edit ---

func editTool() mcpsdk.Tool {
	return mcpsdk.Tool{
		Name:        "remote_edit",
		Description: "Edit a file on the remote codespace by replacing exactly one occurrence of old_str with new_str. Replaces the local 'edit' tool.",
		InputSchema: mcpsdk.ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"codespace": codespaceParam,
				"path": map[string]any{
					"type":        "string",
					"description": "Path to the file to edit",
				},
				"old_str": map[string]any{
					"type":        "string",
					"description": "The exact string to find and replace (must match exactly once)",
				},
				"new_str": map[string]any{
					"type":        "string",
					"description": "The replacement string",
				},
			},
			Required: []string{"path", "old_str", "new_str"},
		},
	}
}

func editHandler(reg *registry.Registry) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		c, err := resolveExecutor(reg, req)
		if err != nil {
			return toolError(err.Error()), nil
		}
		path, err := requiredString(req, "path")
		if err != nil {
			return toolError(err.Error()), nil
		}
		oldStr, err := requiredString(req, "old_str")
		if err != nil {
			return toolError(err.Error()), nil
		}
		newStr, err := requiredString(req, "new_str")
		if err != nil {
			return toolError(err.Error()), nil
		}

		if err := c.EditFile(ctx, path, oldStr, newStr); err != nil {
			return toolError(err.Error()), nil
		}
		return toolSuccess(fmt.Sprintf("Successfully edited %s", path)), nil
	}
}

// --- remote_create ---

func createTool() mcpsdk.Tool {
	return mcpsdk.Tool{
		Name:        "remote_create",
		Description: "Create a new file on the remote codespace with the given content. Parent directories are created automatically. Replaces the local 'create' tool.",
		InputSchema: mcpsdk.ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"codespace": codespaceParam,
				"path": map[string]any{
					"type":        "string",
					"description": "Path for the new file",
				},
				"file_text": map[string]any{
					"type":        "string",
					"description": "Content of the file to create",
				},
			},
			Required: []string{"path", "file_text"},
		},
	}
}

func createHandler(reg *registry.Registry) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		c, err := resolveExecutor(reg, req)
		if err != nil {
			return toolError(err.Error()), nil
		}
		path, err := requiredString(req, "path")
		if err != nil {
			return toolError(err.Error()), nil
		}
		content, err := requiredString(req, "file_text")
		if err != nil {
			return toolError(err.Error()), nil
		}

		if err := c.CreateFile(ctx, path, content); err != nil {
			return toolError(err.Error()), nil
		}
		return toolSuccess(fmt.Sprintf("Created %s", path)), nil
	}
}

// --- remote_bash ---

const (
	defaultRemoteBashInitialWait = 2.0
	asyncRemoteBashInitialDelay  = 1.0
	sessionExitedMarker          = "[session exited]"
)

func bashTool() mcpsdk.Tool {
	return mcpsdk.Tool{
		Name:        "remote_bash",
		Description: "Execute a bash command on the remote codespace. By default, it starts a remote session, waits briefly for quick completion, and returns final output when the command exits quickly. If the command is still running, it returns partial output and a shellId for follow-up reads with remote_read_bash. Use mode 'async' for interactive or explicitly backgrounded commands. Replaces the local 'bash' tool.",
		InputSchema: mcpsdk.ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"codespace": codespaceParam,
				"command": map[string]any{
					"type":        "string",
					"description": "The bash command to execute",
				},
				"description": map[string]any{
					"type":        "string",
					"description": "A short description of what this command does",
				},
				"mode": map[string]any{
					"type":        "string",
					"description": "Execution mode: 'sync' (default) waits briefly for quick completion before returning final output or a shellId, 'async' always returns a shellId for continued interaction",
					"enum":        []string{"sync", "async"},
				},
				"initial_wait": map[string]any{
					"type":        "number",
					"description": "Seconds to wait for initial output in sync mode (default: 2). If the command hasn't completed, returns partial output and a shellId for follow-up reads with remote_read_bash. Use larger values for builds/tests when you want more inline output before switching to reads.",
				},
				"shellId": map[string]any{
					"type":        "string",
					"description": "Session identifier for async mode. Auto-generated if not provided.",
				},
				"cwd": map[string]any{
					"type":        "string",
					"description": "Optional working directory for this call. Pass it explicitly for parallel-safe remote_bash usage instead of relying on remote_cd ordering.",
				},
			},
			Required: []string{"command"},
		},
	}
}

func bashHandler(reg *registry.Registry) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		c, err := resolveExecutor(reg, req)
		if err != nil {
			return toolError(err.Error()), nil
		}
		command, err := requiredString(req, "command")
		if err != nil {
			return toolError(err.Error()), nil
		}

		mode := optionalString(req, "mode")
		shellId := optionalString(req, "shellId")
		cwd := optionalString(req, "cwd")
		if shellId == "" {
			shellId = fmt.Sprintf("sh-%d", time.Now().UnixMilli())
		}

		if mode == "async" {
			if err := c.StartSession(ctx, shellId, command, cwd); err != nil {
				return toolError(err.Error()), nil
			}
			// Wait briefly and capture initial output
			time.Sleep(time.Duration(asyncRemoteBashInitialDelay * float64(time.Second)))
			output, _ := c.ReadSession(ctx, shellId)
			return toolSuccess(fmt.Sprintf("Started async session: %s\n\n%s", shellId, output)), nil
		}

		initialWait := optionalFloat(req, "initial_wait", defaultRemoteBashInitialWait)
		if err := c.StartSession(ctx, shellId, command, cwd); err != nil {
			return runBashSyncFallback(ctx, c, command, cwd), nil
		}
		time.Sleep(time.Duration(initialWait * float64(time.Second)))
		output, err := c.ReadSession(ctx, shellId)
		if err != nil {
			if stopErr := c.StopSession(ctx, shellId); stopErr != nil {
				return toolError(fmt.Sprintf("%s\n\nAdditionally, failed to stop session %s after read failure: %v", err.Error(), shellId, stopErr)), nil
			}
			return toolError(err.Error()), nil
		}

		if sessionOutputExited(output) {
			finalOutput := trimSessionExitMarker(output)
			if err := c.StopSession(ctx, shellId); err != nil {
				if finalOutput != "" {
					finalOutput += "\n"
				}
				finalOutput += fmt.Sprintf("[cleanup warning: failed to stop completed session %s: %v]", shellId, err)
			}
			return toolSuccess(finalOutput), nil
		}

		return toolSuccess(fmt.Sprintf("%s\n\n[shellId: %s — use remote_read_bash to check for more output]", output, shellId)), nil
	}
}

func runBashSyncFallback(ctx context.Context, c ssh.Executor, command, cwd string) *mcpsdk.CallToolResult {
	stdout, stderr, exitCode, err := c.RunBash(ctx, command, cwd)
	if err != nil {
		errMsg := err.Error()
		if ctx.Err() != nil {
			errMsg += "\n\nHint: This command may have timed out. Use initial_wait parameter (e.g., initial_wait=60) or mode='async' for long-running commands."
		}
		return toolError(errMsg)
	}

	var result strings.Builder
	if stdout != "" {
		result.WriteString(stdout)
	}
	if stderr != "" {
		if result.Len() > 0 {
			result.WriteString("\n")
		}
		result.WriteString("STDERR:\n")
		result.WriteString(stderr)
	}
	if exitCode != 0 {
		result.WriteString(fmt.Sprintf("\n[exit code: %d]", exitCode))
	}

	return toolSuccess(result.String())
}

func sessionOutputExited(output string) bool {
	return strings.Contains(output, sessionExitedMarker)
}

func trimSessionExitMarker(output string) string {
	lines := strings.Split(output, "\n")
	filtered := lines[:0]
	for _, line := range lines {
		if strings.TrimSpace(line) == sessionExitedMarker {
			continue
		}
		filtered = append(filtered, line)
	}
	return strings.TrimRight(strings.Join(filtered, "\n"), "\n")
}

// --- remote_write_bash ---

func writeBashTool() mcpsdk.Tool {
	return mcpsdk.Tool{
		Name:        "remote_write_bash",
		Description: "Send input to a remote bash session on the codespace. Supports special keys: {enter}, {up}, {down}, {left}, {right}, {backspace}. Replaces the local 'write_bash' tool.",
		InputSchema: mcpsdk.ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"codespace": codespaceParam,
				"shellId": map[string]any{
					"type":        "string",
					"description": "The session ID returned by remote_bash when it keeps a session open",
				},
				"input": map[string]any{
					"type":        "string",
					"description": "The input to send. Can include special keys like {enter}, {up}, {down}.",
				},
				"delay": map[string]any{
					"type":        "number",
					"description": "Seconds to wait before reading output (default: 2)",
				},
			},
			Required: []string{"shellId"},
		},
	}
}

func writeBashHandler(reg *registry.Registry) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		c, err := resolveExecutor(reg, req)
		if err != nil {
			return toolError(err.Error()), nil
		}
		shellId, err := requiredString(req, "shellId")
		if err != nil {
			return toolError(err.Error()), nil
		}

		input := optionalString(req, "input")
		if input != "" {
			if err := c.WriteSession(ctx, shellId, input); err != nil {
				return toolError(err.Error()), nil
			}
		}

		delay := optionalFloat(req, "delay", 2)
		time.Sleep(time.Duration(delay * float64(time.Second)))

		output, err := c.ReadSession(ctx, shellId)
		if err != nil {
			return toolError(err.Error()), nil
		}
		return toolSuccess(output), nil
	}
}

// --- remote_read_bash ---

func readBashTool() mcpsdk.Tool {
	return mcpsdk.Tool{
		Name:        "remote_read_bash",
		Description: "Read output from a remote bash session on the codespace. Returns the last 100 lines of the session's terminal output. If a command hasn't completed, call again with a longer delay. Use exponential backoff between reads to minimize overhead. Replaces the local 'read_bash' tool.",
		InputSchema: mcpsdk.ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"codespace": codespaceParam,
				"shellId": map[string]any{
					"type":        "string",
					"description": "The session ID returned by remote_bash when it keeps a session open",
				},
				"delay": map[string]any{
					"type":        "number",
					"description": "Seconds to wait before reading output (default: 2). Use longer delays for slow commands to avoid unnecessary reads.",
				},
			},
			Required: []string{"shellId"},
		},
	}
}

func readBashHandler(reg *registry.Registry) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		c, err := resolveExecutor(reg, req)
		if err != nil {
			return toolError(err.Error()), nil
		}
		shellId, err := requiredString(req, "shellId")
		if err != nil {
			return toolError(err.Error()), nil
		}

		delay := optionalFloat(req, "delay", 2)
		time.Sleep(time.Duration(delay * float64(time.Second)))

		output, err := c.ReadSession(ctx, shellId)
		if err != nil {
			return toolError(err.Error()), nil
		}
		return toolSuccess(output), nil
	}
}

// --- remote_stop_bash ---

func stopBashTool() mcpsdk.Tool {
	return mcpsdk.Tool{
		Name:        "remote_stop_bash",
		Description: "Stop a remote bash session on the codespace. Replaces the local 'stop_bash' tool.",
		InputSchema: mcpsdk.ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"codespace": codespaceParam,
				"shellId": map[string]any{
					"type":        "string",
					"description": "The session ID to stop",
				},
			},
			Required: []string{"shellId"},
		},
	}
}

func stopBashHandler(reg *registry.Registry) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		c, err := resolveExecutor(reg, req)
		if err != nil {
			return toolError(err.Error()), nil
		}
		shellId, err := requiredString(req, "shellId")
		if err != nil {
			return toolError(err.Error()), nil
		}

		if err := c.StopSession(ctx, shellId); err != nil {
			return toolError(err.Error()), nil
		}
		return toolSuccess(fmt.Sprintf("Session %s stopped.", shellId)), nil
	}
}

// --- remote_list_bash ---

func listBashTool() mcpsdk.Tool {
	return mcpsdk.Tool{
		Name:        "remote_list_bash",
		Description: "List active remote bash sessions on the codespace. Replaces the local 'list_bash' tool.",
		InputSchema: mcpsdk.ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"codespace": codespaceParam,
			},
		},
	}
}

func listBashHandler(reg *registry.Registry) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		c, err := resolveExecutor(reg, req)
		if err != nil {
			return toolError(err.Error()), nil
		}
		result, err := c.ListSessions(ctx)
		if err != nil {
			return toolError(err.Error()), nil
		}
		if result == "" {
			return toolSuccess("No active sessions."), nil
		}
		return toolSuccess(result), nil
	}
}

// --- remote_grep ---

func grepTool() mcpsdk.Tool {
	return mcpsdk.Tool{
		Name:        "remote_grep",
		Description: "Search for a pattern in files on the remote codespace using ripgrep (with grep fallback). Replaces the local 'grep' tool.",
		InputSchema: mcpsdk.ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"codespace": codespaceParam,
				"pattern": map[string]any{
					"type":        "string",
					"description": "The regex pattern to search for",
				},
				"path": map[string]any{
					"type":        "string",
					"description": "Directory or file to search in (defaults to '.' within cwd)",
				},
				"glob": map[string]any{
					"type":        "string",
					"description": "Glob pattern to filter files (e.g., '*.go', '*.ts')",
				},
				"cwd": map[string]any{
					"type":        "string",
					"description": "Optional working directory for this call. Pass it explicitly for parallel-safe remote_grep usage instead of relying on remote_cd ordering.",
				},
			},
			Required: []string{"pattern"},
		},
	}
}

func grepHandler(reg *registry.Registry) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		c, err := resolveExecutor(reg, req)
		if err != nil {
			return toolError(err.Error()), nil
		}
		pattern, err := requiredString(req, "pattern")
		if err != nil {
			return toolError(err.Error()), nil
		}

		path := optionalString(req, "path")
		glob := optionalString(req, "glob")
		cwd := optionalString(req, "cwd")

		result, err := c.Grep(ctx, pattern, path, glob, cwd)
		if err != nil {
			return toolError(err.Error()), nil
		}
		if result == "" {
			return toolSuccess("No matches found."), nil
		}
		return toolSuccess(result), nil
	}
}

// --- remote_glob ---

func globTool() mcpsdk.Tool {
	return mcpsdk.Tool{
		Name:        "remote_glob",
		Description: "Find files matching a glob pattern on the remote codespace. Replaces the local 'glob' tool.",
		InputSchema: mcpsdk.ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"codespace": codespaceParam,
				"pattern": map[string]any{
					"type":        "string",
					"description": "The glob pattern to match files against (e.g., '*.go', '**/*.ts')",
				},
				"path": map[string]any{
					"type":        "string",
					"description": "Directory to search in (defaults to '.' within cwd)",
				},
				"cwd": map[string]any{
					"type":        "string",
					"description": "Optional working directory for this call. Pass it explicitly for parallel-safe remote_glob usage instead of relying on remote_cd ordering.",
				},
			},
			Required: []string{"pattern"},
		},
	}
}

func globHandler(reg *registry.Registry) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		c, err := resolveExecutor(reg, req)
		if err != nil {
			return toolError(err.Error()), nil
		}
		pattern, err := requiredString(req, "pattern")
		if err != nil {
			return toolError(err.Error()), nil
		}

		path := optionalString(req, "path")
		cwd := optionalString(req, "cwd")

		result, err := c.Glob(ctx, pattern, path, cwd)
		if err != nil {
			return toolError(err.Error()), nil
		}
		if result == "" {
			return toolSuccess("No matches found."), nil
		}
		return toolSuccess(result), nil
	}
}

// --- helpers ---

func requiredString(req mcpsdk.CallToolRequest, key string) (string, error) {
	args := req.GetArguments()
	val, ok := args[key]
	if !ok {
		return "", fmt.Errorf("missing required parameter: %s", key)
	}
	s, ok := val.(string)
	if !ok {
		return "", fmt.Errorf("parameter %s must be a string", key)
	}
	return s, nil
}

func optionalString(req mcpsdk.CallToolRequest, key string) string {
	args := req.GetArguments()
	val, ok := args[key]
	if !ok {
		return ""
	}
	s, _ := val.(string)
	return s
}

func optionalFloat(req mcpsdk.CallToolRequest, key string, defaultVal float64) float64 {
	args := req.GetArguments()
	val, ok := args[key]
	if !ok {
		return defaultVal
	}
	f, ok := val.(float64)
	if !ok {
		return defaultVal
	}
	return f
}

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	}
	return 0, false
}

func toolSuccess(text string) *mcpsdk.CallToolResult {
	return &mcpsdk.CallToolResult{
		Content: []mcpsdk.Content{
			mcpsdk.TextContent{
				Type: "text",
				Text: text,
			},
		},
	}
}

func toolError(text string) *mcpsdk.CallToolResult {
	return &mcpsdk.CallToolResult{
		IsError: true,
		Content: []mcpsdk.Content{
			mcpsdk.TextContent{
				Type: "text",
				Text: text,
			},
		},
	}
}

// --- remote_cd ---

func cdTool() mcpsdk.Tool {
	return mcpsdk.Tool{
		Name:        "remote_cd",
		Description: "Change the default working directory on the remote codespace for later sequential remote_bash, remote_grep, and remote_glob calls that omit cwd. For parallel calls, pass cwd explicitly instead of relying on remote_cd ordering. The directory must exist on the codespace.",
		InputSchema: mcpsdk.ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"codespace": codespaceParam,
				"path": map[string]any{
					"type":        "string",
					"description": "The directory path to change to on the codespace",
				},
			},
			Required: []string{"path"},
		},
	}
}

func cdHandler(reg *registry.Registry) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		c, err := resolveExecutor(reg, req)
		if err != nil {
			return toolError(err.Error()), nil
		}
		path, err := requiredString(req, "path")
		if err != nil {
			return toolError(err.Error()), nil
		}

		// Validate the directory exists on the codespace
		quoted := "'" + strings.ReplaceAll(path, "'", "'\"'\"'") + "'"
		stdout, _, exitCode, execErr := c.RunBash(ctx, fmt.Sprintf("cd %s && pwd", quoted), c.GetWorkdir())
		if execErr != nil {
			return toolError(fmt.Sprintf("failed to change directory: %v", execErr)), nil
		}
		if exitCode != 0 {
			return toolError(fmt.Sprintf("directory does not exist: %s", path)), nil
		}

		resolved := strings.TrimSpace(stdout)
		if resolved == "" {
			resolved = path
		}
		c.SetWorkdir(resolved)
		return toolSuccess(fmt.Sprintf("Changed working directory to %s", resolved)), nil
	}
}

// --- remote_cwd ---

func cwdTool() mcpsdk.Tool {
	return mcpsdk.Tool{
		Name:        "remote_cwd",
		Description: "Get the current default working directory used by remote_bash, remote_grep, and remote_glob when cwd is not provided.",
		InputSchema: mcpsdk.ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"codespace": codespaceParam,
			},
		},
	}
}

func cwdHandler(reg *registry.Registry) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		c, err := resolveExecutor(reg, req)
		if err != nil {
			return toolError(err.Error()), nil
		}
		return toolSuccess(c.GetWorkdir()), nil
	}
}

// --- open_shell ---

func openShellTool() mcpsdk.Tool {
	return mcpsdk.Tool{
		Name:        "open_shell",
		Description: "Open an interactive SSH shell to the codespace in a new terminal tab/window. Use this when the user asks for a shell, terminal, or SSH access to the codespace.",
		InputSchema: mcpsdk.ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"codespace": codespaceParam,
			},
		},
	}
}

func openShellHandler(reg *registry.Registry) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		alias := optionalString(req, "codespace")
		cs, err := reg.Resolve(alias)
		if err != nil {
			return toolError(err.Error()), nil
		}
		codespaceName := cs.Name

		authMode, err := codespaceenv.ParseGitHubAuthMode(os.Getenv(codespaceenv.GitHubAuthModeEnvVar))
		if err != nil {
			return toolError(fmt.Sprintf("invalid %s: %v", codespaceenv.GitHubAuthModeEnvVar, err)), nil
		}
		self, err := os.Executable()
		if err != nil {
			return toolError(fmt.Sprintf("finding executable: %v", err)), nil
		}
		args := []string{self, "proxy", "--codespace", codespaceName, "--github-auth", string(authMode), "--shell"}
		if cs.Workdir != "" {
			args = append(args, "--workdir", cs.Workdir)
		}
		quoted := make([]string, len(args))
		for i, arg := range args {
			quoted[i] = shellQuote(arg)
		}
		sshCmd := strings.Join(quoted, " ")

		if err := openTerminalTab(sshCmd, "codespace: "+codespaceName); err != nil {
			return toolError(fmt.Sprintf("Failed to open shell: %v", err)), nil
		}
		return toolSuccess("Opened SSH shell to codespace in a new terminal tab."), nil
	}
}

// openTerminalTab opens a new terminal tab with the given command.
// Uses COPILOT_TERMINAL env var to determine the terminal to use.
// Supported values: "cmux" (default if cmux is detected), "macos" (Terminal.app), or a custom command template.
func openTerminalTab(command, title string) error {
	terminal := os.Getenv("COPILOT_TERMINAL")

	if terminal == "" {
		// Auto-detect: prefer cmux, then ghostty, then iterm2, then Terminal.app
		if findCmuxCLI() != "" {
			terminal = "cmux"
		} else if _, err := os.Stat("/Applications/Ghostty.app"); err == nil {
			terminal = "ghostty"
		} else if _, err := os.Stat("/Applications/iTerm.app"); err == nil {
			terminal = "iterm2"
		} else {
			terminal = "macos"
		}
	}

	switch terminal {
	case "cmux":
		return openCmuxTab(command, title)
	case "ghostty":
		return openGhosttyWindow(command)
	case "iterm2":
		return openITerm2Tab(command)
	case "macos":
		return openMacOSTab(command)
	default:
		// Custom command template: replace {} with the SSH command
		customCmd := strings.ReplaceAll(terminal, "{}", command)
		return exec.Command("sh", "-c", customCmd).Run()
	}
}

func findCmuxCLI() string {
	// Check common cmux CLI locations
	paths := []string{
		"/Applications/cmux.app/Contents/Resources/bin/cmux",
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func openCmuxTab(command, title string) error {
	cmuxCLI := findCmuxCLI()
	if cmuxCLI == "" {
		return fmt.Errorf("cmux CLI not found")
	}

	// Create a new terminal tab (surface) in the current workspace
	out, err := exec.Command(cmuxCLI, "new-surface", "--type", "terminal").Output()
	if err != nil {
		return fmt.Errorf("cmux new-surface: %w", err)
	}

	// Parse surface ref (e.g., "OK surface:18 pane:5 workspace:5")
	var surfaceRef string
	for _, field := range strings.Fields(string(out)) {
		if strings.HasPrefix(field, "surface:") {
			surfaceRef = field
			break
		}
	}
	if surfaceRef == "" {
		return nil
	}

	// Send the command and press Enter
	exec.Command(cmuxCLI, "send", "--surface", surfaceRef, command).Run()
	exec.Command(cmuxCLI, "send-key", "--surface", surfaceRef, "Enter").Run()

	// Rename the tab
	exec.Command(cmuxCLI, "tab-action", "--action", "rename",
		"--tab", surfaceRef, "--title", title).Run()
	return nil
}

func openMacOSTab(command string) error {
	script := fmt.Sprintf(`tell application "Terminal"
	activate
	do script "%s"
end tell`, strings.ReplaceAll(command, `"`, `\"`))
	return exec.Command("osascript", "-e", script).Run()
}

func openGhosttyWindow(command string) error {
	return exec.Command("open", "-na", "Ghostty", "--args", "-e", command).Run()
}

func openITerm2Tab(command string) error {
	escaped := strings.ReplaceAll(command, `"`, `\"`)
	escaped = strings.ReplaceAll(escaped, `\`, `\\`)
	script := fmt.Sprintf(`tell application "iTerm2"
	activate
	tell current window
		create tab with default profile command "%s"
	end tell
end tell`, escaped)
	return exec.Command("osascript", "-e", script).Run()
}

// --- list_codespaces ---

func listCodespacesTool() mcpsdk.Tool {
	return mcpsdk.Tool{
		Name:        "list_codespaces",
		Description: "List codespaces that are currently connected in this session, with their aliases, repositories, branches, and working directories.",
		InputSchema: mcpsdk.ToolInputSchema{
			Type:       "object",
			Properties: map[string]any{},
		},
	}
}

func listCodespacesHandler(reg *registry.Registry) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		all := reg.All()
		if len(all) == 0 {
			return toolSuccess("No codespaces connected. Use list_available_codespaces to see codespaces you can connect to."), nil
		}

		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("%-12s %-30s %-20s %s\n", "Alias", "Repository", "Branch", "Workdir"))
		sb.WriteString(strings.Repeat("-", 80) + "\n")
		for _, cs := range all {
			branch := cs.Branch
			if branch == "" {
				branch = "(unknown)"
			}
			sb.WriteString(fmt.Sprintf("%-12s %-30s %-20s %s\n", cs.Alias, cs.Repository, branch, cs.Workdir))
		}
		return toolSuccess(sb.String()), nil
	}
}

// --- list_available_codespaces ---

func listAvailableCodespacesTool() mcpsdk.Tool {
	return mcpsdk.Tool{
		Name:        "list_available_codespaces",
		Description: "List all GitHub Codespaces available to connect to (runs gh codespace list locally). Use this to discover codespaces before connecting with connect_codespace.",
		InputSchema: mcpsdk.ToolInputSchema{
			Type:       "object",
			Properties: map[string]any{},
		},
	}
}

func listAvailableCodespacesHandler(ghRunner GHRunner) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		output, err := ghRunner.Run(ctx, "codespace", "list",
			"--json", "name,displayName,repository,state",
			"--limit", "50")
		if err != nil {
			return toolError(fmt.Sprintf("failed to list codespaces: %v", err)), nil
		}

		var codespaces []struct {
			Name        string `json:"name"`
			DisplayName string `json:"displayName"`
			Repository  string `json:"repository"`
			State       string `json:"state"`
		}
		if err := json.Unmarshal([]byte(output), &codespaces); err != nil {
			return toolError(fmt.Sprintf("parsing codespace list: %v", err)), nil
		}

		if len(codespaces) == 0 {
			return toolSuccess("No codespaces found. Use create_codespace to create one."), nil
		}

		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("%-45s %-30s %-12s %s\n", "Name", "Repository", "State", "Display Name"))
		sb.WriteString(strings.Repeat("-", 100) + "\n")
		for _, cs := range codespaces {
			sb.WriteString(fmt.Sprintf("%-45s %-30s %-12s %s\n", cs.Name, cs.Repository, cs.State, cs.DisplayName))
		}
		sb.WriteString("\nConnect with: connect_codespace(name=\"<codespace-name>\")")
		return toolSuccess(sb.String()), nil
	}
}
