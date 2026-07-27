package main

import (
	"slices"
	"strings"
	"testing"
)

func TestRemoteExplorerInlineAgentUsesRemoteCurrentDirectoryGuidance(t *testing.T) {
	agent := remoteExplorerInlineAgent(true)
	if agent == nil {
		t.Fatal("remoteExplorerInlineAgent(true) returned nil")
	}

	for _, want := range []string{
		"remote GitHub Codespaces",
		"repository source",
		"remote_grep",
		"remote_glob",
		"remote_view",
		"remote_bash",
		"list_codespaces",
		"`codespace` alias",
		"cwd explicitly",
		"Do not use local built-in",
	} {
		if !strings.Contains(agent.Prompt, want) {
			t.Errorf("remote explorer prompt missing %q:\n%s", want, agent.Prompt)
		}
	}
	for _, tool := range []string{"remote_grep", "remote_glob", "remote_view", "remote_bash", "remote_cwd", "list_codespaces"} {
		if !slices.Contains(agent.Tools, tool) {
			t.Errorf("remote explorer tools missing %q: %v", tool, agent.Tools)
		}
	}
}

func TestRemoteExplorerAgentVariantsSharePrompt(t *testing.T) {
	markdown := remoteExplorerAgentMarkdown()
	if !strings.Contains(markdown, remoteExplorerAgentPrompt) {
		t.Fatal("on-disk agent does not contain the inline prompt")
	}
	if !strings.Contains(markdown, "codespace/*") {
		t.Fatal("on-disk agent does not allow codespace tools")
	}
}

func TestRemoteExplorerInlineAgentRequiresRemoteTools(t *testing.T) {
	if agent := remoteExplorerInlineAgent(false); agent != nil {
		t.Fatalf("remoteExplorerInlineAgent(false) = %#v, want nil", agent)
	}
}
