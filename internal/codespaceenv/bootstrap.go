package codespaceenv

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"strings"
)

const (
	// SecretsPath is the codespace-managed env file loaded by login shells.
	SecretsPath = "/workspaces/.codespaces/shared/.env-secrets"

	dotComGitHubAPIURL    = "https://api.github.com"
	dotComGitHubServerURL = "https://github.com"
)

// BuildShellBootstrap returns a shell snippet that restores the codespace's
// GitHub auth env for non-login commands.
func BuildShellBootstrap() string {
	return BuildShellBootstrapFromPath(SecretsPath)
}

// BuildShellBootstrapFromPath is a test seam for BuildShellBootstrap.
func BuildShellBootstrapFromPath(path string) string {
	quotedPath := shellQuote(path)
	quotedDefaultServerURL := shellQuote(dotComGitHubServerURL)
	quotedDotComAPIURL := shellQuote(dotComGitHubAPIURL)

	return fmt.Sprintf(`if [ -f %[1]s ]; then
while IFS= read -r line; do
key="$(printf '%%s' "$line" | sed 's/=.*//')"
value="$(printf '%%s' "$line" | sed 's/^[^=]*=//')"
if [ -n "$key" ]; then
decoded="$(printf '%%s' "$value" | base64 --decode 2>/dev/null)"
if [ $? -ne 0 ]; then
decoded="$(printf '%%s' "$value" | base64 -D 2>/dev/null)" || continue
fi
case "$key" in
GITHUB_TOKEN|GH_TOKEN) export "$key=$decoded" 2>/dev/null ;;
*) printenv "$key" >/dev/null 2>&1 || export "$key=$decoded" 2>/dev/null ;;
esac
fi
done < %[1]s
true
fi
if [ -z "${GITHUB_SERVER_URL:-}" ]; then
api_url="${GITHUB_API_URL:-}"
api_url="${api_url%%/}"
if [ -z "$api_url" ]; then
export GITHUB_SERVER_URL=%[2]s
elif [ "$api_url" = %[3]s ]; then
export GITHUB_SERVER_URL=%[2]s
elif [ "${api_url%%/api/v3}" != "$api_url" ]; then
export GITHUB_SERVER_URL="${api_url%%/api/v3}"
else
export GITHUB_SERVER_URL="$api_url"
fi
fi
if [ -n "${GITHUB_TOKEN:-}" ]; then
export GH_TOKEN="$GITHUB_TOKEN"
elif [ -n "${GH_TOKEN:-}" ]; then
export GITHUB_TOKEN="$GH_TOKEN"
fi`, quotedPath, quotedDefaultServerURL, quotedDotComAPIURL)
}

// ApplyProcessBootstrap refreshes the current process environment from the
// codespace secrets file and fills in the GitHub auth env expected by git and gh.
func ApplyProcessBootstrap() {
	ApplyProcessBootstrapFromPath(SecretsPath)
}

// ApplyProcessBootstrapFromPath is a test seam for ApplyProcessBootstrap.
func ApplyProcessBootstrapFromPath(path string) {
	for key, value := range loadSecrets(path) {
		if shouldOverwriteExisting(key) {
			_ = os.Setenv(key, value)
			continue
		}
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		_ = os.Setenv(key, value)
	}

	switch {
	case os.Getenv("GITHUB_TOKEN") != "":
		_ = os.Setenv("GH_TOKEN", os.Getenv("GITHUB_TOKEN"))
	case os.Getenv("GH_TOKEN") != "":
		_ = os.Setenv("GITHUB_TOKEN", os.Getenv("GH_TOKEN"))
	}

	if os.Getenv("GITHUB_SERVER_URL") == "" {
		_ = os.Setenv("GITHUB_SERVER_URL", githubServerURL(os.Getenv("GITHUB_API_URL")))
	}
}

func loadSecrets(path string) map[string]string {
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()

	return parseSecrets(file)
}

func parseSecrets(r io.Reader) map[string]string {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	secrets := make(map[string]string)
	for scanner.Scan() {
		line := scanner.Text()
		key, encoded, ok := strings.Cut(line, "=")
		if !ok || key == "" {
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			continue
		}
		secrets[key] = string(decoded)
	}
	return secrets
}

func shouldOverwriteExisting(key string) bool {
	switch key {
	case "GITHUB_TOKEN", "GH_TOKEN":
		return true
	default:
		return false
	}
}

func githubServerURL(apiURL string) string {
	apiURL = strings.TrimRight(apiURL, "/")

	switch {
	case apiURL == "":
		return dotComGitHubServerURL
	case apiURL == dotComGitHubAPIURL:
		return dotComGitHubServerURL
	case strings.HasSuffix(apiURL, "/api/v3"):
		return strings.TrimSuffix(apiURL, "/api/v3")
	default:
		return apiURL
	}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}
