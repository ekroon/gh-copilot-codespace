package mcp

import (
	"context"
	"fmt"
	"strings"

	mcpsdk "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func (r *WorkspaceRuntime) artifactTools() []runtimeTool {
	var tools []runtimeTool
	for _, name := range []string{"artifact_view", "artifact_write", "artifact_import", "artifact_export"} {
		properties := map[string]any{
			"path": map[string]any{"type": "string", "description": "Local output file within configured ArtifactDir; never the checkout."},
		}
		required := []string{"path"}
		description := "Explicit local artifact read, confined to ArtifactDir."
		if name != "artifact_view" {
			properties["overwrite"] = map[string]any{"type": "boolean", "description": "Replace an existing destination (default false)."}
		}
		switch name {
		case "artifact_write":
			properties["content"] = map[string]any{"type": "string", "description": "Text to write to the explicit local output file."}
			required = append(required, "content")
			description = "Explicit local text output, confined to ArtifactDir; atomic writes, no symlink traversal."
		case "artifact_import", "artifact_export":
			properties["remote_path"] = map[string]any{"type": "string", "description": "Path within the pinned remote workdir (relative to startup workdir or absolute)."}
			required = append(required, "remote_path")
			description = "Copy one file from pinned remote workspace into local ArtifactDir (import), or from local ArtifactDir to pinned remote workspace (export). Rooted, symlink-safe file copy; no implicit synchronization."
		}
		toolName := name
		tools = append(tools, runtimeTool{
			tool: mcpsdk.Tool{Name: name, Description: description, InputSchema: mcpsdk.ToolInputSchema{
				Type: "object", Properties: properties, Required: required,
			}},
			handler: func(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
				return r.callArtifact(ctx, toolName, req)
			},
		})
	}
	return tools
}

func (r *WorkspaceRuntime) artifactPath(value string) error {
	if r.artifact == "" {
		return fmt.Errorf("local artifacts are disabled: configure an explicit ArtifactDir")
	}
	if strings.TrimSpace(value) == "" || invalidWorkspacePath(value) {
		return fmt.Errorf("invalid artifact path %q", value)
	}
	_, err := localCopyCandidate(r.artifact, value)
	return err
}

func (r *WorkspaceRuntime) callArtifact(ctx context.Context, name string, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	localPath, err := requiredString(req, "path")
	if err != nil {
		return toolErrorFor(err), nil
	}
	if err := r.artifactPath(localPath); err != nil {
		return toolErrorFor(err), nil
	}
	overwrite, err := optionalBoolArgStrict(req, "overwrite")
	if err != nil {
		return toolErrorFor(err), nil
	}
	switch name {
	case "artifact_view":
		data, err := readLocalCopySourceRootedWithHooks(ctx, r.artifact, localPath, localCopyReadHooks{})
		if err != nil {
			return toolErrorFor(err), nil
		}
		return toolSuccess(string(data)), nil
	case "artifact_write":
		content, ok := req.GetArguments()["content"].(string)
		if !ok {
			return toolError("content must be a string"), nil
		}
		if err := writeLocalFileAtomicRootedWithHooks(ctx, r.artifact, localPath, []byte(content), overwrite, localCopyWriteHooks{}); err != nil {
			return toolErrorFor(err), nil
		}
		return toolSuccess("Wrote local artifact " + localPath), nil
	default:
		if err := r.connectedPrimary(); err != nil {
			return toolErrorFor(err), nil
		}
		value, err := requiredString(req, "remote_path")
		if err != nil {
			return toolErrorFor(err), nil
		}
		remotePath, err := r.remotePath(value, r.workdir)
		if err != nil {
			return toolErrorFor(err), nil
		}
		remote := copyEndpoint{remote: true, alias: r.alias, path: strings.TrimPrefix(remotePath, r.workdir+"/")}
		local := copyEndpoint{path: localPath}
		if remotePath == r.workdir {
			return toolError("remote_path must name a file within the workdir"), nil
		}
		if name == "artifact_import" {
			return copyFromRemote(ctx, r.reg, r.artifact, remote, local, overwrite)
		}
		return copyToRemote(ctx, r.reg, r.artifact, local, remote, overwrite)
	}
}

func (r *WorkspaceRuntime) artifactCopyHandler() server.ToolHandlerFunc {
	bridge := remoteCopyHandler(r.reg, r.artifact)
	return func(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		if r.artifact == "" {
			return toolError("remote_copy local side is disabled: configure an explicit ArtifactDir"), nil
		}
		if _, err := optionalBoolArgStrict(req, "overwrite"); err != nil {
			return toolErrorFor(err), nil
		}
		for _, key := range []string{"source", "destination"} {
			value, err := requiredString(req, key)
			if err != nil {
				return toolErrorFor(err), nil
			}
			endpoint, err := parseCopyEndpoint(value)
			if err != nil {
				return toolErrorFor(err), nil
			}
			if !endpoint.remote {
				if err := r.artifactPath(value); err != nil {
					return toolErrorFor(err), nil
				}
			} else if endpoint.alias == "" {
				return toolError("remote_copy requires an explicit remote alias"), nil
			}
		}
		return bridge(ctx, req)
	}
}
