package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	pathpkg "path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/ekroon/gh-copilot-codespace/internal/registry"
	"github.com/ekroon/gh-copilot-codespace/internal/ssh"
)

const codespaceNativeStatusEnv = "COPILOT_CODESPACE_STATUS_FILE"

const (
	nativeStatusInterval = time.Second
	nativeStatusTimeout  = 2 * time.Second
	nativeStatusMaxAge   = 30 * time.Second
	nativeStatusMaxBytes = 16 * 1024
)

// Only Git metadata: no index refresh, diff, or worktree scan. symbolic-ref
// returns 1 for detached HEAD, but other failures must not become a success.
const nativeStatusGitQuery = `git --no-optional-locks symbolic-ref --quiet --short HEAD || {
	code=$?
	[ "$code" -eq 1 ] || exit "$code"
	commit=$(git --no-optional-locks rev-parse --verify --short HEAD) || exit $?
	printf 'detached@%s\n' "$commit"
}`

// A branch is usable only with a fresh checkedAt and no error. All samples,
// including initializing/disconnected/errors, retain the pinned identity.
type nativeStatusCache struct {
	Version   int       `json:"version"`
	Alias     string    `json:"alias"`
	Name      string    `json:"name"`
	Cwd       string    `json:"cwd"`
	CheckedAt time.Time `json:"checkedAt"`
	Branch    string    `json:"branch,omitempty"`
	Error     string    `json:"error,omitempty"`
}

var nativeStatusEscapes = regexp.MustCompile(`(?:\x1b\[|\x{009b})[0-?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[ -/]*[@-~]`)

func nativeStatusText(text string, limit int) string {
	text = nativeStatusEscapes.ReplaceAllString(text, "")
	text = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return ' '
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, text)
	text = strings.Join(strings.Fields(text), " ")
	runes := []rune(text)
	if len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return text
}

// runNativeStatus is intentionally cache-only. CLI stdin includes LOCAL cwd
// and Git metadata: drain it, but never parse it or use it as a fallback.
// Operational cache failures render successfully so the UI shows a warning.
func runNativeStatus(args []string, in io.Reader, out io.Writer) error {
	if len(args) != 1 || !filepath.IsAbs(args[0]) {
		return fmt.Errorf("usage: native-status ABS_PATH")
	}
	if _, err := io.Copy(io.Discard, in); err != nil {
		return fmt.Errorf("draining status input: %w", err)
	}
	status, reason := readNativeStatusCache(args[0])
	now := time.Now()
	checked := "checked unknown"
	if !status.CheckedAt.IsZero() {
		age := now.Sub(status.CheckedAt)
		checked = fmt.Sprintf("checked %s (%ds ago)", status.CheckedAt.UTC().Format(time.RFC3339), int64(age/time.Second))
		if age < 0 {
			reason = "future cache timestamp"
			checked = "checked " + status.CheckedAt.UTC().Format(time.RFC3339) + " (future timestamp)"
		} else if age > nativeStatusMaxAge {
			reason = "stale cache; host unavailable"
		}
	}
	if reason == "" {
		reason = status.Error
	}
	branch := nativeStatusText(status.Branch, 96)
	if reason == "" && branch == "" {
		reason = "branch unknown"
	}
	if reason != "" {
		branch = "unknown (" + nativeStatusText(reason, 140) + ")"
	}
	target := nativeStatusText(status.Alias, 64)
	if target == "" {
		target = nativeStatusText(status.Name, 64)
	}
	if target == "" {
		target = "unavailable"
	}
	cwd := nativeStatusText(status.Cwd, 180)
	if cwd == "" {
		cwd = "unknown"
	}
	// Keep branch and observation age ahead of long paths on narrow terminals.
	_, err := fmt.Fprintf(out, "remote %s | branch: %s | %s | cwd: %s\n", target, branch, checked, cwd)
	return err
}

func readNativeStatusCache(filename string) (nativeStatusCache, string) {
	var status nativeStatusCache
	info, err := os.Lstat(filename)
	if err != nil {
		return status, "cache unavailable; initializing or host unavailable"
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return status, "cache unavailable: requires a private regular file"
	}
	file, err := os.Open(filename)
	if err != nil {
		return status, "cache unavailable: cannot read"
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, nativeStatusMaxBytes+1))
	if err != nil {
		return status, "cache unavailable: cannot read"
	}
	if len(data) > nativeStatusMaxBytes || json.Unmarshal(data, &status) != nil {
		return nativeStatusCache{}, "malformed cache"
	}
	if status.Version != 1 || status.CheckedAt.IsZero() || (status.Alias == "" && status.Name == "") || !pathpkg.IsAbs(status.Cwd) {
		return nativeStatusCache{}, "malformed cache"
	}
	return status, ""
}

type nativeStatusSampler struct {
	reg      *registry.Registry
	primary  *registry.ManagedCodespace
	executor ssh.Executor
	pinned   nativeStatusCache
	filename string
	stderr   io.Writer
}

type connectedStatusExecutor interface {
	RunBashIfConnected(context.Context, string, string) (string, string, int, error)
}

// Called only after executor wrapping and native workspace validation, before
// serving requests. Capture values once: remote_cd mutates Workdir/SetWorkdir.
func newNativeStatusSampler(reg *registry.Registry, alias, filename string, stderr io.Writer) (*nativeStatusSampler, error) {
	if !filepath.IsAbs(filename) {
		return nil, fmt.Errorf("native status cache must be an absolute path")
	}
	primary, err := reg.Resolve(alias)
	if err != nil {
		return nil, fmt.Errorf("pinning native status: %w", err)
	}
	if primary.Executor == nil || !pathpkg.IsAbs(primary.Workdir) || pathpkg.Clean(primary.Workdir) == "/" {
		return nil, fmt.Errorf("native status requires a connected executor and pinned remote workdir")
	}
	s := &nativeStatusSampler{
		reg: reg, primary: primary, executor: primary.Executor,
		pinned:   nativeStatusCache{Version: 1, Alias: alias, Name: primary.Name, Cwd: pathpkg.Clean(primary.Workdir)},
		filename: filename, stderr: stderr,
	}
	initial := s.pinned
	initial.CheckedAt, initial.Error = time.Now().UTC(), "initializing"
	if err := s.publish(initial); err != nil {
		fmt.Fprintf(stderr, "native status cache: %v\n", err)
		return nil, fmt.Errorf("initializing native status cache: %w", err)
	}
	return s, nil
}

func (s *nativeStatusSampler) targetError() string {
	primary, err := s.reg.Resolve(s.pinned.Alias)
	if err != nil {
		return "pinned target removed or disconnected"
	}
	if primary != s.primary {
		return "pinned target replaced; start a new native session"
	}
	return ""
}

func (s *nativeStatusSampler) sample(ctx context.Context) nativeStatusCache {
	result := s.pinned // Never reuse the previous observation's branch.
	result.CheckedAt = time.Now().UTC()
	if result.Error = s.targetError(); result.Error != "" {
		return result
	}
	// Direct SSH RunBash creates a process/transport per call. Do not silently
	// turn daemon-disabled/dial-fallback sessions into periodic SSH polling.
	if _, direct := s.executor.(*ssh.Client); direct {
		result.Error = "persistent daemon unavailable; direct SSH polling disabled"
		return result
	}
	probe, ok := s.executor.(connectedStatusExecutor)
	if !ok {
		result.Error = "persistent executor does not support passive status queries"
		return result
	}
	// Status never wakes/reconnects a target. Ordinary commands retain their
	// existing recovery behavior; the next sample can use their restored channel.
	probeCtx, cancel := context.WithTimeout(ctx, nativeStatusTimeout)
	defer cancel()
	stdout, stderr, code, err := probe.RunBashIfConnected(probeCtx, nativeStatusGitQuery, s.pinned.Cwd)
	if err == nil {
		err = probeCtx.Err()
	}
	if err != nil {
		result.Error = "remote probe failed: " + nativeStatusText(err.Error(), 120)
	} else if code != 0 {
		result.Error = fmt.Sprintf("remote git/cwd exit %d: %s", code, nativeStatusText(stderr, 120))
	} else {
		branch := strings.TrimSpace(stdout)
		if branch == "" || strings.ContainsAny(branch, "\r\n") {
			result.Error = "remote git returned invalid branch metadata"
		} else {
			result.Branch = branch
		}
	}
	// Also reject replacement/removal while the request was in flight.
	if reason := s.targetError(); reason != "" {
		result.Branch, result.Error = "", reason
	}
	return result
}

func (s *nativeStatusSampler) publish(status nativeStatusCache) error {
	file, err := os.CreateTemp(filepath.Dir(s.filename), ".native-status-*")
	if err != nil {
		return err
	}
	// CreateTemp uses mode 0600. Rename only a complete, closed JSON file.
	defer os.Remove(file.Name())
	writeErr := json.NewEncoder(file).Encode(status)
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(file.Name(), s.filename)
}

func (s *nativeStatusSampler) publishOrReport(status nativeStatusCache) {
	if err := s.publish(status); err != nil {
		fmt.Fprintf(s.stderr, "native status cache: %v\n", err)
	}
}

func (s *nativeStatusSampler) start(ctx context.Context) func() {
	sampleCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			final := s.pinned
			final.CheckedAt, final.Error = time.Now().UTC(), "disconnected: extension host exited"
			s.publishOrReport(final)
		}()
		ticker := time.NewTicker(nativeStatusInterval)
		defer ticker.Stop()
		for {
			if sampleCtx.Err() != nil {
				return
			}
			s.publishOrReport(s.sample(sampleCtx)) // Immediate first sample.
			// One goroutine and one synchronous probe: ticks coalesce, and
			// neither status painting nor tool events can create overlap.
			select {
			case <-sampleCtx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return sync.OnceFunc(func() {
		cancel()
		<-done
	})
}

// Own the sampler lifetime even when EOF ends the server while ctx is alive.
// Joining here precedes runExtensionHostIO's deferred executor cleanup.
func runExtensionHostWithNativeStatus(ctx context.Context, in io.Reader, out io.Writer, runtime extensionHostRuntime, bootstrap *bootstrapPayload, sampler *nativeStatusSampler) error {
	if sampler != nil {
		stop := sampler.start(ctx)
		defer stop()
	}
	return runExtensionHostServer(ctx, in, out, runtime, bootstrap)
}
