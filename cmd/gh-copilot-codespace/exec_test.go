package main

import (
	"errors"
	"os"
	"strings"
	"testing"
)

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
