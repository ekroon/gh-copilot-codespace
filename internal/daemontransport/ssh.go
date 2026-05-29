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
type Deployer func(*ssh.Client, string) (string, error)

var commandContext = exec.CommandContext

// SSHTransport runs the daemon process inside a GitHub Codespace over SSH.
type SSHTransport struct {
	client        *ssh.Client
	codespaceName string
	deployFunc    Deployer
	sshStateFunc  func(*ssh.Client) (sshConfigPath, sshHost string)
}

// NewSSHTransport returns a Transport that starts daemon processes over the SSH
// client already configured by the launcher. Pass deployBinary as the optional
// deployer from the cmd package to avoid an import cycle.
func NewSSHTransport(client *ssh.Client, codespaceName string, deployer ...Deployer) *SSHTransport {
	deployFunc := Deployer(func(*ssh.Client, string) (string, error) {
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
	}
}

// Name returns the transport diagnostic name.
func (t *SSHTransport) Name() string { return "ssh" }

// Deploy installs the daemon binary in the codespace via the injected deployer.
func (t *SSHTransport) Deploy(ctx context.Context) (string, error) {
	_ = ctx
	return t.deployFunc(t.client, t.codespaceName)
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
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("daemontransport: start ssh daemon: %w", err)
	}

	return &sshSpawnedStream{processStream: newProcessStream(cmd, stdin, stdout)}, nil
}

// Close releases transport resources. The SSH client owns multiplexing state,
// so there is nothing to release here.
func (t *SSHTransport) Close() error { return nil }

type sshSpawnedStream struct {
	*processStream
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
