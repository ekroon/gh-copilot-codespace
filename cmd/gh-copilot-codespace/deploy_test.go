package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ekroon/gh-copilot-codespace/internal/daemonproto"
	"github.com/ekroon/gh-copilot-codespace/internal/helperinfo"
	"github.com/ekroon/gh-copilot-codespace/internal/ssh"
)

type fakeDeployRemote struct {
	t               *testing.T
	expectedDigest  string
	info            helperinfo.Info
	failInstall     error
	oldFixedInUse   bool
	installed       bool
	commands        []string
	installCommands []string
}

func (f *fakeDeployRemote) run(_ context.Context, command string, stdin []byte) (string, error) {
	f.t.Helper()
	f.commands = append(f.commands, command)

	switch {
	case strings.Contains(command, `printf '%s\n' "$HOME"`):
		return "/home/codespace\n", nil
	case strings.Contains(command, "sha256sum"):
		if f.installed {
			return f.expectedDigest + "\n", nil
		}
		return "", nil
	case strings.Contains(command, "base64 -d"):
		f.installCommands = append(f.installCommands, command)
		if f.oldFixedInUse && strings.Contains(command, legacyRemoteBinaryPath) {
			return "", errors.New("text file busy")
		}
		if f.failInstall != nil {
			return "", f.failInstall
		}
		if len(stdin) == 0 {
			f.t.Fatal("install command received no binary input")
		}
		f.installed = true
		return "", nil
	case strings.Contains(command, " helper-info"):
		return helperInfoJSON(f.t, f.info), nil
	default:
		f.t.Fatalf("unexpected remote command: %s", command)
		return "", nil
	}
}

func helperInfoJSON(t *testing.T, info helperinfo.Info) string {
	t.Helper()
	data, err := info.Marshal()
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	return string(data)
}

func testDeployDeps(t *testing.T, remote *fakeDeployRemote) deployBinaryDeps {
	t.Helper()
	dir := t.TempDir()
	binaryPath := filepath.Join(dir, "gh-copilot-codespace")
	content := []byte("current helper binary")
	if err := os.WriteFile(binaryPath, content, 0o755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	digest := sha256.Sum256(content)
	remote.expectedDigest = hex.EncodeToString(digest[:])

	return deployBinaryDeps{
		detectArch: func(context.Context, remoteCommand) (string, error) { return "amd64", nil },
		getLinuxBinary: func(context.Context, string) (string, func(), error) {
			return binaryPath, nil, nil
		},
		binaryVersion: func(string) (string, error) { return "test-version", nil },
		remoteCommand: remote.run,
	}
}

func compatibleTestHelperInfo() helperinfo.Info {
	return helperinfo.Info{
		SchemaVersion:      helperinfo.SchemaVersion,
		Version:            "test-version",
		DaemonProtocol:     daemonproto.ProtocolVersion,
		FilesystemProtocol: helperinfo.FilesystemProtocolVersion,
		Capabilities:       []string{helperinfo.CapabilityDaemon, helperinfo.CapabilityFilesystem},
	}
}

func TestDeployBinaryIgnoresOldFixedHelper(t *testing.T) {
	remote := &fakeDeployRemote{
		t:             t,
		info:          compatibleTestHelperInfo(),
		oldFixedInUse: true,
	}
	client := ssh.NewClient("demo")

	got, err := deployBinaryWithDeps(context.Background(), client, "demo", testDeployDeps(t, remote))
	if err != nil {
		t.Fatalf("deployBinaryWithDeps() error = %v", err)
	}
	if got == legacyRemoteBinaryPath {
		t.Fatalf("helper path = %q, want content-addressed path", got)
	}
	if !strings.Contains(got, remote.expectedDigest) || !strings.Contains(got, "daemon-v"+daemonproto.ProtocolVersion) {
		t.Fatalf("helper path = %q, want digest and protocol", got)
	}
	if client.FilesystemHelperPath() != got {
		t.Fatalf("client helper path = %q, want %q", client.FilesystemHelperPath(), got)
	}
}

func TestDeployBinaryDoesNotReplaceInUseBinary(t *testing.T) {
	remote := &fakeDeployRemote{
		t:             t,
		info:          compatibleTestHelperInfo(),
		oldFixedInUse: true,
	}

	if _, err := deployBinaryWithDeps(context.Background(), ssh.NewClient("demo"), "demo", testDeployDeps(t, remote)); err != nil {
		t.Fatalf("deployBinaryWithDeps() error = %v", err)
	}
	if len(remote.installCommands) != 1 {
		t.Fatalf("install commands = %d, want 1", len(remote.installCommands))
	}
	command := remote.installCommands[0]
	for _, want := range []string{"mv -n", ".upload-"} {
		if !strings.Contains(command, want) {
			t.Fatalf("install command missing %q: %s", want, command)
		}
	}
	if strings.Contains(command, legacyRemoteBinaryPath) {
		t.Fatalf("install command touched legacy fixed helper: %s", command)
	}
}

func TestDeployBinaryFailureLeavesHelperUnselected(t *testing.T) {
	remote := &fakeDeployRemote{
		t:           t,
		info:        compatibleTestHelperInfo(),
		failInstall: errors.New("copy failed"),
	}
	client := ssh.NewClient("demo")

	_, err := deployBinaryWithDeps(context.Background(), client, "demo", testDeployDeps(t, remote))
	if err == nil || !strings.Contains(err.Error(), "copy failed") {
		t.Fatalf("deployBinaryWithDeps() error = %v, want copy failure", err)
	}
	if client.FilesystemHelperPath() != "" {
		t.Fatalf("client helper path = %q, want empty", client.FilesystemHelperPath())
	}
}

func TestDeployBinaryRejectsCapabilityMismatch(t *testing.T) {
	info := compatibleTestHelperInfo()
	info.FilesystemProtocol = "0"
	remote := &fakeDeployRemote{t: t, info: info}
	client := ssh.NewClient("demo")

	_, err := deployBinaryWithDeps(context.Background(), client, "demo", testDeployDeps(t, remote))
	if err == nil || !strings.Contains(err.Error(), "filesystem protocol") {
		t.Fatalf("deployBinaryWithDeps() error = %v, want filesystem protocol mismatch", err)
	}
	if client.FilesystemHelperPath() != "" {
		t.Fatalf("client helper path = %q, want empty", client.FilesystemHelperPath())
	}
}

func TestRestoreDeployedHelperRejectsOldFixedV1Helper(t *testing.T) {
	info := compatibleTestHelperInfo()
	info.SchemaVersion = 0
	remote := &fakeDeployRemote{t: t, info: info}
	client := ssh.NewClient("demo")

	err := restoreDeployedHelperWithDeps(
		context.Background(),
		client,
		"demo",
		legacyRemoteBinaryPath,
		deployBinaryDeps{remoteCommand: remote.run},
	)
	if err == nil || !strings.Contains(err.Error(), "schema version") {
		t.Fatalf("restoreDeployedHelperWithDeps() error = %v, want v1 rejection", err)
	}
	if client.FilesystemHelperPath() != "" {
		t.Fatalf("client helper path = %q, want empty", client.FilesystemHelperPath())
	}
}

func TestRestoreDeployedHelperWithDeps_CancelPropagation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	client := ssh.NewClient("demo")
	deps := deployBinaryDeps{
		remoteCommand: func(cmdCtx context.Context, command string, stdin []byte) (string, error) {
			select {
			case <-cmdCtx.Done():
				return "", cmdCtx.Err()
			default:
				t.Fatalf("remote command %q unexpectedly started with stdin %d", command, len(stdin))
				return "", nil
			}
		},
	}

	err := restoreDeployedHelperWithDeps(ctx, client, "demo", legacyRemoteBinaryPath, deps)
	if err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("restoreDeployedHelperWithDeps() error = %v, want context cancellation", err)
	}
}

func TestHelperDaemonProtocolMatchesDaemonHandshake(t *testing.T) {
	if helperinfo.DaemonProtocolVersion != daemonproto.ProtocolVersion {
		t.Fatalf(
			"helper daemon protocol = %q, daemon handshake = %q",
			helperinfo.DaemonProtocolVersion,
			daemonproto.ProtocolVersion,
		)
	}
}

func TestDeployBinaryWithDeps_CancelPropagation(t *testing.T) {
	// Prove that a cancelled context causes deployBinaryWithDeps to return
	// promptly even when a dependency blocks on ctx.Done.
	ctx, cancel := context.WithCancel(context.Background())

	client := ssh.NewClient("demo")
	deps := deployBinaryDeps{
		detectArch: func(dctx context.Context, _ remoteCommand) (string, error) {
			// Block until context is cancelled.
			<-dctx.Done()
			return "", dctx.Err()
		},
		getLinuxBinary: func(context.Context, string) (string, func(), error) {
			t.Fatal("getLinuxBinary should not be called")
			return "", nil, nil
		},
		binaryVersion: func(string) (string, error) { return "v", nil },
		remoteCommand: func(context.Context, string, []byte) (string, error) {
			t.Fatal("remoteCommand should not be called")
			return "", nil
		},
	}

	done := make(chan error, 1)
	go func() {
		_, err := deployBinaryWithDeps(ctx, client, "demo", deps)
		done <- err
	}()

	// Cancel after brief delay.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "context canceled") {
			t.Fatalf("expected context canceled error, got: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("deployBinaryWithDeps did not return after context cancel")
	}
}
