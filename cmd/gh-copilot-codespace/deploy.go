package main

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/ekroon/gh-copilot-codespace/internal/ssh"
)

const remoteBinaryDir = "/tmp/gh-copilot-codespace-bin"

// deployBinary copies this binary to the codespace for use as a remote exec agent.
// In dev mode (go run / local build), it cross-compiles for linux.
// In release mode (installed via mise/gh), it downloads the matching linux binary.
// Returns the remote path to the deployed binary.
func deployBinary(sshClient *ssh.Client, codespaceName string) (string, error) {
	// Detect codespace architecture
	arch, err := detectCodespaceArch(codespaceName)
	if err != nil {
		return "", fmt.Errorf("detecting codespace arch: %w", err)
	}

	remotePath := remoteBinaryDir + "/gh-copilot-codespace"

	// Check if binary already exists on codespace and is current
	localBin, _ := os.Executable()
	localInfo, err := os.Stat(localBin)
	if err != nil {
		return "", fmt.Errorf("stat local binary: %w", err)
	}

	// Quick check: if remote binary exists and has the same size, skip deploy
	sizeCheck := fmt.Sprintf("stat -c %%s %s 2>/dev/null || echo 0", remotePath)
	out, _ := sshCommand(codespaceName, sizeCheck)
	remoteSize := strings.TrimSpace(out)
	if remoteSize == fmt.Sprintf("%d", localInfo.Size()) && runtime.GOOS == "linux" && runtime.GOARCH == arch {
		// Same binary, skip deploy
		return remotePath, nil
	}

	fmt.Fprintln(os.Stderr, "Deploying exec agent to codespace...")

	// Get a linux binary for the codespace
	linuxBinary, cleanup, err := getLinuxBinary(arch)
	if err != nil {
		return "", fmt.Errorf("getting linux binary: %w", err)
	}
	if cleanup != nil {
		defer cleanup()
	}

	// Transfer via base64 over SSH (gh codespace cp uses SCP which is unreliable)
	binData, err := os.ReadFile(linuxBinary)
	if err != nil {
		return "", fmt.Errorf("reading binary: %w", err)
	}

	encoded := base64.StdEncoding.EncodeToString(binData)
	installCmd := fmt.Sprintf("mkdir -p %s && base64 -d > %s && chmod +x %s",
		remoteBinaryDir, remotePath, remotePath)

	cmd := exec.Command("gh", "codespace", "ssh", "-c", codespaceName, "--", installCmd)
	cmd.Stdin = strings.NewReader(encoded)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("copying binary to codespace: %w: %s", err, out)
	}

	fmt.Fprintf(os.Stderr, "  ✓ Deployed exec agent (%s)\n", arch)
	return remotePath, nil
}

// detectCodespaceArch returns the codespace's CPU architecture (amd64 or arm64).
func detectCodespaceArch(codespaceName string) (string, error) {
	out, err := sshCommand(codespaceName, "uname -m")
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

// getLinuxBinary returns a path to a linux binary for the given arch.
// Returns the path and an optional cleanup function.
func getLinuxBinary(arch string) (string, func(), error) {
	// If we're already on linux with matching arch, use ourselves
	if runtime.GOOS == "linux" && runtime.GOARCH == arch {
		self, err := os.Executable()
		if err != nil {
			return "", nil, err
		}
		return self, nil, nil
	}

	// Try cross-compile first (dev mode — Go installed)
	if path, cleanup, err := crossCompile(arch); err == nil {
		return path, cleanup, nil
	}

	// Fall back to downloading from release
	return downloadReleaseBinary(arch)
}

// crossCompile builds a linux binary for the given arch.
func crossCompile(arch string) (string, func(), error) {
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
	cmd := exec.Command(goPath, "build", "-ldflags=-s -w", "-o", outPath, "./cmd/gh-copilot-codespace")
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

// downloadReleaseBinary downloads the linux binary from the latest GitHub release.
func downloadReleaseBinary(arch string) (string, func(), error) {
	tmpDir, err := os.MkdirTemp("", "gh-copilot-codespace-download-*")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { os.RemoveAll(tmpDir) }

	pattern := fmt.Sprintf("gh-copilot-codespace-linux-%s", arch)
	outPath := filepath.Join(tmpDir, "gh-copilot-codespace")

	cmd := exec.Command("gh", "release", "download",
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
