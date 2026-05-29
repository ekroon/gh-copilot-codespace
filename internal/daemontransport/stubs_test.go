package daemontransport

import (
	"context"
	"errors"
	"testing"
)

func TestDevContainerTransportReturnsNotImplemented(t *testing.T) {
	transport := NewDevContainerTransport("container-one")
	if transport.Name() != "devcontainer" {
		t.Fatalf("Name = %q, want devcontainer", transport.Name())
	}
	if transport.Container != "container-one" {
		t.Fatalf("Container = %q, want container-one", transport.Container)
	}
	if _, err := transport.Deploy(context.Background()); !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("Deploy error = %v, want ErrNotImplemented", err)
	}
	if stream, err := transport.Spawn(context.Background(), "/remote/bin"); stream != nil || !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("Spawn = (%v, %v), want nil, ErrNotImplemented", stream, err)
	}
	if err := transport.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestWSLTransportReturnsNotImplemented(t *testing.T) {
	transport := NewWSLTransport("Ubuntu")
	if transport.Name() != "wsl" {
		t.Fatalf("Name = %q, want wsl", transport.Name())
	}
	if transport.Distro != "Ubuntu" {
		t.Fatalf("Distro = %q, want Ubuntu", transport.Distro)
	}
	if _, err := transport.Deploy(context.Background()); !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("Deploy error = %v, want ErrNotImplemented", err)
	}
	if stream, err := transport.Spawn(context.Background(), "/remote/bin"); stream != nil || !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("Spawn = (%v, %v), want nil, ErrNotImplemented", stream, err)
	}
	if err := transport.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
