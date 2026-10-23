package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ekroon/gh-copilot-codespace/internal/daemonproto"
	"github.com/ekroon/gh-copilot-codespace/internal/helperinfo"
	"github.com/ekroon/gh-copilot-codespace/internal/ssh"
)

func buildLinuxHelperFixture(t *testing.T, goos, arch string) string {
	t.Helper()
	dir := t.TempDir()
	source := filepath.Join(dir, "main.go")
	if err := os.WriteFile(source, []byte("package main\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "helper")
	cmd := exec.Command("go", "build", "-o", binary, source)
	cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+arch, "CGO_ENABLED=0")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s/%s fixture: %v\n%s", goos, arch, err, output)
	}
	return binary
}

func TestLinuxHelperOverrideUsesSuppliedArtifactWithoutCleanup(t *testing.T) {
	artifact := buildLinuxHelperFixture(t, "linux", runtime.GOARCH)
	t.Setenv("COPILOT_CODESPACE_LINUX_HELPER", artifact)
	got, cleanup, err := getLinuxBinary(context.Background(), runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	if got != artifact || cleanup != nil {
		t.Fatalf("override = %q, cleanup present = %v; want %q with no cleanup", got, cleanup != nil, artifact)
	}
	if _, err := os.Stat(artifact); err != nil {
		t.Fatalf("user artifact was deleted: %v", err)
	}
}

func TestLinuxHelperOverrideRejectsInvalidArtifactWithoutFallback(t *testing.T) {
	dir := t.TempDir()
	text := filepath.Join(dir, "not-elf")
	if err := os.WriteFile(text, []byte("not an ELF binary"), 0o600); err != nil {
		t.Fatal(err)
	}
	unreadable := filepath.Join(dir, "unreadable")
	if err := os.WriteFile(unreadable, []byte("unreadable"), 0); err != nil {
		t.Fatal(err)
	}
	// A well-formed empty ELF header has no Go buildinfo.
	elfHeader := make([]byte, 64)
	copy(elfHeader, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	elfHeader[16], elfHeader[18], elfHeader[20], elfHeader[52] = 2, 62, 1, 64
	noBuildInfo := filepath.Join(dir, "no-buildinfo")
	if err := os.WriteFile(noBuildInfo, elfHeader, 0o600); err != nil {
		t.Fatal(err)
	}
	otherArch := "arm64"
	if runtime.GOARCH == otherArch {
		otherArch = "amd64"
	}
	cases := []struct {
		name, artifact, want string
	}{
		{"empty", "", "absolute"},
		{"relative", "relative/helper", "absolute"},
		{"missing", filepath.Join(dir, "missing"), "stat"},
		{"directory", dir, "regular"},
		{"text", text, "ELF"},
		{"no Go buildinfo", noBuildInfo, "buildinfo"},
		{"Darwin", buildLinuxHelperFixture(t, "darwin", runtime.GOARCH), "ELF"},
		{"wrong architecture", buildLinuxHelperFixture(t, "linux", otherArch), "GOARCH"},
		{"non-Linux ELF", buildLinuxHelperFixture(t, "freebsd", runtime.GOARCH), "GOOS"},
	}
	if os.Geteuid() != 0 {
		cases = append(cases, struct{ name, artifact, want string }{"unreadable", unreadable, "open"})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("COPILOT_CODESPACE_LINUX_HELPER", tc.artifact)
			got, cleanup, err := getLinuxBinary(context.Background(), runtime.GOARCH)
			if err == nil || !strings.Contains(err.Error(), "COPILOT_CODESPACE_LINUX_HELPER") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("override error = %v, want explicit override %q failure", err, tc.want)
			}
			if got != "" || cleanup != nil {
				t.Fatalf("invalid override fell back: %q, cleanup present = %v", got, cleanup != nil)
			}
		})
	}
}

func TestLinuxHelperOverrideUnsetPreservesNativeBinary(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("native Linux self-binary path only")
	}
	t.Setenv("COPILOT_CODESPACE_LINUX_HELPER", "")
	if err := os.Unsetenv("COPILOT_CODESPACE_LINUX_HELPER"); err != nil {
		t.Fatal(err)
	}
	got, cleanup, err := getLinuxBinary(context.Background(), runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if got != self || cleanup != nil {
		t.Fatalf("default native helper = %q, cleanup present = %v; want %q", got, cleanup != nil, self)
	}
}

func TestNativeDeploymentNeverCrossCompilesLocalSource(t *testing.T) {
	otherArch := "arm64"
	if runtime.GOARCH == otherArch {
		otherArch = "amd64"
	}
	for _, mode := range []string{"native initial", "native lifecycle", "legacy"} {
		t.Run(mode, func(t *testing.T) {
			bin := t.TempDir()
			marker := filepath.Join(t.TempDir(), "local-build-attempted")
			for name, script := range map[string]string{
				"go": "#!/bin/sh\n/bin/mkdir \"$FAKE_GO_MARKER\"\nexit 73\n",
				"gh": "#!/bin/sh\nexit 74\n",
			} {
				if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", bin)
			t.Setenv("FAKE_GO_MARKER", marker)
			t.Setenv(codespaceRemoteNativeEnv, "")
			t.Setenv(codespaceWorkspaceConfigEnv, "")
			t.Setenv(codespaceLinuxHelperEnv, "")
			if err := os.Unsetenv(codespaceLinuxHelperEnv); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "native initial":
				t.Setenv(codespaceRemoteNativeEnv, "1")
			case "native lifecycle":
				t.Setenv(codespaceWorkspaceConfigEnv, `{"alias":"pinned"}`)
			}
			got, cleanup, err := getLinuxBinary(context.Background(), otherArch)
			if cleanup != nil {
				defer cleanup()
			}
			if mode == "legacy" {
				if _, err := os.Stat(marker); err != nil {
					t.Fatalf("legacy no longer attempts local cross-build: %v", err)
				}
				return
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Errorf("native deployment attempted a local source build: %v", err)
			}
			if err == nil || !strings.Contains(err.Error(), codespaceLinuxHelperEnv) || !strings.Contains(err.Error(), "remote-native") {
				t.Errorf("missing native override guidance: %v", err)
			}
			if got != "" || cleanup != nil {
				t.Errorf("failed native deployment returned helper %q, cleanup present = %v", got, cleanup != nil)
			}
		})
	}
}

func TestNativeDeploymentCanReuseLinuxSelf(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("native Linux self-binary path only")
	}
	t.Setenv(codespaceRemoteNativeEnv, "1")
	t.Setenv(codespaceLinuxHelperEnv, "")
	if err := os.Unsetenv(codespaceLinuxHelperEnv); err != nil {
		t.Fatal(err)
	}
	got, cleanup, err := getLinuxBinary(context.Background(), runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if got != self || cleanup != nil {
		t.Fatalf("native self helper = %q, cleanup present = %v; want %q", got, cleanup != nil, self)
	}
}

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
