package main

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
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

	result := rewriteMCPServerForSSH(server, "my-cs", "/workspaces/repo", "/tmp/gh-copilot-codespace-bin/gh-copilot-codespace")

	if result == nil {
		t.Fatal("rewriteMCPServerForSSH returned nil")
	}

	args, ok := result["args"].([]string)
	if !ok {
		t.Fatal("args not []string")
	}

	// Should use structured exec, not bash -c
	if args[0] != "codespace" || args[1] != "ssh" {
		t.Errorf("args should start with [codespace ssh], got %v", args[:2])
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

	// Should contain "exec" subcommand
	foundExec := false
	for _, a := range args {
		if a == "exec" {
			foundExec = true
			break
		}
	}
	if !foundExec {
		t.Errorf("args should contain 'exec', got %v", args)
	}

	// Should NOT contain "bash -c" (that's the old pattern)
	for i, a := range args {
		if a == "bash" && i+1 < len(args) && args[i+1] == "-c" {
			t.Errorf("should not use 'bash -c' with remote binary, got %v", args)
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

	// Should contain --env for API_KEY
	foundEnv := false
	for i, a := range args {
		if a == "--env" && i+1 < len(args) && args[i+1] == "API_KEY=secret" {
			foundEnv = true
			break
		}
	}
	if !foundEnv {
		t.Errorf("args should contain '--env API_KEY=secret', got %v", args)
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

	result := rewriteMCPServerForSSH(server, "cs", "/workspaces/repo", "")

	if result == nil {
		t.Fatal("rewriteMCPServerForSSH returned nil")
	}

	args := result["args"].([]string)

	// Should use bash -c fallback with quoted command
	foundBash := false
	for i, a := range args {
		if a == "bash" && i+1 < len(args) && args[i+1] == "-c" {
			foundBash = true
			// The command after -c should be shell-quoted (single quotes)
			if i+2 < len(args) && !strings.HasPrefix(args[i+2], "'") {
				t.Errorf("bash -c command should be shell-quoted, got %q", args[i+2])
			}
			break
		}
	}
	if !foundBash {
		t.Errorf("should use 'bash -c' fallback without remote binary, got %v", args)
	}
	if bashCmd := args[len(args)-1]; !contains(bashCmd, ".env-secrets") {
		t.Errorf("fallback command should bootstrap codespace auth env, got %q", bashCmd)
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

	result := rewriteHooksForSSH([]byte(hooksJSON), "my-cs", "/workspaces/repo", "/tmp/gh-copilot-codespace-bin/gh-copilot-codespace")
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

	// Should contain the remote binary path and exec subcommand
	if !contains(bash, "/tmp/gh-copilot-codespace-bin/gh-copilot-codespace") {
		t.Errorf("should contain remote binary path, got %q", bash)
	}
	if !contains(bash, "exec") {
		t.Errorf("should contain 'exec', got %q", bash)
	}
	if !contains(bash, "--workdir") {
		t.Errorf("should contain '--workdir', got %q", bash)
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

	result := rewriteHooksForSSH([]byte(hooksJSON), "cs", "/workspaces/repo", "")
	if result == nil {
		t.Fatal("rewriteHooksForSSH returned nil")
	}

	var parsed map[string]any
	json.Unmarshal(result, &parsed)
	hooks := parsed["hooks"].(map[string]any)
	ss := hooks["sessionStart"].([]any)
	hook := ss[0].(map[string]any)
	bash := hook["bash"].(string)

	// Fallback should use bash -c
	if !contains(bash, "bash -c") {
		t.Errorf("fallback should use 'bash -c', got %q", bash)
	}
	if !contains(bash, ".env-secrets") {
		t.Errorf("fallback hook should bootstrap codespace auth env, got %q", bash)
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
