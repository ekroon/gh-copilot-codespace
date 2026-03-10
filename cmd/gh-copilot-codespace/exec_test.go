package main

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/ekroon/gh-copilot-codespace/internal/codespaceenv"
)

func TestRewriteMCPServerForSSH_WithRemoteBinary(t *testing.T) {
	server := map[string]any{
		"type":    "stdio",
		"command": "python3",
		"args":    []any{"server.py", "--mode", "dev"},
		"env": map[string]any{
			"API_KEY": "secret",
		},
	}

	t.Setenv("GITHUB_TOKEN", "local-token")

	result := rewriteMCPServerForSSH("/usr/local/bin/self", codespaceenv.GitHubAuthLocal, server, "my-cs", "/workspaces/repo", "/tmp/gh-copilot-codespace-bin/gh-copilot-codespace")

	if result == nil {
		t.Fatal("rewriteMCPServerForSSH returned nil")
	}

	args, ok := result["args"].([]string)
	if !ok {
		t.Fatal("args not []string")
	}

	// Rewritten server should invoke the local proxy helper, not gh directly.
	if args[0] != "proxy" {
		t.Errorf("args should start with [proxy], got %v", args[0])
	}

	// Should contain the remote binary path
	found := false
	for _, a := range args {
		if a == "/tmp/gh-copilot-codespace-bin/gh-copilot-codespace" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("args should contain remote binary path, got %v", args)
	}

	if got := result["command"]; got != "/usr/local/bin/self" {
		t.Fatalf("command = %v, want /usr/local/bin/self", got)
	}

	for _, forbidden := range []string{"local-token", "GITHUB_TOKEN=local-token"} {
		if contains(strings.Join(args, " "), forbidden) {
			t.Fatalf("rewritten args should not serialize local token %q: %v", forbidden, args)
		}
	}

	// Should contain --workdir
	foundWorkdir := false
	for i, a := range args {
		if a == "--workdir" && i+1 < len(args) && args[i+1] == "/workspaces/repo" {
			foundWorkdir = true
			break
		}
	}
	if !foundWorkdir {
		t.Errorf("args should contain '--workdir /workspaces/repo', got %v", args)
	}

	// Should contain mode and env forwarding through the helper
	for _, want := range []string{"--github-auth", "local", "--env", "API_KEY=secret"} {
		if !contains(strings.Join(args, "\n"), want) {
			t.Errorf("args should contain %q, got %v", want, args)
		}
	}

	// Command should appear after --
	foundSeparator := false
	foundCmd := false
	for _, a := range args {
		if a == "--" {
			foundSeparator = true
		}
		if foundSeparator && a == "python3" {
			foundCmd = true
		}
	}
	if !foundSeparator || !foundCmd {
		t.Errorf("command should appear after -- separator, got %v", args)
	}
}

func TestRewriteMCPServerForSSH_FallbackWithoutBinary(t *testing.T) {
	server := map[string]any{
		"command": "python3",
		"args":    []any{"server.py"},
	}

	result := rewriteMCPServerForSSH("/usr/local/bin/self", codespaceenv.GitHubAuthCodespace, server, "cs", "/workspaces/repo", "")

	if result == nil {
		t.Fatal("rewriteMCPServerForSSH returned nil")
	}

	args := result["args"].([]string)

	if got := result["command"]; got != "/usr/local/bin/self" {
		t.Fatalf("command = %v, want /usr/local/bin/self", got)
	}
	if !contains(strings.Join(args, " "), "proxy") {
		t.Errorf("fallback rewrite should still use proxy helper, got %v", args)
	}
}

func TestRewriteHooksForSSH_WithRemoteBinary(t *testing.T) {
	hooksJSON := `{
		"version": 1,
		"hooks": {
			"preToolUse": [
				{
					"type": "command",
					"bash": "./scripts/check.sh",
					"cwd": "scripts",
					"env": {"LOG_LEVEL": "INFO"}
				}
			]
		}
	}`

	t.Setenv("GITHUB_TOKEN", "local-token")

	result := rewriteHooksForSSH("/usr/local/bin/self", codespaceenv.GitHubAuthLocal, []byte(hooksJSON), "my-cs", "/workspaces/repo", "/tmp/gh-copilot-codespace-bin/gh-copilot-codespace")
	if result == nil {
		t.Fatal("rewriteHooksForSSH returned nil")
	}

	var parsed map[string]any
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	hooks := parsed["hooks"].(map[string]any)
	preToolUse := hooks["preToolUse"].([]any)
	hook := preToolUse[0].(map[string]any)
	bash := hook["bash"].(string)

	// Should contain the proxy helper, mode, and remote binary path.
	if !contains(bash, "/usr/local/bin/self") {
		t.Errorf("should contain self binary path, got %q", bash)
	}
	if !contains(bash, "proxy") {
		t.Errorf("should contain proxy subcommand, got %q", bash)
	}
	if !contains(bash, "/tmp/gh-copilot-codespace-bin/gh-copilot-codespace") {
		t.Errorf("should contain remote binary path, got %q", bash)
	}
	if !contains(bash, "--github-auth") || !contains(bash, "local") {
		t.Errorf("should contain auth mode, got %q", bash)
	}
	if contains(bash, "local-token") {
		t.Errorf("hook rewrite should not serialize local token, got %q", bash)
	}

	// cwd and env should be removed from the hook object
	if _, ok := hook["cwd"]; ok {
		t.Error("cwd should be removed")
	}
	if _, ok := hook["env"]; ok {
		t.Error("env should be removed")
	}
}

func TestRewriteHooksForSSH_FallbackWithoutBinary(t *testing.T) {
	hooksJSON := `{"version":1,"hooks":{"sessionStart":[{"type":"command","bash":"echo hi","cwd":"."}]}}`

	result := rewriteHooksForSSH("/usr/local/bin/self", codespaceenv.GitHubAuthCodespace, []byte(hooksJSON), "cs", "/workspaces/repo", "")
	if result == nil {
		t.Fatal("rewriteHooksForSSH returned nil")
	}

	var parsed map[string]any
	json.Unmarshal(result, &parsed)
	hooks := parsed["hooks"].(map[string]any)
	ss := hooks["sessionStart"].([]any)
	hook := ss[0].(map[string]any)
	bash := hook["bash"].(string)

	if !contains(bash, "proxy") {
		t.Errorf("fallback should use proxy helper, got %q", bash)
	}
	if contains(bash, ".env-secrets") {
		t.Errorf("fallback hook should not bake bootstrap snippet into the hook file, got %q", bash)
	}
}

func TestRunExecBootstrapsCodespaceEnv(t *testing.T) {
	originalApply := applyCodespaceEnv
	originalExec := execProcess
	t.Cleanup(func() {
		applyCodespaceEnv = originalApply
		execProcess = originalExec
	})

	applyCodespaceEnv = func() {
		_ = os.Setenv("GITHUB_TOKEN", "bootstrap-token")
		_ = os.Setenv("GITHUB_SERVER_URL", "https://github.com")
	}

	var gotEnv map[string]string
	execProcess = func(_ string, _ []string, env []string) error {
		gotEnv = envSliceToMap(env)
		return errors.New("stop exec")
	}

	err := runExec([]string{"--", "sh"})
	if err == nil || err.Error() != "stop exec" {
		t.Fatalf("runExec() error = %v, want stop exec", err)
	}
	if gotEnv["GITHUB_TOKEN"] != "bootstrap-token" {
		t.Fatalf("GITHUB_TOKEN = %q, want bootstrap-token", gotEnv["GITHUB_TOKEN"])
	}
	if gotEnv["GITHUB_SERVER_URL"] != "https://github.com" {
		t.Fatalf("GITHUB_SERVER_URL = %q, want https://github.com", gotEnv["GITHUB_SERVER_URL"])
	}
}

func TestRunExecExplicitEnvOverridesBootstrap(t *testing.T) {
	originalApply := applyCodespaceEnv
	originalExec := execProcess
	t.Cleanup(func() {
		applyCodespaceEnv = originalApply
		execProcess = originalExec
	})

	applyCodespaceEnv = func() {
		_ = os.Setenv("GITHUB_TOKEN", "bootstrap-token")
	}

	var gotEnv map[string]string
	execProcess = func(_ string, _ []string, env []string) error {
		gotEnv = envSliceToMap(env)
		return errors.New("stop exec")
	}

	err := runExec([]string{"--env", "GITHUB_TOKEN=flag-token", "--", "sh"})
	if err == nil || err.Error() != "stop exec" {
		t.Fatalf("runExec() error = %v, want stop exec", err)
	}
	if gotEnv["GITHUB_TOKEN"] != "flag-token" {
		t.Fatalf("GITHUB_TOKEN = %q, want flag-token", gotEnv["GITHUB_TOKEN"])
	}
}

func TestRunExecExplicitEnvOverridesBootstrapHostValues(t *testing.T) {
	originalApply := applyCodespaceEnv
	originalExec := execProcess
	t.Cleanup(func() {
		applyCodespaceEnv = originalApply
		execProcess = originalExec
	})

	applyCodespaceEnv = func() {
		_ = os.Setenv("GITHUB_TOKEN", "bootstrap-token")
		_ = os.Setenv("GITHUB_API_URL", "https://api.github.com")
		_ = os.Setenv("GITHUB_SERVER_URL", "https://github.com")
	}

	var gotEnv map[string]string
	execProcess = func(_ string, _ []string, env []string) error {
		gotEnv = envSliceToMap(env)
		return errors.New("stop exec")
	}

	err := runExec([]string{
		"--env", "GITHUB_TOKEN=flag-token",
		"--env", "GH_TOKEN=flag-token",
		"--env", "GITHUB_API_URL=https://ghe.example.com/api/v3",
		"--env", "GITHUB_SERVER_URL=https://ghe.example.com",
		"--", "sh",
	})
	if err == nil || err.Error() != "stop exec" {
		t.Fatalf("runExec() error = %v, want stop exec", err)
	}
	if gotEnv["GITHUB_TOKEN"] != "flag-token" || gotEnv["GH_TOKEN"] != "flag-token" {
		t.Fatalf("token env = %#v, want explicit flag-token overrides", gotEnv)
	}
	if gotEnv["GITHUB_API_URL"] != "https://ghe.example.com/api/v3" {
		t.Fatalf("GITHUB_API_URL = %q, want https://ghe.example.com/api/v3", gotEnv["GITHUB_API_URL"])
	}
	if gotEnv["GITHUB_SERVER_URL"] != "https://ghe.example.com" {
		t.Fatalf("GITHUB_SERVER_URL = %q, want https://ghe.example.com", gotEnv["GITHUB_SERVER_URL"])
	}
}

func envSliceToMap(env []string) map[string]string {
	result := make(map[string]string, len(env))
	for _, kv := range env {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) == 2 {
			result[parts[0]] = parts[1]
		}
	}
	return result
}
