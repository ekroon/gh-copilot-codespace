package main

import (
	"strings"
	"testing"

	"github.com/ekroon/gh-copilot-codespace/internal/mcp"
)

func TestBuildPreamble_SingleCodespaceUsesCurrentDirectoryModel(t *testing.T) {
	context := PreambleContext{
		Codespaces: []PreambleCodespace{
			{Alias: "github", Workdir: "/workspaces/github"},
		},
	}
	got := BuildPreamble(context)

	for _, want := range []string{
		"# Codespace Remote Development",
		"/workspaces/github",
		"current directory",
		"project instructions, agents, and context",
		"NOT synchronized",
		"remote_view",
		"remote_edit",
		"remote_create",
		"remote_apply_patch",
		"remote_grep",
		"remote_glob",
		"builds, tests, linters",
		"dependency",
		"repository scripts",
		"git commands",
		"remote_bash",
		"pass `cwd` explicitly",
		"local project instructions and agents",
		"Copilot session artifacts",
		"explicit local-only work",
		"remote_copy",
		"one-time transfer",
		"not synchronization",
		"@remote-explorer",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in single-codespace preamble:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"--resume", "launcher workspace", "MCP"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("unexpected %q in single-codespace preamble:\n%s", unwanted, got)
		}
	}
}

func TestBuildPreamble_ModeDoesNotChangeGuidance(t *testing.T) {
	context := PreambleContext{
		Codespaces: []PreambleCodespace{
			{Alias: "github", Workdir: "/workspaces/github"},
		},
	}
	context.Mode = PreambleModeMirror
	mirror := BuildPreamble(context)
	context.Mode = PreambleModeHere
	here := BuildPreamble(context)

	if mirror != here {
		t.Fatalf("preamble modes produced different guidance:\nmirror:\n%s\nhere:\n%s", mirror, here)
	}
}

func TestBuildPreamble_MultiCodespaceRetainsAliasesAndCwdGuidance(t *testing.T) {
	got := BuildPreamble(PreambleContext{
		Codespaces: []PreambleCodespace{
			{Alias: "github", Repository: "github/github", Branch: "main", Workdir: "/workspaces/github"},
			{Alias: "docs", Repository: "github/docs", Workdir: "/workspaces/docs"},
		},
	})

	for _, want := range []string{
		"# Multi-Codespace Remote Development",
		"github/github",
		"github/docs",
		"(default)",
		"`codespace` parameter",
		"list_codespaces",
		"remote_apply_patch",
		"pass `cwd` explicitly",
		"current directory",
		"NOT synchronized",
		"remote_copy",
		"cs://<alias>/<path>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in multi-codespace preamble:\n%s", want, got)
		}
	}
}

func TestBuildPreamble_SelectedOnlyDescribesFixedStartupSet(t *testing.T) {
	tests := []struct {
		name       string
		codespaces []PreambleCodespace
	}{
		{
			name: "single codespace",
			codespaces: []PreambleCodespace{
				{Alias: "github", Workdir: "/workspaces/github"},
			},
		},
		{
			name: "multiple codespaces",
			codespaces: []PreambleCodespace{
				{Alias: "github", Workdir: "/workspaces/github"},
				{Alias: "docs", Workdir: "/workspaces/docs"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildPreamble(PreambleContext{
				Codespaces: tt.codespaces,
				AccessPolicy: mcp.CodespaceAccessPolicy{
					SelectedOnly: true,
				},
			})

			for _, want := range []string{
				"--selected-only",
				"limited to the codespaces connected at startup",
				"Codespace lifecycle tools are unavailable",
			} {
				if !strings.Contains(got, want) {
					t.Errorf("missing %q in selected-only preamble:\n%s", want, got)
				}
			}
			for _, unwanted := range []string{
				"list_available_codespaces",
				"get_codespace_options",
				"create_codespace",
				"connect_codespace",
				"delete_codespace",
			} {
				if strings.Contains(got, unwanted) {
					t.Errorf("unexpected %q in selected-only preamble:\n%s", unwanted, got)
				}
			}
		})
	}
}

func TestBuildPreamble_ZeroSelectedOnlyHasNoLifecycleGuidance(t *testing.T) {
	got := BuildPreamble(PreambleContext{
		AccessPolicy: mcp.CodespaceAccessPolicy{
			SelectedOnly: true,
		},
	})

	for _, want := range []string{
		"# Codespace Lifecycle Session",
		"--selected-only",
		"requires at least one codespace selected at startup",
		"Codespace lifecycle tools are unavailable",
		"current directory",
		"project instructions, agents, and context",
		"repository work",
		"remote_*",
		"remote_apply_patch",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in zero-codespace preamble:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{
		"--resume",
		"launcher workspace",
		"project source code is not available locally",
		"list_available_codespaces",
		"get_codespace_options",
		"create_codespace",
		"connect_codespace",
		"delete_codespace",
	} {
		if strings.Contains(got, unwanted) {
			t.Errorf("unexpected %q in zero-codespace preamble:\n%s", unwanted, got)
		}
	}
}
