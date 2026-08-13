package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// remoteExplorerAgentName is the canonical name of the codespace-aware
// exploration agent. The same name is used for the on-disk agent file (MCP
// mode) and the inline customAgents entry (extension-tools mode), so the agent
// is invoked the same way from a user's perspective regardless of transport.
const remoteExplorerAgentName = "remote-explorer"

// remoteExplorerAgentDescription is the description shown to the parent agent
// when it picks a sub-agent to delegate to.
const remoteExplorerAgentDescription = "Explore and search repository source on remote codespaces using remote tools. Use this agent instead of the built-in explore agent for finding files, searching code patterns, understanding codebase structure, reading specific files, and answering questions about remote code."

// remoteExplorerAgentPrompt is the system prompt for the inline agent. The
// frontmatter form on disk includes the same prompt body below; keeping them
// in one place ensures both transports describe the agent identically.
const remoteExplorerAgentPrompt = `You are a fast code exploration agent for remote GitHub Codespaces.

## Available tools

Use these remote tools to explore and diagnose the codespace:
- **list_codespaces** — list connected codespaces, aliases, and repository workdirs
- **remote_view** — read file contents with line numbers
- **remote_grep** — search for patterns in files (ripgrep)
- **remote_glob** — find files by name patterns
- **remote_bash** — run bash, gh, git, build, test, and diagnostic commands
- **remote_write_bash** — send input or special keys to an interactive/long-lived bash session
- **remote_read_bash** — read more output from an existing bash session
- **remote_list_bash** — list active bash sessions
- **remote_stop_bash** — stop an active bash session
- **remote_cwd** — inspect the current default working directory when diagnosing, but do not rely on it for repository work

## Guidelines

- All repository source lives on remote GitHub Codespaces. Do not use local built-in file, search, or shell tools for repository work.
- Use list_codespaces first, then use the target repository workdir from that result as your initial base directory.
- Pass explicit cwd to every remote_bash, remote_grep, and remote_glob call.
- Never rely on remote_cd or shared sticky cwd mutation; treat cwd as explicit per call.
- Use remote_write_bash/remote_read_bash/remote_list_bash/remote_stop_bash for async or interactive sessions.
- Be concise — return focused answers under 300 words
- Search broadly first, then narrow down
- Use remote_grep for content search, remote_glob for file discovery and structure
- With multiple codespaces, pass the ` + "`codespace` alias" + ` to every remote tool call that needs an explicit target
- Read only the relevant portions of files (use view_range)
- Keep to exploration and diagnosis; do not use local file/search/shell tools for remote repository work
`

// remoteExplorerExtensionTools is the shared command-capable allow-list used by
// the remote-explorer agent in extension-tools mode. The names match the
// runtime tool names exposed by the extension host (no "codespace/" namespace —
// that's an MCP-mode concept).
var remoteExplorerExtensionTools = []string{
	"remote_grep",
	"remote_glob",
	"remote_view",
	"remote_bash",
	"remote_write_bash",
	"remote_read_bash",
	"remote_stop_bash",
	"remote_list_bash",
	"remote_cwd",
	"list_codespaces",
}

func remoteExplorerMCPTools() []string {
	tools := make([]string, 0, len(remoteExplorerExtensionTools))
	for _, tool := range remoteExplorerExtensionTools {
		tools = append(tools, "codespace/"+tool)
	}
	return tools
}

// remoteExplorerInlineAgent returns the customAgentWire entry forwarded over
// `joinSession({ customAgents })` from the generated extension. Returns nil if
// the agent should not be advertised in the current context (e.g. no remote
// tools available).
func remoteExplorerInlineAgent(haveRemoteTools bool) *customAgentWire {
	if !haveRemoteTools {
		return nil
	}
	return &customAgentWire{
		Name:        remoteExplorerAgentName,
		Description: remoteExplorerAgentDescription,
		Prompt:      remoteExplorerAgentPrompt,
		Model:       "claude-haiku-4.5",
		Tools:       append([]string(nil), remoteExplorerExtensionTools...),
	}
}

// remoteExplorerAgentMarkdown returns the on-disk agent file body used in MCP
// mode (Copilot reads `.github/agents/*.agent.md`). The `tools` list uses MCP
// namespacing because in MCP mode the remote_* tools come from the `codespace`
// MCP server.
func remoteExplorerAgentMarkdown() string {
	tools := remoteExplorerMCPTools()
	toolLines := make([]string, 0, len(tools))
	for _, tool := range tools {
		toolLines = append(toolLines, "  - "+tool)
	}
	return fmt.Sprintf(`---
name: %s
description: >-
  %s
model: claude-haiku-4.5
tools:
%s
---

%s`, remoteExplorerAgentName, remoteExplorerAgentDescription, strings.Join(toolLines, "\n"), remoteExplorerAgentPrompt)
}

// writeRemoteExplorerAgentFile renders the on-disk variant into
// `<mirrorDir>/.github/agents/remote-explorer.agent.md`. Called from the
// launcher in MCP mode.
func writeRemoteExplorerAgentFile(mirrorDir string) {
	agentsDir := filepath.Join(mirrorDir, ".github", "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(agentsDir, remoteExplorerAgentName+".agent.md"), []byte(remoteExplorerAgentMarkdown()), 0o644)
}
