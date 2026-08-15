//go:build integration

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ekroon/gh-copilot-codespace/internal/daemonclient"
	"github.com/ekroon/gh-copilot-codespace/internal/daemontransport"
	"github.com/ekroon/gh-copilot-codespace/internal/mcp"
	"github.com/ekroon/gh-copilot-codespace/internal/registry"
	"github.com/ekroon/gh-copilot-codespace/internal/ssh"
)

// dialDaemon brings up a full SSH-backed DaemonExecutor against the test
// codespace. Caller is responsible for nothing — t.Cleanup closes the
// executor (which closes the stream and SSHTransport).
func dialDaemon(t *testing.T, cs string) *daemonclient.Executor {
	t.Helper()
	client := testSSHClient(t, cs)
	transport := daemontransport.NewSSHTransport(client, cs, func(ctx context.Context, c *ssh.Client, name string) (string, error) {
		return deployBinary(ctx, c, name)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	exec, err := daemonclient.Dial(ctx, transport)
	if err != nil {
		t.Fatalf("daemonclient.Dial: %v", err)
	}
	t.Cleanup(func() { _ = exec.Close() })
	return exec
}

func TestIntegration_DaemonDialAndPing(t *testing.T) {
	cs := testCodespace(t)
	exec := dialDaemon(t, cs)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := exec.Ping(ctx)
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if !result.Pong || result.PID <= 0 {
		t.Fatalf("Ping result = %+v, want Pong=true PID>0", result)
	}
}

func TestIntegration_DaemonRecoversAfterCodespaceShutdown(t *testing.T) {
	if os.Getenv("TEST_CODESPACE_RECOVERY") != "1" {
		t.Skip("TEST_CODESPACE_RECOVERY=1 not set")
	}
	cs := testCodespace(t)
	daemonExec := dialDaemon(t, cs)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	_, _, code, err := daemonExec.RunBash(ctx, "echo recovery-prime", "/tmp")
	cancel()
	if err != nil || code != 0 {
		t.Fatalf("prime RunBash: err=%v code=%d", err, code)
	}
	lastActivity := time.Now()

	t.Cleanup(func() {
		wakeCtx, wakeCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer wakeCancel()
		_ = exec.CommandContext(wakeCtx, "gh", "codespace", "ssh", "-c", cs, "--", "true").Run()
	})

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	output, err := exec.CommandContext(stopCtx, "gh", "codespace", "stop", "-c", cs).CombinedOutput()
	stopCancel()
	if err != nil {
		t.Fatalf("gh codespace stop: %v\n%s", err, output)
	}
	waitForCodespaceState(t, cs, "Shutdown", 2*time.Minute)
	if remaining := 6*time.Second - time.Since(lastActivity); remaining > 0 {
		time.Sleep(remaining)
	}

	recoverCtx, recoverCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer recoverCancel()
	stdout, stderr, code, err := daemonExec.RunBash(recoverCtx, "echo recovered-after-shutdown", "/tmp")
	if err != nil {
		t.Fatalf("RunBash after shutdown: %v", err)
	}
	if code != 0 || !strings.Contains(stdout, "recovered-after-shutdown") {
		t.Fatalf("RunBash after shutdown: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	waitForCodespaceState(t, cs, "Available", 2*time.Minute)
}

func waitForCodespaceState(t *testing.T, name, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		output, err := exec.CommandContext(ctx, "gh", "codespace", "list", "--json", "name,state").Output()
		cancel()
		if err == nil {
			var entries []struct {
				Name  string `json:"name"`
				State string `json:"state"`
			}
			if json.Unmarshal(output, &entries) == nil {
				for _, entry := range entries {
					if entry.Name == name && entry.State == want {
						return
					}
				}
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("codespace %q did not reach state %q within %s", name, want, timeout)
}

func TestIntegration_DaemonRunBashEcho(t *testing.T) {
	cs := testCodespace(t)
	exec := dialDaemon(t, cs)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	stdout, _, code, err := exec.RunBash(ctx, "echo hello-from-daemon", "/tmp")
	if err != nil {
		t.Fatalf("RunBash: %v", err)
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout, "hello-from-daemon") {
		t.Fatalf("stdout = %q, want it to contain 'hello-from-daemon'", stdout)
	}
}

func TestIntegration_DaemonRunBashExitCodePassthrough(t *testing.T) {
	cs := testCodespace(t)
	exec := dialDaemon(t, cs)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, _, code, err := exec.RunBash(ctx, "exit 42", "/tmp")
	if err != nil {
		t.Fatalf("RunBash returned err for non-zero exit (should be nil): %v", err)
	}
	if code != 42 {
		t.Fatalf("exit code = %d, want 42", code)
	}
}

func TestIntegration_DaemonRunBashEnvSecretsLoaded(t *testing.T) {
	cs := testCodespace(t)
	exec := dialDaemon(t, cs)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// CODESPACE_NAME is always set inside a codespace; verifies the per-request
	// env bootstrap actually fired.
	stdout, _, code, err := exec.RunBash(ctx, "echo $CODESPACE_NAME", "/tmp")
	if err != nil {
		t.Fatalf("RunBash: %v", err)
	}
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(stdout, cs) {
		t.Fatalf("CODESPACE_NAME in daemon env = %q, want %q", stdout, cs)
	}
}

func TestIntegration_DaemonCreateEditViewRoundTrip(t *testing.T) {
	cs := testCodespace(t)
	exec := dialDaemon(t, cs)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	path := fmt.Sprintf("/tmp/daemon-it-%d.txt", time.Now().UnixNano())
	defer exec.RunBash(context.Background(), fmt.Sprintf("rm -f %s", path), "/tmp")

	if err := exec.CreateFile(ctx, path, "alpha\nbeta\ngamma\n"); err != nil {
		t.Fatalf("CreateFile: %v", err)
	}

	if err := exec.EditFile(ctx, path, "beta", "BETA"); err != nil {
		t.Fatalf("EditFile: %v", err)
	}

	view, err := exec.ViewFile(ctx, path, nil)
	if err != nil {
		t.Fatalf("ViewFile: %v", err)
	}
	if !strings.Contains(view, "BETA") || strings.Contains(view, "beta\n") {
		t.Fatalf("ViewFile result = %q, want BETA present and beta gone", view)
	}
}

func TestIntegration_DaemonContextCancelKillsRemoteProcess(t *testing.T) {
	cs := testCodespace(t)
	exec := dialDaemon(t, cs)

	// Use a sentinel file so we can verify the remote process actually died.
	sentinel := fmt.Sprintf("/tmp/daemon-cancel-%d.txt", time.Now().UnixNano())
	defer exec.RunBash(context.Background(), fmt.Sprintf("rm -f %s %s.done", sentinel, sentinel), "/tmp")

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, _, _, err := exec.RunBash(
		ctx,
		fmt.Sprintf(`sh -c 'echo started > %s; sleep 30; echo done > %s.done'`, sentinel, sentinel),
		"/tmp",
	)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("RunBash returned nil err on canceled context, want error")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("RunBash returned after %v, want < 5s (cancel not propagated)", elapsed)
	}

	// Give SIGTERM a moment to land and the sleep to die.
	time.Sleep(1500 * time.Millisecond)

	checkCtx, checkCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer checkCancel()
	stdout, _, code, err := exec.RunBash(checkCtx, fmt.Sprintf("test -f %s.done && echo SURVIVED || echo KILLED", sentinel), "/tmp")
	if err != nil || code != 0 {
		t.Fatalf("sentinel check failed: err=%v code=%d", err, code)
	}
	if !strings.Contains(stdout, "KILLED") {
		t.Fatalf("remote sleep survived cancel: stdout=%q (sentinel %s.done exists)", stdout, sentinel)
	}
}

func TestIntegration_DaemonConcurrentCalls(t *testing.T) {
	cs := testCodespace(t)
	exec := dialDaemon(t, cs)

	const N = 12
	var wg sync.WaitGroup
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			stdout, _, code, err := exec.RunBash(ctx, fmt.Sprintf("echo concurrent-%d", i), "/tmp")
			if err != nil {
				errs <- fmt.Errorf("call %d: %w", i, err)
				return
			}
			if code != 0 {
				errs <- fmt.Errorf("call %d: exit=%d", i, code)
				return
			}
			want := fmt.Sprintf("concurrent-%d", i)
			if !strings.Contains(stdout, want) {
				errs <- fmt.Errorf("call %d: stdout=%q does not contain %q", i, stdout, want)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

func TestIntegration_DaemonSessionLifecycle(t *testing.T) {
	cs := testCodespace(t)
	exec := dialDaemon(t, cs)

	sessionID := fmt.Sprintf("it-session-%d", time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := exec.StartSession(ctx, sessionID, "bash", "/tmp"); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		_ = exec.StopSession(stopCtx, sessionID)
	})

	if err := exec.WriteSession(ctx, sessionID, "echo SESSION_MARK_42{enter}"); err != nil {
		t.Fatalf("WriteSession: %v", err)
	}

	deadline := time.Now().Add(15 * time.Second)
	var lastOut string
	for time.Now().Before(deadline) {
		out, err := exec.ReadSession(ctx, sessionID)
		if err != nil {
			t.Fatalf("ReadSession: %v", err)
		}
		lastOut = out
		if strings.Contains(out, "SESSION_MARK_42") {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if !strings.Contains(lastOut, "SESSION_MARK_42") {
		t.Fatalf("session output never contained SESSION_MARK_42; last read = %q", lastOut)
	}

	list, err := exec.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if !strings.Contains(list, sessionID) {
		t.Fatalf("ListSessions output %q missing %q", list, sessionID)
	}
}

// TestIntegration_DaemonReconnectsAfterRemoteDaemonDeath kills only the helper
// daemon process this test spawned — the codespace itself is untouched — and
// checks that the interrupted call is reported instead of replayed while later
// calls run on a fresh daemon process.
func TestIntegration_DaemonReconnectsAfterRemoteDaemonDeath(t *testing.T) {
	cs := testCodespace(t)
	exec := dialDaemon(t, cs)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ping, err := exec.Ping(ctx)
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	pid := ping.PID
	marker := fmt.Sprintf("/tmp/it-daemon-death-%d.txt", time.Now().UnixNano())
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_, _, _, _ = exec.RunBash(cleanupCtx, fmt.Sprintf("rm -f %s", marker), "/tmp")
	})

	command := fmt.Sprintf("printf 'run\\n' >> %s; kill -9 %d", marker, pid)
	if _, _, _, err := exec.RunBash(ctx, command, "/tmp"); err == nil {
		t.Fatal("RunBash succeeded, want connection loss after the daemon was killed")
	} else {
		var lost *daemonclient.ConnectionLostError
		if !errors.As(err, &lost) {
			t.Fatalf("RunBash error = %v (%T), want *daemonclient.ConnectionLostError", err, err)
		}
		if !lost.OutcomeUnknown {
			t.Fatalf("OutcomeUnknown = false, want true for a request that reached the daemon: %v", lost)
		}
		if !lost.Reconnected {
			t.Fatalf("Reconnected = false, want a restored connection: %v", lost)
		}
		if lost.NewGeneration == lost.OldGeneration {
			t.Fatalf("generations = %d -> %d, want a new generation", lost.OldGeneration, lost.NewGeneration)
		}
		if lost.Cause == nil {
			t.Fatal("Cause = nil, want the terminal cause of the lost connection")
		}
		t.Logf("connection loss cause: %v", lost.Cause)
	}

	after, err := exec.Ping(ctx)
	if err != nil {
		t.Fatalf("Ping after reconnect: %v", err)
	}
	if after.PID == pid {
		t.Fatalf("daemon pid = %d after reconnect, want a new process", after.PID)
	}

	stdout, _, exitCode, err := exec.RunBash(ctx, fmt.Sprintf("cat %s", marker), "/tmp")
	if err != nil {
		t.Fatalf("RunBash after reconnect: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("cat %s exit code = %d, want 0", marker, exitCode)
	}
	if stdout != "run\n" {
		t.Fatalf("marker content = %q, want exactly one recorded run", stdout)
	}
}

// TestIntegration_DaemonDeathCleansUpProcessSessions verifies that a new daemon
// removes stale cgroups left by a SIGKILLed daemon and terminates their process.
func TestIntegration_DaemonDeathCleansUpProcessSessions(t *testing.T) {
	cs := testCodespace(t)
	exec := dialDaemon(t, cs)
	if !exec.SupportsProcessSessions() {
		t.Skip("daemon does not advertise daemon-managed process sessions")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ping, err := exec.Ping(ctx)
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}

	sessionID := fmt.Sprintf("it-lost-session-%d", time.Now().UnixNano())
	pidPath := fmt.Sprintf("/tmp/%s.pid", sessionID)
	if err := exec.StartProcessSession(ctx, sessionID, fmt.Sprintf("printf %%s $$ > %s; exec sleep 120", pidPath), "/tmp"); err != nil {
		t.Fatalf("StartProcessSession: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		_, _, _, _ = exec.RunBash(cleanupCtx, fmt.Sprintf("rm -f %s", pidPath), "/tmp")
	})

	var processPID int
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		stdout, _, exitCode, runErr := exec.RunBash(ctx, fmt.Sprintf("cat %s 2>/dev/null", pidPath), "/tmp")
		if runErr == nil && exitCode == 0 {
			processPID, runErr = strconv.Atoi(strings.TrimSpace(stdout))
			if runErr != nil {
				t.Fatalf("parse process pid %q: %v", stdout, runErr)
			}
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if processPID <= 0 {
		t.Fatal("process session did not publish its pid")
	}

	if _, _, _, err := exec.RunBash(ctx, fmt.Sprintf("kill -9 %d", ping.PID), "/tmp"); err == nil {
		t.Fatal("RunBash succeeded, want connection loss after the daemon was killed")
	}

	if _, err := exec.ReadSession(ctx, sessionID); err == nil {
		t.Fatal("ReadSession succeeded after daemon death, want the retained session to be gone")
	}

	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_, _, exitCode, runErr := exec.RunBash(ctx, fmt.Sprintf("kill -0 %d 2>/dev/null", processPID), "/tmp")
		if runErr != nil {
			t.Fatalf("check process %d: %v", processPID, runErr)
		}
		if exitCode != 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("process %d survived daemon SIGKILL and reconnect cleanup", processPID)
}

func TestIntegration_DaemonProcessSessionDoesNotShadowTmuxAfterReconnect(t *testing.T) {
	cs := testCodespace(t)
	sessionID := fmt.Sprintf("it-reconnect-%d", time.Now().UnixNano())

	first := dialDaemon(t, cs)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := first.StartSession(ctx, sessionID, "sleep 30", "/tmp"); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close first daemon: %v", err)
	}

	second := dialDaemon(t, cs)
	t.Cleanup(func() { _ = second.StopSession(context.Background(), sessionID) })
	if !second.SupportsProcessSessions() {
		t.Fatal("second daemon does not advertise process sessions")
	}
	if err := second.StartProcessSession(ctx, sessionID, "printf shadowed", "/tmp"); err == nil {
		t.Fatal("StartProcessSession with retained tmux ID error = nil")
	}
}

func TestIntegration_DaemonViewFileLineNumbers(t *testing.T) {
	cs := testCodespace(t)
	exec := dialDaemon(t, cs)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	path := fmt.Sprintf("/tmp/daemon-view-%d.txt", time.Now().UnixNano())
	defer exec.RunBash(context.Background(), fmt.Sprintf("rm -f %s", path), "/tmp")

	if err := exec.CreateFile(ctx, path, "one\ntwo\nthree\nfour\nfive\n"); err != nil {
		t.Fatalf("CreateFile: %v", err)
	}

	view, err := exec.ViewFile(ctx, path, []int{2, 4})
	if err != nil {
		t.Fatalf("ViewFile: %v", err)
	}
	if !strings.Contains(view, "2. two") || !strings.Contains(view, "4. four") {
		t.Fatalf("view range output missing expected lines: %q", view)
	}
	if strings.Contains(view, "1. one") || strings.Contains(view, "5. five") {
		t.Fatalf("view range output should not contain lines outside [2,4]: %q", view)
	}
}

func TestIntegration_WrapExecutorsWithDaemonSwapsExecutor(t *testing.T) {
	cs := testCodespace(t)
	t.Setenv("COPILOT_CODESPACE_NO_DAEMON", "")

	client := testSSHClient(t, cs)
	reg := registry.New()
	if err := reg.Register(&registry.ManagedCodespace{
		Alias:      "it",
		Name:       cs,
		Repository: "test/repo",
		Workdir:    "/tmp",
		Executor:   client,
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	closers := wrapExecutorsWithDaemon(ctx, reg)
	t.Cleanup(func() {
		for _, c := range closers {
			c()
		}
	})
	if len(closers) != 1 {
		t.Fatalf("closers len = %d, want 1 (daemon dial should have succeeded)", len(closers))
	}

	got := reg.All()[0].Executor
	if _, ok := got.(*daemonclient.Executor); !ok {
		t.Fatalf("Executor type = %T, want *daemonclient.Executor", got)
	}

	// Smoke-test that the swapped executor actually works through the registry.
	stdout, _, code, err := got.RunBash(ctx, "echo wired-up", "/tmp")
	if err != nil || code != 0 {
		t.Fatalf("swapped executor RunBash: err=%v code=%d", err, code)
	}
	if !strings.Contains(stdout, "wired-up") {
		t.Fatalf("swapped executor stdout = %q, want 'wired-up'", stdout)
	}
}

func TestIntegration_WrapExecutorsRespectsOptOut(t *testing.T) {
	cs := testCodespace(t)
	t.Setenv("COPILOT_CODESPACE_NO_DAEMON", "1")

	client := testSSHClient(t, cs)
	reg := registry.New()
	if err := reg.Register(&registry.ManagedCodespace{
		Alias:    "it",
		Name:     cs,
		Workdir:  "/tmp",
		Executor: client,
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	closers := wrapExecutorsWithDaemon(context.Background(), reg)
	if len(closers) != 0 {
		t.Fatalf("opt-out should produce no closers, got %d", len(closers))
	}
	got := reg.All()[0].Executor
	if _, ok := got.(*ssh.Client); !ok {
		t.Fatalf("Executor type after opt-out = %T, want *ssh.Client", got)
	}
}

// silence unused-import warnings when running with build tag stripped checks.
var _ = filepath.Join

// --- end-to-end extension-host subprocess tests -----------------------------

// buildExtensionHostBinary compiles the project binary to a tempfile and
// returns its path. The binary is reused across tests via a package-level
// cache to keep the integration suite responsive.
var (
	builtExtensionBinaryMu sync.Mutex
	builtExtensionBinary   string
)

func buildExtensionHostBinary(t *testing.T) string {
	t.Helper()
	builtExtensionBinaryMu.Lock()
	defer builtExtensionBinaryMu.Unlock()
	if builtExtensionBinary != "" {
		if _, err := os.Stat(builtExtensionBinary); err == nil {
			return builtExtensionBinary
		}
	}

	dir, err := os.MkdirTemp("", "ghcs-extension-bin-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	bin := filepath.Join(dir, "gh-copilot-codespace")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/gh-copilot-codespace")
	// Resolve project root from the test's working directory.
	cmd.Dir = projectRoot(t)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("go build extension-host binary: %v", err)
	}
	builtExtensionBinary = bin
	return bin
}

func projectRoot(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	// Tests run inside cmd/gh-copilot-codespace; the module root is two levels up.
	return filepath.Clean(filepath.Join(cwd, "..", ".."))
}

// extensionHostRPC is a thin client over the extension-host stdio JSON.
type extensionHostRPC struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	enc    *json.Encoder
	dec    *json.Decoder
	nextID int
}

type rpcRequest struct {
	ID     int            `json:"id"`
	Method string         `json:"method"`
	Tool   string         `json:"tool,omitempty"`
	Args   map[string]any `json:"args,omitempty"`
}

type rpcResponse struct {
	ID     int             `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  any             `json:"error,omitempty"`
}

func startExtensionHost(t *testing.T, cs, workdir string) *extensionHostRPC {
	return startExtensionHostWithLifecycleConfig(t, cs, workdir, "")
}

func startExtensionHostWithLifecycleConfig(t *testing.T, cs, workdir, lifecycleConfig string) *extensionHostRPC {
	t.Helper()
	bin := buildExtensionHostBinary(t)

	// Use the SAME CODESPACE_REGISTRY shape the wrapper extension would emit.
	registryEntries := []map[string]string{{
		"alias":   "it",
		"name":    cs,
		"workdir": workdir,
	}}
	regJSON, err := json.Marshal(registryEntries)
	if err != nil {
		t.Fatalf("marshal registry: %v", err)
	}

	cmd := exec.Command(bin, "extension-host")
	cmd.Env = append(os.Environ(),
		"CODESPACE_REGISTRY="+string(regJSON),
		codespaceLifecycleConfigEnv+"="+lifecycleConfig,
		"COPILOT_CODESPACE_EXTENSION_MODE=mirror",
		// Make sure no opt-out is inherited from the shell.
		"COPILOT_CODESPACE_NO_DAEMON=",
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start extension-host: %v", err)
	}

	rpc := &extensionHostRPC{
		cmd:    cmd,
		stdin:  stdin,
		stdout: bufio.NewReader(stdout),
		enc:    json.NewEncoder(stdin),
	}
	rpc.dec = json.NewDecoder(rpc.stdout)

	t.Cleanup(func() {
		_ = stdin.Close()
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-done
			t.Errorf("extension-host did not exit after stdin close")
		}
	})
	return rpc
}

func (r *extensionHostRPC) call(t *testing.T, method, tool string, args map[string]any) rpcResponse {
	t.Helper()
	r.nextID++
	req := rpcRequest{ID: r.nextID, Method: method, Tool: tool, Args: args}
	if err := r.enc.Encode(req); err != nil {
		t.Fatalf("encode %s: %v", method, err)
	}
	var resp rpcResponse
	if err := r.dec.Decode(&resp); err != nil {
		t.Fatalf("decode %s response: %v", method, err)
	}
	if resp.ID != req.ID {
		t.Fatalf("response id = %d, want %d", resp.ID, req.ID)
	}
	return resp
}

// TestIntegration_ExtensionHostEndToEnd spawns the binary in extension-host
// mode against the real test codespace, drives the stdio JSON protocol the way
// the JS extension would, and verifies that:
//   - list_tools returns the remote_* tools plus the SDK-delivered preamble
//   - remote_bash actually round-trips through the daemon over SSH
//   - the host shuts down cleanly when stdin closes
//
// This is the only test that exercises the full extension-tools path —
// subprocess boundary, env-driven registry hydration, daemon wiring inside
// runExtensionHostIO, and the tool runtime dispatch — in one shot.
func TestIntegration_ExtensionHostEndToEnd(t *testing.T) {
	cs := testCodespace(t)
	rpc := startExtensionHost(t, cs, "/tmp")

	// 1. list_tools — must include remote_bash and a non-empty preamble.
	resp := rpc.call(t, "list_tools", "", nil)
	if resp.Error != nil {
		t.Fatalf("list_tools error: %v", resp.Error)
	}
	var bootstrap struct {
		Tools         []struct{ Name string }
		SystemMessage *struct {
			Mode    string
			Content string
		} `json:"systemMessage"`
		CustomAgents []struct {
			Name    string
			Prompt  string
			Tools   []string
			Model   string
			Display string `json:"displayName"`
		} `json:"customAgents"`
	}
	if err := json.Unmarshal(resp.Result, &bootstrap); err != nil {
		t.Fatalf("decode bootstrap: %v", err)
	}
	if len(bootstrap.Tools) == 0 {
		t.Fatal("list_tools returned 0 tools")
	}
	haveTool := func(name string) bool {
		for _, tool := range bootstrap.Tools {
			if tool.Name == name {
				return true
			}
		}
		return false
	}
	for _, want := range []string{
		"remote_bash",
		"remote_write_bash",
		"remote_read_bash",
		"remote_stop_bash",
		"remote_list_bash",
		"remote_cwd",
		"list_codespaces",
	} {
		if !haveTool(want) {
			t.Fatalf("%s missing from tools; got %d tools", want, len(bootstrap.Tools))
		}
	}
	if bootstrap.SystemMessage == nil {
		t.Fatal("list_tools returned no systemMessage (preamble should be SDK-delivered in extension-tools mode)")
	}
	if bootstrap.SystemMessage.Mode != "append" {
		t.Fatalf("systemMessage.mode = %q, want append", bootstrap.SystemMessage.Mode)
	}
	if len(bootstrap.SystemMessage.Content) < 50 {
		t.Fatalf("systemMessage.content suspiciously short: %q", bootstrap.SystemMessage.Content)
	}
	if !strings.Contains(strings.ToLower(bootstrap.SystemMessage.Content), "codespace") {
		t.Fatalf("preamble does not mention codespace; got: %s", bootstrap.SystemMessage.Content)
	}
	// remote-explorer agent should be advertised because reg has 1 codespace.
	if len(bootstrap.CustomAgents) == 0 {
		t.Errorf("expected at least one custom agent (remote-explorer) when codespaces are connected")
	} else {
		agent := bootstrap.CustomAgents[0]
		if agent.Name != remoteExplorerAgentName {
			t.Fatalf("custom agent name = %q, want %q", agent.Name, remoteExplorerAgentName)
		}
		if !strings.Contains(agent.Prompt, "remote_bash") {
			t.Fatalf("custom agent prompt missing remote_bash guidance: %q", agent.Prompt)
		}
		for _, want := range []string{
			"remote_bash",
			"remote_write_bash",
			"remote_read_bash",
			"remote_stop_bash",
			"remote_list_bash",
			"remote_cwd",
			"list_codespaces",
		} {
			if !slicesContain(agent.Tools, want) {
				t.Fatalf("custom agent tools missing %q: %v", want, agent.Tools)
			}
		}
		for _, forbidden := range []string{
			"remote_cd",
			"remote_edit",
			"remote_create",
			"remote_copy",
			"remote_apply_patch",
			"list_available_codespaces",
			"get_codespace_options",
			"create_codespace",
			"connect_codespace",
			"delete_codespace",
			"open_shell",
		} {
			if slicesContain(agent.Tools, forbidden) {
				t.Fatalf("custom agent tools unexpectedly include %q: %v", forbidden, agent.Tools)
			}
		}
	}

	// 2. remote_bash through the daemon — verify round-trip with a unique sentinel.
	sentinel := fmt.Sprintf("e2e-extension-host-%d", time.Now().UnixNano())
	resp = rpc.call(t, "call_tool", "remote_bash", map[string]any{
		"codespace":    "it",
		"command":      fmt.Sprintf("printf %%s %s", sentinel),
		"initial_wait": 5,
	})
	if resp.Error != nil {
		t.Fatalf("remote_bash error: %v", resp.Error)
	}
	var bashResult struct {
		TextResultForLlm string `json:"textResultForLlm"`
		ResultType       string `json:"resultType"`
	}
	if err := json.Unmarshal(resp.Result, &bashResult); err != nil {
		t.Fatalf("decode remote_bash result: %v", err)
	}
	if bashResult.ResultType != "success" {
		t.Fatalf("remote_bash result_type = %q, want success; full result: %s",
			bashResult.ResultType, bashResult.TextResultForLlm)
	}
	if !strings.Contains(bashResult.TextResultForLlm, sentinel) {
		t.Fatalf("remote_bash output missing sentinel %q; got: %s", sentinel, bashResult.TextResultForLlm)
	}

	// 3. A second call on the same host to prove the persistent daemon stream
	//    actually persists across calls (a per-call reconnect would still work
	//    but the daemon connection is supposed to be long-lived).
	resp = rpc.call(t, "call_tool", "remote_bash", map[string]any{
		"codespace":    "it",
		"command":      "printf second-call",
		"initial_wait": 5,
	})
	if resp.Error != nil {
		t.Fatalf("second remote_bash error: %v", resp.Error)
	}
	if err := json.Unmarshal(resp.Result, &bashResult); err != nil {
		t.Fatalf("decode second remote_bash result: %v", err)
	}
	if !strings.Contains(bashResult.TextResultForLlm, "second-call") {
		t.Fatalf("second remote_bash output unexpected: %s", bashResult.TextResultForLlm)
	}

	// 4. Unknown method must produce an error response, not crash the host.
	resp = rpc.call(t, "wat", "", nil)
	if resp.Error == nil {
		t.Fatal("unknown method should produce an error response")
	}
}

func TestIntegration_ExtensionHostSelectedOnlyKeepsProvisionedCodespace(t *testing.T) {
	cs := testCodespace(t)
	lifecycleConfig := lifecycleConfigEnvJSON(mcp.LifecycleConfig{
		AccessPolicy: mcp.CodespaceAccessPolicy{
			SelectedOnly:          true,
			AllowedCodespaceNames: []string{cs},
		},
	})
	rpc := startExtensionHostWithLifecycleConfig(t, cs, "/tmp", lifecycleConfig)

	resp := rpc.call(t, "list_tools", "", nil)
	if resp.Error != nil {
		t.Fatalf("list_tools error: %v", resp.Error)
	}
	var bootstrap struct {
		Tools []struct{ Name string }
	}
	if err := json.Unmarshal(resp.Result, &bootstrap); err != nil {
		t.Fatalf("decode bootstrap: %v", err)
	}
	haveTool := func(name string) bool {
		for _, tool := range bootstrap.Tools {
			if tool.Name == name {
				return true
			}
		}
		return false
	}
	for _, want := range []string{"remote_bash", "remote_view", "list_codespaces"} {
		if !haveTool(want) {
			t.Fatalf("selected-only tools missing %q", want)
		}
	}
	for _, forbidden := range []string{
		"list_available_codespaces",
		"get_codespace_options",
		"create_codespace",
		"connect_codespace",
		"delete_codespace",
	} {
		if haveTool(forbidden) {
			t.Fatalf("selected-only tools unexpectedly include %q", forbidden)
		}
	}

	resp = rpc.call(t, "call_tool", "delete_codespace", map[string]any{
		"codespace": "it",
	})
	if resp.Error == nil || !strings.Contains(fmt.Sprint(resp.Error), `unknown tool "delete_codespace"`) {
		t.Fatalf("delete_codespace error = %v, want unknown tool", resp.Error)
	}

	sentinel := fmt.Sprintf("selected-only-still-connected-%d", time.Now().UnixNano())
	resp = rpc.call(t, "call_tool", "remote_bash", map[string]any{
		"codespace":    "it",
		"command":      fmt.Sprintf("printf %%s %s", sentinel),
		"initial_wait": 5,
	})
	if resp.Error != nil {
		t.Fatalf("remote_bash after rejected delete error: %v", resp.Error)
	}
	var bashResult struct {
		TextResultForLlm string `json:"textResultForLlm"`
		ResultType       string `json:"resultType"`
	}
	if err := json.Unmarshal(resp.Result, &bashResult); err != nil {
		t.Fatalf("decode remote_bash result: %v", err)
	}
	if bashResult.ResultType != "success" || !strings.Contains(bashResult.TextResultForLlm, sentinel) {
		t.Fatalf("provisioned codespace unavailable after rejected delete: %+v", bashResult)
	}
}

func TestIntegration_ExtensionHostWarmRemoteBashUnder500Milliseconds(t *testing.T) {
	cs := testCodespace(t)
	rpc := startExtensionHost(t, cs, "/tmp")

	callBash := func(command string) (string, time.Duration) {
		t.Helper()
		start := time.Now()
		resp := rpc.call(t, "call_tool", "remote_bash", map[string]any{
			"codespace":    "it",
			"command":      command,
			"initial_wait": 5,
		})
		elapsed := time.Since(start)
		if resp.Error != nil {
			t.Fatalf("remote_bash error: %v", resp.Error)
		}
		var result struct {
			TextResultForLlm string `json:"textResultForLlm"`
			ResultType       string `json:"resultType"`
		}
		if err := json.Unmarshal(resp.Result, &result); err != nil {
			t.Fatalf("decode remote_bash result: %v", err)
		}
		if result.ResultType != "success" {
			t.Fatalf("remote_bash result_type = %q, want success; result: %s", result.ResultType, result.TextResultForLlm)
		}
		return result.TextResultForLlm, elapsed
	}

	callBash("true")

	for i := 0; i < 5; i++ {
		sentinel := fmt.Sprintf("warm-%d", i)
		output, elapsed := callBash("printf %s " + sentinel)
		t.Logf("warm remote_bash %d: %v", i+1, elapsed)
		if !strings.Contains(output, sentinel) {
			t.Fatalf("warm remote_bash output = %q, want %q", output, sentinel)
		}
		if elapsed > 500*time.Millisecond {
			t.Fatalf("warm remote_bash %d took %v, want <= 500ms", i+1, elapsed)
		}
	}
}

func TestIntegration_ExtensionHostRemoteBashReceivesGitHubAuthEnvironment(t *testing.T) {
	cs := testCodespace(t)
	rpc := startExtensionHost(t, cs, "/tmp")

	resp := rpc.call(t, "call_tool", "remote_bash", map[string]any{
		"codespace": "it",
		"command": `test -n "$GITHUB_TOKEN" &&
test "$GH_TOKEN" = "$GITHUB_TOKEN" &&
test -n "$GITHUB_SERVER_URL" &&
test -n "$(gh auth token)" &&
case ":$PATH:" in *":$HOME/.local/bin:"*) true;; *) false;; esac &&
case ":$PATH:" in *":$HOME/.local/share/mise/shims:"*) true;; *) false;; esac &&
printf auth-environment-ok`,
		"initial_wait": 5,
	})
	if resp.Error != nil {
		t.Fatalf("remote_bash error: %v", resp.Error)
	}
	var result struct {
		TextResultForLlm string `json:"textResultForLlm"`
		ResultType       string `json:"resultType"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("decode remote_bash result: %v", err)
	}
	if result.ResultType != "success" {
		t.Fatalf("remote_bash result_type = %q, want success; result: %s", result.ResultType, result.TextResultForLlm)
	}
	if !strings.Contains(result.TextResultForLlm, "auth-environment-ok") {
		t.Fatalf("remote_bash output = %q, want auth-environment-ok", result.TextResultForLlm)
	}
}

func TestIntegration_ExtensionHostRetainsTimedOutProcessSession(t *testing.T) {
	cs := testCodespace(t)
	rpc := startExtensionHost(t, cs, "/tmp")
	shellID := fmt.Sprintf("retained-%d", time.Now().UnixNano())

	resp := rpc.call(t, "call_tool", "remote_bash", map[string]any{
		"codespace":    "it",
		"command":      "printf started; sleep 0.3; printf finished",
		"shellId":      shellID,
		"initial_wait": 0.05,
	})
	if resp.Error != nil {
		t.Fatalf("remote_bash error: %v", resp.Error)
	}
	var result struct {
		TextResultForLlm string `json:"textResultForLlm"`
		ResultType       string `json:"resultType"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("decode remote_bash result: %v", err)
	}
	if result.ResultType != "success" || !strings.Contains(result.TextResultForLlm, shellID) {
		t.Fatalf("remote_bash did not retain session %q: %+v", shellID, result)
	}

	readStart := time.Now()
	resp = rpc.call(t, "call_tool", "remote_read_bash", map[string]any{
		"codespace": "it",
		"shellId":   shellID,
		"delay":     30,
	})
	readElapsed := time.Since(readStart)
	if resp.Error != nil {
		t.Fatalf("remote_read_bash error: %v", resp.Error)
	}
	if readElapsed >= 5*time.Second {
		t.Fatalf("remote_read_bash returned after %v, want completion before the 30s delay", readElapsed)
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("decode remote_read_bash result: %v", err)
	}
	if !strings.Contains(result.TextResultForLlm, "started") ||
		!strings.Contains(result.TextResultForLlm, "finished") ||
		!strings.Contains(result.TextResultForLlm, daemonSessionExitMarker) {
		t.Fatalf("remote_read_bash output = %q, want retained final output", result.TextResultForLlm)
	}

	resp = rpc.call(t, "call_tool", "remote_stop_bash", map[string]any{
		"codespace": "it",
		"shellId":   shellID,
	})
	if resp.Error != nil {
		t.Fatalf("remote_stop_bash error: %v", resp.Error)
	}
}

func TestIntegration_ExtensionHostCleansBackgroundProcessAfterShellExit(t *testing.T) {
	cs := testCodespace(t)
	rpc := startExtensionHost(t, cs, "/tmp")

	resp := rpc.call(t, "call_tool", "remote_bash", map[string]any{
		"codespace":    "it",
		"command":      "sleep 30 & printf %s $!",
		"initial_wait": 5,
	})
	if resp.Error != nil {
		t.Fatalf("remote_bash error: %v", resp.Error)
	}
	var result struct {
		TextResultForLlm string `json:"textResultForLlm"`
		ResultType       string `json:"resultType"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("decode remote_bash result: %v", err)
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(result.TextResultForLlm))
	if err != nil {
		t.Fatalf("parse background child pid from %q: %v", result.TextResultForLlm, err)
	}

	resp = rpc.call(t, "call_tool", "remote_bash", map[string]any{
		"codespace":    "it",
		"command":      fmt.Sprintf("test ! -e /proc/%d && printf background-cleaned", childPID),
		"initial_wait": 5,
	})
	if resp.Error != nil {
		t.Fatalf("background cleanup check error: %v", resp.Error)
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("decode background cleanup result: %v", err)
	}
	if !strings.Contains(result.TextResultForLlm, "background-cleaned") {
		t.Fatalf("background child %d survived shell cleanup: %s", childPID, result.TextResultForLlm)
	}
}
