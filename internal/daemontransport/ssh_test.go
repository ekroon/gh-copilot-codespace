package daemontransport

import (
	"context"
	"io"
	"os"
	"os/exec"
	"reflect"
	"testing"

	"github.com/ekroon/gh-copilot-codespace/internal/ssh"
)

func TestSSHTransportSpawnUsesMultiplexWhenConfigPresent(t *testing.T) {
	capture := installFakeCommandContext(t)
	client := ssh.NewClient("codespace-one")
	transport := NewSSHTransport(client, "codespace-one")
	transport.sshStateFunc = func(*ssh.Client) (string, string) {
		return "project/.ssh-config", "cs.codespace-one"
	}

	stream, err := transport.Spawn(context.Background(), "/remote/bin")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if capture.name != "ssh" {
		t.Fatalf("command name = %q, want ssh", capture.name)
	}
	wantArgs := []string{"-F", "project/.ssh-config", "cs.codespace-one", "'/remote/bin' daemon"}
	if !reflect.DeepEqual(capture.args, wantArgs) {
		t.Fatalf("command args = %#v, want %#v", capture.args, wantArgs)
	}
}

func TestSSHTransportSpawnFallsBackToGhCodespaceWhenNoMultiplex(t *testing.T) {
	capture := installFakeCommandContext(t)
	client := ssh.NewClient("codespace-two")
	transport := NewSSHTransport(client, "codespace-two")

	stream, err := transport.Spawn(context.Background(), "/remote/bin")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if capture.name != "gh" {
		t.Fatalf("command name = %q, want gh", capture.name)
	}
	wantArgs := []string{"codespace", "ssh", "-c", "codespace-two", "--", "'/remote/bin' daemon"}
	if !reflect.DeepEqual(capture.args, wantArgs) {
		t.Fatalf("command args = %#v, want %#v", capture.args, wantArgs)
	}
}

func TestSSHTransportDeployDelegatesToInjectedFunc(t *testing.T) {
	client := ssh.NewClient("codespace-three")
	called := false
	transport := NewSSHTransport(client, "codespace-three", func(_ context.Context, gotClient *ssh.Client, gotName string) (string, error) {
		called = true
		if gotClient != client {
			t.Fatalf("deployer client = %p, want %p", gotClient, client)
		}
		if gotName != "codespace-three" {
			t.Fatalf("deployer codespace = %q, want codespace-three", gotName)
		}
		return "/remote/path", nil
	})

	path, err := transport.Deploy(context.Background())
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if !called {
		t.Fatal("deployer was not called")
	}
	if path != "/remote/path" {
		t.Fatalf("Deploy path = %q, want /remote/path", path)
	}
}

func TestSSHTransportRecoverRefreshesMultiplexing(t *testing.T) {
	client := ssh.NewClient("codespace-recover")
	called := 0
	transport := NewSSHTransport(client, "codespace-recover")
	transport.recoverFunc = func(ctx context.Context, got *ssh.Client) error {
		called++
		if got != client {
			t.Fatalf("Recover client = %p, want %p", got, client)
		}
		return ctx.Err()
	}

	if err := transport.Recover(context.Background()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if called != 1 {
		t.Fatalf("recover calls = %d, want 1", called)
	}
}

type capturedCommand struct {
	name string
	args []string
}

func installFakeCommandContext(t *testing.T) *capturedCommand {
	t.Helper()
	capture := &capturedCommand{}
	old := commandContext
	commandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		capture.name = name
		capture.args = append([]string(nil), args...)
		helperArgs := append([]string{"-test.run=TestDaemonTransportCommandHelper", "--", name}, args...)
		cmd := exec.CommandContext(ctx, os.Args[0], helperArgs...)
		cmd.Env = append(os.Environ(), "GO_WANT_DAEMONTRANSPORT_HELPER=1")
		return cmd
	}
	t.Cleanup(func() { commandContext = old })
	return capture
}

func TestDaemonTransportCommandHelper(t *testing.T) {
	if os.Getenv("GO_WANT_DAEMONTRANSPORT_HELPER") != "1" {
		return
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}
