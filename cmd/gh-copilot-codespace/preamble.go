package main

import (
	"fmt"
	"strings"

	"github.com/ekroon/gh-copilot-codespace/internal/mcp"
)

// PreambleMode is retained for compatibility with launcher call sites.
// Preamble rendering now uses one current-directory model regardless of mode.
type PreambleMode int

const (
	PreambleModeMirror PreambleMode = iota
	PreambleModeHere
)

// PreambleCodespace describes a connected codespace for preamble rendering.
type PreambleCodespace struct {
	Alias      string
	Repository string
	Branch     string
	Workdir    string
}

// PreambleContext bundles everything needed to render any of the codespace preambles.
type PreambleContext struct {
	Mode         PreambleMode
	Codespaces   []PreambleCodespace
	AccessPolicy mcp.CodespaceAccessPolicy
}

// BuildPreamble selects and renders the appropriate preamble for the context.
func BuildPreamble(ctx PreambleContext) string {
	switch {
	case len(ctx.Codespaces) == 0:
		return buildZeroPreamble(ctx)
	case len(ctx.Codespaces) == 1:
		return buildSinglePreamble(ctx)
	default:
		return buildMultiPreamble(ctx)
	}
}

func buildSinglePreamble(ctx PreambleContext) string {
	cs := ctx.Codespaces[0]
	var sb strings.Builder
	sb.WriteString("# Codespace Remote Development\n\n")
	fmt.Fprintf(&sb, "The repository working copy for implementation lives on the codespace at %s.\n\n", cs.Workdir)
	writeCurrentDirectoryGuidance(&sb)
	writeSelectedOnlyGuidance(&sb, ctx.AccessPolicy)
	writeRemoteRoutingGuidance(&sb)
	return sb.String()
}

func buildMultiPreamble(ctx PreambleContext) string {
	var sb strings.Builder
	sb.WriteString("# Multi-Codespace Remote Development\n\n")
	sb.WriteString("You are connected to multiple remote GitHub Codespaces. Repository working copies for implementation live on those codespaces.\n\n")
	writeCurrentDirectoryGuidance(&sb)
	sb.WriteString("## Connected codespaces\n\n")
	sb.WriteString("| Alias | Repository | Branch | Workdir |\n")
	sb.WriteString("|-------|-----------|--------|--------|\n")
	for _, cs := range ctx.Codespaces {
		branch := cs.Branch
		if branch == "" {
			branch = "(default)"
		}
		fmt.Fprintf(&sb, "| %s | %s | %s | %s |\n", cs.Alias, cs.Repository, branch, cs.Workdir)
	}
	sb.WriteString("\n")
	writeSelectedOnlyGuidance(&sb, ctx.AccessPolicy)
	sb.WriteString("\n## Tool routing\n\n")
	sb.WriteString("- All `remote_*` tools accept an optional `codespace` parameter; pass the alias to target a specific codespace.\n")
	sb.WriteString("- Use `list_codespaces` to see currently connected codespaces.\n")
	writeRemoteRoutingItems(&sb)
	return sb.String()
}

func buildZeroPreamble(ctx PreambleContext) string {
	policy := ctx.AccessPolicy
	var sb strings.Builder
	sb.WriteString("# Codespace Lifecycle Session\n\n")
	sb.WriteString("You are not connected to any remote GitHub Codespaces yet. The local checkout in Copilot's current directory may supply project instructions, agents, and context, but wait for a codespace connection before doing repository work.\n\n")
	sb.WriteString("## What to do first\n\n")

	switch {
	case policy.SelectedOnly:
		sb.WriteString("- `--selected-only` requires at least one codespace selected at startup; this session has none.\n")
		sb.WriteString("- Codespace lifecycle tools are unavailable in selected-only sessions.\n")
		sb.WriteString("- Relaunch and select the codespace or codespaces the agent should use.\n")
	default:
		sb.WriteString("- Use `list_available_codespaces` to discover existing codespaces you can connect to.\n")
		sb.WriteString("- Use `get_codespace_options` and then `create_codespace` to create a new codespace for the repository you need.\n")
		sb.WriteString("- Use `connect_codespace` to attach an existing codespace to this session.\n")
	}

	sb.WriteString("- After at least one codespace is connected, use `list_codespaces` to confirm aliases, then use the `remote_*` tools for source-code work.\n")
	sb.WriteString("- After connecting, route source reads, edits, creates, patches, and searches through `remote_view`, `remote_edit`, `remote_create`, `remote_apply_patch`, `remote_grep`, and `remote_glob`.\n")
	sb.WriteString("- Route builds, tests, linters, dependency operations, repository scripts, and git commands through `remote_bash`.\n")
	sb.WriteString("- Reserve built-in local tools for local project instructions and agents, Copilot session artifacts, and explicit local-only work.\n")
	sb.WriteString("- Do not make placeholder, empty, or no-op tool calls. Only call tools with the real arguments needed for the task.\n")
	sb.WriteString("- Use `remote_copy` only for an explicit one-time transfer after connecting; it is not synchronization.\n\n")
	return sb.String()
}

func writeSelectedOnlyGuidance(sb *strings.Builder, policy mcp.CodespaceAccessPolicy) {
	if !policy.SelectedOnly {
		return
	}
	sb.WriteString("## Selected-only session\n\n")
	sb.WriteString("- This session was launched with `--selected-only` and is limited to the codespaces connected at startup.\n")
	sb.WriteString("- Codespace lifecycle tools are unavailable; do not create, connect, disconnect, or delete codespaces.\n\n")
}

func writeCurrentDirectoryGuidance(sb *strings.Builder) {
	sb.WriteString("A local checkout in Copilot's current directory supplies project instructions, agents, and context. It is NOT synchronized with any codespace working copy and must not be used implicitly for repository implementation work.\n\n")
}

func writeRemoteRoutingGuidance(sb *strings.Builder) {
	sb.WriteString("## Tool routing\n\n")
	writeRemoteRoutingItems(sb)
}

func writeRemoteRoutingItems(sb *strings.Builder) {
	sb.WriteString("- **Repository source reads, edits, creates, patches, and searches**: use `remote_view`, `remote_edit`, `remote_create`, `remote_apply_patch`, `remote_grep`, and `remote_glob`.\n")
	sb.WriteString("- **Repository commands**: use `remote_bash` for builds, tests, linters, dependency installs or updates, repository scripts, and git commands.\n")
	sb.WriteString("- For `remote_bash`, `remote_grep`, and `remote_glob`, pass `cwd` explicitly when execution must be targeted or parallel-safe; `remote_cd` only changes the default for later sequential calls.\n")
	sb.WriteString("- Reserve built-in local tools for local project instructions and agents, Copilot session artifacts, and explicit local-only work.\n")
	sb.WriteString("- Local and remote files are separate. Use `remote_copy` only for an explicit one-time transfer to or from `cs://<alias>/<path>`; it is not synchronization.\n")
	sb.WriteString("- **Codebase exploration on a codespace**: delegate to `@remote-explorer`; the built-in explore agent cannot reach remote files.\n")
	sb.WriteString("- Do not make placeholder, empty, or no-op tool calls. Only call tools with the real arguments needed for the task.\n\n")
}
