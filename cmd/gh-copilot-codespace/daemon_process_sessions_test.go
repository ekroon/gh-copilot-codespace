package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestDaemonProcessSessionCompletesWithoutTmux(t *testing.T) {
	sessionID := "process-quick"
	if err := daemonStartProcessSession(context.Background(), sessionID, "printf process-done", t.TempDir()); err != nil {
		t.Fatalf("daemonStartProcessSession() error = %v", err)
	}
	t.Cleanup(func() { _ = daemonStopProcessSession(context.Background(), sessionID) })

	output, completed, err := daemonWaitProcessSession(context.Background(), sessionID, time.Second)
	if err != nil {
		t.Fatalf("daemonWaitProcessSession() error = %v", err)
	}
	if !completed {
		t.Fatal("completed = false, want true")
	}
	if !strings.Contains(output, "process-done") || !strings.Contains(output, daemonSessionExitMarker) {
		t.Fatalf("output = %q, want command output and exit marker", output)
	}
}

func TestDaemonSessionIDReservationRejectsCrossBackendDuplicate(t *testing.T) {
	sessionID := "cross-backend-reservation"
	if err := daemonReserveSessionID(sessionID, "process"); err != nil {
		t.Fatalf("first daemonReserveSessionID() error = %v", err)
	}
	t.Cleanup(func() { daemonReleaseSessionID(sessionID) })

	if err := daemonReserveSessionID(sessionID, "tmux"); err == nil {
		t.Fatal("second daemonReserveSessionID() error = nil")
	}
}

func TestDaemonProcessEnvironmentPrependsMisePaths(t *testing.T) {
	got := daemonProcessEnvironment([]string{
		"HOME=/home/codespace",
		"PATH=/usr/local/bin:/usr/bin",
		"OTHER=value",
	})
	wantPath := "PATH=/home/codespace/.local/bin:/home/codespace/.local/share/mise/shims:/usr/local/bin:/usr/bin"
	if !slices.Contains(got, wantPath) {
		t.Fatalf("daemonProcessEnvironment() = %v, want %q", got, wantPath)
	}
}

func TestDaemonProcessSessionSurvivesInitialWait(t *testing.T) {
	sessionID := "process-slow"
	if err := daemonStartProcessSession(context.Background(), sessionID, "printf started; sleep 0.2; printf finished", t.TempDir()); err != nil {
		t.Fatalf("daemonStartProcessSession() error = %v", err)
	}
	t.Cleanup(func() { _ = daemonStopProcessSession(context.Background(), sessionID) })

	output, completed, err := daemonWaitProcessSession(context.Background(), sessionID, 25*time.Millisecond)
	if err != nil {
		t.Fatalf("first daemonWaitProcessSession() error = %v", err)
	}
	if completed {
		t.Fatalf("first completed = true, want false; output = %q", output)
	}

	output, completed, err = daemonWaitProcessSession(context.Background(), sessionID, time.Second)
	if err != nil {
		t.Fatalf("second daemonWaitProcessSession() error = %v", err)
	}
	if !completed {
		t.Fatal("second completed = false, want true")
	}
	if !strings.Contains(output, "started") || !strings.Contains(output, "finished") {
		t.Fatalf("output = %q, want started and finished", output)
	}
}

func TestDaemonProcessSessionRejectsDuplicateWithoutRerunning(t *testing.T) {
	sessionID := "process-duplicate"
	path := filepath.Join(t.TempDir(), "runs")
	command := "printf x >> " + shellQuote(path) + "; sleep 0.2"
	if err := daemonStartProcessSession(context.Background(), sessionID, command, t.TempDir()); err != nil {
		t.Fatalf("first daemonStartProcessSession() error = %v", err)
	}
	t.Cleanup(func() { _ = daemonStopProcessSession(context.Background(), sessionID) })

	if err := daemonStartProcessSession(context.Background(), sessionID, command, t.TempDir()); err == nil {
		t.Fatal("duplicate daemonStartProcessSession() error = nil")
	}
	if _, _, err := daemonWaitProcessSession(context.Background(), sessionID, time.Second); err != nil {
		t.Fatalf("daemonWaitProcessSession() error = %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(content) != "x" {
		t.Fatalf("side effects = %q, want one execution", content)
	}
}

func TestDaemonProcessSessionRejectsInput(t *testing.T) {
	sessionID := "process-input"
	if err := daemonStartProcessSession(context.Background(), sessionID, "sleep 30", t.TempDir()); err != nil {
		t.Fatalf("daemonStartProcessSession() error = %v", err)
	}
	t.Cleanup(func() { _ = daemonStopProcessSession(context.Background(), sessionID) })

	err := daemonWriteProcessSession(context.Background(), sessionID, "hello{enter}")
	if err == nil || !strings.Contains(err.Error(), "mode=async") {
		t.Fatalf("daemonWriteProcessSession() error = %v, want async guidance", err)
	}
}

func TestDaemonProcessSessionReportsExitCode(t *testing.T) {
	sessionID := "process-exit-code"
	if err := daemonStartProcessSession(context.Background(), sessionID, "printf failed; exit 7", t.TempDir()); err != nil {
		t.Fatalf("daemonStartProcessSession() error = %v", err)
	}
	t.Cleanup(func() { _ = daemonStopProcessSession(context.Background(), sessionID) })

	output, completed, err := daemonWaitProcessSession(context.Background(), sessionID, time.Second)
	if err != nil {
		t.Fatalf("daemonWaitProcessSession() error = %v", err)
	}
	if !completed {
		t.Fatal("completed = false, want true")
	}
	if !strings.Contains(output, "failed") || !strings.Contains(output, "[exit code: 7]") {
		t.Fatalf("output = %q, want command output and exit code", output)
	}
}

func TestDaemonProcessSessionTreatsWaitDelayAsSuccessfulShellExit(t *testing.T) {
	sessionID := "process-wait-delay"
	if err := daemonStartProcessSession(context.Background(), sessionID, "sleep 30 & printf background-started", t.TempDir()); err != nil {
		t.Fatalf("daemonStartProcessSession() error = %v", err)
	}
	t.Cleanup(func() { _ = daemonStopProcessSession(context.Background(), sessionID) })

	output, completed, err := daemonWaitProcessSession(context.Background(), sessionID, time.Second)
	if err != nil {
		t.Fatalf("daemonWaitProcessSession() error = %v", err)
	}
	if !completed {
		t.Fatal("completed = false, want true")
	}
	if !strings.Contains(output, "background-started") {
		t.Fatalf("output = %q, want background-started", output)
	}
}

func TestDaemonProcessSessionStopKillsProcessTree(t *testing.T) {
	sessionID := "process-stop-tree"
	pidPath := filepath.Join(t.TempDir(), "child.pid")
	command := "sleep 30 & child=$!; printf %s \"$child\" > " + shellQuote(pidPath) + "; wait"
	if err := daemonStartProcessSession(context.Background(), sessionID, command, t.TempDir()); err != nil {
		t.Fatalf("daemonStartProcessSession() error = %v", err)
	}

	var childPID int
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		content, err := os.ReadFile(pidPath)
		if err == nil {
			childPID, err = strconv.Atoi(string(content))
			if err != nil {
				t.Fatalf("parse child pid: %v", err)
			}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if childPID == 0 {
		t.Fatal("child pid was not written")
	}

	if err := daemonStopProcessSession(context.Background(), sessionID); err != nil {
		t.Fatalf("daemonStopProcessSession() error = %v", err)
	}
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(childPID, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("child process %d survived session stop", childPID)
}

func TestDaemonProcessSessionStopKillsSetsidDescendant(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid is not installed")
	}

	sessionID := "process-stop-setsid"
	pidPath := filepath.Join(t.TempDir(), "child.pid")
	command := "setsid sh -c 'sleep 30' & child=$!; printf %s \"$child\" > " + shellQuote(pidPath) + "; wait"
	if err := daemonStartProcessSession(context.Background(), sessionID, command, t.TempDir()); err != nil {
		t.Fatalf("daemonStartProcessSession() error = %v", err)
	}

	var childPID int
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		content, err := os.ReadFile(pidPath)
		if err == nil {
			childPID, err = strconv.Atoi(string(content))
			if err != nil {
				t.Fatalf("parse child pid: %v", err)
			}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if childPID == 0 {
		t.Fatal("setsid child pid was not written")
	}

	if err := daemonStopProcessSession(context.Background(), sessionID); err != nil {
		t.Fatalf("daemonStopProcessSession() error = %v", err)
	}
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(childPID, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("setsid child process %d survived session stop", childPID)
}

func TestDaemonProcessSessionForcedStopSkipsGracePeriod(t *testing.T) {
	sessionID := "process-force-stop"
	if err := daemonStartProcessSession(context.Background(), sessionID, "trap '' TERM; sleep 30", t.TempDir()); err != nil {
		t.Fatalf("daemonStartProcessSession() error = %v", err)
	}
	value, ok := daemonProcessSessions.Load(sessionID)
	if !ok {
		t.Fatalf("session %q is not tracked", sessionID)
	}
	value.(*daemonProcessSession).forceStopNow()

	start := time.Now()
	if err := daemonStopProcessSession(context.Background(), sessionID); err != nil {
		t.Fatalf("daemonStopProcessSession() error = %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("forced stop took %v, want <= 1s", elapsed)
	}
}
