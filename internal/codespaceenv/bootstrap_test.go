package codespaceenv

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildShellBootstrapFromPathRefreshesGitHubToken(t *testing.T) {
	path := writeSecretsFile(t, map[string]string{
		"GITHUB_TOKEN": "fresh-token",
		"OTHER_SECRET": "from-file",
	})

	cmd := exec.Command("sh", "-c", BuildShellBootstrapFromPath(path)+`
printf 'GITHUB_TOKEN=%s
GH_TOKEN=%s
GITHUB_SERVER_URL=%s
OTHER_SECRET=%s
' "$GITHUB_TOKEN" "$GH_TOKEN" "$GITHUB_SERVER_URL" "$OTHER_SECRET"`)
	cmd.Env = mergeEnv(os.Environ(), map[string]string{
		"GITHUB_TOKEN": "stale-token",
		"OTHER_SECRET": "preserved",
	})

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("shell bootstrap failed: %v\noutput: %s", err, out)
	}

	values := parseOutput(string(out))
	if values["GITHUB_TOKEN"] != "fresh-token" {
		t.Fatalf("GITHUB_TOKEN = %q, want fresh-token\noutput:\n%s", values["GITHUB_TOKEN"], out)
	}
	if values["GH_TOKEN"] != "fresh-token" {
		t.Fatalf("GH_TOKEN = %q, want fresh-token", values["GH_TOKEN"])
	}
	if values["GITHUB_SERVER_URL"] != "https://github.com" {
		t.Fatalf("GITHUB_SERVER_URL = %q, want https://github.com", values["GITHUB_SERVER_URL"])
	}
	if values["OTHER_SECRET"] != "preserved" {
		t.Fatalf("OTHER_SECRET = %q, want preserved", values["OTHER_SECRET"])
	}
}

func TestBuildShellBootstrapFromPathDerivesEnterpriseServerURL(t *testing.T) {
	cmd := exec.Command("sh", "-c", BuildShellBootstrapFromPath(filepath.Join(t.TempDir(), "missing"))+`
printf 'GITHUB_SERVER_URL=%s
' "$GITHUB_SERVER_URL"`)
	cmd.Env = mergeEnv(os.Environ(), map[string]string{
		"GITHUB_API_URL": "https://ghe.example.com/api/v3",
	})

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("shell bootstrap failed: %v\noutput: %s", err, out)
	}

	values := parseOutput(string(out))
	if values["GITHUB_SERVER_URL"] != "https://ghe.example.com" {
		t.Fatalf("GITHUB_SERVER_URL = %q, want https://ghe.example.com", values["GITHUB_SERVER_URL"])
	}
}

func TestApplyProcessBootstrapFromPathRefreshesGitHubToken(t *testing.T) {
	path := writeSecretsFile(t, map[string]string{
		"GITHUB_TOKEN": "fresh-token",
		"OTHER_SECRET": "from-file",
	})

	t.Setenv("GITHUB_TOKEN", "stale-token")
	t.Setenv("OTHER_SECRET", "preserved")
	t.Setenv("GITHUB_API_URL", "https://api.github.com")
	t.Setenv("GITHUB_SERVER_URL", "")
	t.Setenv("GH_TOKEN", "")

	ApplyProcessBootstrapFromPath(path)

	if got := os.Getenv("GITHUB_TOKEN"); got != "fresh-token" {
		t.Fatalf("GITHUB_TOKEN = %q, want fresh-token", got)
	}
	if got := os.Getenv("GH_TOKEN"); got != "fresh-token" {
		t.Fatalf("GH_TOKEN = %q, want fresh-token", got)
	}
	if got := os.Getenv("GITHUB_SERVER_URL"); got != "https://github.com" {
		t.Fatalf("GITHUB_SERVER_URL = %q, want https://github.com", got)
	}
	if got := os.Getenv("OTHER_SECRET"); got != "preserved" {
		t.Fatalf("OTHER_SECRET = %q, want preserved", got)
	}
}

func TestApplyProcessBootstrapFromPathBackfillsGitHubTokenFromGHToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "gh-only-token")
	t.Setenv("GITHUB_SERVER_URL", "")

	ApplyProcessBootstrapFromPath(filepath.Join(t.TempDir(), "missing"))

	if got := os.Getenv("GITHUB_TOKEN"); got != "gh-only-token" {
		t.Fatalf("GITHUB_TOKEN = %q, want gh-only-token", got)
	}
}

func writeSecretsFile(t *testing.T, entries map[string]string) string {
	t.Helper()

	var lines []string
	for key, value := range entries {
		lines = append(lines, key+"="+base64.StdEncoding.EncodeToString([]byte(value)))
	}

	path := filepath.Join(t.TempDir(), ".env-secrets")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatalf("write secrets file: %v", err)
	}
	return path
}

func parseOutput(output string) map[string]string {
	values := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			values[key] = value
		}
	}
	return values
}

func mergeEnv(base []string, updates map[string]string) []string {
	result := make(map[string]string, len(base)+len(updates))
	for _, kv := range base {
		key, value, ok := strings.Cut(kv, "=")
		if ok {
			result[key] = value
		}
	}
	for key, value := range updates {
		result[key] = value
	}

	merged := make([]string, 0, len(result))
	for key, value := range result {
		merged = append(merged, key+"="+value)
	}
	return merged
}
