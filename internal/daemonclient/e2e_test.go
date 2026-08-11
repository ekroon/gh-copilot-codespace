package daemonclient_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ekroon/gh-copilot-codespace/internal/daemonclient"
	"github.com/ekroon/gh-copilot-codespace/internal/daemontransport"
	"github.com/ekroon/gh-copilot-codespace/internal/mcp"
	"github.com/ekroon/gh-copilot-codespace/internal/registry"
)

var e2eChdirMu sync.Mutex

type e2eCodespace struct {
	alias string
	name  string
	cwd   string
	exec  *daemonclient.Executor
}

func newE2ERuntime(t *testing.T, spaces ...e2eCodespace) *mcp.ToolRuntime {
	t.Helper()
	reg := registry.New()
	for _, space := range spaces {
		name := space.name
		if name == "" {
			name = space.alias + "-cs"
		}
		if err := reg.Register(&registry.ManagedCodespace{
			Alias:    space.alias,
			Name:     name,
			Workdir:  space.cwd,
			Executor: space.exec,
		}); err != nil {
			t.Fatalf("register %s: %v", space.alias, err)
		}
	}
	return mcp.NewToolRuntime(reg, mcp.LifecycleConfig{})
}

func dialDaemonForE2E(t *testing.T, cwd string) *daemonclient.Executor {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	var oldwd string
	if cwd != "" {
		e2eChdirMu.Lock()
		var err error
		oldwd, err = os.Getwd()
		if err != nil {
			e2eChdirMu.Unlock()
			t.Fatalf("Getwd: %v", err)
		}
		if err := os.Chdir(cwd); err != nil {
			e2eChdirMu.Unlock()
			t.Fatalf("Chdir %s: %v", cwd, err)
		}
	}

	e, err := daemonclient.Dial(ctx, daemontransport.NewLocalTransport(daemonclient.DaemonBinaryForTests()))

	if cwd != "" {
		if chdirErr := os.Chdir(oldwd); chdirErr != nil && err == nil {
			err = fmt.Errorf("restore cwd: %w", chdirErr)
		}
		e2eChdirMu.Unlock()
	}
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if cwd != "" {
		e.SetWorkdir(cwd)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e
}

func singleCodespaceRuntime(t *testing.T) (*mcp.ToolRuntime, *daemonclient.Executor) {
	t.Helper()
	dir := daemonclient.TempDirForTests(t)
	e := dialDaemonForE2E(t, dir)
	return newE2ERuntime(t, e2eCodespace{alias: "test", cwd: dir, exec: e}), e
}

func findRuntimeTool(t *testing.T, runtime *mcp.ToolRuntime, candidates ...string) string {
	t.Helper()
	defs := runtime.Definitions()
	byName := make(map[string]mcp.ToolDefinition, len(defs))
	for _, def := range defs {
		byName[def.Name] = def
	}
	for _, name := range candidates {
		if _, ok := byName[name]; ok {
			return name
		}
	}
	names := make([]string, 0, len(defs))
	for _, def := range defs {
		names = append(names, def.Name)
	}
	t.Fatalf("none of tools %v found in runtime definitions %v", candidates, names)
	return ""
}

func requireRuntimeToolArgs(t *testing.T, runtime *mcp.ToolRuntime, toolName string, required ...string) {
	t.Helper()
	for _, def := range runtime.Definitions() {
		if def.Name != toolName {
			continue
		}
		if def.Parameters.Type != "object" {
			t.Fatalf("%s parameters type = %q, want object", toolName, def.Parameters.Type)
		}
		for _, name := range required {
			if _, ok := def.Parameters.Properties[name]; !ok {
				t.Fatalf("%s parameters missing %q property", toolName, name)
			}
		}
		return
	}
	t.Fatalf("tool %s not found", toolName)
}

func callRuntime(t *testing.T, runtime *mcp.ToolRuntime, toolName string, args map[string]any) mcp.RuntimeCallResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := runtime.Call(ctx, toolName, args)
	if err != nil {
		t.Fatalf("runtime.Call(%s): %v", toolName, err)
	}
	return result
}

func requireSuccess(t *testing.T, result mcp.RuntimeCallResult) {
	t.Helper()
	if result.ResultType != "success" {
		t.Fatalf("result type = %q, want success; text: %s", result.ResultType, result.TextResultForLlm)
	}
}

func TestE2E_ToolRuntimeViewFileViaDaemon(t *testing.T) {
	runtime, _ := singleCodespaceRuntime(t)
	viewTool := findRuntimeTool(t, runtime, "remote_view", "view", "view_file")
	requireRuntimeToolArgs(t, runtime, viewTool, "path")

	path := filepath.Join(daemonclient.TempDirForTests(t), "multiline.txt")
	if err := os.WriteFile(path, []byte("alpha\nbeta\ngamma\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	result := callRuntime(t, runtime, viewTool, map[string]any{"path": path})
	requireSuccess(t, result)
	if !strings.Contains(result.TextResultForLlm, "1. alpha\n2. beta\n3. gamma\n") {
		t.Fatalf("view text = %q, want daemon line-numbered content", result.TextResultForLlm)
	}
}

func TestE2E_ToolRuntimeRunBashViaDaemon(t *testing.T) {
	runtime, exec := singleCodespaceRuntime(t)
	bashTool := findRuntimeTool(t, runtime, "remote_bash", "run_bash", "bash")
	requireRuntimeToolArgs(t, runtime, bashTool, "command")
	const shellID = "e2e-echo-hello"
	t.Cleanup(func() { _ = exec.StopSession(context.Background(), shellID) })

	result := callRuntime(t, runtime, bashTool, map[string]any{
		"command":      "echo hello",
		"shellId":      shellID,
		"initial_wait": 1.0,
	})
	requireSuccess(t, result)
	if !strings.Contains(result.TextResultForLlm, "hello") {
		t.Fatalf("bash text = %q, want hello", result.TextResultForLlm)
	}
	if strings.Contains(result.TextResultForLlm, "[exit code:") {
		t.Fatalf("bash text = %q, want zero exit without non-zero exit marker", result.TextResultForLlm)
	}
}

func TestE2E_ToolRuntimeRunBashExitCodeNonZero(t *testing.T) {
	runtime, exec := singleCodespaceRuntime(t)
	bashTool := findRuntimeTool(t, runtime, "remote_bash", "run_bash", "bash")
	const shellID = "e2e-exit-42"
	t.Cleanup(func() { _ = exec.StopSession(context.Background(), shellID) })

	result := callRuntime(t, runtime, bashTool, map[string]any{
		"command":      "exit 42",
		"shellId":      shellID,
		"initial_wait": 1.0,
	})
	requireSuccess(t, result)
	if !strings.Contains(result.TextResultForLlm, "[exit code: 42]") {
		t.Fatalf("bash text = %q, want exit code 42", result.TextResultForLlm)
	}
}

func TestE2E_ToolRuntimeCreateEditViewRoundTrip(t *testing.T) {
	runtime, _ := singleCodespaceRuntime(t)
	createTool := findRuntimeTool(t, runtime, "remote_create", "create_file", "create")
	editTool := findRuntimeTool(t, runtime, "remote_edit", "edit_file", "edit")
	viewTool := findRuntimeTool(t, runtime, "remote_view", "view", "view_file")
	requireRuntimeToolArgs(t, runtime, createTool, "path", "file_text")
	requireRuntimeToolArgs(t, runtime, editTool, "path", "old_str", "new_str")

	path := filepath.Join(daemonclient.TempDirForTests(t), "nested", "roundtrip.txt")
	createResult := callRuntime(t, runtime, createTool, map[string]any{"path": path, "file_text": "hello world\n"})
	requireSuccess(t, createResult)
	editResult := callRuntime(t, runtime, editTool, map[string]any{"path": path, "old_str": "world", "new_str": "daemon"})
	requireSuccess(t, editResult)

	disk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(disk) != "hello daemon\n" {
		t.Fatalf("disk content = %q, want edited content", string(disk))
	}

	viewResult := callRuntime(t, runtime, viewTool, map[string]any{"path": path})
	requireSuccess(t, viewResult)
	if !strings.Contains(viewResult.TextResultForLlm, "1. hello daemon\n") {
		t.Fatalf("view text = %q, want edited content", viewResult.TextResultForLlm)
	}
}

func TestE2E_ToolRuntimeConcurrentCalls(t *testing.T) {
	runtime, _ := singleCodespaceRuntime(t)
	viewTool := findRuntimeTool(t, runtime, "remote_view", "view", "view_file")
	path := filepath.Join(daemonclient.TempDirForTests(t), "shared.txt")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	want := "1. one\n2. two\n3. three\n"

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	errs := make(chan error, 20)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			result, err := runtime.Call(ctx, viewTool, map[string]any{"path": path})
			if err != nil {
				errs <- fmt.Errorf("call %d: %w", i, err)
				return
			}
			if result.ResultType != "success" {
				errs <- fmt.Errorf("call %d result type = %q text=%q", i, result.ResultType, result.TextResultForLlm)
				return
			}
			if result.TextResultForLlm != want {
				errs <- fmt.Errorf("call %d text = %q, want %q", i, result.TextResultForLlm, want)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestE2E_ContextCancelPropagatesThroughRuntime(t *testing.T) {
	runtime, _ := singleCodespaceRuntime(t)
	bashTool := findRuntimeTool(t, runtime, "remote_bash", "run_bash", "bash")

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	result, err := runtime.Call(ctx, bashTool, map[string]any{
		"command": "sleep 30",
		// Force the sync fallback path so ToolRuntime waits on daemon RunBash,
		// letting the context cancel reach the daemon process group.
		"shellId": "force-fallback\x00",
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("runtime.Call(%s): %v", bashTool, err)
	}
	if elapsed >= 3*time.Second {
		t.Fatalf("context cancellation took %v, want < 3s", elapsed)
	}
	if result.ResultType != "failure" {
		t.Fatalf("result type = %q, want failure; text: %s", result.ResultType, result.TextResultForLlm)
	}
	if !strings.Contains(result.TextResultForLlm, "context deadline exceeded") && !strings.Contains(result.TextResultForLlm, "context canceled") {
		t.Fatalf("result text = %q, want context cancellation", result.TextResultForLlm)
	}
}

func TestE2E_MultipleCodespacesIndependentDaemons(t *testing.T) {
	dirA := daemonclient.TempDirForTests(t)
	dirB := daemonclient.TempDirForTests(t)
	if err := os.WriteFile(filepath.Join(dirA, "fixture.txt"), []byte("alpha daemon\n"), 0o644); err != nil {
		t.Fatalf("WriteFile alpha: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dirB, "fixture.txt"), []byte("beta daemon\n"), 0o644); err != nil {
		t.Fatalf("WriteFile beta: %v", err)
	}

	execA := dialDaemonForE2E(t, dirA)
	execB := dialDaemonForE2E(t, dirB)
	runtime := newE2ERuntime(t,
		e2eCodespace{alias: "alpha", name: "alpha-cs", cwd: dirA, exec: execA},
		e2eCodespace{alias: "beta", name: "beta-cs", cwd: dirB, exec: execB},
	)
	viewTool := findRuntimeTool(t, runtime, "remote_view", "view", "view_file")

	alpha := callRuntime(t, runtime, viewTool, map[string]any{"codespace": "alpha", "path": "fixture.txt"})
	requireSuccess(t, alpha)
	if alpha.TextResultForLlm != "1. alpha daemon\n" {
		t.Fatalf("alpha view = %q, want alpha daemon", alpha.TextResultForLlm)
	}
	beta := callRuntime(t, runtime, viewTool, map[string]any{"codespace": "beta", "path": "fixture.txt"})
	requireSuccess(t, beta)
	if beta.TextResultForLlm != "1. beta daemon\n" {
		t.Fatalf("beta view = %q, want beta daemon", beta.TextResultForLlm)
	}
}
