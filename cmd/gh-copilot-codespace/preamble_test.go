package main

import (
	"strings"
	"testing"

	"github.com/ekroon/gh-copilot-codespace/internal/mcp"
)

func TestBuildPreamble_SingleMirrorMode(t *testing.T) {
	got := BuildPreamble(PreambleContext{
		Mode: PreambleModeMirror,
		Codespaces: []PreambleCodespace{
			{Alias: "github", Workdir: "/workspaces/github"},
		},
	})
	for _, want := range []string{
		"# Codespace Remote Development",
		"/workspaces/github",
		"remote_view",
		"remote_bash",
		"@remote-explorer",
		"do NOT use local bash for codespace work",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in single-mirror preamble:\n%s", want, got)
		}
	}
	if strings.Contains(got, "A local checkout") {
		t.Fatalf("mirror mode should not mention a local checkout coexisting:\n%s", got)
	}
	if strings.Contains(got, "MCP") {
		t.Fatalf("preamble should be transport-neutral:\n%s", got)
	}
}

func TestBuildPreamble_SingleHereMode(t *testing.T) {
	got := BuildPreamble(PreambleContext{
		Mode: PreambleModeHere,
		Codespaces: []PreambleCodespace{
			{Alias: "github", Workdir: "/workspaces/github"},
		},
	})
	for _, want := range []string{
		"# Codespace Remote Development",
		"/workspaces/github",
		"A local checkout",
		"NOT auto-synced",
		"remote_view",
		"@remote-explorer",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in single-here preamble:\n%s", want, got)
		}
	}
}

func TestBuildPreamble_MultiHereMode(t *testing.T) {
	got := BuildPreamble(PreambleContext{
		Mode: PreambleModeHere,
		Codespaces: []PreambleCodespace{
			{Alias: "github", Repository: "github/github", Branch: "main", Workdir: "/workspaces/github"},
			{Alias: "docs", Repository: "github/docs", Workdir: "/workspaces/docs"},
		},
	})
	for _, want := range []string{
		"# Multi-Codespace Remote Development",
		"github/github",
		"github/docs",
		"(default)", // docs entry has no branch — should render fallback
		"A local checkout",
		"NOT auto-synced",
		"`codespace` parameter",
		"list_codespaces",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in multi-here preamble:\n%s", want, got)
		}
	}
}

func TestBuildPreamble_ZeroCodespaces_SelectedOnly(t *testing.T) {
	got := BuildPreamble(PreambleContext{
		Mode: PreambleModeMirror,
		AccessPolicy: mcp.CodespaceAccessPolicy{
			SelectedOnly: true,
		},
	})
	for _, want := range []string{
		"# Codespace Lifecycle Session",
		"--selected-only",
		"no existing codespaces were selected at startup",
		"create_codespace",
		"--resume",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in zero-selected-only preamble:\n%s", want, got)
		}
	}
	if strings.Contains(got, "MCP") {
		t.Fatalf("zero preamble should be transport-neutral:\n%s", got)
	}
}
