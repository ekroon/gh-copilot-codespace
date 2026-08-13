package main

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestRemoteExplorerInlineAgentUsesRemoteCurrentDirectoryGuidance(t *testing.T) {
	agent := remoteExplorerInlineAgent(true)
	if agent == nil {
		t.Fatal("remoteExplorerInlineAgent(true) returned nil")
	}

	lowerPrompt := strings.ToLower(agent.Prompt)
	for _, want := range []string{
		"remote GitHub Codespaces",
		"repository source",
		"remote_bash",
		"remote_write_bash",
		"remote_read_bash",
		"remote_stop_bash",
		"remote_list_bash",
		"remote_grep",
		"remote_glob",
		"remote_view",
		"remote_cwd",
		"list_codespaces",
		"use list_codespaces",
		"explicit cwd",
		"async",
		"interactive",
		"Do not use local built-in",
	} {
		if !strings.Contains(lowerPrompt, strings.ToLower(want)) {
			t.Errorf("remote explorer prompt missing %q:\n%s", want, agent.Prompt)
		}
	}
	if strings.Contains(strings.ToLower(agent.Prompt), "read-only") {
		t.Fatalf("remote explorer prompt should not describe itself as read-only:\n%s", agent.Prompt)
	}
	for _, tool := range []string{
		"remote_bash",
		"remote_write_bash",
		"remote_read_bash",
		"remote_stop_bash",
		"remote_list_bash",
		"remote_grep",
		"remote_glob",
		"remote_view",
		"remote_cwd",
		"list_codespaces",
	} {
		if !slices.Contains(agent.Tools, tool) {
			t.Errorf("remote explorer tools missing %q: %v", tool, agent.Tools)
		}
	}
}

func TestRemoteExplorerAgentVariantsSharePromptAndTools(t *testing.T) {
	agent := remoteExplorerInlineAgent(true)
	if agent == nil {
		t.Fatal("remoteExplorerInlineAgent(true) returned nil")
	}
	wantExtensionTools := []string{
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
	markdown := remoteExplorerAgentMarkdown()
	if !strings.Contains(markdown, remoteExplorerAgentPrompt) {
		t.Fatal("on-disk agent does not contain the inline prompt")
	}
	if strings.Contains(markdown, "codespace/*") {
		t.Fatal("on-disk agent uses wildcard codespace tool access")
	}
	if !reflect.DeepEqual(agent.Tools, wantExtensionTools) {
		t.Fatalf("inline tools mismatch: got %v, want %v", agent.Tools, wantExtensionTools)
	}
	if got, want := remoteExplorerMarkdownTools(t, markdown), remoteExplorerMCPTools(); !reflect.DeepEqual(got, want) {
		t.Fatalf("markdown tools = %v, want %v", got, want)
	}
	for _, forbidden := range []string{"\n  - read\n", "\n  - search\n"} {
		if strings.Contains(markdown, forbidden) {
			t.Fatalf("on-disk agent unexpectedly allows local tool %q", strings.TrimSpace(forbidden))
		}
	}
}

func TestRemoteExplorerAgentAllowListExcludesMutators(t *testing.T) {
	agent := remoteExplorerInlineAgent(true)
	if agent == nil {
		t.Fatal("remoteExplorerInlineAgent(true) returned nil")
	}

	for _, forbidden := range []string{
		"remote_edit",
		"remote_create",
		"remote_copy",
		"remote_apply_patch",
		"remote_cd",
		"list_available_codespaces",
		"get_codespace_options",
		"create_codespace",
		"connect_codespace",
		"delete_codespace",
		"open_shell",
	} {
		if slices.Contains(agent.Tools, forbidden) {
			t.Fatalf("inline agent unexpectedly allows mutating tool %q", forbidden)
		}
	}

	markdownTools := remoteExplorerMarkdownTools(t, remoteExplorerAgentMarkdown())
	for _, forbidden := range []string{
		"codespace/remote_edit",
		"codespace/remote_create",
		"codespace/remote_copy",
		"codespace/remote_apply_patch",
		"codespace/remote_cd",
		"codespace/list_available_codespaces",
		"codespace/get_codespace_options",
		"codespace/create_codespace",
		"codespace/connect_codespace",
		"codespace/delete_codespace",
		"codespace/open_shell",
	} {
		if slices.Contains(markdownTools, forbidden) {
			t.Fatalf("markdown agent unexpectedly allows mutating tool %q", forbidden)
		}
	}
}

func TestRemoteExplorerInlineAgentRequiresRemoteTools(t *testing.T) {
	if agent := remoteExplorerInlineAgent(false); agent != nil {
		t.Fatalf("remoteExplorerInlineAgent(false) = %#v, want nil", agent)
	}
}

func remoteExplorerMarkdownTools(t *testing.T, markdown string) []string {
	t.Helper()

	lines := strings.Split(markdown, "\n")
	var tools []string
	inTools := false
	for _, line := range lines {
		switch {
		case line == "tools:":
			inTools = true
		case inTools && line == "---":
			return tools
		case inTools && strings.HasPrefix(line, "  - "):
			tools = append(tools, strings.TrimSpace(strings.TrimPrefix(line, "  - ")))
		}
	}
	if !inTools {
		t.Fatalf("agent markdown missing tools section:\n%s", markdown)
	}
	t.Fatalf("agent markdown tools section missing closing frontmatter:\n%s", markdown)
	return nil
}
