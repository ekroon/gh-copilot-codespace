package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/ekroon/gh-copilot-codespace/internal/daemonclient"
	"github.com/ekroon/gh-copilot-codespace/internal/daemontransport"
	"github.com/ekroon/gh-copilot-codespace/internal/registry"
	"github.com/ekroon/gh-copilot-codespace/internal/ssh"
)

// daemonDisabledEnv is the user-facing opt-out.
const daemonDisabledEnv = "COPILOT_CODESPACE_NO_DAEMON"

var daemonDeployBinary = deployBinary

// daemonDisabled returns true when the user has opted out of the persistent
// daemon transport for extension-tools mode. Used to fall back to the per-call
// ssh.Client behaviour.
func daemonDisabled() bool {
	v := os.Getenv(daemonDisabledEnv)
	return v == "1" || v == "true" || v == "yes"
}

// wrapExecutorsWithDaemon replaces every executor in the registry with a
// DaemonExecutor when possible. Each codespace gets its own daemon connection
// over SSHTransport. Failures are logged and the original ssh.Client is left in
// place so the extension session still works (just at per-call SSH cost).
//
// Caller is responsible for closing the returned cleanup funcs when the
// extension-host process exits; in practice, the OS will reap the daemons when
// the SSH stream closes, so cleanup is best-effort and may be a no-op.
func wrapExecutorsWithDaemon(ctx context.Context, reg *registry.Registry) []func() {
	if daemonDisabled() {
		return nil
	}

	var closers []func()
	for _, cs := range reg.All() {
		sshClient, ok := cs.Executor.(*ssh.Client)
		if !ok {
			continue
		}

		transport := daemontransport.NewSSHTransport(sshClient, cs.Name, daemonDeployBinary)
		dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		exec, err := daemonclient.Dial(dialCtx, transport)
		cancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "daemon: dial failed for %s, falling back to ssh: %v\n", cs.Alias, err)
			_ = transport.Close()
			continue
		}

		if wd := sshClient.GetWorkdir(); wd != "" {
			exec.SetWorkdir(wd)
		}
		cs.Executor = exec
		closers = append(closers, func() { _ = exec.Close() })
	}
	return closers
}
