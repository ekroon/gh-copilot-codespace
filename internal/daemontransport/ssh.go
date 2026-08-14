package daemontransport

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/ekroon/gh-copilot-codespace/internal/ssh"
)

// Deployer installs the daemon-capable binary in a codespace and returns the
// absolute path to launch remotely.
type Deployer func(ctx context.Context, client *ssh.Client, codespaceName string) (string, error)

var commandContext = exec.CommandContext

// SSHTransport runs the daemon process inside a GitHub Codespace over SSH.
type SSHTransport struct {
	client        *ssh.Client
	codespaceName string
	deployFunc    Deployer
	sshStateFunc  func(*ssh.Client) (sshConfigPath, sshHost string)
	recoverFunc   func(context.Context, *ssh.Client) error
}

// NewSSHTransport returns a Transport that starts daemon processes over the SSH
// client already configured by the launcher. Pass deployBinary as the optional
// deployer from the cmd package to avoid an import cycle.
func NewSSHTransport(client *ssh.Client, codespaceName string, deployer ...Deployer) *SSHTransport {
	deployFunc := Deployer(func(context.Context, *ssh.Client, string) (string, error) {
		return "", ErrNotImplemented
	})
	if len(deployer) > 0 && deployer[0] != nil {
		deployFunc = deployer[0]
	}
	return &SSHTransport{
		client:        client,
		codespaceName: codespaceName,
		deployFunc:    deployFunc,
		sshStateFunc: func(c *ssh.Client) (string, string) {
			return c.SSHConfigPath(), c.SSHHost()
		},
		recoverFunc: func(ctx context.Context, c *ssh.Client) error {
			return c.RefreshMultiplexing(ctx)
		},
	}
}

// Name returns the transport diagnostic name.
func (t *SSHTransport) Name() string { return "ssh" }

// Deploy installs the daemon binary in the codespace via the injected deployer.
func (t *SSHTransport) Deploy(ctx context.Context) (string, error) {
	return t.deployFunc(ctx, t.client, t.codespaceName)
}

// Recover wakes the Codespace, retires stale SSH multiplexing state, and
// establishes a fresh master before daemonclient reconnects.
func (t *SSHTransport) Recover(ctx context.Context) error {
	return t.recoverFunc(ctx, t.client)
}

// Spawn starts `<remotePath> daemon` in the codespace and returns its stdio
// stream.
//
// The SSH process is intentionally bound to a fresh background context, not
// the caller's ctx. The caller's ctx exists only to govern startup; once Start
// returns, lifetime is owned by the returned stream's Close. Binding to the
// caller's ctx would kill the daemon the moment Dial returns and its caller's
// deferred cancel() fires.
func (t *SSHTransport) Spawn(ctx context.Context, remotePath string) (io.ReadWriteCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	remoteCommand := fmt.Sprintf("%s daemon", shellQuote(remotePath))
	sshConfigPath, sshHost := t.sshStateFunc(t.client)

	var cmd *exec.Cmd
	if sshConfigPath != "" {
		cmd = commandContext(context.Background(), "ssh", "-F", sshConfigPath, sshHost, remoteCommand)
	} else {
		cmd = commandContext(context.Background(), "gh", "codespace", "ssh", "-c", t.codespaceName, "--", remoteCommand)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("daemontransport: open daemon stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("daemontransport: open daemon stdout: %w", err)
	}
	stderr := newStderrTail(os.Stderr, StderrTailLimit)
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("daemontransport: start ssh daemon: %w", err)
	}

	return &sshSpawnedStream{processStream: newProcessStream(t.Name(), cmd, stdin, stdout, stderr)}, nil
}

// Close releases transport resources. The SSH client owns multiplexing state,
// so there is nothing to release here.
func (t *SSHTransport) Close() error { return nil }

var _ Recoverer = (*SSHTransport)(nil)

type sshSpawnedStream struct {
	*processStream
}

// The SSH stream must keep reporting terminal causes; daemonclient type-asserts
// the spawned stream, not the embedded processStream.
var _ TerminalErrorReporter = (*sshSpawnedStream)(nil)

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
