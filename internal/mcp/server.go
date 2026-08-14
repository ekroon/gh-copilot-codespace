package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	pathpkg "path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ekroon/gh-copilot-codespace/internal/registry"
	"github.com/ekroon/gh-copilot-codespace/internal/ssh"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

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

// --- remote_copy ---

type copyEndpoint struct {
	remote bool
	alias  string
	path   string
}

func remoteCopyTool() mcpsdk.Tool {
	return mcpsdk.Tool{
		Name:        "remote_copy",
		Description: "Copy one file between the local working directory and a connected codespace. Use local paths for local files and cs://<alias>/<path> for remote files under that codespace's workdir (aliases come from list_codespaces). Direction is inferred from source and destination. This is a one-time copy, not synchronization; destination files are not overwritten unless overwrite=true.",
		InputSchema: mcpsdk.ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"source": map[string]any{
					"type":        "string",
					"description": "Local file path or remote URI like cs://github/src/app.go. Remote paths are relative to the codespace workdir.",
				},
				"destination": map[string]any{
					"type":        "string",
					"description": "Local file path or remote URI like cs://github/src/app.go. One side must be local and the other remote.",
				},
				"overwrite": map[string]any{
					"type":        "boolean",
					"description": "Overwrite the destination if it already exists (default: false).",
				},
			},
			Required: []string{"source", "destination"},
		},
	}
}

func remoteCopyHandler(reg *registry.Registry, localRoot string) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		source, err := requiredString(req, "source")
		if err != nil {
			return toolErrorFor(err), nil
		}
		destination, err := requiredString(req, "destination")
		if err != nil {
			return toolErrorFor(err), nil
		}
		overwrite := optionalBoolArg(req, "overwrite", false)

		src, err := parseCopyEndpoint(source)
		if err != nil {
			return toolError(fmt.Sprintf("source: %v", err)), nil
		}
		dst, err := parseCopyEndpoint(destination)
		if err != nil {
			return toolError(fmt.Sprintf("destination: %v", err)), nil
		}
		if src.remote == dst.remote {
			return toolError("remote_copy requires exactly one local path and one cs:// remote path"), nil
		}

		root, err := resolveLocalRoot(localRoot)
		if err != nil {
			return toolErrorFor(err), nil
		}
		if src.remote {
			return copyFromRemote(ctx, reg, root, src, dst, overwrite)
		}
		return copyToRemote(ctx, reg, root, src, dst, overwrite)
	}
}

func optionalBoolArg(req mcpsdk.CallToolRequest, key string, defaultValue bool) bool {
	raw, ok := req.GetArguments()[key]
	if !ok {
		return defaultValue
	}
	value, ok := raw.(bool)
	if !ok {
		return defaultValue
	}
	return value
}

func parseCopyEndpoint(value string) (copyEndpoint, error) {
	if strings.TrimSpace(value) == "" {
		return copyEndpoint{}, fmt.Errorf("path is required")
	}
	if !strings.HasPrefix(value, "cs://") {
		return copyEndpoint{path: value}, nil
	}

	rest := strings.TrimPrefix(value, "cs://")
	slash := strings.Index(rest, "/")
	if slash < 0 {
		return copyEndpoint{}, fmt.Errorf("remote URI must be cs://<alias>/<path>")
	}
	alias := rest[:slash]
	remotePath := rest[slash+1:]
	if remotePath == "" {
		return copyEndpoint{}, fmt.Errorf("remote URI must include a path")
	}
	return copyEndpoint{remote: true, alias: alias, path: remotePath}, nil
}

func resolveLocalRoot(localRoot string) (string, error) {
	if localRoot == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("getting local workdir: %w", err)
		}
		localRoot = wd
	}
	abs, err := filepath.Abs(localRoot)
	if err != nil {
		return "", fmt.Errorf("resolving local workdir: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(abs))
	if err != nil {
		return "", fmt.Errorf("resolving local workdir symlinks: %w", err)
	}
	return resolved, nil
}

func resolveLocalCopyPath(root, value string) (string, error) {
	candidate, err := localCopyCandidate(root, value)
	if err != nil {
		return "", err
	}
	resolved, err := resolvePathSymlinks(candidate)
	if err != nil {
		return "", fmt.Errorf("resolving local path %q: %w", value, err)
	}
	if err := ensureLocalPathContained(root, resolved, value); err != nil {
		return "", err
	}
	return resolved, nil
}

func resolveLocalCopySource(root, value string) (string, error) {
	candidate, err := localCopyCandidate(root, value)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(candidate)
	if err != nil {
		return "", fmt.Errorf("reading local source: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("local source %q is a symbolic link", value)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("local source %q is not a regular file", value)
	}
	resolvedParent, err := resolvePathSymlinks(filepath.Dir(candidate))
	if err != nil {
		return "", fmt.Errorf("resolving local source %q: %w", value, err)
	}
	resolved := filepath.Join(resolvedParent, filepath.Base(candidate))
	if err := ensureLocalPathContained(root, resolved, value); err != nil {
		return "", err
	}
	return resolved, nil
}

func resolveLocalCopyDestination(root, value string) (string, error) {
	candidate, err := localCopyCandidate(root, value)
	if err != nil {
		return "", err
	}
	if info, err := os.Lstat(candidate); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("local destination %q is a symbolic link", value)
		}
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("checking local destination %q: %w", value, err)
	}
	resolvedParent, err := resolvePathSymlinks(filepath.Dir(candidate))
	if err != nil {
		return "", fmt.Errorf("resolving local destination %q: %w", value, err)
	}
	resolved := filepath.Join(resolvedParent, filepath.Base(candidate))
	if err := ensureLocalPathContained(root, resolved, value); err != nil {
		return "", err
	}
	return resolved, nil
}

func localCopyCandidate(root, value string) (string, error) {
	var candidate string
	if filepath.IsAbs(value) {
		candidate = filepath.Clean(value)
	} else {
		candidate = filepath.Clean(filepath.Join(root, value))
	}
	rel, err := filepath.Rel(root, candidate)
	if err != nil {
		return "", fmt.Errorf("resolving local path: %w", err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("local path %q escapes local workdir %q", value, root)
	}
	return candidate, nil
}

func ensureLocalPathContained(root, resolved, value string) error {
	resolvedRel, err := filepath.Rel(root, resolved)
	if err != nil {
		return fmt.Errorf("resolving local path: %w", err)
	}
	if resolvedRel == ".." || strings.HasPrefix(resolvedRel, ".."+string(filepath.Separator)) || filepath.IsAbs(resolvedRel) {
		return fmt.Errorf("local path %q escapes local workdir %q", value, root)
	}
	return nil
}

func resolvePathSymlinks(candidate string) (string, error) {
	current := candidate
	var missing []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return resolved, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		if _, lstatErr := os.Lstat(current); lstatErr == nil {
			return "", err
		} else if !os.IsNotExist(lstatErr) {
			return "", lstatErr
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func resolveRemoteCopyPath(cs *registry.ManagedCodespace, value string) (string, error) {
	for _, segment := range strings.Split(value, "/") {
		if segment == ".." {
			return "", fmt.Errorf("remote path %q escapes codespace workdir", value)
		}
	}
	clean := pathpkg.Clean("/" + strings.TrimPrefix(value, "/"))
	rel := strings.TrimPrefix(clean, "/")
	if rel == "" || rel == "." {
		return "", fmt.Errorf("remote path is required")
	}
	return pathpkg.Join(cs.Workdir, rel), nil
}

func copyToRemote(ctx context.Context, reg *registry.Registry, localRoot string, src, dst copyEndpoint, overwrite bool) (*mcpsdk.CallToolResult, error) {
	if err := ctx.Err(); err != nil {
		return toolError(fmt.Sprintf("reading local source: %v", err)), nil
	}
	source, localPath, err := openLocalCopySourceRooted(localRoot, src.path, localCopyReadHooks{})
	if err != nil {
		return toolErrorFor(err), nil
	}
	defer source.Close()

	cs, err := reg.Resolve(dst.alias)
	if err != nil {
		return toolErrorFor(err), nil
	}
	remotePath, err := resolveRemoteCopyPath(cs, dst.path)
	if err != nil {
		return toolErrorFor(err), nil
	}
	if !overwrite {
		exists, err := remotePathExists(ctx, cs.Executor, remotePath)
		if err != nil {
			return toolErrorFor(err), nil
		}
		if exists {
			return toolError(fmt.Sprintf("destination %s already exists on codespace %q; set overwrite=true to replace it", remotePath, cs.Alias)), nil
		}
	}
	content, err := readOpenedLocalCopySource(ctx, source, localCopyReadHooks{})
	if err != nil {
		return toolErrorFor(err), nil
	}
	copyExecutor, ok := cs.Executor.(ssh.RootedFileExecutor)
	if !ok {
		return toolError("copy to codespace: executor does not support rooted file copies"), nil
	}
	if err := copyExecutor.WriteFileRooted(ctx, ssh.RootedWriteRequest{
		Path:      remotePath,
		Root:      cs.Workdir,
		Data:      content,
		Overwrite: overwrite,
	}); err != nil {
		return toolErrorForf(err, "copy to codespace"), nil
	}
	return toolSuccess(fmt.Sprintf("Copied %s to cs://%s/%s", localPath, cs.Alias, strings.TrimPrefix(dst.path, "/"))), nil
}

type localCopyReadHooks struct {
	afterParentOpen func() error
	afterStat       func() error
	reader          func(io.Reader) io.Reader
}

type localCopyWriteHooks struct {
	afterParentOpen  func() error
	afterTempCreated func() error
	beforeInstall    func() error
	afterInstall     func() error
}

type localCopyContextReader struct {
	ctx context.Context
	src io.Reader
}

func (r localCopyContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.src.Read(p)
	if ctxErr := r.ctx.Err(); ctxErr != nil {
		return n, ctxErr
	}
	return n, err
}

func readLocalCopySourceWithHooks(ctx context.Context, path string, hooks localCopyReadHooks) ([]byte, error) {
	return readLocalCopySourceRootedWithHooks(ctx, filepath.Dir(path), filepath.Base(path), hooks)
}

func readLocalCopySourceRootedWithHooks(ctx context.Context, root, value string, hooks localCopyReadHooks) ([]byte, error) {
	file, _, err := openLocalCopySourceRooted(root, value, hooks)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return readOpenedLocalCopySource(ctx, file, hooks)
}

func readOpenedLocalCopySource(ctx context.Context, file *os.File, hooks localCopyReadHooks) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("reading local source: %w", err)
	}
	if hooks.afterStat != nil {
		if err := hooks.afterStat(); err != nil {
			return nil, fmt.Errorf("reading local source: %w", err)
		}
	}

	var source io.Reader = file
	if hooks.reader != nil {
		source = hooks.reader(source)
	}
	source = localCopyContextReader{ctx: ctx, src: source}
	content, err := io.ReadAll(io.LimitReader(source, ssh.MaxFileTransferBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading local source: %w", err)
	}
	if len(content) > ssh.MaxFileTransferBytes {
		return nil, fmt.Errorf("%w: local source grew beyond %d bytes",
			ssh.ErrFileTransferTooLarge, ssh.MaxFileTransferBytes)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("reading local source: %w", err)
	}
	return content, nil
}

func copyFromRemote(ctx context.Context, reg *registry.Registry, localRoot string, src, dst copyEndpoint, overwrite bool) (*mcpsdk.CallToolResult, error) {
	cs, err := reg.Resolve(src.alias)
	if err != nil {
		return toolErrorFor(err), nil
	}
	remotePath, err := resolveRemoteCopyPath(cs, src.path)
	if err != nil {
		return toolErrorFor(err), nil
	}
	localPath, exists, err := inspectLocalCopyDestinationRooted(localRoot, dst.path)
	if err != nil {
		return toolErrorFor(err), nil
	}
	if !overwrite && exists {
		return toolError(fmt.Sprintf("destination %s already exists locally; set overwrite=true to replace it", localPath)), nil
	}
	copyExecutor, ok := cs.Executor.(ssh.RootedFileExecutor)
	if !ok {
		return toolError("copy from codespace: executor does not support rooted file copies"), nil
	}
	content, err := copyExecutor.ReadFileRooted(ctx, ssh.RootedReadRequest{
		Path: remotePath,
		Root: cs.Workdir,
	})
	if err != nil {
		return toolErrorForf(err, "copy from codespace"), nil
	}
	if err := writeLocalFileAtomicRootedWithHooks(ctx, localRoot, dst.path, content, overwrite, localCopyWriteHooks{}); err != nil {
		return toolError(fmt.Sprintf("writing local destination: %v", err)), nil
	}
	return toolSuccess(fmt.Sprintf("Copied cs://%s/%s to %s", cs.Alias, strings.TrimPrefix(src.path, "/"), localPath)), nil
}

func remotePathExists(ctx context.Context, executor ssh.Executor, path string) (bool, error) {
	_, stderr, exitCode, err := executor.RunBash(ctx, fmt.Sprintf("test -e %[1]s || test -L %[1]s", shellQuote(path)), "/")
	if err != nil {
		return false, fmt.Errorf("checking remote destination: %w", err)
	}
	switch exitCode {
	case 0:
		return true, nil
	case 1:
		return false, nil
	default:
		return false, fmt.Errorf("checking remote destination failed (exit %d): %s", exitCode, strings.TrimSpace(stderr))
	}
}

// --- remote_view ---

func viewTool() mcpsdk.Tool {
	return mcpsdk.Tool{
		Name:        "remote_view",
		Description: "View a file or directory on the remote codespace. Supports local view-style ranges, large-file overrides, directory listings, and structured image or binary metadata when the executor provides them. Replaces the local 'view' tool.",
		InputSchema: mcpsdk.ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"codespace": codespaceParam,
				"path": map[string]any{
					"type":        "string",
					"description": "Path to the file or directory to view",
				},
				"view_range": map[string]any{
					"type":        "array",
					"description": "Optional [start_line, end_line] range. Use -1 for end_line to read to end of file.",
					"items":       map[string]any{"type": "integer"},
					"minItems":    2,
					"maxItems":    2,
				},
				"forceReadLargeFiles": map[string]any{
					"type":        "boolean",
					"description": "When true, bypasses large-file safeguards if the remote executor supports it.",
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
			return toolErrorFor(err), nil
		}
		path, err := requiredString(req, "path")
		if err != nil {
			return toolErrorFor(err), nil
		}

		viewRange, err := optionalViewRangeArg(req)
		if err != nil {
			return toolErrorFor(err), nil
		}
		forceReadLargeFiles, err := optionalBoolArgStrict(req, "forceReadLargeFiles")
		if err != nil {
			return toolErrorFor(err), nil
		}

		result, err := ssh.ExecuteView(ctx, c, ssh.ViewRequest{
			Path:                path,
			ViewRange:           viewRange,
			ForceReadLargeFiles: forceReadLargeFiles,
		})
		if err != nil {
			return toolErrorFor(err), nil
		}
		return viewToolSuccess(path, result), nil
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
			return toolErrorFor(err), nil
		}
		path, err := requiredString(req, "path")
		if err != nil {
			return toolErrorFor(err), nil
		}
		oldStr, err := requiredString(req, "old_str")
		if err != nil {
			return toolErrorFor(err), nil
		}
		newStr, err := requiredString(req, "new_str")
		if err != nil {
			return toolErrorFor(err), nil
		}

		if err := c.EditFile(ctx, path, oldStr, newStr); err != nil {
			return toolErrorFor(err), nil
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
			return toolErrorFor(err), nil
		}
		path, err := requiredString(req, "path")
		if err != nil {
			return toolErrorFor(err), nil
		}
		content, err := requiredString(req, "file_text")
		if err != nil {
			return toolErrorFor(err), nil
		}

		if err := c.CreateFile(ctx, path, content); err != nil {
			return toolErrorFor(err), nil
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

var remoteShellIDSequence atomic.Uint64

func bashTool() mcpsdk.Tool {
	return mcpsdk.Tool{
		Name:        "remote_bash",
		Description: "Execute a bash command on the remote codespace. By default, it starts one lightweight non-PTY process, waits briefly for quick completion, and retains that same process under a shellId if it is still running. Use mode 'async' for stdin, PTY, or explicitly backgrounded commands. Replaces the local 'bash' tool.",
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
			return toolErrorFor(err), nil
		}
		command, err := requiredString(req, "command")
		if err != nil {
			return toolErrorFor(err), nil
		}

		mode := optionalString(req, "mode")
		shellId := optionalString(req, "shellId")
		cwd := optionalString(req, "cwd")
		if shellId == "" {
			shellId = fmt.Sprintf("sh-%d-%d", time.Now().UnixMilli(), remoteShellIDSequence.Add(1))
		}

		if mode == "async" {
			if err := c.StartSession(ctx, shellId, command, cwd); err != nil {
				return toolErrorFor(err), nil
			}
			// Wait briefly and capture initial output
			select {
			case <-ctx.Done():
				return toolErrorFor(ctx.Err()), nil
			case <-time.After(time.Duration(asyncRemoteBashInitialDelay * float64(time.Second))):
			}
			output, _ := c.ReadSession(ctx, shellId)
			return toolSuccess(fmt.Sprintf("Started async session: %s\n\n%s", shellId, output)), nil
		}

		initialWait := optionalFloat(req, "initial_wait", defaultRemoteBashInitialWait)
		if starter, ok := c.(ssh.ProcessSessionStarter); ok && starter.SupportsProcessSessions() {
			if err := starter.StartProcessSession(ctx, shellId, command, cwd); err != nil {
				return toolErrorFor(err), nil
			}
		} else if err := c.StartSession(ctx, shellId, command, cwd); err != nil {
			if !canFallbackAfterSessionStartError(ctx, err) {
				return toolErrorFor(err), nil
			}
			return runBashSyncFallback(ctx, c, command, cwd), nil
		}
		wait := time.Duration(initialWait * float64(time.Second))
		output, completed, err := waitForRemoteSession(ctx, c, shellId, wait)
		if err != nil {
			if stopErr := stopSessionForCleanup(c, shellId); stopErr != nil {
				return toolError(fmt.Sprintf("%s\n\nAdditionally, failed to stop session %s after read failure: %v", toolErrorMessage(err), shellId, stopErr)), nil
			}
			return toolErrorFor(err), nil
		}

		if completed {
			finalOutput := trimSessionExitMarker(output)
			if err := stopSessionForCleanup(c, shellId); err != nil {
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

func canFallbackAfterSessionStartError(ctx context.Context, err error) bool {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	_, connectionLost := connectionLostGuidance(err)
	return !connectionLost
}

func waitForRemoteSession(ctx context.Context, c ssh.Executor, shellID string, wait time.Duration) (string, bool, error) {
	if waiter, ok := c.(ssh.SessionWaiter); ok && waiter.SupportsWaitSession() {
		return waiter.WaitSession(ctx, shellID, wait)
	}

	output, err := waitForSessionOutput(ctx, c, shellID, wait)
	return output, sessionOutputExited(output), err
}

func waitForSessionOutput(ctx context.Context, c ssh.Executor, shellID string, wait time.Duration) (string, error) {
	deadline := time.Now().Add(wait)
	delay := 100 * time.Millisecond
	var output string

	for {
		readCtx := ctx
		cancel := func() {}
		if wait > 0 {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return output, nil
			}
			readCtx, cancel = context.WithTimeout(ctx, remaining)
		}

		nextOutput, err := c.ReadSession(readCtx, shellID)
		cancel()
		if err != nil && ctx.Err() == nil && wait > 0 && errors.Is(err, context.DeadlineExceeded) {
			return output, nil
		}
		if err != nil || wait <= 0 {
			return nextOutput, err
		}
		output = nextOutput
		if sessionOutputExited(output) {
			return output, nil
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			return output, nil
		}
		if delay > remaining {
			delay = remaining
		}

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return "", ctx.Err()
		case <-timer.C:
		}

		if delay < time.Second {
			delay *= 2
			if delay > time.Second {
				delay = time.Second
			}
		}
	}
}

func stopSessionForCleanup(c ssh.Executor, shellID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return c.StopSession(ctx, shellID)
}

func runBashSyncFallback(ctx context.Context, c ssh.Executor, command, cwd string) *mcpsdk.CallToolResult {
	stdout, stderr, exitCode, err := c.RunBash(ctx, command, cwd)
	if err != nil {
		errMsg := toolErrorMessage(err)
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
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) == sessionExitedMarker {
			lines = append(lines[:i], lines[i+1:]...)
			break
		}
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n")
}

// --- remote_write_bash ---

func writeBashTool() mcpsdk.Tool {
	return mcpsdk.Tool{
		Name:        "remote_write_bash",
		Description: "Send input to an async remote bash session on the codespace, then wait for completion or the requested delay before returning output. Sync process sessions are non-interactive. Supports special keys: {enter}, {up}, {down}, {left}, {right}, {backspace}. Replaces the local 'write_bash' tool.",
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
					"description": "Maximum seconds to wait for the session to complete before returning current output (default: 2). Returns sooner when the session completes.",
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
			return toolErrorFor(err), nil
		}
		shellId, err := requiredString(req, "shellId")
		if err != nil {
			return toolErrorFor(err), nil
		}

		input := optionalString(req, "input")
		if input != "" {
			if err := c.WriteSession(ctx, shellId, input); err != nil {
				return toolErrorFor(err), nil
			}
		}

		delay := optionalFloat(req, "delay", 2)
		output, _, err := waitForRemoteSession(ctx, c, shellId, time.Duration(delay*float64(time.Second)))
		if err != nil {
			return toolErrorFor(err), nil
		}
		return toolSuccess(output), nil
	}
}

// --- remote_read_bash ---

func readBashTool() mcpsdk.Tool {
	return mcpsdk.Tool{
		Name:        "remote_read_bash",
		Description: "Wait for a remote bash session to complete, then return its last 100 lines of output. If the requested delay expires first, returns current output so the session can be checked again. Replaces the local 'read_bash' tool.",
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
					"description": "Maximum seconds to wait for the session to complete before returning current output (default: 2). Returns sooner when the session completes.",
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
			return toolErrorFor(err), nil
		}
		shellId, err := requiredString(req, "shellId")
		if err != nil {
			return toolErrorFor(err), nil
		}

		delay := optionalFloat(req, "delay", 2)
		output, _, err := waitForRemoteSession(ctx, c, shellId, time.Duration(delay*float64(time.Second)))
		if err != nil {
			return toolErrorFor(err), nil
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
			return toolErrorFor(err), nil
		}
		shellId, err := requiredString(req, "shellId")
		if err != nil {
			return toolErrorFor(err), nil
		}

		if err := c.StopSession(ctx, shellId); err != nil {
			return toolErrorFor(err), nil
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
			return toolErrorFor(err), nil
		}
		result, err := c.ListSessions(ctx)
		if err != nil {
			return toolErrorFor(err), nil
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
		Description: "Search file contents on the remote codespace with local rg-style options. Replaces the local 'rg' tool.",
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
					"description": "Legacy single directory or file to search in (defaults to '.' within cwd)",
				},
				"paths": map[string]any{
					"description": "A single path or multiple paths to search in (defaults to '.' within cwd)",
					"oneOf": []any{
						map[string]any{"type": "string"},
						map[string]any{
							"type":  "array",
							"items": map[string]any{"type": "string"},
						},
					},
				},
				"output_mode": map[string]any{
					"type":        "string",
					"description": "Output format",
					"enum": []string{
						string(ssh.GrepOutputModeContent),
						string(ssh.GrepOutputModeFilesWithMatches),
						string(ssh.GrepOutputModeCount),
					},
				},
				"glob": map[string]any{
					"type":        "string",
					"description": "Glob pattern to filter files (e.g., '*.go', '*.ts')",
				},
				"type": map[string]any{
					"type":        "string",
					"description": "File type filter (e.g., 'go', 'ts', 'py')",
				},
				"-i": map[string]any{
					"type":        "boolean",
					"description": "Case insensitive search",
				},
				"-A": map[string]any{
					"type":        "integer",
					"description": "Lines of context after each match",
				},
				"-B": map[string]any{
					"type":        "integer",
					"description": "Lines of context before each match",
				},
				"-C": map[string]any{
					"type":        "integer",
					"description": "Lines of context before and after each match",
				},
				"-n": map[string]any{
					"type":        "boolean",
					"description": "Show line numbers",
				},
				"head_limit": map[string]any{
					"type":        "integer",
					"description": "Limit output to the first N results",
				},
				"multiline": map[string]any{
					"type":        "boolean",
					"description": "Enable multiline mode where patterns can span lines",
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
			return toolErrorFor(err), nil
		}
		pattern, err := requiredString(req, "pattern")
		if err != nil {
			return toolErrorFor(err), nil
		}

		paths, err := optionalPathsArg(req, "paths")
		if err != nil {
			return toolErrorFor(err), nil
		}
		outputMode, err := optionalGrepOutputModeArg(req, "output_mode")
		if err != nil {
			return toolErrorFor(err), nil
		}
		caseInsensitive, err := optionalBoolArgStrict(req, "-i")
		if err != nil {
			return toolErrorFor(err), nil
		}
		afterContext, err := optionalIntArg(req, "-A")
		if err != nil {
			return toolErrorFor(err), nil
		}
		beforeContext, err := optionalIntArg(req, "-B")
		if err != nil {
			return toolErrorFor(err), nil
		}
		contextLines, err := optionalIntArg(req, "-C")
		if err != nil {
			return toolErrorFor(err), nil
		}
		lineNumbers, err := optionalBoolPtrArg(req, "-n")
		if err != nil {
			return toolErrorFor(err), nil
		}
		headLimit, err := optionalIntArg(req, "head_limit")
		if err != nil {
			return toolErrorFor(err), nil
		}
		multiline, err := optionalBoolArgStrict(req, "multiline")
		if err != nil {
			return toolErrorFor(err), nil
		}

		result, err := ssh.ExecuteGrep(ctx, c, ssh.GrepRequest{
			Pattern:         pattern,
			Path:            optionalString(req, "path"),
			Paths:           paths,
			Glob:            optionalString(req, "glob"),
			OutputMode:      outputMode,
			Type:            optionalString(req, "type"),
			CaseInsensitive: caseInsensitive,
			AfterContext:    afterContext,
			BeforeContext:   beforeContext,
			Context:         contextLines,
			LineNumbers:     lineNumbers,
			HeadLimit:       headLimit,
			Multiline:       multiline,
			Cwd:             optionalString(req, "cwd"),
		})
		if err != nil {
			return toolErrorFor(err), nil
		}
		if result.Output == "" {
			return toolStructuredSuccess("No matches found.", result), nil
		}
		return toolStructuredSuccess(result.Output, result), nil
	}
}

// --- remote_glob ---

func globTool() mcpsdk.Tool {
	return mcpsdk.Tool{
		Name:        "remote_glob",
		Description: "Find files matching a glob pattern on the remote codespace with local glob-style path selection. Replaces the local 'glob' tool.",
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
					"description": "Legacy single directory to search in (defaults to '.' within cwd)",
				},
				"paths": map[string]any{
					"description": "A single directory or multiple directories to search in (defaults to '.' within cwd)",
					"oneOf": []any{
						map[string]any{"type": "string"},
						map[string]any{
							"type":  "array",
							"items": map[string]any{"type": "string"},
						},
					},
				},
				"cwd": map[string]any{
					"type":        "string",
					"description": "Optional working directory for this call. Pass it explicitly for parallel-safe remote_glob usage instead of relying on remote_cd ordering.",
				},
				"limit": map[string]any{
					"type":        "integer",
					"description": fmt.Sprintf("Maximum number of matches to return (defaults to %d, capped at %d).", ssh.DefaultGlobLimit, ssh.MaxGlobLimit),
					"minimum":     1,
					"maximum":     ssh.MaxGlobLimit,
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
			return toolErrorFor(err), nil
		}
		pattern, err := requiredString(req, "pattern")
		if err != nil {
			return toolErrorFor(err), nil
		}

		paths, err := optionalPathsArg(req, "paths")
		if err != nil {
			return toolErrorFor(err), nil
		}
		limit, err := optionalIntArg(req, "limit")
		if err != nil {
			return toolErrorFor(err), nil
		}

		result, err := ssh.ExecuteGlob(ctx, c, ssh.GlobRequest{
			Pattern: pattern,
			Path:    optionalString(req, "path"),
			Paths:   paths,
			Cwd:     optionalString(req, "cwd"),
			Limit:   limit,
		})
		if err != nil {
			return toolErrorFor(err), nil
		}
		if result.Output == "" {
			return toolStructuredSuccess("No matches found.", result), nil
		}
		return toolStructuredSuccess(result.Output, result), nil
	}
}

// --- remote_apply_patch ---

func applyPatchTool() mcpsdk.Tool {
	return mcpsdk.Tool{
		Name:        "remote_apply_patch",
		Description: "Apply a canonical apply_patch payload on the remote codespace. Replaces the local 'apply_patch' tool for repository changes that must happen remotely.",
		InputSchema: mcpsdk.ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"codespace": codespaceParam,
				"patch": map[string]any{
					"type":        "string",
					"description": "The canonical apply_patch payload beginning with '*** Begin Patch' and ending with '*** End Patch'.",
				},
				"cwd": map[string]any{
					"type":        "string",
					"description": "Optional working directory for this call.",
				},
			},
			Required: []string{"patch"},
		},
	}
}

func applyPatchHandler(reg *registry.Registry) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		c, err := resolveExecutor(reg, req)
		if err != nil {
			return toolErrorFor(err), nil
		}
		patch, err := requiredString(req, "patch")
		if err != nil {
			return toolErrorFor(err), nil
		}

		result, err := ssh.ExecuteApplyPatch(ctx, c, ssh.ApplyPatchRequest{
			Patch: patch,
			Cwd:   optionalString(req, "cwd"),
		})
		if err != nil {
			return toolErrorFor(err), nil
		}
		return toolStructuredSuccess(applyPatchResultText(result), result), nil
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

func optionalBoolArgStrict(req mcpsdk.CallToolRequest, key string) (bool, error) {
	args := req.GetArguments()
	val, ok := args[key]
	if !ok {
		return false, nil
	}
	b, ok := val.(bool)
	if !ok {
		return false, fmt.Errorf("parameter %s must be a boolean", key)
	}
	return b, nil
}

func optionalBoolPtrArg(req mcpsdk.CallToolRequest, key string) (*bool, error) {
	args := req.GetArguments()
	val, ok := args[key]
	if !ok {
		return nil, nil
	}
	b, ok := val.(bool)
	if !ok {
		return nil, fmt.Errorf("parameter %s must be a boolean", key)
	}
	return &b, nil
}

func optionalIntArg(req mcpsdk.CallToolRequest, key string) (int, error) {
	args := req.GetArguments()
	val, ok := args[key]
	if !ok {
		return 0, nil
	}
	n, ok := toInt(val)
	if !ok {
		return 0, fmt.Errorf("parameter %s must be an integer", key)
	}
	return n, nil
}

func optionalViewRangeArg(req mcpsdk.CallToolRequest) ([]int, error) {
	args := req.GetArguments()
	val, ok := args["view_range"]
	if !ok {
		return nil, nil
	}
	viewRange, err := intSliceArg("view_range", val)
	if err != nil {
		return nil, err
	}
	if len(viewRange) != 2 {
		return nil, fmt.Errorf("view_range must contain exactly 2 integers")
	}
	start, end := viewRange[0], viewRange[1]
	if start < 1 {
		return nil, fmt.Errorf("view_range start_line must be >= 1")
	}
	if end != -1 && end < start {
		return nil, fmt.Errorf("view_range end_line must be -1 or >= start_line")
	}
	return viewRange, nil
}

func optionalPathsArg(req mcpsdk.CallToolRequest, key string) ([]string, error) {
	args := req.GetArguments()
	val, ok := args[key]
	if !ok {
		return nil, nil
	}
	switch typed := val.(type) {
	case string:
		return []string{typed}, nil
	case []string:
		return append([]string(nil), typed...), nil
	case []any:
		out := make([]string, 0, len(typed))
		for i, item := range typed {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("parameter %s[%d] must be a string", key, i)
			}
			out = append(out, s)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("parameter %s must be a string or array of strings", key)
	}
}

func optionalGrepOutputModeArg(req mcpsdk.CallToolRequest, key string) (ssh.GrepOutputMode, error) {
	mode := optionalString(req, key)
	switch ssh.GrepOutputMode(mode) {
	case "":
		return "", nil
	case ssh.GrepOutputModeContent, ssh.GrepOutputModeFilesWithMatches, ssh.GrepOutputModeCount:
		return ssh.GrepOutputMode(mode), nil
	default:
		return "", fmt.Errorf("parameter %s must be one of: %s, %s, %s", key, ssh.GrepOutputModeContent, ssh.GrepOutputModeFilesWithMatches, ssh.GrepOutputModeCount)
	}
}

func intSliceArg(key string, value any) ([]int, error) {
	switch typed := value.(type) {
	case []int:
		return append([]int(nil), typed...), nil
	case []any:
		out := make([]int, 0, len(typed))
		for i, item := range typed {
			n, ok := toInt(item)
			if !ok {
				return nil, fmt.Errorf("parameter %s[%d] must be an integer", key, i)
			}
			out = append(out, n)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("parameter %s must be an array of integers", key)
	}
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

func toolStructuredSuccess(text string, structured any, extra ...mcpsdk.Content) *mcpsdk.CallToolResult {
	content := make([]mcpsdk.Content, 0, 1+len(extra))
	content = append(content, mcpsdk.TextContent{
		Type: "text",
		Text: text,
	})
	content = append(content, extra...)
	return &mcpsdk.CallToolResult{
		Content:           content,
		StructuredContent: structured,
	}
}

func viewToolSuccess(path string, result ssh.ViewResult) *mcpsdk.CallToolResult {
	content := []mcpsdk.Content{
		mcpsdk.TextContent{
			Type: "text",
			Text: viewResultText(path, result),
		},
	}
	structured := result
	if result.Kind == ssh.ViewKindImage {
		structured.Base64Data = ""
		if result.Base64Data != "" && result.MimeType != "" {
			content = append(content, mcpsdk.ImageContent{
				Type:     "image",
				Data:     result.Base64Data,
				MIMEType: result.MimeType,
			})
		}
	}
	return &mcpsdk.CallToolResult{
		Content:           content,
		StructuredContent: structured,
	}
}

func viewResultText(path string, result ssh.ViewResult) string {
	if result.Content != "" {
		return result.Content
	}
	switch result.Kind {
	case ssh.ViewKindDirectory:
		if len(result.Entries) == 0 {
			return ""
		}
		return strings.Join(result.Entries, "\n")
	case ssh.ViewKindImage:
		if result.MimeType != "" {
			return fmt.Sprintf("%s (%s)", path, result.MimeType)
		}
		return path
	default:
		if result.Content == "" && result.MimeType != "" && (result.Truncated || result.Base64Data != "") {
			return fmt.Sprintf("%s (%s)", path, result.MimeType)
		}
		return result.Content
	}
}

func applyPatchResultText(result ssh.ApplyPatchResult) string {
	if strings.TrimSpace(result.Output) != "" {
		return result.Output
	}
	switch result.FilesChanged {
	case 1:
		return "Applied patch to 1 file."
	case 0:
		return "Patch applied."
	default:
		return fmt.Sprintf("Applied patch to %d files.", result.FilesChanged)
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
			return toolErrorFor(err), nil
		}
		path, err := requiredString(req, "path")
		if err != nil {
			return toolErrorFor(err), nil
		}

		// Validate the directory exists on the codespace
		quoted := "'" + strings.ReplaceAll(path, "'", "'\"'\"'") + "'"
		stdout, _, exitCode, execErr := c.RunBash(ctx, fmt.Sprintf("cd %s && pwd", quoted), c.GetWorkdir())
		if execErr != nil {
			return toolErrorForf(execErr, "failed to change directory"), nil
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
			return toolErrorFor(err), nil
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
			return toolErrorFor(err), nil
		}
		codespaceName := cs.Name

		sshCmd := fmt.Sprintf("gh codespace ssh -c %s", codespaceName)

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

func listAvailableCodespacesHandler(ghRunner GHRunner, policy CodespaceAccessPolicy) server.ToolHandlerFunc {
	return listAvailableCodespacesHandlerWithState(newLifecycleState(LifecycleConfig{
		GHRunner:     ghRunner,
		AccessPolicy: policy,
	}))
}

func listAvailableCodespacesHandlerWithState(state *lifecycleState) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		output, err := state.cfg.GHRunner.Run(ctx, "codespace", "list",
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

		policy := state.accessPolicy()
		filtered := codespaces[:0]
		for _, cs := range codespaces {
			if !policy.allowsExistingCodespace(cs.Name) {
				continue
			}
			filtered = append(filtered, cs)
		}
		codespaces = filtered

		if len(codespaces) == 0 {
			return toolSuccess(policy.emptyAvailableCodespacesMessage()), nil
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
