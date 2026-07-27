//go:build integration

package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/ekroon/gh-copilot-codespace/internal/mcp"
	"github.com/ekroon/gh-copilot-codespace/internal/registry"
	"github.com/ekroon/gh-copilot-codespace/internal/ssh"
)

// These tests require a running codespace with test fixtures.
// Run: TEST_CODESPACE=<name> go test -tags integration -v ./cmd/gh-copilot-codespace/

func testCodespace(t *testing.T) string {
	t.Helper()
	cs := os.Getenv("TEST_CODESPACE")
	if cs == "" {
		t.Skip("TEST_CODESPACE not set")
	}
	return cs
}

func testWorkdir(t *testing.T) string {
	t.Helper()
	wd := os.Getenv("TEST_WORKDIR")
	if wd == "" {
		return "/workspaces/ekroon"
	}
	return wd
}

func testSSHClient(t *testing.T, cs string) *ssh.Client {
	t.Helper()
	client := ssh.NewClient(cs)
	ctx := context.Background()
	if err := client.SetupMultiplexing(ctx); err != nil {
		t.Logf("SSH multiplexing not available, using fallback: %v", err)
	}
	return client
}

func TestIntegration_DeployAndExec(t *testing.T) {
	cs := testCodespace(t)

	client := testSSHClient(t, cs)

	// Deploy binary to codespace
	remotePath, err := deployBinary(client, cs)
	if err != nil {
		t.Fatalf("deployBinary: %v", err)
	}

	if remotePath == "" {
		t.Fatal("deployBinary returned empty path")
	}

	// Verify the binary exists and is executable
	out, err := exec.Command("gh", "codespace", "ssh", "-c", cs, "--",
		remotePath, "exec", "--workdir", "/tmp", "--", "echo", "hello-from-exec").CombinedOutput()
	if err != nil {
		t.Fatalf("exec on codespace failed: %v\nOutput: %s", err, string(out))
	}

	if !strings.Contains(string(out), "hello-from-exec") {
		t.Errorf("exec output should contain 'hello-from-exec', got: %s", string(out))
	}
}

func TestIntegration_DeployAndExecWithEnv(t *testing.T) {
	cs := testCodespace(t)

	client := testSSHClient(t, cs)

	remotePath, err := deployBinary(client, cs)
	if err != nil {
		t.Fatalf("deployBinary: %v", err)
	}

	// Test that --env properly sets environment variables
	out, err := exec.Command("gh", "codespace", "ssh", "-c", cs, "--",
		remotePath, "exec", "--env", "TEST_VAR=copilot-e2e", "--", "printenv", "TEST_VAR").CombinedOutput()
	if err != nil {
		t.Fatalf("exec with env failed: %v\nOutput: %s", err, string(out))
	}

	if !strings.Contains(string(out), "copilot-e2e") {
		t.Errorf("exec should output env var value, got: %s", string(out))
	}
}

func TestIntegration_DeployAndExecWithWorkdir(t *testing.T) {
	cs := testCodespace(t)
	wd := testWorkdir(t)

	client := testSSHClient(t, cs)

	remotePath, err := deployBinary(client, cs)
	if err != nil {
		t.Fatalf("deployBinary: %v", err)
	}

	// Test that --workdir properly changes directory
	out, err := exec.Command("gh", "codespace", "ssh", "-c", cs, "--",
		remotePath, "exec", "--workdir", wd, "--", "pwd").CombinedOutput()
	if err != nil {
		t.Fatalf("exec with workdir failed: %v\nOutput: %s", err, string(out))
	}

	if !strings.Contains(string(out), wd) {
		t.Errorf("exec should output workdir %q, got: %s", wd, string(out))
	}
}

// --- Lifecycle integration tests ---

// TestIntegration_ListAvailableCodespaces verifies that list_available_codespaces
// runs gh cs list locally and returns results.
func TestIntegration_ListAvailableCodespaces(t *testing.T) {
	_ = testCodespace(t) // skip if no codespace configured

	ctx := context.Background()
	runner := &mcp.RealGHRunner{}
	out, err := runner.Run(ctx, "codespace", "list",
		"--json", "name,displayName,repository,state",
		"--limit", "50")
	if err != nil {
		t.Fatalf("gh codespace list failed: %v", err)
	}

	var codespaces []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(out), &codespaces); err != nil {
		t.Fatalf("parsing output: %v", err)
	}
	if len(codespaces) == 0 {
		t.Fatal("expected at least one codespace")
	}
	t.Logf("Found %d codespace(s)", len(codespaces))
}

// TestIntegration_ConnectCodespace verifies that connecting to an existing
// codespace sets up SSH multiplexing and registers in the registry.
func TestIntegration_ConnectCodespace(t *testing.T) {
	cs := testCodespace(t)

	ctx := context.Background()
	sshClient := ssh.NewClient(cs)
	if err := sshClient.SetupMultiplexing(ctx); err != nil {
		t.Logf("SSH multiplexing warning: %v", err)
	}

	// Verify we can run a command
	stdout, _, exitCode, err := sshClient.Exec(ctx, "echo connected-ok")
	if err != nil {
		t.Fatalf("exec failed: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("exit code %d", exitCode)
	}
	if !strings.Contains(stdout, "connected-ok") {
		t.Errorf("expected 'connected-ok', got %q", stdout)
	}

	// Register in registry
	reg := registry.New()
	if err := reg.Register(&registry.ManagedCodespace{
		Alias:    "test",
		Name:     cs,
		Executor: sshClient,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	// Verify resolution
	resolved, err := reg.Resolve("test")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.Name != cs {
		t.Errorf("resolved name %q, want %q", resolved.Name, cs)
	}
}
