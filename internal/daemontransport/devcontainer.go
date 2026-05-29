package daemontransport

import (
	"context"
	"io"
)

// DevContainerTransport is a stub for future daemon access through docker exec
// or `devcontainer exec` against a running container.
type DevContainerTransport struct {
	Container string // container id or devcontainer workspace name
}

// NewDevContainerTransport returns a placeholder devcontainer transport.
func NewDevContainerTransport(container string) *DevContainerTransport {
	return &DevContainerTransport{Container: container}
}

// Name returns the transport diagnostic name.
func (t *DevContainerTransport) Name() string { return "devcontainer" }

// Deploy is not implemented yet.
func (t *DevContainerTransport) Deploy(ctx context.Context) (string, error) {
	_ = ctx
	return "", ErrNotImplemented
}

// Spawn is not implemented yet.
func (t *DevContainerTransport) Spawn(ctx context.Context, remotePath string) (io.ReadWriteCloser, error) {
	_, _ = ctx, remotePath
	return nil, ErrNotImplemented
}

// Close releases transport resources. The stub has none.
func (t *DevContainerTransport) Close() error { return nil }
