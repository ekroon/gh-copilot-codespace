package daemonclient_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ekroon/gh-copilot-codespace/internal/daemonclient"
	"github.com/ekroon/gh-copilot-codespace/internal/daemontransport"
)

// daemonPID returns the pid of the daemon process currently backing e.
func daemonPID(t *testing.T, e *daemonclient.Executor) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := e.Ping(ctx)
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if !result.Pong || result.PID == 0 {
		t.Fatalf("Ping result = %+v, want pong with non-zero pid", result)
	}
	return result.PID
}

// killDaemon terminates the daemon subprocess and waits until the executor has
// observed its connection as terminal, so follow-up assertions never race the
// reader goroutine.
func killDaemon(t *testing.T, e *daemonclient.Executor, pid int) {
	t.Helper()
	proc, err := os.FindProcess(pid)
	if err != nil {
		t.Fatalf("FindProcess(%d): %v", pid, err)
	}
	if err := proc.Signal(os.Kill); err != nil {
		t.Fatalf("kill daemon %d: %v", pid, err)
	}
	daemonclient.WaitConnectionDeadForTests(t, e, 10*time.Second)
}

func requireConnectionLost(t *testing.T, err error) *daemonclient.ConnectionLostError {
	t.Helper()
	var lost *daemonclient.ConnectionLostError
	if !errors.As(err, &lost) {
		t.Fatalf("error = %v (%T), want *daemonclient.ConnectionLostError", err, err)
	}
	return lost
}

// TestE2E_DaemonDeathDuringCallIsNotReplayedAndReconnects drives a real daemon
// subprocess, makes it die while one operation is in flight, and proves that
// the interrupted operation is reported (not replayed) while the executor
// restores a working connection for later operations.
func TestE2E_DaemonDeathDuringCallIsNotReplayedAndReconnects(t *testing.T) {
	dir := daemonclient.TempDirForTests(t)
	e := dialDaemonForE2E(t, dir)
	pid := daemonPID(t, e)

	marker := filepath.Join(dir, "runs.txt")
	// The command records one side effect and then kills the daemon that is
	// executing it, so the client can never receive a response.
	command := fmt.Sprintf("printf 'run\\n' >> %s; kill -9 %d", shellQuoteForTest(marker), pid)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, _, _, err := e.RunBash(ctx, command, dir)
	if err == nil {
		t.Fatal("RunBash succeeded, want connection loss after daemon death")
	}
	lost := requireConnectionLost(t, err)

	if !lost.OutcomeUnknown {
		t.Fatalf("OutcomeUnknown = false, want true for a request that reached the daemon: %v", lost)
	}
	if !lost.Reconnected {
		t.Fatalf("Reconnected = false, want a restored connection: %v", lost)
	}
	if lost.NewGeneration == lost.OldGeneration {
		t.Fatalf("generations = %d -> %d, want a new generation", lost.OldGeneration, lost.NewGeneration)
	}

	var terminal *daemontransport.TerminalProcessError
	if !errors.As(lost.Cause, &terminal) {
		t.Fatalf("Cause = %v, want a *daemontransport.TerminalProcessError describing the dead process", lost.Cause)
	}
	if terminal.Transport != "local" {
		t.Fatalf("terminal transport = %q, want local", terminal.Transport)
	}
	if terminal.WaitErr == nil {
		t.Fatalf("terminal WaitErr = nil, want the process exit cause")
	}

	// The replacement generation must be a different daemon process serving
	// operations normally.
	newPID := daemonPID(t, e)
	if newPID == pid {
		t.Fatalf("daemon pid = %d after reconnect, want a new process", newPID)
	}

	stdout, _, exitCode, err := e.RunBash(ctx, "printf 'after\\n'", dir)
	if err != nil {
		t.Fatalf("RunBash after reconnect: %v", err)
	}
	if exitCode != 0 || !strings.Contains(stdout, "after") {
		t.Fatalf("RunBash after reconnect = %q (exit %d), want after", stdout, exitCode)
	}

	// The interrupted command ran exactly once: the executor never replays it.
	content, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", marker, err)
	}
	if string(content) != "run\n" {
		t.Fatalf("marker content = %q, want exactly one recorded run", string(content))
	}
}

// TestE2E_DaemonDeathBetweenCallsReconnectsTransparently covers the daemon
// dying while the executor is idle: the next operation transparently uses a
// fresh daemon process instead of failing.
func TestE2E_DaemonDeathBetweenCallsReconnectsTransparently(t *testing.T) {
	dir := daemonclient.TempDirForTests(t)
	e := dialDaemonForE2E(t, dir)
	pid := daemonPID(t, e)

	killDaemon(t, e, pid)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	path := filepath.Join(dir, "after-reconnect.txt")
	if err := e.CreateFile(ctx, path, "recovered\n"); err != nil {
		t.Fatalf("CreateFile after daemon death: %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	if string(content) != "recovered\n" {
		t.Fatalf("content = %q, want recovered", string(content))
	}

	newPID := daemonPID(t, e)
	if newPID == pid {
		t.Fatalf("daemon pid = %d after reconnect, want a new process", newPID)
	}
}

// TestE2E_DaemonDeathLosesDaemonManagedSessions verifies the retained-session
// semantics the MCP guidance promises: sessions owned by the daemon process do
// not survive its death. It is skipped where the daemon cannot own process
// sessions (no delegated cgroup v2 support).
func TestE2E_DaemonDeathLosesDaemonManagedSessions(t *testing.T) {
	dir := daemonclient.TempDirForTests(t)
	e := dialDaemonForE2E(t, dir)
	if !e.SupportsProcessSessions() {
		t.Skip("daemon does not advertise daemon-managed process sessions")
	}
	pid := daemonPID(t, e)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const sessionID = "e2e-retained-session"
	if err := e.StartProcessSession(ctx, sessionID, "sleep 60", dir); err != nil {
		t.Skipf("daemon-managed process sessions unavailable: %v", err)
	}
	t.Cleanup(func() { _ = e.StopSession(context.Background(), sessionID) })
	if _, err := e.ReadSession(ctx, sessionID); err != nil {
		t.Fatalf("ReadSession before daemon death: %v", err)
	}

	killDaemon(t, e, pid)

	if _, err := e.ReadSession(ctx, sessionID); err == nil {
		t.Fatal("ReadSession succeeded after daemon death, want the retained session to be gone")
	} else {
		var remote *daemonclient.RemoteError
		if !errors.As(err, &remote) {
			t.Fatalf("ReadSession error = %v (%T), want a remote error from the new daemon", err, err)
		}
	}
}

// TestE2E_ConnectionLossSurfacesGuidanceThroughRuntime checks that a real
// daemon death during a tool call reaches the model as connection-loss
// guidance instead of a raw transport error.
func TestE2E_ConnectionLossSurfacesGuidanceThroughRuntime(t *testing.T) {
	runtime, e := singleCodespaceRuntime(t)
	if e.SupportsProcessSessions() {
		t.Skip("daemon owns process sessions; this test needs the synchronous remote_bash path")
	}
	bashTool := findRuntimeTool(t, runtime, "remote_bash", "run_bash", "bash")
	pid := daemonPID(t, e)

	result := callRuntime(t, runtime, bashTool, map[string]any{
		"command": fmt.Sprintf("kill -9 %d", pid),
		// A shell id the daemon cannot accept forces the synchronous fallback,
		// so the daemon death interrupts an in-flight RunBash call.
		"shellId": "force-fallback\x00",
	})
	if result.ResultType != "failure" {
		t.Fatalf("result type = %q, want failure; text: %s", result.ResultType, result.TextResultForLlm)
	}
	if !strings.Contains(result.TextResultForLlm, "Remote connection lost") {
		t.Fatalf("result text = %q, want connection-loss guidance", result.TextResultForLlm)
	}
	if !strings.Contains(result.TextResultForLlm, "outcome of this call is unknown") {
		t.Fatalf("result text = %q, want unknown-outcome guidance", result.TextResultForLlm)
	}

	newPID := daemonPID(t, e)
	if newPID == pid {
		t.Fatalf("daemon pid = %d after reconnect, want a new process", newPID)
	}
}

func shellQuoteForTest(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
