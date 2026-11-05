package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestExtensionHostStartupRepairCancellation(t *testing.T) {
	artifact := buildLinuxHelperFixture(t, "linux", "amd64")
	home := t.TempDir()
	t.Setenv("HOME", home)
	bin := t.TempDir()
	gh := `#!/bin/sh
printf 'Host cs.test\n  HostName example.com\n'
`
	ssh := `#!/bin/sh
for last; do :; done
case "$last" in
  *"base64 -d"*) printf '%s\n' "$$" > "$REPAIR_STARTED"; exec sleep 60 ;;
  *"sha256sum"*) printf '%064d\n' 1 ;;
  *"uname -m"*) printf 'x86_64\n' ;;
  *'printf '*'"$HOME"'*) printf '/home/codespace\n' ;;
esac
`
	for name, content := range map[string]string{"gh": gh, "ssh": ssh} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(content), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	started := filepath.Join(home, "repair-started")
	t.Setenv("REPAIR_STARTED", started)
	t.Setenv(codespaceLinuxHelperEnv, artifact)
	t.Setenv(codespaceRemoteNativeEnv, "1")
	t.Setenv(daemonDisabledEnv, "1")
	t.Setenv(codespaceLifecycleConfigEnv, "")
	data, err := json.Marshal([]registryEntry{{
		Alias: "test", Name: "test", Workdir: "/workspaces/repo",
		HelperPath: "/home/codespace/" + strings.Repeat("0", 64) + "/gh-copilot-codespace",
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODESPACE_REGISTRY", string(data))
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		err := runExtensionHostIO(ctx, strings.NewReader(""), io.Discard)
		close(finished)
		result <- err
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
			return
		default:
		}
		if data, err := os.ReadFile(started); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				if proc, err := os.FindProcess(pid); err == nil {
					_ = proc.Kill()
				}
			}
		}
	})
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.After(5 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		select {
		case err := <-result:
			t.Fatalf("host exited before starting repair: %v", err)
		case <-deadline:
			t.Fatal("startup never reached helper repair")
		case <-ticker.C:
		}
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("startup cancellation error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("startup repair ignored cancellation")
	}
}

// TestExtensionHostSubprocess_ConcurrentListCodespaces proves that a fast
// list_codespaces returns before a slow list_available_codespaces by running
// the real extension-host protocol against a fake blocking gh command.
func TestExtensionHostSubprocess_ConcurrentListCodespaces(t *testing.T) {
	if os.Getenv("EXTENSION_HOST_SUBPROCESS") == "1" {
		// We are the child process — run the extension host.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if err := runExtensionHostIO(ctx, os.Stdin, os.Stdout); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
		return
	}

	// Create a fake gh script that blocks only for "codespace list" (used by
	// list_available_codespaces), but responds immediately to other commands.
	fakeGHDir := t.TempDir()
	fakeGH := filepath.Join(fakeGHDir, "gh")
	script := `#!/bin/sh
case "$*" in
  *"codespace list"*)
    exec sleep 5
    ;;
  *)
    echo '[]'
    ;;
esac
`
	if err := os.WriteFile(fakeGH, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	// Start the test binary as a subprocess with the fake gh in PATH.
	cmd := exec.Command(os.Args[0], "-test.run=TestExtensionHostSubprocess_ConcurrentListCodespaces")
	cmd.Env = append(os.Environ(),
		"EXTENSION_HOST_SUBPROCESS=1",
		"CODESPACE_REGISTRY=[]",
		codespaceLifecycleConfigEnv+"=",
		codespaceLocalWorkdirEnv+"=",
		"PATH="+fakeGHDir+":"+os.Getenv("PATH"),
	)
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	enc := json.NewEncoder(stdinPipe)
	dec := json.NewDecoder(stdoutPipe)

	// Send slow request first (list_available_codespaces calls fake gh which sleeps).
	enc.Encode(extensionHostRequest{ID: float64(1), Method: "call_tool", Tool: "list_available_codespaces", Args: map[string]any{}})
	// Give the first request time to be dispatched.
	time.Sleep(100 * time.Millisecond)
	// Send fast request (list_codespaces reads in-memory registry — instant).
	enc.Encode(extensionHostRequest{ID: float64(2), Method: "call_tool", Tool: "list_codespaces", Args: map[string]any{}})

	// The fast response (id:2) should arrive first within 2 seconds.
	type result struct {
		resp extensionHostResponse
		err  error
	}
	ch := make(chan result, 2)
	go func() {
		for i := 0; i < 2; i++ {
			var resp extensionHostResponse
			err := dec.Decode(&resp)
			ch <- result{resp: resp, err: err}
		}
	}()

	// First response should be id:2 (fast).
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("first response decode error: %v", r.err)
		}
		if r.resp.ID != float64(2) {
			t.Fatalf("first response ID = %v, want 2 (list_codespaces should complete first)", r.resp.ID)
		}
		if r.resp.Error != nil {
			t.Fatalf("first response error: %v", r.resp.Error)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for fast list_codespaces response")
	}

	// Close stdin to trigger shutdown — this cancels the slow tool.
	stdinPipe.Close()

	// The slow response (id:1) may or may not arrive — it's acceptable for it
	// to be dropped during shutdown since sendResponse selects on hostCtx.Done.
	// What matters is that the child exits promptly.

	// Child must exit promptly after stdin close.
	exitCh := make(chan error, 1)
	go func() { exitCh <- cmd.Wait() }()
	select {
	case err := <-exitCh:
		if err != nil {
			t.Logf("child exit: %v (acceptable for test subprocess)", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("child process did not exit within 3s after stdin close")
	}
}
