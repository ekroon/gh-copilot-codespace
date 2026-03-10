package main

import (
	"fmt"
	"os"
	"strings"
	"syscall"

	"github.com/ekroon/gh-copilot-codespace/internal/codespaceenv"
)

var (
	applyCodespaceEnv = codespaceenv.ApplyProcessBootstrap
	execProcess       = syscall.Exec
)

// runExec runs a command with optional workdir and env setup.
// Used on the codespace as a structured alternative to bash -c with shell escaping.
//
// Usage: gh-copilot-codespace exec [--workdir DIR] [--env K=V]... -- COMMAND [ARGS...]
func runExec(args []string) error {
	var workdir string
	var envVars []string
	var cmdArgs []string

	// Parse flags before --
	i := 0
	for i < len(args) {
		switch {
		case args[i] == "--workdir" && i+1 < len(args):
			workdir = args[i+1]
			i += 2
		case args[i] == "--env" && i+1 < len(args):
			envVars = append(envVars, args[i+1])
			i += 2
		case args[i] == "--":
			cmdArgs = args[i+1:]
			i = len(args) // break out of loop
		default:
			return fmt.Errorf("unknown flag %q (use -- before command)", args[i])
		}
	}

	if len(cmdArgs) == 0 {
		return fmt.Errorf("no command specified (use: exec [--workdir DIR] [--env K=V]... -- COMMAND [ARGS...])")
	}

	applyCodespaceEnv()

	// Change to workdir if specified
	if workdir != "" {
		if err := os.Chdir(workdir); err != nil {
			return fmt.Errorf("chdir %q: %w", workdir, err)
		}
	}

	// Set environment variables
	for _, kv := range envVars {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) != 2 {
			return fmt.Errorf("invalid env var %q (expected K=V)", kv)
		}
		os.Setenv(parts[0], parts[1])
	}

	// Find the command in PATH
	command := cmdArgs[0]
	path, err := lookPath(command)
	if err != nil {
		return fmt.Errorf("command not found: %s", command)
	}

	// Replace this process with the command
	return execProcess(path, cmdArgs, os.Environ())
}

// lookPath finds the full path to a command, handling absolute paths.
func lookPath(cmd string) (string, error) {
	if strings.Contains(cmd, "/") {
		// Absolute or relative path — verify it exists
		if _, err := os.Stat(cmd); err != nil {
			return "", err
		}
		return cmd, nil
	}
	// Search PATH
	for _, dir := range strings.Split(os.Getenv("PATH"), ":") {
		if dir == "" {
			dir = "."
		}
		path := dir + "/" + cmd
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("%s: not found in PATH", cmd)
}
