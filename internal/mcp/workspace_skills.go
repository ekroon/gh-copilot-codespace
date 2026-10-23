package mcp

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/ekroon/gh-copilot-codespace/internal/ssh"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"gopkg.in/yaml.v3"
)

type workspaceSkill struct {
	Name        string `json:"name" yaml:"name"`
	Description string `json:"description" yaml:"description"`
	Path        string `json:"path" yaml:"-"`
	Base        string `json:"base" yaml:"-"`
	content     string
}

func (r *WorkspaceRuntime) skillTool() mcpsdk.Tool {
	return mcpsdk.Tool{
		Name:        "workspace_skill",
		Description: "List explicitly configured REMOTE skills (omit name), or load by exact listed name. Only SKILL.md in a configured SkillRoots directory or its immediate children is considered; no ambient/local skill roots. First configured root, then lexically first path wins duplicate names. Returns markdown with remote path/base; companion files remain remote. This is an extension tool, not a native skillProvider. Configured roots: " + strings.Join(r.roots, ", "),
		InputSchema: mcpsdk.ToolInputSchema{
			Type: "object", Properties: map[string]any{
				"name": map[string]any{"type": "string", "description": "Exact skill name from listing; omit to list names, descriptions and remote paths. Arbitrary paths are not accepted."},
			},
		},
	}
}

func (r *WorkspaceRuntime) skillHandler() server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		for key := range req.GetArguments() {
			if key != "name" {
				return toolError("workspace_skill accepts only an optional exact name, never a path or root"), nil
			}
		}
		name := ""
		if value, ok := req.GetArguments()["name"]; ok {
			var valid bool
			name, valid = value.(string)
			if !valid || !workspaceIdentifier.MatchString(name) {
				return toolError("invalid skill name; use an exact name from workspace_skill listing"), nil
			}
		}
		if err := r.connectedPrimary(); err != nil {
			return toolErrorFor(err), nil
		}
		skills, duplicates, err := r.discoverSkills(ctx)
		if err != nil {
			return toolErrorFor(err), nil
		}
		if name != "" {
			for _, skill := range skills {
				if skill.Name == name {
					return toolStructuredSuccess(fmt.Sprintf("Remote skill: %s\nPath: %s\nCompanion-file base (remote): %s\n\n%s", skill.Name, skill.Path, skill.Base, skill.content), skill), nil
				}
			}
			return toolError(fmt.Sprintf("skill %q was not found in explicitly configured remote SkillRoots", name)), nil
		}
		var lines []string
		for _, skill := range skills {
			lines = append(lines, fmt.Sprintf("%s — %s\n  remote path: %s; companion base: %s", skill.Name, skill.Description, skill.Path, skill.Base))
		}
		if len(lines) == 0 {
			lines = append(lines, "No skills found in explicitly configured remote SkillRoots. No ambient or local roots were searched.")
		}
		for _, duplicate := range duplicates {
			lines = append(lines, "Ignored duplicate (first configured root / lexical path wins): "+duplicate)
		}
		return toolStructuredSuccess(strings.Join(lines, "\n"), map[string]any{"skills": skills, "ignoredDuplicates": duplicates}), nil
	}
}

func (r *WorkspaceRuntime) discoverSkills(ctx context.Context) ([]workspaceSkill, []string, error) {
	skills := make([]workspaceSkill, 0)
	var duplicates []string
	seen := make(map[string]bool)
	if len(r.roots) == 0 {
		return skills, duplicates, nil
	}
	reader, ok := r.primary.Executor.(ssh.RootedFileExecutor)
	if !ok {
		return nil, nil, fmt.Errorf("workspace skills require an executor supporting rooted remote reads")
	}
	for _, root := range r.roots {
		result, err := ssh.ExecuteGlob(ctx, r.primary.Executor, ssh.GlobRequest{
			// Basename matching works in both fd-backed native helpers and
			// SSH fallback executors; path globs have different fd semantics.
			Pattern: "SKILL.md", Paths: []string{root}, Cwd: root, Limit: ssh.MaxGlobLimit,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("listing skill root %s: %w", root, err)
		}
		if result.Truncated {
			return nil, nil, fmt.Errorf("skill root %s listing was truncated; configure narrower explicit roots", root)
		}
		candidates := strings.Split(strings.TrimSpace(result.Output), "\n")
		sort.Strings(candidates)
		pathsSeen := make(map[string]bool)
		for _, candidate := range candidates {
			if candidate == "" {
				continue
			}
			if invalidWorkspacePath(candidate) {
				return nil, nil, fmt.Errorf("invalid skill path returned for root %s: %q", root, candidate)
			}
			resolved := candidate
			if !path.IsAbs(resolved) {
				resolved = path.Join(root, candidate)
			}
			resolved = path.Clean(resolved)
			if !strings.HasPrefix(resolved, root+"/") {
				return nil, nil, fmt.Errorf("skill path %q is outside configured root %s", candidate, root)
			}
			relative := strings.TrimPrefix(resolved, root+"/")
			if path.Base(relative) != "SKILL.md" || strings.Count(relative, "/") > 1 || pathsSeen[resolved] {
				continue
			}
			pathsSeen[resolved] = true
			data, err := reader.ReadFileRooted(ctx, ssh.RootedReadRequest{Path: resolved, Root: root})
			if err != nil {
				return nil, nil, fmt.Errorf("reading remote skill %s: %w", resolved, err)
			}
			skill, err := parseWorkspaceSkill(data, resolved)
			if err != nil {
				return nil, nil, err
			}
			if seen[skill.Name] {
				duplicates = append(duplicates, skill.Name+" at "+resolved)
				continue
			}
			seen[skill.Name] = true
			skills = append(skills, skill)
		}
	}
	sort.Slice(skills, func(i, j int) bool { return skills[i].Name < skills[j].Name })
	return skills, duplicates, nil
}

func parseWorkspaceSkill(data []byte, remotePath string) (workspaceSkill, error) {
	if len(data) > 256*1024 {
		return workspaceSkill{}, fmt.Errorf("skill %s exceeds 256 KiB", remotePath)
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return workspaceSkill{}, fmt.Errorf("skill %s requires YAML frontmatter with name and description", remotePath)
	}
	end := strings.Index(text[4:], "\n---\n")
	if end < 0 {
		return workspaceSkill{}, fmt.Errorf("skill %s has unterminated YAML frontmatter", remotePath)
	}
	var skill workspaceSkill
	if err := yaml.Unmarshal([]byte(text[4:4+end]), &skill); err != nil {
		return workspaceSkill{}, fmt.Errorf("skill %s frontmatter: %w", remotePath, err)
	}
	if !workspaceIdentifier.MatchString(skill.Name) || strings.TrimSpace(skill.Description) == "" {
		return workspaceSkill{}, fmt.Errorf("skill %s requires a safe name and nonempty description", remotePath)
	}
	skill.Path, skill.Base, skill.content = remotePath, path.Dir(remotePath), text
	return skill, nil
}
