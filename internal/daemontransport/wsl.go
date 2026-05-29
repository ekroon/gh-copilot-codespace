package daemontransport

import (
	"context"
	"io"
)

// WSLTransport is a stub for future daemon access through `wsl` against a
// running Windows Subsystem for Linux distro.
type WSLTransport struct {
	Distro string
}

// NewWSLTransport returns a placeholder WSL transport.
func NewWSLTransport(distro string) *WSLTransport {
	return &WSLTransport{Distro: distro}
}

// Name returns the transport diagnostic name.
func (t *WSLTransport) Name() string { return "wsl" }

// Deploy is not implemented yet.
func (t *WSLTransport) Deploy(ctx context.Context) (string, error) {
	_ = ctx
	return "", ErrNotImplemented
}

// Spawn is not implemented yet.
func (t *WSLTransport) Spawn(ctx context.Context, remotePath string) (io.ReadWriteCloser, error) {
	_, _ = ctx, remotePath
	return nil, ErrNotImplemented
}

// Close releases transport resources. The stub has none.
func (t *WSLTransport) Close() error { return nil }
