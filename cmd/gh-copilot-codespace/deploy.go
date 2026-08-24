package main

import (
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	pathpkg "path"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/ekroon/gh-copilot-codespace/internal/daemonproto"
	"github.com/ekroon/gh-copilot-codespace/internal/helperinfo"
	"github.com/ekroon/gh-copilot-codespace/internal/ssh"
)

// Published helpers are intentionally retained. A helper may still back a
// daemon or per-call fallback, so automatic cleanup must never unlink it.
const (
	legacyRemoteBinaryPath = "/tmp/gh-copilot-codespace-bin/gh-copilot-codespace"
	remoteHelperCacheDir   = ".cache/gh-copilot-codespace/helpers"
)

type deployBinaryDeps struct {
	detectArch     func(ctx context.Context, remote remoteCommand) (string, error)
	getLinuxBinary func(ctx context.Context, arch string) (string, func(), error)
	binaryVersion  func(binaryPath string) (string, error)
	remoteCommand  remoteCommand
}

type remoteCommand func(ctx context.Context, command string, stdin []byte) (string, error)

// deployBinary copies this binary to the codespace for use as a remote exec agent.
// In dev mode (go run / local build), it cross-compiles for linux.
// In release mode (installed via mise/gh), it downloads the matching linux binary.
// Returns the remote path to the deployed binary.
func deployBinary(ctx context.Context, sshClient *ssh.Client, codespaceName string) (string, error) {
	return deployBinaryWithDeps(ctx, sshClient, codespaceName, deployBinaryDeps{
		detectArch:     detectCodespaceArch,
		getLinuxBinary: getLinuxBinary,
		binaryVersion:  binaryVersion,
		remoteCommand: func(ctx context.Context, command string, stdin []byte) (string, error) {
			return runRemoteCommandOnClient(ctx, sshClient, command, stdin)
		},
	})
}

func deployBinaryWithDeps(ctx context.Context, sshClient *ssh.Client, codespaceName string, deps deployBinaryDeps) (string, error) {
	arch, err := deps.detectArch(ctx, deps.remoteCommand)
	if err != nil {
		return "", fmt.Errorf("detecting codespace arch: %w", err)
	}

	linuxBinary, cleanup, err := deps.getLinuxBinary(ctx, arch)
	if err != nil {
		return "", fmt.Errorf("getting linux binary: %w", err)
	}
	if cleanup != nil {
		defer cleanup()
	}

	binData, err := os.ReadFile(linuxBinary)
	if err != nil {
		return "", fmt.Errorf("reading binary: %w", err)
	}
	digestBytes := sha256.Sum256(binData)
	digest := hex.EncodeToString(digestBytes[:])
	version, err := deps.binaryVersion(linuxBinary)
	if err != nil {
		return "", fmt.Errorf("reading binary version: %w", err)
	}

	homeOut, err := deps.remoteCommand(ctx, `printf '%s\n' "$HOME"`, nil)
	if err != nil {
		return "", fmt.Errorf("detecting remote home: %w", err)
	}
	remoteHome := strings.TrimSpace(homeOut)
	if !pathpkg.IsAbs(remoteHome) {
		return "", fmt.Errorf("detecting remote home: got non-absolute path %q", remoteHome)
	}

	compatibilityDir := fmt.Sprintf(
		"layout-v%d-daemon-v%s-filesystem-v%s",
		helperinfo.SchemaVersion,
		daemonproto.ProtocolVersion,
		helperinfo.FilesystemProtocolVersion,
	)
	remotePath := pathpkg.Join(
		remoteHome,
		remoteHelperCacheDir,
		compatibilityDir,
		sanitizeHelperPathComponent(version),
		digest,
		"gh-copilot-codespace",
	)

	remoteDigest, err := deployedHelperDigest(ctx, deps, remotePath)
	if err != nil {
		return "", err
	}
	if remoteDigest != "" && remoteDigest != digest {
		return "", fmt.Errorf("content-addressed helper %q has digest %q, want %q; refusing to replace it", remotePath, remoteDigest, digest)
	}

	if remoteDigest == "" {
		fmt.Fprintln(os.Stderr, "Deploying exec agent to codespace...")
		remoteDir := pathpkg.Dir(remotePath)
		installCmd := fmt.Sprintf(`set -eu
dir=%s
dest=%s
mkdir -p "$dir"
tmp="$dir/.upload-$$"
trap 'rm -f "$tmp"' EXIT HUP INT TERM
umask 077
base64 -d > "$tmp"
chmod 0755 "$tmp"
mv -n "$tmp" "$dest"
test -x "$dest"`,
			deployShellQuote(remoteDir),
			deployShellQuote(remotePath),
		)
		encoded := []byte(base64.StdEncoding.EncodeToString(binData))
		if _, err := deps.remoteCommand(ctx, installCmd, encoded); err != nil {
			return "", fmt.Errorf("installing content-addressed helper: %w", err)
		}

		remoteDigest, err = deployedHelperDigest(ctx, deps, remotePath)
		if err != nil {
			return "", err
		}
		if remoteDigest != digest {
			return "", fmt.Errorf("deployed helper digest %q does not match local digest %q", remoteDigest, digest)
		}
	}

	info, err := probeDeployedHelper(ctx, deps, remotePath)
	if err != nil {
		return "", err
	}
	if info.Version != version {
		return "", fmt.Errorf("deployed helper version %q does not match candidate version %q", info.Version, version)
	}
	if err := sshClient.SelectFilesystemHelper(remotePath, info); err != nil {
		return "", err
	}
	fmt.Fprintf(os.Stderr, "  ✓ Deployed exec agent (%s)\n", arch)
	return remotePath, nil
}

func deployedHelperDigest(ctx context.Context, deps deployBinaryDeps, remotePath string) (string, error) {
	command := fmt.Sprintf(
		"if [ -f %s ]; then sha256sum -- %s | awk '{print $1}'; fi",
		deployShellQuote(remotePath),
		deployShellQuote(remotePath),
	)
	out, err := deps.remoteCommand(ctx, command, nil)
	if err != nil {
		return "", fmt.Errorf("checking deployed helper digest: %w", err)
	}
	return strings.TrimSpace(out), nil
}

func probeDeployedHelper(ctx context.Context, deps deployBinaryDeps, remotePath string) (helperinfo.Info, error) {
	out, err := deps.remoteCommand(ctx, deployShellQuote(remotePath)+" helper-info", nil)
	if err != nil {
		return helperinfo.Info{}, fmt.Errorf("probing deployed helper compatibility: %w", err)
	}
	info, err := helperinfo.Parse([]byte(strings.TrimSpace(out)))
	if err != nil {
		return helperinfo.Info{}, fmt.Errorf("probing deployed helper compatibility: %w", err)
	}
	return info, nil
}

func restoreDeployedHelper(ctx context.Context, sshClient *ssh.Client, codespaceName, remotePath string) error {
	return restoreDeployedHelperWithDeps(
		ctx,
		sshClient,
		codespaceName,
		remotePath,
		deployBinaryDeps{remoteCommand: func(ctx context.Context, command string, stdin []byte) (string, error) {
			return runRemoteCommandOnClient(ctx, sshClient, command, stdin)
		}},
	)
}

func restoreDeployedHelperWithDeps(ctx context.Context, sshClient *ssh.Client, codespaceName, remotePath string, deps deployBinaryDeps) error {
	info, err := probeDeployedHelper(ctx, deps, remotePath)
	if err != nil {
		return err
	}
	return sshClient.SelectFilesystemHelper(remotePath, info)
}

func deployShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func binaryVersion(binaryPath string) (string, error) {
	build, err := buildinfo.ReadFile(binaryPath)
	if err != nil {
		return "", err
	}
	return helperinfo.VersionFromBuildInfo(build), nil
}

func sanitizeHelperPathComponent(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, value)
}

// detectCodespaceArch returns the codespace's CPU architecture (amd64 or arm64).
func detectCodespaceArch(ctx context.Context, remote remoteCommand) (string, error) {
	out, err := remote(ctx, "uname -m", nil)
	if err != nil {
		return "", err
	}
	machine := strings.TrimSpace(out)
	switch machine {
	case "x86_64":
		return "amd64", nil
	case "aarch64", "arm64":
		return "arm64", nil
	default:
		return "", fmt.Errorf("unsupported architecture: %s", machine)
	}
}

func runRemoteCommandOnClient(ctx context.Context, sshClient *ssh.Client, command string, stdin []byte) (string, error) {
	stdout, stderr, exitCode, err := sshClient.ExecWithInput(ctx, command, stdin)
	if err != nil {
		return "", err
	}
	if exitCode != 0 {
		trimmed := strings.TrimSpace(stderr)
		if trimmed == "" {
			return "", fmt.Errorf("remote command failed (exit %d)", exitCode)
		}
		return "", fmt.Errorf("remote command failed (exit %d): %s", exitCode, trimmed)
	}
	return stdout, nil
}

// getLinuxBinary returns a path to a linux binary for the given arch.
// Returns the path and an optional cleanup function.
func getLinuxBinary(ctx context.Context, arch string) (string, func(), error) {
	// If we're already on linux with matching arch, use ourselves
	if runtime.GOOS == "linux" && runtime.GOARCH == arch {
		self, err := os.Executable()
		if err != nil {
			return "", nil, err
		}
		return self, nil, nil
	}

	// Try cross-compile first (dev mode — Go installed)
	if path, cleanup, err := crossCompile(ctx, arch); err == nil {
		return path, cleanup, nil
	}

	// Fall back to downloading from release
	return downloadReleaseBinary(ctx, arch)
}

// crossCompile builds a linux binary for the given arch.
func crossCompile(ctx context.Context, arch string) (string, func(), error) {
	// Check if Go is available
	goPath, err := exec.LookPath("go")
	if err != nil {
		return "", nil, fmt.Errorf("go not found")
	}

	// Find the module root (where go.mod lives)
	modRoot, err := findModuleRoot()
	if err != nil {
		return "", nil, fmt.Errorf("finding module root: %w", err)
	}

	tmpDir, err := os.MkdirTemp("", "gh-copilot-codespace-cross-*")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { os.RemoveAll(tmpDir) }

	outPath := filepath.Join(tmpDir, "gh-copilot-codespace")
	cmd := exec.CommandContext(ctx, goPath, "build", "-ldflags=-s -w", "-o", outPath, "./cmd/gh-copilot-codespace")
	cmd.Dir = modRoot
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+arch, "CGO_ENABLED=0")
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("cross-compile failed: %w", err)
	}

	fmt.Fprintf(os.Stderr, "  ✓ Cross-compiled for linux/%s\n", arch)
	return outPath, cleanup, nil
}

// findModuleRoot walks up from the current executable to find go.mod.
func findModuleRoot() (string, error) {
	// Try the directory containing the executable first
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(self)

	// Walk up looking for go.mod
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	// Fall back to current working directory and walk up
	dir, err = os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	return "", fmt.Errorf("go.mod not found")
}

// downloadReleaseBinary downloads the linux binary from the exact release
// associated with this build. It deliberately never resolves an untagged
// "latest" asset.
func downloadReleaseBinary(ctx context.Context, arch string) (string, func(), error) {
	tmpDir, err := os.MkdirTemp("", "gh-copilot-codespace-download-*")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { os.RemoveAll(tmpDir) }

	pattern := fmt.Sprintf("gh-copilot-codespace-linux-%s", arch)
	outPath := filepath.Join(tmpDir, "gh-copilot-codespace")
	currentBuild, _ := debug.ReadBuildInfo()
	releaseTag, err := helperinfo.ReleaseTagFromBuildInfo(currentBuild)
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("resolving exact release: %w", err)
	}

	cmd := exec.CommandContext(ctx, "gh", "release", "download",
		releaseTag,
		"--repo", "ekroon/gh-copilot-codespace",
		"--pattern", pattern,
		"--output", outPath)
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("download failed: %w", err)
	}

	if err := os.Chmod(outPath, 0o755); err != nil {
		cleanup()
		return "", nil, err
	}

	fmt.Fprintf(os.Stderr, "  ✓ Downloaded linux/%s binary from release\n", arch)
	return outPath, cleanup, nil
}
