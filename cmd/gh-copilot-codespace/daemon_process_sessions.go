package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	daemonProcessOutputLimit = 1024 * 1024
	daemonProcessCgroupRoot  = "/sys/fs/cgroup/gh-copilot-codespace"
)

var (
	daemonProcessSupportOnce sync.Once
	daemonProcessSupport     bool
)

type daemonProcessOutput struct {
	mu   sync.Mutex
	data []byte
}

func (o *daemonProcessOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.data = append(o.data, p...)
	if len(o.data) > daemonProcessOutputLimit {
		o.data = append([]byte(nil), o.data[len(o.data)-daemonProcessOutputLimit:]...)
	}
	return len(p), nil
}

func (o *daemonProcessOutput) tail() string {
	o.mu.Lock()
	defer o.mu.Unlock()

	output := string(o.data)
	lines := strings.Split(output, "\n")
	if len(lines) > 101 {
		lines = lines[len(lines)-101:]
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n ")
}

type daemonProcessSession struct {
	cmd    *exec.Cmd
	pgid   int
	output *daemonProcessOutput
	cgroup *daemonProcessCgroup

	statusMu  sync.RWMutex
	completed bool
	exitCode  int
	waitErr   error
	stopErr   error

	done     chan struct{}
	stopOnce sync.Once
	stopped  chan struct{}
}

var daemonProcessSessions sync.Map
var daemonSessionIDs sync.Map

func daemonReserveSessionID(sessionID, backend string) error {
	if sessionID == "" {
		return errors.New("session id is required")
	}
	if existing, loaded := daemonSessionIDs.LoadOrStore(sessionID, backend); loaded {
		return fmt.Errorf("session %q already exists with backend %s", sessionID, existing)
	}
	return nil
}

func daemonReleaseSessionID(sessionID string) {
	daemonSessionIDs.Delete(sessionID)
}

func daemonStartProcessSession(ctx context.Context, sessionID, command, cwd string) error {
	if err := ctx.Err(); err != nil {
		return errDaemonCanceled
	}
	if err := daemonReserveSessionID(sessionID, "process"); err != nil {
		return err
	}
	keepReservation := false
	defer func() {
		if !keepReservation {
			daemonReleaseSessionID(sessionID)
		}
	}()
	state := &daemonProcessSession{
		output:   &daemonProcessOutput{},
		done:     make(chan struct{}),
		stopped:  make(chan struct{}),
		exitCode: -1,
	}

	cgroup, cgroupErr := daemonCreateProcessCgroup(sessionID)
	if cgroupErr != nil {
		if daemonProcessSessionsSupported() {
			return fmt.Errorf("create process session cgroup: %w", cgroupErr)
		}
		fmt.Fprintf(os.Stderr, "codespace-mcp: process session isolation unavailable: %v\n", cgroupErr)
	}
	state.cgroup = cgroup

	gateRead, gateWrite, err := os.Pipe()
	if err != nil {
		if cgroup != nil {
			cgroup.remove()
		}
		return fmt.Errorf("create process start gate: %w", err)
	}
	defer gateWrite.Close()

	cmd := exec.Command("bash", "-c", `IFS= read -r _ <&3; exec bash -c "$1"`, "ghcs-session", command)
	if cwd != "" {
		cmd.Dir = cwd
	}
	cmd.ExtraFiles = []*os.File{gateRead}
	cmd.Env = os.Environ()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout = state.output
	cmd.Stderr = state.output
	cmd.WaitDelay = 100 * time.Millisecond
	state.cmd = cmd

	if err := cmd.Start(); err != nil {
		_ = gateRead.Close()
		if cgroup != nil {
			cgroup.remove()
		}
		return fmt.Errorf("start process session: %w", err)
	}
	_ = gateRead.Close()
	state.pgid = cmd.Process.Pid
	if cgroup != nil {
		if err := cgroup.addProcess(cmd.Process.Pid); err != nil {
			_ = syscall.Kill(-state.pgid, syscall.SIGKILL)
			_, _ = gateWrite.WriteString("\n")
			_ = cmd.Wait()
			cgroup.remove()
			return fmt.Errorf("isolate process session: %w", err)
		}
	}
	daemonProcessSessions.Store(sessionID, state)
	keepReservation = true

	go func() {
		runErr := cmd.Wait()
		exitCode := 0
		var waitErr error
		if runErr != nil {
			var exitErr *exec.ExitError
			switch {
			case errors.As(runErr, &exitErr):
				exitCode = exitErr.ExitCode()
			case errors.Is(runErr, exec.ErrWaitDelay) && cmd.ProcessState != nil:
				exitCode = cmd.ProcessState.ExitCode()
			default:
				exitCode = -1
				waitErr = runErr
			}
		}

		state.statusMu.Lock()
		state.completed = true
		state.exitCode = exitCode
		state.waitErr = waitErr
		state.statusMu.Unlock()
		close(state.done)
	}()
	if _, err := gateWrite.WriteString("\n"); err != nil {
		_ = daemonStopProcessSession(context.Background(), sessionID)
		return fmt.Errorf("release process start gate: %w", err)
	}

	if err := ctx.Err(); err != nil {
		_ = daemonStopProcessSession(context.Background(), sessionID)
		return errDaemonCanceled
	}
	return nil
}

func daemonReadProcessSession(_ context.Context, sessionID string) (string, bool, error) {
	value, ok := daemonProcessSessions.Load(sessionID)
	if !ok {
		return "", false, fmt.Errorf("session %q does not exist", sessionID)
	}
	state := value.(*daemonProcessSession)
	return state.snapshot()
}

func daemonWaitProcessSession(ctx context.Context, sessionID string, timeout time.Duration) (string, bool, error) {
	value, ok := daemonProcessSessions.Load(sessionID)
	if !ok {
		return "", false, fmt.Errorf("session %q does not exist", sessionID)
	}
	state := value.(*daemonProcessSession)

	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case <-state.done:
		case <-timer.C:
		case <-ctx.Done():
			return "", false, ctx.Err()
		}
	}
	return state.snapshot()
}

func (s *daemonProcessSession) snapshot() (string, bool, error) {
	s.statusMu.RLock()
	completed := s.completed
	exitCode := s.exitCode
	waitErr := s.waitErr
	s.statusMu.RUnlock()
	output := s.output.tail()

	if completed {
		if output != "" {
			output += "\n"
		}
		output += daemonSessionExitMarker
		if exitCode != 0 {
			output += fmt.Sprintf("\n[exit code: %d]", exitCode)
		}
	}
	if waitErr != nil {
		return output, completed, fmt.Errorf("wait for process session: %w", waitErr)
	}
	return output, completed, nil
}

func daemonWriteProcessSession(ctx context.Context, sessionID, input string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	value, ok := daemonProcessSessions.Load(sessionID)
	if !ok {
		return fmt.Errorf("session %q does not exist", sessionID)
	}
	_ = value
	_ = input
	return fmt.Errorf("session %q is non-interactive; use remote_bash mode=async for stdin or PTY support", sessionID)
}

func daemonStopProcessSession(ctx context.Context, sessionID string) error {
	value, ok := daemonProcessSessions.Load(sessionID)
	if !ok {
		return fmt.Errorf("session %q does not exist", sessionID)
	}
	state := value.(*daemonProcessSession)
	state.beginStop(sessionID)

	select {
	case <-state.stopped:
		state.statusMu.RLock()
		defer state.statusMu.RUnlock()
		return state.stopErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *daemonProcessSession) beginStop(sessionID string) {
	s.stopOnce.Do(func() {
		go func() {
			defer close(s.stopped)
			defer daemonProcessSessions.CompareAndDelete(sessionID, s)
			defer daemonReleaseSessionID(sessionID)

			if s.cgroup != nil {
				s.stopCgroupSession(sessionID)
				return
			}

			select {
			case <-s.done:
				daemonSignalProcessGroupMembers(s.pgid, syscall.SIGKILL)
				return
			default:
			}
			daemonSignalProcessTree(s.cmd.Process.Pid, s.pgid, syscall.SIGTERM)
			timer := time.NewTimer(2 * time.Second)
			select {
			case <-s.done:
				timer.Stop()
				return
			case <-timer.C:
			}

			daemonSignalProcessTree(s.cmd.Process.Pid, s.pgid, syscall.SIGKILL)
			finalTimer := time.NewTimer(3 * time.Second)
			defer finalTimer.Stop()
			select {
			case <-s.done:
			case <-finalTimer.C:
				s.statusMu.Lock()
				s.stopErr = fmt.Errorf("timed out waiting for session %q process tree to exit", sessionID)
				s.statusMu.Unlock()
			}
		}()
	})
}

func (s *daemonProcessSession) stopCgroupSession(sessionID string) {
	select {
	case <-s.done:
	default:
		daemonSignalProcessTree(s.cmd.Process.Pid, s.pgid, syscall.SIGTERM)
		timer := time.NewTimer(2 * time.Second)
		select {
		case <-s.done:
			timer.Stop()
		case <-timer.C:
		}
	}

	if err := s.cgroup.kill(); err != nil {
		s.statusMu.Lock()
		s.stopErr = fmt.Errorf("kill session %q cgroup: %w", sessionID, err)
		s.statusMu.Unlock()
		daemonSignalProcessTree(s.cmd.Process.Pid, s.pgid, syscall.SIGKILL)
	}

	finalTimer := time.NewTimer(3 * time.Second)
	select {
	case <-s.done:
		finalTimer.Stop()
	case <-finalTimer.C:
		s.statusMu.Lock()
		s.stopErr = fmt.Errorf("timed out waiting for session %q process tree to exit", sessionID)
		s.statusMu.Unlock()
	}
	s.cgroup.remove()
}

type daemonProcessCgroup struct {
	path string
}

func daemonProcessSessionsSupported() bool {
	daemonProcessSupportOnce.Do(func() {
		if runtime.GOOS != "linux" {
			return
		}
		if err := os.MkdirAll(daemonProcessCgroupRoot, 0o755); err != nil {
			return
		}
		probe := filepath.Join(daemonProcessCgroupRoot, fmt.Sprintf(".probe-%d", os.Getpid()))
		if err := os.Mkdir(probe, 0o755); err != nil {
			return
		}
		defer os.Remove(probe)

		cmd := exec.Command("sleep", "30")
		if err := cmd.Start(); err != nil {
			return
		}
		if err := os.WriteFile(filepath.Join(probe, "cgroup.procs"), []byte(strconv.Itoa(cmd.Process.Pid)), 0o644); err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return
		}
		_ = syscall.Kill(cmd.Process.Pid, syscall.SIGKILL)
		if err := cmd.Wait(); err == nil {
			return
		}
		daemonProcessSupport = os.Remove(probe) == nil
	})
	return daemonProcessSupport
}

func daemonCreateProcessCgroup(sessionID string) (*daemonProcessCgroup, error) {
	if runtime.GOOS != "linux" {
		return nil, nil
	}
	if !daemonProcessSessionsSupported() {
		return nil, errors.New("cgroup v2 delegation is unavailable")
	}
	sum := sha256.Sum256([]byte(sessionID))
	path := filepath.Join(daemonProcessCgroupRoot, fmt.Sprintf("%d-%x", os.Getpid(), sum[:8]))
	if err := os.Mkdir(path, 0o755); err != nil {
		return nil, err
	}
	return &daemonProcessCgroup{path: path}, nil
}

func (c *daemonProcessCgroup) addProcess(pid int) error {
	return os.WriteFile(filepath.Join(c.path, "cgroup.procs"), []byte(strconv.Itoa(pid)), 0o644)
}

func (c *daemonProcessCgroup) kill() error {
	err := os.WriteFile(filepath.Join(c.path, "cgroup.kill"), []byte("1"), 0o644)
	if err == nil {
		return nil
	}
	if fallbackErr := c.killProcesses(); fallbackErr != nil {
		return fmt.Errorf("cgroup.kill: %v; cgroup.procs fallback: %w", err, fallbackErr)
	}
	return nil
}

func (c *daemonProcessCgroup) killProcesses() error {
	procsPath := filepath.Join(c.path, "cgroup.procs")
	for range 100 {
		data, err := os.ReadFile(procsPath)
		if err != nil {
			return err
		}
		fields := strings.Fields(string(data))
		if len(fields) == 0 {
			return nil
		}
		for _, field := range fields {
			pid, err := strconv.Atoi(field)
			if err == nil && pid > 0 {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.New("cgroup remained populated after SIGKILL")
}

func (c *daemonProcessCgroup) remove() {
	for range 20 {
		if err := os.Remove(c.path); err == nil || errors.Is(err, os.ErrNotExist) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func daemonSignalProcessTree(rootPID, pgid int, signal syscall.Signal) {
	descendants := daemonDescendantPIDs(rootPID)
	for i := len(descendants) - 1; i >= 0; i-- {
		_ = syscall.Kill(descendants[i], signal)
	}
	if pgid > 0 {
		_ = syscall.Kill(-pgid, signal)
	}
}

func daemonSignalProcessGroupMembers(pgid int, signal syscall.Signal) {
	if pgid <= 0 {
		return
	}
	output, err := exec.Command("ps", "-e", "-o", "pid=", "-o", "pgid=").Output()
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		pid, pidErr := strconv.Atoi(fields[0])
		processGroup, groupErr := strconv.Atoi(fields[1])
		if pidErr == nil && groupErr == nil && processGroup == pgid {
			_ = syscall.Kill(pid, signal)
		}
	}
}

func daemonDescendantPIDs(rootPID int) []int {
	output, err := exec.Command("ps", "-e", "-o", "pid=", "-o", "ppid=").Output()
	if err != nil {
		return nil
	}

	children := make(map[int][]int)
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		pid, pidErr := strconv.Atoi(fields[0])
		ppid, ppidErr := strconv.Atoi(fields[1])
		if pidErr == nil && ppidErr == nil && pid > 0 {
			children[ppid] = append(children[ppid], pid)
		}
	}

	var descendants []int
	var visit func(int)
	visit = func(parent int) {
		for _, child := range children[parent] {
			descendants = append(descendants, child)
			visit(child)
		}
	}
	visit(rootPID)
	return descendants
}

func daemonListProcessSessions() string {
	var lines []string
	now := time.Now().Unix()
	daemonProcessSessions.Range(func(key, _ any) bool {
		lines = append(lines, fmt.Sprintf("%s%s %d %d", daemonTmuxPrefix, key.(string), now, now))
		return true
	})
	return strings.Join(lines, "\n")
}

func daemonStopAllProcessSessions() {
	var states []*daemonProcessSession
	daemonProcessSessions.Range(func(key, value any) bool {
		state := value.(*daemonProcessSession)
		states = append(states, state)
		state.beginStop(key.(string))
		return true
	})
	for _, state := range states {
		<-state.stopped
	}
}
