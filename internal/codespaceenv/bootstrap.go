package codespaceenv

import (
	"bufio"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

const (
	// SecretsPath is the codespace-managed env file loaded by login shells.
	SecretsPath = "/workspaces/.codespaces/shared/.env-secrets"

	// GitHubAuthModeEnvVar is the process env var used to pass the session-wide
	// GitHub auth mode from the launcher to local helper processes.
	GitHubAuthModeEnvVar = "CODESPACE_GITHUB_AUTH"

	dotComGitHubAPIURL    = "https://api.github.com"
	dotComGitHubServerURL = "https://github.com"
)

// GitHubAuthMode controls which GitHub auth source should win for a session.
type GitHubAuthMode string

const (
	GitHubAuthCodespace GitHubAuthMode = "codespace"
	GitHubAuthLocal     GitHubAuthMode = "local"
)

var ErrMissingLocalGitHubToken = errors.New("local GitHub auth requires GITHUB_TOKEN or GH_TOKEN in the local environment")

// GitHubAuthEnv contains the GitHub-related env that should be forwarded for
// a session when local auth mode is enabled.
type GitHubAuthEnv struct {
	Token     string
	APIURL    string
	ServerURL string
}

// ParseGitHubAuthMode validates a user-facing GitHub auth mode string.
func ParseGitHubAuthMode(raw string) (GitHubAuthMode, error) {
	if raw == "" {
		return GitHubAuthCodespace, nil
	}

	switch mode := GitHubAuthMode(strings.TrimSpace(raw)); mode {
	case GitHubAuthCodespace, GitHubAuthLocal:
		return mode, nil
	default:
		return "", fmt.Errorf("invalid GitHub auth mode %q (want %q or %q)", raw, GitHubAuthCodespace, GitHubAuthLocal)
	}
}

// ResolveLocalGitHubAuth returns the GitHub auth env that should be forwarded
// from the local machine into the codespace when local auth mode is enabled.
func ResolveLocalGitHubAuth() (GitHubAuthEnv, error) {
	return ResolveLocalGitHubAuthFromEnv(os.LookupEnv)
}

// ResolveLocalGitHubAuthFromEnv is a test seam for ResolveLocalGitHubAuth.
func ResolveLocalGitHubAuthFromEnv(lookup func(string) (string, bool)) (GitHubAuthEnv, error) {
	token, _ := lookup("GITHUB_TOKEN")
	if token == "" {
		token, _ = lookup("GH_TOKEN")
	}
	if token == "" {
		return GitHubAuthEnv{}, ErrMissingLocalGitHubToken
	}

	apiURL, _ := lookup("GITHUB_API_URL")
	serverURL, _ := lookup("GITHUB_SERVER_URL")
	apiURL = strings.TrimSpace(apiURL)
	serverURL = strings.TrimSpace(serverURL)

	switch {
	case apiURL != "" && serverURL == "":
		serverURL = githubServerURL(apiURL)
	case apiURL == "" && serverURL != "":
		apiURL = githubAPIURL(serverURL)
	}

	return GitHubAuthEnv{
		Token:     token,
		APIURL:    apiURL,
		ServerURL: serverURL,
	}, nil
}

// EnvPairs returns the stable K=V pairs used for forwarding GitHub auth.
func (a GitHubAuthEnv) EnvPairs() []string {
	if a.Token == "" {
		return nil
	}

	pairs := []string{
		"GITHUB_TOKEN=" + a.Token,
		"GH_TOKEN=" + a.Token,
	}
	if a.APIURL != "" {
		pairs = append(pairs, "GITHUB_API_URL="+a.APIURL)
	}
	if a.ServerURL != "" {
		pairs = append(pairs, "GITHUB_SERVER_URL="+a.ServerURL)
	}
	return pairs
}

// ShellExportPrefix converts K=V pairs into `export KEY='value' && ...`.
func ShellExportPrefix(envVars []string) (string, error) {
	if len(envVars) == 0 {
		return "", nil
	}

	exports := make([]string, 0, len(envVars))
	for _, kv := range envVars {
		key, value, ok := strings.Cut(kv, "=")
		if !ok || key == "" {
			return "", fmt.Errorf("invalid env var %q (expected K=V)", kv)
		}
		exports = append(exports, fmt.Sprintf("export %s=%s", key, shellQuote(value)))
	}
	return strings.Join(exports, " && "), nil
}

// SessionEnvPairs returns the GitHub-related K=V pairs needed for the given
// session auth mode.
func SessionEnvPairs(mode GitHubAuthMode) ([]string, error) {
	if mode != GitHubAuthLocal {
		return nil, nil
	}
	authEnv, err := ResolveLocalGitHubAuth()
	if err != nil {
		return nil, err
	}
	return authEnv.EnvPairs(), nil
}

// SessionEnvExportPrefix is like SessionEnvPairs but rendered as shell exports.
func SessionEnvExportPrefix(mode GitHubAuthMode) (string, error) {
	envVars, err := SessionEnvPairs(mode)
	if err != nil {
		return "", err
	}
	return ShellExportPrefix(envVars)
}

// WrapShellCommand applies the codespace bootstrap auth env and, when local
// mode is enabled, the locally resolved GitHub auth overrides.
func WrapShellCommand(mode GitHubAuthMode, command string) (string, error) {
	wrapped := BuildShellBootstrap()
	exportPrefix, err := SessionEnvExportPrefix(mode)
	if err != nil {
		return "", err
	}
	if exportPrefix != "" {
		wrapped += " && " + exportPrefix
	}
	return wrapped + " && " + command, nil
}

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

func githubAPIURL(serverURL string) string {
	serverURL = strings.TrimRight(serverURL, "/")

	switch {
	case serverURL == "":
		return ""
	case serverURL == dotComGitHubServerURL:
		return dotComGitHubAPIURL
	default:
		return serverURL + "/api/v3"
	}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}
