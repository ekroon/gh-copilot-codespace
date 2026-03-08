package mcp

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ekroon/gh-copilot-codespace/internal/delegate"
	"github.com/ekroon/gh-copilot-codespace/internal/registry"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func delegateTaskTool() mcpsdk.Tool {
	return mcpsdk.Tool{
		Name:        "delegate_task",
		Description: "Start an autonomous remote Copilot delegate task on a codespace using the headless worker extra. Returns a task ID; use read_delegate_task to check progress and get the final answer.",
		InputSchema: mcpsdk.ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"codespace": codespaceParam,
				"prompt": map[string]any{
					"type":        "string",
					"description": "Task instructions for the remote Copilot worker.",
				},
				"cwd": map[string]any{
					"type":        "string",
					"description": "Working directory on the codespace for this delegate task. Defaults to the codespace workdir.",
				},
				"model": map[string]any{
					"type":        "string",
					"description": "Optional model override for the remote delegate session.",
				},
			},
			Required: []string{"prompt"},
		},
	}
}

func delegateTaskHandler(reg *registry.Registry, manager delegate.TaskManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		prompt, err := requiredString(req, "prompt")
		if err != nil {
			return toolError(err.Error()), nil
		}

		cs, err := reg.Resolve(optionalString(req, "codespace"))
		if err != nil {
			return toolError(err.Error()), nil
		}

		taskID, err := manager.StartTask(delegate.StartOptions{
			CodespaceName: cs.Name,
			Workdir:       cs.Workdir,
			ExecAgent:     cs.ExecAgent,
			Cwd:           defaultCwd(optionalString(req, "cwd"), cs.Workdir),
			Prompt:        prompt,
			Model:         optionalString(req, "model"),
			OnComplete: func(callbackCtx context.Context) {
				refreshCodespaceBranch(callbackCtx, reg, cs.Alias, cs.Executor, cs.Workdir)
			},
		})
		if err != nil {
			return toolError(fmt.Sprintf("starting delegate task: %v", err)), nil
		}

		return toolSuccess(fmt.Sprintf("Started delegate task.\nTask ID: %s\nCodespace: %s\nUse read_delegate_task(task_id=%q) to read progress or the final result.", taskID, cs.Alias, taskID)), nil
	}
}

func readDelegateTaskTool() mcpsdk.Tool {
	return mcpsdk.Tool{
		Name:        "read_delegate_task",
		Description: "Read the current status, progress log, and final result of a delegate task started with delegate_task.",
		InputSchema: mcpsdk.ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"task_id": map[string]any{
					"type":        "string",
					"description": "Delegate task ID returned by delegate_task.",
				},
			},
			Required: []string{"task_id"},
		},
	}
}

func readDelegateTaskHandler(manager delegate.TaskManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		taskID, err := requiredString(req, "task_id")
		if err != nil {
			return toolError(err.Error()), nil
		}

		snapshot, err := manager.GetTask(taskID)
		if err != nil {
			return toolError(err.Error()), nil
		}
		return toolSuccess(formatDelegateSnapshot(snapshot)), nil
	}
}

func cancelDelegateTaskTool() mcpsdk.Tool {
	return mcpsdk.Tool{
		Name:        "cancel_delegate_task",
		Description: "Cancel a running delegate task started with delegate_task.",
		InputSchema: mcpsdk.ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"task_id": map[string]any{
					"type":        "string",
					"description": "Delegate task ID returned by delegate_task.",
				},
			},
			Required: []string{"task_id"},
		},
	}
}

func cancelDelegateTaskHandler(manager delegate.TaskManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		taskID, err := requiredString(req, "task_id")
		if err != nil {
			return toolError(err.Error()), nil
		}

		if err := manager.CancelTask(taskID); err != nil {
			return toolError(err.Error()), nil
		}
		return toolSuccess(fmt.Sprintf("Canceled delegate task %s.", taskID)), nil
	}
}

func formatDelegateSnapshot(snapshot delegate.TaskSnapshot) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Task ID: %s\n", snapshot.ID))
	sb.WriteString(fmt.Sprintf("Status: %s\n", snapshot.Status))
	sb.WriteString(fmt.Sprintf("Codespace: %s\n", snapshot.CodespaceName))
	if snapshot.Cwd != "" {
		sb.WriteString(fmt.Sprintf("Cwd: %s\n", snapshot.Cwd))
	}
	if snapshot.Model != "" {
		sb.WriteString(fmt.Sprintf("Model: %s\n", snapshot.Model))
	}
	sb.WriteString(fmt.Sprintf("Updated: %s\n", snapshot.UpdatedAt.Format(time.RFC3339)))
	if snapshot.Error != "" {
		sb.WriteString("\nError:\n")
		sb.WriteString(snapshot.Error)
		sb.WriteString("\n")
	}
	if snapshot.Result != "" {
		sb.WriteString("\nResult:\n")
		sb.WriteString(snapshot.Result)
		sb.WriteString("\n")
	}
	if snapshot.Log != "" {
		sb.WriteString("\nLog:\n")
		sb.WriteString(snapshot.Log)
	}
	return strings.TrimSpace(sb.String())
}

func defaultCwd(cwd, fallback string) string {
	if cwd != "" {
		return cwd
	}
	return fallback
}

func refreshCodespaceBranch(ctx context.Context, reg *registry.Registry, alias string, executor any, workdir string) {
	exec, ok := executor.(interface {
		RunBash(ctx context.Context, command, cwd string) (stdout, stderr string, exitCode int, err error)
	})
	if reg == nil || !ok {
		return
	}

	stdout, stderr, exitCode, err := exec.RunBash(ctx, "git rev-parse --abbrev-ref HEAD", workdir)
	if err != nil || exitCode != 0 {
		_ = stderr
		return
	}

	branch := strings.TrimSpace(stdout)
	if branch != "" {
		_ = reg.UpdateBranch(alias, branch)
	}
}
