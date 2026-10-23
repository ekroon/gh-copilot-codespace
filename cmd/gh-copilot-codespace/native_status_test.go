package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ekroon/gh-copilot-codespace/internal/registry"
	"github.com/ekroon/gh-copilot-codespace/internal/ssh"
)

func statusFixture(t *testing.T, exec ssh.Executor) (*registry.Registry, *registry.ManagedCodespace, string) {
	t.Helper()
	reg := registry.New()
	cs := &registry.ManagedCodespace{Alias: "remote-fixture", Name: "selected-codespace", Workdir: "/workspaces/pinned", Branch: "WRONG-startup", Executor: exec}
	if err := reg.Register(cs); err != nil {
		t.Fatal(err)
	}
	return reg, cs, filepath.Join(t.TempDir(), "status.json")
}

func putStatus(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestNativeStatusRenderer(t *testing.T) {
	// A renderer has no transport or Git dependency, including with hostile stdin.
	t.Setenv("PATH", "")
	t.Setenv("CODESPACE_REGISTRY", "not valid registry JSON")
	now := time.Now().UTC()
	base := nativeStatusCache{Version: 1, Alias: "pinned", Name: "remote-name", Cwd: "/workspaces/remote", Branch: "remote-topic", CheckedAt: now.Add(-3 * time.Second)}
	for _, tc := range []struct {
		name, reason string
		change       func(*nativeStatusCache)
		raw          string
		missing      bool
	}{
		{name: "fresh"},
		{name: "missing", reason: "unavailable", missing: true},
		{name: "malformed", reason: "malformed", raw: "{"},
		{name: "trailing JSON", reason: "malformed", raw: "{} {}"},
		{name: "stale", reason: "stale", change: func(s *nativeStatusCache) { s.CheckedAt = now.Add(-31 * time.Second) }},
		{name: "future", reason: "future", change: func(s *nativeStatusCache) { s.CheckedAt = now.Add(time.Minute) }},
		{name: "zero timestamp", reason: "malformed", change: func(s *nativeStatusCache) { s.CheckedAt = time.Time{} }},
		{name: "bad version", reason: "malformed", change: func(s *nativeStatusCache) { s.Version = 9 }},
		{name: "disconnected clears old branch", reason: "disconnected", change: func(s *nativeStatusCache) { s.Error = "disconnected" }},
		{name: "empty branch", reason: "unknown", change: func(s *nativeStatusCache) { s.Branch = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "status.json")
			status := base
			if tc.change != nil {
				tc.change(&status)
			}
			if !tc.missing {
				if tc.raw != "" {
					if err := os.WriteFile(path, []byte(tc.raw), 0600); err != nil {
						t.Fatal(err)
					}
				} else {
					putStatus(t, path, status)
				}
			}
			input := bytes.NewBufferString(`{"cwd":"/LOCAL-WRONG","git":{"branch":"LOCAL-WRONG"}}`)
			var out bytes.Buffer
			if err := runNativeStatus([]string{path}, input, &out); err != nil {
				t.Fatal(err)
			}
			line := out.String()
			if input.Len() != 0 {
				t.Fatal("stdin was not drained")
			}
			if strings.Contains(line, "LOCAL-WRONG") || strings.Count(line, "\n") != 1 {
				t.Fatalf("untruthful/multiline display: %q", line)
			}
			if !strings.Contains(line, "checked") {
				t.Fatalf("missing observation timestamp: %q", line)
			}
			if tc.reason != "" {
				if !strings.Contains(line, "branch: unknown") || !strings.Contains(line, tc.reason) || strings.Contains(line, "remote-topic") {
					t.Fatalf("expected unknown %s: %q", tc.reason, line)
				}
			} else if !strings.Contains(line, "pinned") || !strings.Contains(line, "/workspaces/remote") || !strings.Contains(line, "remote-topic") || !strings.Contains(line, "3s ago") {
				t.Fatalf("missing pinned live status/age: %q", line)
			}
			if tc.reason == "" && (strings.Index(line, "remote-topic") > strings.Index(line, "/workspaces/remote") || strings.Index(line, "3s ago") > strings.Index(line, "/workspaces/remote")) {
				t.Fatalf("long cwd hides branch/age on narrow terminals: %q", line)
			}
		})
	}
	for _, args := range [][]string{nil, {"relative.json"}, {"/a", "/b"}} {
		if err := runNativeStatus(args, strings.NewReader(""), io.Discard); err == nil {
			t.Fatalf("invalid invocation accepted: %v", args)
		}
	}
}

func TestNativeStatusSanitizesAndRequiresPrivateCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	cache := nativeStatusCache{Version: 1, Alias: "\x1b[31malias\nforged", Name: "remote", Cwd: "/workspaces/\x1b]0;title\afoo\rbar", Branch: "topic\x1b[0m\nother\u202e", CheckedAt: time.Now().Add(-time.Second)}
	putStatus(t, path, cache)
	var out bytes.Buffer
	if err := runNativeStatus([]string{path}, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(out.String(), "\x1b\r\a\u202e") || strings.Contains(out.String(), "[31m") || strings.Count(out.String(), "\n") != 1 {
		t.Fatalf("terminal controls survived: %q", out.String())
	}
	cache.Error = "\x1b[31merror\n" + strings.Repeat("x", 1000)
	putStatus(t, path, cache)
	out.Reset()
	if err := runNativeStatus([]string{path}, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "branch: topic") || len(out.String()) > 500 || strings.ContainsAny(out.String(), "\x1b\r") {
		t.Fatalf("unsafe error: %q", out.String())
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runNativeStatus([]string{path}, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "branch: unknown") || !strings.Contains(out.String(), "private") {
		t.Fatalf("public cache trusted: %q", out.String())
	}
}

type statusExecutor struct {
	ssh.Executor
	run           func(context.Context, string, string) (string, string, int, error)
	ordinaryCalls atomic.Int32
}

func (s *statusExecutor) RunBash(ctx context.Context, command, cwd string) (string, string, int, error) {
	s.ordinaryCalls.Add(1)
	return s.run(ctx, command, cwd)
}

func (s *statusExecutor) RunBashIfConnected(ctx context.Context, command, cwd string) (string, string, int, error) {
	return s.run(ctx, command, cwd)
}

func TestNativeStatusSamplingClearsSuccess(t *testing.T) {
	var calls int
	executor := &statusExecutor{run: func(ctx context.Context, command, cwd string) (string, string, int, error) {
		if cwd != "/workspaces/pinned" {
			t.Fatalf("redirected cwd: %q", cwd)
		}
		if strings.Contains(command, "diff") || strings.Contains(command, "status") {
			t.Fatalf("expensive query: %q", command)
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("probe has no timeout")
		}
		calls++
		switch calls {
		case 1:
			return "live-topic\n", "", 0, nil
		case 2:
			return "detached@abc1234\n", "", 0, nil
		case 3:
			return "live-topic\n", "fatal: not a git repository", 128, nil
		case 4:
			return "live-topic\n", "", 0, errors.New("connection lost")
		default:
			return "", "cwd missing", 1, nil
		}
	}}
	reg, cs, path := statusFixture(t, executor)
	s, err := newNativeStatusSampler(reg, cs.Alias, path, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	initial := readStatus(t, path)
	if initial.Branch != "" || !strings.Contains(initial.Error, "initializing") {
		t.Fatalf("initial: %+v", initial)
	}
	cs.Workdir = "/redirected/by/remote_cd"
	for _, want := range []string{"live-topic", "detached@abc1234", "", "", ""} {
		got := s.sample(context.Background())
		if got.Branch != want || got.Cwd != "/workspaces/pinned" || got.CheckedAt.IsZero() || (want == "" && got.Error == "") {
			t.Fatalf("sample: %+v, want %q", got, want)
		}
		if err := s.publish(got); err != nil {
			t.Fatal(err)
		}
		if disk := readStatus(t, path); disk.Branch != want {
			t.Fatalf("old success survived: %+v", disk)
		}
	}
	if executor.ordinaryCalls.Load() != 0 {
		t.Fatal("status used the ordinary command path, which can wake or reconnect")
	}
}

func TestNativeStatusRegistryIdentityAndNoDirectSSH(t *testing.T) {
	var calls int
	executor := &statusExecutor{run: func(context.Context, string, string) (string, string, int, error) {
		calls++
		return "topic", "", 0, nil
	}}
	reg, cs, path := statusFixture(t, executor)
	s, err := newNativeStatusSampler(reg, cs.Alias, path, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	reg.Deregister(cs.Alias)
	if got := s.sample(context.Background()); got.Branch != "" || !strings.Contains(got.Error, "removed") {
		t.Fatalf("removed: %+v", got)
	}
	replacement := *cs
	if err := reg.Register(&replacement); err != nil {
		t.Fatal(err)
	}
	if got := s.sample(context.Background()); got.Branch != "" || !strings.Contains(got.Error, "replaced") {
		t.Fatalf("replaced: %+v", got)
	}
	if calls != 0 {
		t.Fatal("probed a removed/replaced target")
	}
	// Exposing only ssh.Executor hides the optional passive-query capability.
	reg, cs, path = statusFixture(t, struct{ ssh.Executor }{executor})
	s, err = newNativeStatusSampler(reg, cs.Alias, path, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.sample(context.Background()); got.Branch != "" || !strings.Contains(got.Error, "passive status") {
		t.Fatalf("unsupported executor used: %+v", got)
	}
	if calls != 0 {
		t.Fatal("fell back to an ordinary recovery-capable query")
	}
	reg, cs, path = statusFixture(t, ssh.NewClient("must-never-connect"))
	s, err = newNativeStatusSampler(reg, cs.Alias, path, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	got := s.sample(context.Background())
	if got.Branch != "" || !strings.Contains(got.Error, "direct SSH polling disabled") {
		t.Fatalf("direct transport used: %+v", got)
	}
}

func readStatus(t *testing.T, path string) nativeStatusCache {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var status nativeStatusCache
	if err := json.Unmarshal(data, &status); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("cache not private: %v, %v", info, err)
	}
	return status
}

func TestNativeStatusCancellationJoinsAndWritesDisconnected(t *testing.T) {
	entered := make(chan struct{})
	finished := make(chan struct{})
	var calls atomic.Int32
	executor := &statusExecutor{run: func(ctx context.Context, _, _ string) (string, string, int, error) {
		calls.Add(1)
		close(entered)
		<-ctx.Done()
		close(finished)
		return "", "", -1, ctx.Err()
	}}
	reg, cs, path := statusFixture(t, executor)
	s, err := newNativeStatusSampler(reg, cs.Alias, path, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	stop := s.start(context.Background())
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("no immediate sample")
	}
	// Workdir may mutate concurrently; sampling must never read it after pinning.
	for i := 0; i < 100; i++ {
		cs.Workdir = "/changed"
	}
	stop()
	stop() // idempotent join
	select {
	case <-finished:
	default:
		t.Fatal("stop returned before probe joined")
	}
	got := readStatus(t, path)
	if got.Branch != "" || !strings.Contains(got.Error, "disconnected") || calls.Load() != 1 {
		t.Fatalf("shutdown: %+v, calls %d", got, calls.Load())
	}
}

func TestNativeStatusHostEOFStopsSampler(t *testing.T) {
	executor := &statusExecutor{run: func(ctx context.Context, _, _ string) (string, string, int, error) {
		<-ctx.Done()
		return "", "", -1, ctx.Err()
	}}
	reg, cs, path := statusFixture(t, executor)
	s, err := newNativeStatusSampler(reg, cs.Alias, path, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	parent := context.Background()
	rt := &selectiveBlockRuntime{blockTools: map[string]chan struct{}{}}
	if err := runExtensionHostWithNativeStatus(parent, strings.NewReader(""), io.Discard, rt, nil, s); err != nil {
		t.Fatal(err)
	}
	if parent.Err() != nil {
		t.Fatal("test requires live original context")
	}
	if got := readStatus(t, path); !strings.Contains(got.Error, "disconnected") {
		t.Fatalf("EOF leaked sampler: %+v", got)
	}
}

func TestNativeStatusPollingIsIndependentOfPainting(t *testing.T) {
	var calls, active atomic.Int32
	second := make(chan struct{})
	executor := &statusExecutor{run: func(ctx context.Context, _, _ string) (string, string, int, error) {
		if active.Add(1) != 1 {
			t.Error("overlapping remote probes")
		}
		defer active.Add(-1)
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 2*time.Second {
			t.Error("probe is not bounded to two seconds")
		}
		if calls.Add(1) == 1 {
			return "polled-topic", "", 0, nil
		}
		close(second)
		<-ctx.Done()
		return "", "", -1, ctx.Err()
	}}
	reg, cs, path := statusFixture(t, executor)
	s, err := newNativeStatusSampler(reg, cs.Alias, path, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	stop := s.start(context.Background())
	defer stop()
	until := time.Now().Add(time.Second)
	for readStatus(t, path).Branch != "polled-topic" {
		if time.Now().After(until) {
			t.Fatal("first sample was not immediate")
		}
		time.Sleep(time.Millisecond)
	}
	for i := 0; i < 25; i++ {
		var out bytes.Buffer
		if err := runNativeStatus([]string{path}, strings.NewReader(`{"git":{"branch":"LOCAL"}}`), &out); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "polled-topic") {
			t.Fatalf("fresh cached result missing: %q", out.String())
		}
	}
	if calls.Load() != 1 {
		t.Fatal("UI paints triggered remote work")
	}
	select {
	case <-second:
	case <-time.After(2 * time.Second):
		t.Fatal("autonomous refresh exceeded two seconds")
	}
	// Span another sampling tick while the second query is still in flight.
	time.Sleep(1250 * time.Millisecond)
	if calls.Load() != 2 || active.Load() != 1 {
		t.Fatalf("slow query was overlapped: calls=%d active=%d", calls.Load(), active.Load())
	}
	stop()
	if calls.Load() != 2 || active.Load() != 0 {
		t.Fatalf("probes not joined: calls=%d active=%d", calls.Load(), active.Load())
	}
}

func TestNativeStatusCacheWriteFailures(t *testing.T) {
	executor := &statusExecutor{run: func(context.Context, string, string) (string, string, int, error) { return "topic", "", 0, nil }}
	reg, cs, path := statusFixture(t, executor)
	if _, err := newNativeStatusSampler(reg, cs.Alias, filepath.Join(path, "missing", "status.json"), io.Discard); err == nil {
		t.Fatal("initial cache failure ignored")
	}
	var stderr bytes.Buffer
	s, err := newNativeStatusSampler(reg, cs.Alias, path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	stop := s.start(context.Background())
	stop()
	if !strings.Contains(stderr.String(), "native status cache") {
		t.Fatalf("write error not surfaced: %q", stderr.String())
	}
}

func TestNativeStatusBranchChangeDisposableFixture(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("fixture git %v: %v: %s", args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	git("init", "-b", "fixture-one")
	git("-c", "user.name=Status Fixture", "-c", "user.email=status@example.invalid", "commit", "--allow-empty", "-m", "fixture")
	executor := &statusExecutor{run: func(ctx context.Context, command, cwd string) (string, string, int, error) {
		cmd := exec.CommandContext(ctx, "bash", "-c", command)
		cmd.Dir = cwd
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		if exit, ok := err.(*exec.ExitError); ok {
			return stdout.String(), stderr.String(), exit.ExitCode(), nil
		}
		return stdout.String(), stderr.String(), 0, err
	}}
	reg, cs, path := statusFixture(t, executor)
	cs.Workdir = dir
	s, err := newNativeStatusSampler(reg, cs.Alias, path, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	stop := s.start(context.Background())
	defer stop()
	awaitVisible := func(branch string) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for {
			var out bytes.Buffer
			if err := runNativeStatus([]string{path}, strings.NewReader(`{"git":{"branch":"LOCAL"}}`), &out); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out.String(), "branch: "+branch+" |") {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("branch %q not visible within two seconds: %s", branch, out.String())
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	awaitVisible("fixture-one")
	changed := time.Now()
	git("switch", "-c", "fixture-two")
	awaitVisible("fixture-two")
	t.Logf("actual Git change to cache-rendered branch: %s", time.Since(changed))
	stop()
	commit := git("rev-parse", "--short", "HEAD")
	git("switch", "--detach")
	if got := s.sample(context.Background()); got.Branch != "detached@"+commit {
		t.Fatalf("detached: %+v", got)
	}
	cs.Workdir = "/mutable-do-not-follow"
	// Actual Git failure in a disposable non-repository, not a fake default.
	other := t.TempDir()
	reg, cs, path = statusFixture(t, executor)
	cs.Workdir = other
	s, err = newNativeStatusSampler(reg, cs.Alias, path, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.sample(context.Background()); got.Branch != "" || got.Error == "" {
		t.Fatalf("non-repository: %+v", got)
	}
	reg, cs, path = statusFixture(t, executor)
	cs.Workdir = filepath.Join(t.TempDir(), "missing-cwd")
	s, err = newNativeStatusSampler(reg, cs.Alias, path, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.sample(context.Background()); got.Branch != "" || got.Error == "" {
		t.Fatalf("missing cwd: %+v", got)
	}
}
