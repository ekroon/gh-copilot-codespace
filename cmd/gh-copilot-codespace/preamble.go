package main

import (
	"fmt"
	"strings"

	"github.com/ekroon/gh-copilot-codespace/internal/mcp"
)

// PreambleMode describes how Copilot's cwd relates to the user's local source.
type PreambleMode int

const (
	// PreambleModeMirror means the launcher created a separate mirror dir as cwd
	// (the normal launcher flow). No local source coexists with the remote one.
	PreambleModeMirror PreambleMode = iota
	// PreambleModeHere means the user passed --here so cwd is the user's local
	// checkout. Local and remote sources coexist and are not auto-synced.
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
	fmt.Fprintf(&sb, "You are working on a remote GitHub Codespace. Source code lives on the codespace at %s, NOT locally.\n\n", cs.Workdir)
	if ctx.Mode == PreambleModeHere {
		sb.WriteString("A local checkout of the project also exists at Copilot's launch directory. The local files and the remote codespace are NOT auto-synced — treat them as separate working copies and orchestrate any sync explicitly. Use the built-in local tools (view, edit, create, bash) only when you want to act on the local copy; use the `remote_*` tools to act on the codespace.\n\n")
	}
	sb.WriteString("## Tool routing\n\n")
	sb.WriteString("- **Source code on the codespace** (view/edit/create/search): use `remote_view`, `remote_edit`, `remote_create`, `remote_grep`, `remote_glob`.\n")
	sb.WriteString("- **Shell commands on the codespace**: use `remote_bash`; do NOT use local bash for codespace work.\n")
	sb.WriteString("- **Local session files** (plan.md, session state, notes under `~/.copilot/`): use the built-in local tools (`view`, `edit`, `create`).\n")
	sb.WriteString("- **Codebase exploration on the codespace**: delegate to `@remote-explorer`; the built-in explore agent cannot reach remote files.\n")
	sb.WriteString("- Do not make placeholder, empty, or no-op tool calls. Only call tools with the real arguments needed for the task.\n\n")
	return sb.String()
}

func buildMultiPreamble(ctx PreambleContext) string {
	var sb strings.Builder
	sb.WriteString("# Multi-Codespace Remote Development\n\n")
	sb.WriteString("You are connected to multiple remote GitHub Codespaces. Source code lives on the codespaces, NOT locally.\n\n")
	if ctx.Mode == PreambleModeHere {
		sb.WriteString("A local checkout of the project also exists at Copilot's launch directory. Local files and the remote codespaces are NOT auto-synced — keep them separate and orchestrate any sync explicitly.\n\n")
	}
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
	sb.WriteString("\n## Tool routing\n\n")
	sb.WriteString("- All `remote_*` tools accept an optional `codespace` parameter; pass the alias to target a specific codespace.\n")
	sb.WriteString("- **Source code on a codespace** (view/edit/create/search): use `remote_view`, `remote_edit`, `remote_create`, `remote_grep`, `remote_glob`.\n")
	sb.WriteString("- **Shell commands on a codespace**: use `remote_bash`; do NOT use local bash for codespace work.\n")
	sb.WriteString("- For `remote_bash`, `remote_grep`, `remote_glob`: pass `cwd` explicitly when you need parallel-safe or targeted execution; `remote_cd` only changes the default cwd for later sequential calls.\n")
	sb.WriteString("- Use `list_codespaces` to see currently connected codespaces.\n")
	sb.WriteString("- **Local session files** (plan.md, session state, notes under `~/.copilot/`): use the built-in local tools (`view`, `edit`, `create`).\n")
	sb.WriteString("- **Codebase exploration on a codespace**: delegate to `@remote-explorer`; the built-in explore agent cannot reach remote files.\n")
	sb.WriteString("- Do not make placeholder, empty, or no-op tool calls. Only call tools with the real arguments needed for the task.\n\n")
	return sb.String()
}

func buildZeroPreamble(ctx PreambleContext) string {
	policy := ctx.AccessPolicy
	var sb strings.Builder
	sb.WriteString("# Codespace Lifecycle Session\n\n")
	sb.WriteString("You are not connected to any remote GitHub Codespaces yet, so project source code is not available locally.\n\n")
	sb.WriteString("## What to do first\n\n")

	switch {
	case policy.SelectedOnly && len(policy.AllowedCodespaceNames) == 0:
		sb.WriteString("- This session was launched with `--selected-only`, and no existing codespaces were selected at startup.\n")
		sb.WriteString("- Use `get_codespace_options` and then `create_codespace` to create the first codespace for this session.\n")
		sb.WriteString("- `list_available_codespaces` will not show any existing codespaces for this session, and `connect_codespace` cannot attach an existing codespace until you create one from this session.\n")
	case policy.SelectedOnly:
		sb.WriteString("- This session was launched with `--selected-only`, so existing codespaces are limited to the ones selected at startup plus codespaces created from this session.\n")
		sb.WriteString("- Use `list_available_codespaces` to discover which existing codespaces are currently allowlisted for this session.\n")
		sb.WriteString("- Use `connect_codespace` to attach one of those allowlisted existing codespaces.\n")
		sb.WriteString("- Use `get_codespace_options` and then `create_codespace` to create a new codespace for the repository you need.\n")
	default:
		sb.WriteString("- Use `list_available_codespaces` to discover existing codespaces you can connect to.\n")
		sb.WriteString("- Use `get_codespace_options` and then `create_codespace` to create a new codespace for the repository you need.\n")
		sb.WriteString("- Use `connect_codespace` to attach an existing codespace to this session.\n")
	}

	if policy.SelectedOnly {
		sb.WriteString("- When you `--resume` this session, the allowlist keeps the existing codespaces selected at startup plus any codespaces created from this session.\n")
	}

	sb.WriteString("- After at least one codespace is connected, use `list_codespaces` to confirm aliases, then use the `remote_*` tools for source-code work.\n")
	sb.WriteString("- Do not use local bash for remote source-code work; use `remote_bash` after connecting a codespace. Local bash is only for local-only tasks.\n")
	sb.WriteString("- Do not make placeholder, empty, or no-op tool calls. Only call tools with the real arguments needed for the task.\n")
	sb.WriteString("- **Local session files** (plan.md, session state, notes under `~/.copilot/`): use the built-in local tools (`view`, `edit`, `create`).\n\n")
	return sb.String()
}
