package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/ekroon/gh-copilot-codespace/internal/codespaceenv"
)

var (
	proxyExecProcess = syscall.Exec
	proxyLookPath    = exec.LookPath
)

type proxyOptions struct {
	codespaceName string
	githubAuth    string
	workdir       string
	remoteBinary  string
	envVars       []string
	cmdArgs       []string
	shell         bool
}

// runProxy forwards a local command invocation to a remote codespace command.
// It is used internally for rewritten MCP servers, hooks, and interactive shells.
func runProxy(args []string) error {
	opts, err := parseProxyArgs(args)
	if err != nil {
		return err
	}

	path, execArgs, err := buildProxyExec(opts)
	if err != nil {
		return err
	}

	return proxyExecProcess(path, execArgs, os.Environ())
}

func parseProxyArgs(args []string) (proxyOptions, error) {
	var opts proxyOptions

	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--codespace" && i+1 < len(args):
			opts.codespaceName = args[i+1]
			i++
		case args[i] == "--github-auth" && i+1 < len(args):
			opts.githubAuth = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--github-auth="):
			opts.githubAuth = strings.TrimPrefix(args[i], "--github-auth=")
		case args[i] == "--workdir" && i+1 < len(args):
			opts.workdir = args[i+1]
			i++
		case args[i] == "--remote-binary" && i+1 < len(args):
			opts.remoteBinary = args[i+1]
			i++
		case args[i] == "--env" && i+1 < len(args):
			opts.envVars = append(opts.envVars, args[i+1])
			i++
		case args[i] == "--shell":
			opts.shell = true
		case args[i] == "--":
			opts.cmdArgs = args[i+1:]
			i = len(args)
		default:
			return proxyOptions{}, fmt.Errorf("unknown flag %q", args[i])
		}
	}

	if opts.codespaceName == "" {
		return proxyOptions{}, fmt.Errorf("missing required --codespace")
	}
	if !opts.shell && len(opts.cmdArgs) == 0 {
		return proxyOptions{}, fmt.Errorf("no command specified (use: proxy [flags] -- COMMAND [ARGS...])")
	}

	return opts, nil
}

func buildProxyExec(opts proxyOptions) (string, []string, error) {
	mode, err := codespaceenv.ParseGitHubAuthMode(opts.githubAuth)
	if err != nil {
		return "", nil, err
	}
	if err := validateEnvVars(opts.envVars); err != nil {
		return "", nil, err
	}

	sessionEnvVars, err := codespaceenv.SessionEnvPairs(mode)
	if err != nil {
		return "", nil, fmt.Errorf("--github-auth %s: %w", mode, err)
	}

	ghPath, err := proxyLookPath("gh")
	if err != nil {
		return "", nil, fmt.Errorf("finding gh: %w", err)
	}

	args := []string{"gh", "codespace", "ssh", "-c", opts.codespaceName, "--"}

	switch {
	case opts.shell:
		remoteCommand, err := buildProxyShellCommand(sessionEnvVars, nil, opts.workdir, `exec "${SHELL:-bash}" -l`)
		if err != nil {
			return "", nil, err
		}
		args = append(args, "bash", "-lc", remoteCommand)
	case opts.remoteBinary != "":
		args = append(args, opts.remoteBinary, "exec")
		if opts.workdir != "" {
			args = append(args, "--workdir", opts.workdir)
		}
		for _, kv := range sessionEnvVars {
			args = append(args, "--env", kv)
		}
		for _, kv := range opts.envVars {
			args = append(args, "--env", kv)
		}
		args = append(args, "--")
		args = append(args, opts.cmdArgs...)
	default:
		remoteCommand, err := buildProxyShellCommand(sessionEnvVars, opts.envVars, opts.workdir, "exec "+shellJoinArgs(opts.cmdArgs))
		if err != nil {
			return "", nil, err
		}
		args = append(args, "bash", "-c", remoteCommand)
	}

	return ghPath, args, nil
}

func buildProxyShellCommand(sessionEnvVars, explicitEnvVars []string, workdir, command string) (string, error) {
	segments := []string{codespaceenv.BuildShellBootstrap()}

	sessionExports, err := codespaceenv.ShellExportPrefix(sessionEnvVars)
	if err != nil {
		return "", err
	}
	if sessionExports != "" {
		segments = append(segments, sessionExports)
	}

	explicitExports, err := codespaceenv.ShellExportPrefix(explicitEnvVars)
	if err != nil {
		return "", err
	}
	if explicitExports != "" {
		segments = append(segments, explicitExports)
	}

	if workdir != "" {
		command = fmt.Sprintf("cd %s && %s", shellQuote(workdir), command)
	}
	segments = append(segments, command)

	return strings.Join(segments, " && "), nil
}

func validateEnvVars(envVars []string) error {
	for _, kv := range envVars {
		if key, _, ok := strings.Cut(kv, "="); !ok || key == "" {
			return fmt.Errorf("invalid env var %q (expected K=V)", kv)
		}
	}
	return nil
}
