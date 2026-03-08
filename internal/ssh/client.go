package ssh

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Client manages SSH connections to a GitHub Codespace via gh CLI.
type Client struct {
	codespaceName  string
	mu             sync.Mutex
	sshConfigPath  string // path to generated SSH config with ControlMaster
	sshHost        string // SSH host alias (e.g., "cs.develop-xxx")
	controlSocket  string // path to control socket
	workdir        string // current working directory on the codespace
	commandContext func(ctx context.Context, name string, args ...string) *exec.Cmd
}

// Executor defines the operations that MCP handlers use to interact with a codespace.
type Executor interface {
	ViewFile(ctx context.Context, path string, viewRange []int) (string, error)
	EditFile(ctx context.Context, path, oldStr, newStr string) error
	CreateFile(ctx context.Context, path, content string) error
	RunBash(ctx context.Context, command, cwd string) (stdout, stderr string, exitCode int, err error)
	Grep(ctx context.Context, pattern, path, glob, cwd string) (string, error)
	Glob(ctx context.Context, pattern, path, cwd string) (string, error)
	StartSession(ctx context.Context, sessionID, command, cwd string) error
	WriteSession(ctx context.Context, sessionID, input string) error
	ReadSession(ctx context.Context, sessionID string) (string, error)
	StopSession(ctx context.Context, sessionID string) error
	ListSessions(ctx context.Context) (string, error)
	SetWorkdir(dir string)
	GetWorkdir() string
}

// NewClient creates a new SSH client for the given codespace.
func NewClient(codespaceName string) *Client {
	return &Client{
		codespaceName:  codespaceName,
		commandContext: exec.CommandContext,
	}
}

func (c *Client) command(ctx context.Context, name string, args ...string) *exec.Cmd {
	if c.commandContext != nil {
		return c.commandContext(ctx, name, args...)
	}
	return exec.CommandContext(ctx, name, args...)
}

// SetWorkdir sets the default working directory used when a command/search
// call does not provide an explicit cwd override.
func (c *Client) SetWorkdir(dir string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.workdir = dir
}

// GetWorkdir returns the default working directory. Falls back to
// CODESPACE_WORKDIR env var, then "/workspaces".
func (c *Client) GetWorkdir() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.workdir != "" {
		return c.workdir
	}
	if wd := os.Getenv("CODESPACE_WORKDIR"); wd != "" {
		return wd
	}
	return "/workspaces"
}

func (c *Client) sshState() (sshConfigPath, sshHost, controlSocket string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sshConfigPath, c.sshHost, c.controlSocket
}

func (c *Client) setSSHState(sshConfigPath, sshHost, controlSocket string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sshConfigPath = sshConfigPath
	c.sshHost = sshHost
	c.controlSocket = controlSocket
}

func (c *Client) setSSHConfigPath(sshConfigPath string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sshConfigPath = sshConfigPath
}

// SetupMultiplexing generates an SSH config with ControlMaster and establishes
// a persistent connection. Subsequent Exec calls use this connection (~0.1s vs ~3s).
func (c *Client) SetupMultiplexing(ctx context.Context) error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("getting home dir: %w", err)
	}

	configDir := filepath.Join(homeDir, ".copilot", "codespace-workdirs")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return fmt.Errorf("creating config dir: %w", err)
	}

	controlSocket := filepath.Join(configDir, ".ssh-"+c.codespaceName)
	sshConfigPath := filepath.Join(configDir, ".ssh-config-"+c.codespaceName)

	// Reuse existing multiplexed connection if alive (e.g., set up by the launcher).
	// Avoids calling gh codespace ssh --config which creates a new tunnel and may
	// invalidate the existing ControlMaster's connection and its socket forwardings.
	var sshHost string
	if data, err := os.ReadFile(sshConfigPath); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "Host ") {
				sshHost = strings.TrimPrefix(strings.TrimSpace(line), "Host ")
				break
			}
		}
		if sshHost != "" {
			check := c.command(ctx, "ssh", "-F", sshConfigPath, "-O", "check", sshHost)
			if check.Run() == nil {
				// Smoke-test the tunnel: ssh -O check only verifies the master
				// process is alive, not that the underlying relay still works.
				probe := c.command(ctx, "ssh", "-F", sshConfigPath, "-o", "ConnectTimeout=5", sshHost, "echo ok")
				if out, err := probe.Output(); err == nil && strings.TrimSpace(string(out)) == "ok" {
					c.setSSHState(sshConfigPath, sshHost, controlSocket)
					fmt.Fprintf(os.Stderr, "codespace-mcp: reusing existing SSH multiplexing\n")
					return nil
				}
				fmt.Fprintf(os.Stderr, "codespace-mcp: existing SSH master alive but tunnel broken, reconnecting\n")
			}
		}
	}

	// Get SSH config from gh (contains ProxyCommand, identity file, etc.)
	ghConfig, err := c.command(ctx, "gh", "codespace", "ssh",
		"--config", "-c", c.codespaceName).Output()
	if err != nil {
		return fmt.Errorf("getting SSH config: %w", err)
	}

	config := string(ghConfig)

	// Parse the Host line from gh config (e.g., "Host cs.develop-xxx.main")
	for _, line := range strings.Split(config, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Host ") {
			sshHost = strings.TrimPrefix(line, "Host ")
			break
		}
	}
	if sshHost == "" {
		return fmt.Errorf("could not parse Host from SSH config")
	}

	// Add ControlPath + ControlPersist if not present
	if !strings.Contains(config, "ControlPath") {
		config += fmt.Sprintf("\tControlPath %s\n", controlSocket)
	}
	if !strings.Contains(config, "ControlPersist") {
		config += "\tControlPersist 600\n"
	}

	if err := os.WriteFile(sshConfigPath, []byte(config), 0o600); err != nil {
		return fmt.Errorf("writing SSH config: %w", err)
	}

	// Establish master connection in background (retry once on failure)
	for attempt := 0; attempt < 2; attempt++ {
		cmd := c.command(ctx, "ssh",
			"-F", sshConfigPath,
			"-o", "ControlMaster=yes",
			"-o", "ControlPersist=600",
			"-fN", // background, no command
			sshHost,
		)
		var sshErr bytes.Buffer
		cmd.Stderr = &sshErr
		if err := cmd.Run(); err != nil {
			errDetail := strings.TrimSpace(sshErr.String())
			if attempt == 0 {
				fmt.Fprintf(os.Stderr, "codespace-mcp: SSH multiplexing attempt 1 failed (%v), retrying...\n", errDetail)
				time.Sleep(3 * time.Second)
				continue
			}
			// Final attempt failed — fall back to non-multiplexed mode
			fmt.Fprintf(os.Stderr, "codespace-mcp: SSH multiplexing failed, using fallback: %v (%s)\n", err, errDetail)
			c.setSSHState("", sshHost, controlSocket)
			return nil
		}
		break
	}

	c.setSSHState(sshConfigPath, sshHost, controlSocket)
	fmt.Fprintf(os.Stderr, "codespace-mcp: SSH multiplexing established\n")
	return nil
}

// ControlSocketPath returns the path to the SSH control socket, if multiplexing is active.
func (c *Client) ControlSocketPath() string {
	sshConfigPath, _, controlSocket := c.sshState()
	if sshConfigPath == "" {
		return ""
	}
	return controlSocket
}

// SSHHost returns the SSH host alias for this codespace.
func (c *Client) SSHHost() string {
	_, sshHost, _ := c.sshState()
	return sshHost
}

// SSHConfigPath returns the path to the generated SSH config file.
func (c *Client) SSHConfigPath() string {
	sshConfigPath, _, _ := c.sshState()
	return sshConfigPath
}

var sshTransportErrorMarkers = []string{
	"broken pipe",
	"connection closed",
	"connection reset",
	"control socket",
	"kex_exchange_identification",
	"mux_client_",
	"ssh_exchange_identification",
	"stdio forwarding failed",
}

func (c *Client) runRemoteCommand(ctx context.Context, wrapped string, useMultiplex bool) (stdout string, stderr string, exitCode int, err error) {
	var cmd *exec.Cmd
	if useMultiplex {
		sshConfigPath, sshHost, _ := c.sshState()
		cmd = c.command(ctx, "ssh", "-F", sshConfigPath, sshHost, wrapped)
	} else {
		cmd = c.command(ctx, "gh", "codespace", "ssh",
			"-c", c.codespaceName,
			"--", wrapped,
		)
	}

	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	runErr := cmd.Run()
	stdout = outBuf.String()
	stderr = errBuf.String()

	if runErr != nil {
		if ctx.Err() != nil {
			return stdout, stderr, -1, fmt.Errorf("command cancelled: %w", ctx.Err())
		}
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return stdout, stderr, -1, fmt.Errorf("failed to execute command: %w", runErr)
		}
	}

	return stdout, stderr, exitCode, nil
}

func (c *Client) disableMultiplexing() {
	_, _, controlSocket := c.sshState()
	if controlSocket != "" {
		if err := os.Remove(controlSocket); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "codespace-mcp: failed to remove SSH control socket %s: %v\n", controlSocket, err)
		}
	}
	c.setSSHConfigPath("")
}

func isRetryableTransportFailure(useMultiplex bool, exitCode int, stderr string) bool {
	if !useMultiplex || exitCode != 255 {
		return false
	}
	trimmed := strings.TrimSpace(stderr)
	lower := strings.ToLower(trimmed)
	for _, marker := range sshTransportErrorMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func logDiagnostic(label, text string) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return
	}
	fmt.Fprintf(os.Stderr, "codespace-mcp: %s: %s\n", label, trimmed)
}

func (c *Client) probeMultiplexing(ctx context.Context) bool {
	sshConfigPath, sshHost, _ := c.sshState()
	if sshConfigPath == "" || sshHost == "" {
		return false
	}

	probe := c.command(ctx, "ssh", "-F", sshConfigPath, "-o", "ConnectTimeout=5", sshHost, "echo ok")
	out, err := probe.Output()
	return err == nil && strings.TrimSpace(string(out)) == "ok"
}

// Exec runs a command on the codespace and returns stdout, stderr, and exit code.
func (c *Client) Exec(ctx context.Context, command string) (stdout string, stderr string, exitCode int, err error) {
	// Ensure codespace-injected secrets are available for git auth etc.
	wrapped := envSecretsLoader + " && " + command
	sshConfigPath, _, _ := c.sshState()
	return c.runRemoteCommand(ctx, wrapped, sshConfigPath != "")
}

func (c *Client) execReadOnly(ctx context.Context, command string) (stdout string, stderr string, exitCode int, err error) {
	wrapped := envSecretsLoader + " && " + command
	sshConfigPath, _, _ := c.sshState()
	useMultiplex := sshConfigPath != ""
	stdout, stderr, exitCode, err = c.runRemoteCommand(ctx, wrapped, useMultiplex)
	if err != nil {
		return stdout, stderr, exitCode, err
	}
	trimmedStderr := strings.TrimSpace(stderr)
	if !isRetryableTransportFailure(useMultiplex, exitCode, stderr) && !(useMultiplex && exitCode == 255 && trimmedStderr == "" && !c.probeMultiplexing(ctx)) {
		return stdout, stderr, exitCode, nil
	}

	fmt.Fprintln(os.Stderr, "codespace-mcp: SSH transport failed for read-only command, retrying without multiplexing")
	c.disableMultiplexing()
	return c.runRemoteCommand(ctx, wrapped, false)
}

func (c *Client) resolveWorkdir(cwd string) string {
	if cwd != "" {
		return cwd
	}
	return c.GetWorkdir()
}

func wrapCommandInWorkdir(command, cwd string) string {
	return fmt.Sprintf("cd %s && %s", shellQuote(cwd), command)
}

// ViewFile reads a file with line numbers. If viewRange is provided [start, end], only those lines are shown.
func (c *Client) ViewFile(ctx context.Context, path string, viewRange []int) (string, error) {
	var cmd string
	if len(viewRange) == 2 {
		if viewRange[1] == -1 {
			cmd = fmt.Sprintf("awk 'NR>=%d {print NR\". \"$0}' %s",
				viewRange[0], shellQuote(path))
		} else {
			cmd = fmt.Sprintf("awk 'NR>=%d && NR<=%d {print NR\". \"$0}' %s",
				viewRange[0], viewRange[1], shellQuote(path))
		}
	} else {
		cmd = fmt.Sprintf("awk '{print NR\". \"$0}' %s", shellQuote(path))
	}

	stdout, stderr, exitCode, err := c.execReadOnly(ctx, cmd)
	if err != nil {
		return "", fmt.Errorf("view file: %w", err)
	}
	if exitCode != 0 {
		return "", formatCommandFailure("view file", exitCode, stderr)
	}
	return stdout, nil
}

// EditFile replaces exactly one occurrence of oldStr with newStr in the file.
func (c *Client) EditFile(ctx context.Context, path, oldStr, newStr string) error {
	// Read file content via SSH
	stdout, stderr, exitCode, err := c.Exec(ctx, fmt.Sprintf("base64 < %s", shellQuote(path)))
	if err != nil {
		return fmt.Errorf("edit file (read): %w", err)
	}
	if exitCode != 0 {
		return fmt.Errorf("edit file (read) failed (exit %d): %s", exitCode, strings.TrimSpace(stderr))
	}

	content, err := base64.StdEncoding.DecodeString(strings.TrimSpace(stdout))
	if err != nil {
		return fmt.Errorf("edit file (decode): %w", err)
	}

	// Do the replacement in Go
	contentStr := string(content)
	count := strings.Count(contentStr, oldStr)
	if count == 0 {
		return fmt.Errorf("old_str not found in file")
	}
	if count > 1 {
		return fmt.Errorf("old_str found %d times, must be unique", count)
	}

	newContent := strings.Replace(contentStr, oldStr, newStr, 1)

	// Write back via SSH
	b64 := base64.StdEncoding.EncodeToString([]byte(newContent))
	cmd := fmt.Sprintf("echo %s | base64 -d > %s", shellQuote(b64), shellQuote(path))
	_, stderr, exitCode, err = c.Exec(ctx, cmd)
	if err != nil {
		return fmt.Errorf("edit file (write): %w", err)
	}
	if exitCode != 0 {
		return fmt.Errorf("edit file (write) failed (exit %d): %s", exitCode, strings.TrimSpace(stderr))
	}
	return nil
}

// CreateFile creates a new file with the given content, creating parent directories as needed.
func (c *Client) CreateFile(ctx context.Context, path, content string) error {
	b64 := base64.StdEncoding.EncodeToString([]byte(content))
	dir := pathDir(path)

	cmd := fmt.Sprintf("mkdir -p %s && echo %s | base64 -d > %s",
		shellQuote(dir), shellQuote(b64), shellQuote(path))

	_, stderr, exitCode, err := c.Exec(ctx, cmd)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}
	if exitCode != 0 {
		return fmt.Errorf("create file failed (exit %d): %s", exitCode, stderr)
	}
	return nil
}

// RunBash executes a bash command on the codespace.
func (c *Client) RunBash(ctx context.Context, command, cwd string) (stdout string, stderr string, exitCode int, err error) {
	return c.Exec(ctx, wrapCommandInWorkdir(command, c.resolveWorkdir(cwd)))
}

// Grep searches for a pattern in files on the codespace.
func (c *Client) Grep(ctx context.Context, pattern, path, globPattern, cwd string) (string, error) {
	var args []string
	args = append(args, "rg", "--color=never", "-n")

	if globPattern != "" {
		args = append(args, "--glob", shellQuote(globPattern))
	}

	args = append(args, shellQuote(pattern))

	searchPath := path
	if searchPath == "" {
		searchPath = "."
	}
	args = append(args, shellQuote(searchPath))

	cmd := strings.Join(args, " ")

	// Fallback to grep if rg is not available
	cmd = fmt.Sprintf("(%s) 2>/dev/null || grep -rn %s %s",
		cmd, shellQuote(pattern), shellQuote(searchPath))

	stdout, _, exitCode, err := c.execReadOnly(ctx, wrapCommandInWorkdir(cmd, c.resolveWorkdir(cwd)))
	if err != nil {
		return "", fmt.Errorf("grep: %w", err)
	}
	// Exit code 1 means no matches (normal for grep/rg)
	if exitCode > 1 {
		return "", fmt.Errorf("grep failed with exit code %d", exitCode)
	}
	return stdout, nil
}

// Glob finds files matching a glob pattern on the codespace.
// Supports standard glob patterns like **/*.go, *.ts, src/**/*.test.js.
func (c *Client) Glob(ctx context.Context, pattern, path, cwd string) (string, error) {
	searchPath := path
	if searchPath == "" {
		searchPath = "."
	}

	// Use fd if available (supports glob natively), fallback to find with -name
	// Extract the filename pattern from globs like **/*.go → *.go for find -name
	cmd := fmt.Sprintf(
		"(fd --type f --glob %s --exclude .git %s 2>/dev/null || find %s -name %s -not -path '*/.git/*' 2>/dev/null) | head -200",
		shellQuote(pattern), shellQuote(searchPath), shellQuote(searchPath), shellQuote(globToFindName(pattern)))

	stdout, _, exitCode, err := c.execReadOnly(ctx, wrapCommandInWorkdir(cmd, c.resolveWorkdir(cwd)))
	if err != nil {
		return "", fmt.Errorf("glob: %w", err)
	}
	if exitCode > 1 {
		return "", fmt.Errorf("glob failed with exit code %d", exitCode)
	}
	return stdout, nil
}

// globToFindName extracts a filename pattern from a glob for use with find -name.
// e.g., "**/*.go" → "*.go", "src/**/*.test.js" → "*.test.js", "*.ts" → "*.ts"
func globToFindName(pattern string) string {
	// Take the last path component
	parts := strings.Split(pattern, "/")
	return parts[len(parts)-1]
}

// envSecretsLoader sources codespace-injected secrets (GITHUB_TOKEN, etc.)
// that are normally loaded by the login shell profile. Non-login SSH commands
// skip /etc/profile.d/ scripts, so we load the secrets file directly.
// Guards: skip empty keys, preserve already-exported variables, and suppress
// export errors (2>/dev/null) so that a single malformed entry cannot break the
// && chain and silently drop the user's command.
const envSecretsLoader = `if [ -f /workspaces/.codespaces/shared/.env-secrets ]; then while IFS='=' read -r key value; do if [ -n "$key" ]; then printenv "$key" >/dev/null 2>&1 || export "$key=$(echo "$value" | base64 -d)" 2>/dev/null; fi; done < /workspaces/.codespaces/shared/.env-secrets; true; fi`

const tmuxPrefix = "copilot-"

// misePATH is prepended to PATH for commands that need mise-installed tools.
const misePATH = `PATH="$HOME/.local/bin:$HOME/.local/share/mise/shims:$PATH"`

// tmuxSessionName returns the prefixed tmux session name.
func tmuxSessionName(sessionID string) string {
	return tmuxPrefix + sessionID
}

// execTmux runs a tmux command with mise shims on the PATH.
func (c *Client) execTmux(ctx context.Context, tmuxCmd string) (string, string, int, error) {
	return c.Exec(ctx, misePATH+" && "+tmuxCmd)
}

// StartSession creates a named tmux session running the given command on the codespace.
// Uses remain-on-exit so the pane stays readable even after the command exits.
func (c *Client) StartSession(ctx context.Context, sessionID, command, cwd string) error {
	name := tmuxSessionName(sessionID)

	if err := c.ensureTmux(ctx); err != nil {
		return err
	}

	wrappedCommand := wrapCommandInWorkdir(command, c.resolveWorkdir(cwd))

	// Create session with remain-on-exit so we can read output after command finishes
	cmd := fmt.Sprintf(
		"tmux new-session -d -s %s -x 200 -y 50 %s && tmux set-option -t %s remain-on-exit on",
		shellQuote(name), shellQuote(wrappedCommand), shellQuote(name))

	_, stderr, exitCode, err := c.execTmux(ctx, cmd)
	if err != nil {
		return fmt.Errorf("start session: %w", err)
	}
	if exitCode != 0 {
		return formatCommandFailure("start session", exitCode, stderr)
	}
	return nil
}

// ensureTmux checks if tmux is available on the codespace and installs it via mise if not.
func (c *Client) ensureTmux(ctx context.Context) error {
	if _, _, ec, _ := c.execTmux(ctx, "command -v tmux"); ec == 0 {
		return nil
	}

	fmt.Fprintln(os.Stderr, "codespace-mcp: tmux not found, installing via mise...")

	// Install mise if not available, then install tmux
	installScript := misePATH + ` && (command -v mise >/dev/null 2>&1 || curl -fsSL https://mise.jdx.dev/install.sh | sh) && mise use -g tmux`
	_, stderr, exitCode, err := c.Exec(ctx, installScript)
	if err != nil {
		return fmt.Errorf("installing tmux: %w", err)
	}
	if exitCode != 0 {
		logDiagnostic("tmux install via mise failed", stderr)
		return fmt.Errorf("failed to install tmux via mise (exit %d); verify that the codespace can run `mise use -g tmux`", exitCode)
	}

	// Verify tmux is now available and distinguish PATH problems from missing shims.
	verifyCmd := `command -v tmux >/dev/null 2>&1 || { if [ -x "$HOME/.local/share/mise/shims/tmux" ]; then echo 'tmux shim exists but is not on PATH' >&2; else echo 'tmux shim not found after install' >&2; fi; exit 1; }`
	_, verifyStderr, ec, err := c.execTmux(ctx, verifyCmd)
	if err != nil {
		return fmt.Errorf("verifying tmux installation: %w", err)
	}
	if ec != 0 {
		logDiagnostic("tmux verification after mise install failed", verifyStderr)
		return fmt.Errorf("tmux installation completed but tmux is still unavailable; %s", summarizeTmuxVerificationFailure(verifyStderr))
	}
	return nil
}

// specialKeys maps brace-delimited key names to tmux key names.
var specialKeys = map[string]string{
	"{enter}":     "Enter",
	"{up}":        "Up",
	"{down}":      "Down",
	"{left}":      "Left",
	"{right}":     "Right",
	"{backspace}": "BSpace",
}

// parseInput splits an input string into segments of literal text and special keys.
// Each segment is either a literal string or a tmux key name (prefixed with \x00 to distinguish).
func parseInput(input string) []string {
	var segments []string
	for len(input) > 0 {
		// Find the earliest special key match
		bestIdx := -1
		bestKey := ""
		bestTmux := ""
		for pattern, tmuxKey := range specialKeys {
			idx := strings.Index(input, pattern)
			if idx >= 0 && (bestIdx < 0 || idx < bestIdx) {
				bestIdx = idx
				bestKey = pattern
				bestTmux = tmuxKey
			}
		}
		if bestIdx < 0 {
			// No more special keys; rest is literal
			segments = append(segments, input)
			break
		}
		if bestIdx > 0 {
			segments = append(segments, input[:bestIdx])
		}
		// Mark special keys with a \x00 prefix
		segments = append(segments, "\x00"+bestTmux)
		input = input[bestIdx+len(bestKey):]
	}
	return segments
}

// WriteSession sends keystrokes to a tmux session on the codespace.
// Special key sequences like {enter}, {up}, {down}, {left}, {right}, {backspace}
// are translated to their tmux equivalents.
func (c *Client) WriteSession(ctx context.Context, sessionID, input string) error {
	name := tmuxSessionName(sessionID)
	segments := parseInput(input)

	for _, seg := range segments {
		var cmd string
		if strings.HasPrefix(seg, "\x00") {
			tmuxKey := seg[1:]
			cmd = fmt.Sprintf("tmux send-keys -t %s %s", shellQuote(name), tmuxKey)
		} else {
			cmd = fmt.Sprintf("tmux send-keys -t %s %s", shellQuote(name), shellQuote(seg))
		}

		_, stderr, exitCode, err := c.execTmux(ctx, cmd)
		if err != nil {
			return fmt.Errorf("write session: %w", err)
		}
		if exitCode != 0 {
			return formatCommandFailure("write session", exitCode, stderr)
		}
	}
	return nil
}

// paneDeadRe matches the tmux "Pane is dead" decoration that appears when remain-on-exit is on.
var paneDeadRe = regexp.MustCompile(`(?m)^Pane is dead.*$`)

// cleanPaneOutput strips tmux decorations (like "Pane is dead") and trailing blank lines
// from captured pane output so only actual command output is returned.
func cleanPaneOutput(s string) string {
	s = paneDeadRe.ReplaceAllString(s, "")
	// Trim trailing blank lines
	s = strings.TrimRight(s, "\n ")
	return s
}

// ReadSession captures the current tmux pane content (last 100 lines) from the codespace.
// Works even after the command has exited (thanks to remain-on-exit).
func (c *Client) ReadSession(ctx context.Context, sessionID string) (string, error) {
	name := tmuxSessionName(sessionID)

	// Check if session exists
	checkCmd := fmt.Sprintf("tmux has-session -t %s 2>/dev/null", shellQuote(name))
	if _, _, ec, _ := c.execTmux(ctx, checkCmd); ec != 0 {
		return "", fmt.Errorf("session %q does not exist (command may have exited and been cleaned up)", sessionID)
	}

	cmd := fmt.Sprintf("tmux capture-pane -t %s -p -S -100", shellQuote(name))
	stdout, stderr, exitCode, err := c.execTmux(ctx, cmd)
	if err != nil {
		return "", fmt.Errorf("read session: %w", err)
	}
	if exitCode != 0 {
		return "", formatCommandFailure("read session", exitCode, stderr)
	}

	stdout = cleanPaneOutput(stdout)

	// Check if the pane is dead (command exited)
	statusCmd := fmt.Sprintf("tmux list-panes -t %s -F '#{pane_dead} #{pane_dead_status}' 2>/dev/null", shellQuote(name))
	statusOut, _, _, _ := c.execTmux(ctx, statusCmd)
	paneDead, exitCode, err := parsePaneStatus(statusOut)
	if err == nil && paneDead {
		if stdout != "" {
			stdout += "\n"
		}
		stdout += "[session exited]"
		if exitCode != 0 {
			stdout += fmt.Sprintf("\n[exit code: %d]", exitCode)
		}
	}

	return stdout, nil
}

func parsePaneStatus(status string) (paneDead bool, exitCode int, err error) {
	fields := strings.Fields(strings.TrimSpace(status))
	if len(fields) < 2 {
		return false, 0, errors.New("invalid pane status")
	}

	paneDead = fields[0] == "1"
	exitCode, err = strconv.Atoi(fields[1])
	if err != nil {
		return false, 0, fmt.Errorf("parse pane exit code: %w", err)
	}
	return paneDead, exitCode, nil
}

// StopSession kills a tmux session on the codespace.
func (c *Client) StopSession(ctx context.Context, sessionID string) error {
	name := tmuxSessionName(sessionID)
	cmd := fmt.Sprintf("tmux kill-session -t %s", shellQuote(name))

	_, stderr, exitCode, err := c.execTmux(ctx, cmd)
	if err != nil {
		return fmt.Errorf("stop session: %w", err)
	}
	if exitCode != 0 {
		return formatCommandFailure("stop session", exitCode, stderr)
	}
	return nil
}

// ListSessions lists active copilot-prefixed tmux sessions on the codespace.
func (c *Client) ListSessions(ctx context.Context) (string, error) {
	cmd := "tmux list-sessions -F '#{session_name} #{session_created} #{session_activity}' 2>/dev/null | grep '^" + tmuxPrefix + "'"

	stdout, _, exitCode, err := c.execTmux(ctx, cmd)
	if err != nil {
		return "", fmt.Errorf("list sessions: %w", err)
	}
	// Exit code 1 means no matching sessions (grep found nothing)
	if exitCode > 1 {
		return "", fmt.Errorf("list sessions failed with exit code %d", exitCode)
	}
	return stdout, nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

func formatCommandFailure(action string, exitCode int, stderr string) error {
	trimmed := strings.TrimSpace(stderr)
	if trimmed == "" {
		return fmt.Errorf("%s failed (exit %d)", action, exitCode)
	}
	return fmt.Errorf("%s failed (exit %d): %s", action, exitCode, trimmed)
}

func summarizeTmuxVerificationFailure(stderr string) string {
	lower := strings.ToLower(stderr)
	switch {
	case strings.Contains(lower, "shim exists but is not on path"):
		return "the tmux shim exists, but `$HOME/.local/share/mise/shims` is not on PATH"
	case strings.Contains(lower, "shim not found after install"):
		return "mise did not create a tmux shim in `$HOME/.local/share/mise/shims`"
	default:
		return "verify that `mise use -g tmux` succeeds and that tmux is on PATH in the codespace"
	}
}

// ForwardSocket sets up Unix socket forwarding from a local path to a remote path
// using the existing SSH ControlMaster connection. The forwarding persists as long
// as the master connection is alive. Returns an error if multiplexing is not active.
func (c *Client) ForwardSocket(ctx context.Context, localPath, remotePath string) error {
	sshConfigPath, sshHost, _ := c.sshState()
	if sshConfigPath == "" {
		return fmt.Errorf("SSH multiplexing not active, cannot forward socket")
	}

	fwdSpec := localPath + ":" + remotePath

	// Cancel any existing forwarding for the same spec. Without this,
	// ssh -O forward silently succeeds when the ControlMaster already has the
	// forwarding registered, but does NOT recreate a deleted socket file.
	c.CancelForward(ctx, localPath, remotePath)

	// Remove stale local socket if it exists
	if err := os.Remove(localPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing stale socket %s: %w", localPath, err)
	}

	// StreamLocalBindUnlink=yes tells SSH to remove the socket atomically before
	// binding, avoiding a TOCTOU race between our Remove and the bind.
	cmd := c.command(ctx, "ssh",
		"-F", sshConfigPath,
		"-o", "StreamLocalBindUnlink=yes",
		"-O", "forward",
		"-L", fwdSpec,
		sshHost,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ssh forward: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

// CancelForward cancels an active socket forwarding.
func (c *Client) CancelForward(ctx context.Context, localPath, remotePath string) {
	sshConfigPath, sshHost, _ := c.sshState()
	if sshConfigPath == "" {
		return
	}
	fwdSpec := localPath + ":" + remotePath
	cancel := c.command(ctx, "ssh",
		"-F", sshConfigPath,
		"-O", "cancel",
		"-L", fwdSpec,
		sshHost,
	)
	cancel.Run() // ignore error — forwarding may not exist
}

func pathDir(path string) string {
	i := strings.LastIndex(path, "/")
	if i < 0 {
		return "."
	}
	return path[:i]
}
