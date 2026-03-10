package main

import (
	"strings"
	"testing"
)

func TestBuildProxyExecUsesRemoteBinaryAndLocalAuth(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "local-token")
	t.Setenv("GITHUB_API_URL", "https://ghe.example.com/api/v3")

	path, args, err := buildProxyExec(proxyOptions{
		codespaceName: "my-cs",
		githubAuth:    "local",
		workdir:       "/workspaces/repo",
		remoteBinary:  "/tmp/gh-copilot-codespace-bin/gh-copilot-codespace",
		envVars:       []string{"API_KEY=secret"},
		cmdArgs:       []string{"python3", "server.py"},
	})
	if err != nil {
		t.Fatalf("buildProxyExec() error = %v", err)
	}

	if path == "" {
		t.Fatal("path is empty")
	}
	if got, want := args[:5], []string{"gh", "codespace", "ssh", "-c", "my-cs"}; !equalStringSlices(got, want) {
		t.Fatalf("args[:5] = %v, want %v", got, want)
	}

	for _, want := range []string{
		"/tmp/gh-copilot-codespace-bin/gh-copilot-codespace",
		"exec",
		"--workdir",
		"/workspaces/repo",
		"GITHUB_TOKEN=local-token",
		"GH_TOKEN=local-token",
		"GITHUB_API_URL=https://ghe.example.com/api/v3",
		"GITHUB_SERVER_URL=https://ghe.example.com",
		"API_KEY=secret",
		"python3",
		"server.py",
	} {
		if !containsArg(args, want) {
			t.Fatalf("buildProxyExec() missing %q in %v", want, args)
		}
	}
}

func TestBuildProxyExecUsesGHTokenWhenGitHubTokenMissing(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "gh-only-token")
	t.Setenv("GITHUB_API_URL", "")
	t.Setenv("GITHUB_SERVER_URL", "https://github.example.com")

	_, args, err := buildProxyExec(proxyOptions{
		codespaceName: "cs",
		githubAuth:    "local",
		remoteBinary:  "/tmp/gh-copilot-codespace-bin/gh-copilot-codespace",
		cmdArgs:       []string{"python3", "server.py"},
	})
	if err != nil {
		t.Fatalf("buildProxyExec() error = %v", err)
	}

	for _, want := range []string{
		"GITHUB_TOKEN=gh-only-token",
		"GH_TOKEN=gh-only-token",
		"GITHUB_API_URL=https://github.example.com/api/v3",
		"GITHUB_SERVER_URL=https://github.example.com",
	} {
		if !containsArg(args, want) {
			t.Fatalf("buildProxyExec() missing %q in %v", want, args)
		}
	}
}

func TestBuildProxyExecErrorsWhenLocalTokenMissing(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")

	_, _, err := buildProxyExec(proxyOptions{
		codespaceName: "cs",
		githubAuth:    "local",
		remoteBinary:  "/tmp/gh-copilot-codespace-bin/gh-copilot-codespace",
		cmdArgs:       []string{"python3", "server.py"},
	})
	if err == nil {
		t.Fatal("buildProxyExec() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "GITHUB_TOKEN or GH_TOKEN") {
		t.Fatalf("error = %q, want missing token message", err)
	}
}

func TestBuildProxyExecFallbackKeepsExplicitEnvAfterSessionAuth(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "local-token")

	_, args, err := buildProxyExec(proxyOptions{
		codespaceName: "cs",
		githubAuth:    "local",
		workdir:       "/workspaces/repo",
		envVars:       []string{"GITHUB_TOKEN=flag-token"},
		cmdArgs:       []string{"python3", "server.py"},
	})
	if err != nil {
		t.Fatalf("buildProxyExec() error = %v", err)
	}

	if got, want := args[len(args)-3], "bash"; got != want {
		t.Fatalf("args[len-3] = %q, want %q", got, want)
	}
	remoteCmd := args[len(args)-1]
	for _, want := range []string{
		".env-secrets",
		"export GITHUB_TOKEN='local-token'",
		"export GH_TOKEN='local-token'",
		"export GITHUB_TOKEN='flag-token'",
		"cd '/workspaces/repo' && exec 'python3' 'server.py'",
	} {
		if !strings.Contains(remoteCmd, want) {
			t.Fatalf("fallback command missing %q in %q", want, remoteCmd)
		}
	}
}

func equalStringSlices(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}
