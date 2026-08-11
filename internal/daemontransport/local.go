package daemontransport

import (
	"context"
	"fmt"
	"io"
	"os"
)

// LocalTransport runs the daemon as a local child process.
type LocalTransport struct {
	binaryPath string
}

// NewLocalTransport returns a Transport that runs the daemon as a local child
// process. Useful for tests and for development on the launcher host where no
// remote sandbox is available.
func NewLocalTransport(binaryPath string) *LocalTransport {
	return &LocalTransport{binaryPath: binaryPath}
}

// Name returns the transport diagnostic name.
func (t *LocalTransport) Name() string { return "local" }

// Deploy is a no-op for local transports and returns the configured binary.
func (t *LocalTransport) Deploy(ctx context.Context) (string, error) {
	_ = ctx
	return t.binaryPath, nil
}

// Spawn starts `remotePath daemon` locally and returns its stdio stream.
//
// The daemon process is intentionally bound to a fresh background context, not
// the caller's ctx. The caller's ctx exists only to govern startup (and is
// observed via context.WithCancel below so that ctx cancellation during Start
// kills the half-started process). Once Start returns, the process lifetime is
// owned by the returned stream's Close — binding it to the caller's ctx would
// kill the daemon as soon as Dial returns.
func (t *LocalTransport) Spawn(ctx context.Context, remotePath string) (io.ReadWriteCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cmd := commandContext(context.Background(), remotePath, "daemon")

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
		return nil, fmt.Errorf("daemontransport: start local daemon: %w", err)
	}

	return newProcessStream(t.Name(), cmd, stdin, stdout, stderr), nil
}

// Close releases transport resources. LocalTransport has none.
func (t *LocalTransport) Close() error { return nil }
