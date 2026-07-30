package ssh

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestViewRequestLegacyViewRangeNormalizesInvalidRanges(t *testing.T) {
	req := ViewRequest{Path: "/workspaces/repo/main.go", ViewRange: []int{7}}

	if got := req.LegacyViewRange(); got != nil {
		t.Fatalf("LegacyViewRange() = %v, want nil", got)
	}
}

func TestGrepRequestNormalizePreservesLegacyDefaults(t *testing.T) {
	got := (GrepRequest{Pattern: "needle"}).Normalize()

	if got.OutputMode != GrepOutputModeContent {
		t.Fatalf("OutputMode = %q, want %q", got.OutputMode, GrepOutputModeContent)
	}
	if !reflect.DeepEqual(got.Paths, []string{"."}) {
		t.Fatalf("Paths = %v, want [.]", got.Paths)
	}
	if got.Path != "." {
		t.Fatalf("Path = %q, want \".\"", got.Path)
	}
	if got.LineNumbers == nil || !*got.LineNumbers {
		t.Fatalf("LineNumbers = %v, want true", got.LineNumbers)
	}
}

func TestGrepRequestLegacyPathRejectsMultiplePaths(t *testing.T) {
	_, err := (GrepRequest{
		Pattern: "needle",
		Paths:   []string{"cmd", "internal"},
	}).LegacyPath()

	if !errors.Is(err, ErrMultiplePathsUnsupported) {
		t.Fatalf("LegacyPath() error = %v, want %v", err, ErrMultiplePathsUnsupported)
	}
}

func TestFilesystemSafetyLimitsRemainBounded(t *testing.T) {
	if MaxFileTransferBytes != 16*1024*1024 {
		t.Fatalf("MaxFileTransferBytes = %d, want 16 MiB", MaxFileTransferBytes)
	}
	if MaxViewBytes <= 0 || MaxDirectoryEntries <= 0 || MaxGrepOutputBytes <= 0 || MaxGrepInputBytes <= 0 {
		t.Fatalf("invalid bounds: view=%d directory=%d grep_output=%d grep_input=%d", MaxViewBytes, MaxDirectoryEntries, MaxGrepOutputBytes, MaxGrepInputBytes)
	}
	if MaxGrepInputBytes != MaxFileTransferBytes {
		t.Fatalf("MaxGrepInputBytes = %d, want shared file-transfer bound %d", MaxGrepInputBytes, MaxFileTransferBytes)
	}
}

func TestGlobRequestNormalizePreservesLegacyDefaults(t *testing.T) {
	got := (GlobRequest{Pattern: "**/*.go"}).Normalize()

	if !reflect.DeepEqual(got.Paths, []string{"."}) {
		t.Fatalf("Paths = %v, want [.]", got.Paths)
	}
	if got.Path != "." {
		t.Fatalf("Path = %q, want \".\"", got.Path)
	}
	if got.Limit != DefaultGlobLimit {
		t.Fatalf("Limit = %d, want %d", got.Limit, DefaultGlobLimit)
	}
}

func TestGlobRequestNormalizeClampsLimit(t *testing.T) {
	got := (GlobRequest{Pattern: "**/*.go", Limit: MaxGlobLimit + 1}).Normalize()

	if got.Limit != MaxGlobLimit {
		t.Fatalf("Limit = %d, want %d", got.Limit, MaxGlobLimit)
	}
}

type legacyViewRecorder struct {
	path      string
	viewRange []int
}

func (e *legacyViewRecorder) ViewFile(_ context.Context, path string, viewRange []int) (string, error) {
	e.path = path
	e.viewRange = append([]int(nil), viewRange...)
	return "1. package main\n", nil
}

type legacyGrepRecorder struct {
	pattern string
	path    string
	glob    string
	cwd     string
}

func (e *legacyGrepRecorder) Grep(_ context.Context, pattern, path, glob, cwd string) (string, error) {
	e.pattern = pattern
	e.path = path
	e.glob = glob
	e.cwd = cwd
	return "cmd/main.go:3:needle\n", nil
}

type legacyGlobRecorder struct {
	pattern string
	path    string
	cwd     string
}

func (e *legacyGlobRecorder) Glob(_ context.Context, pattern, path, cwd string) (string, error) {
	e.pattern = pattern
	e.path = path
	e.cwd = cwd
	return "internal/ssh/client.go\n", nil
}

type patchExecutor struct {
	req ApplyPatchRequest
}

func (e *patchExecutor) ApplyPatch(_ context.Context, req ApplyPatchRequest) (ApplyPatchResult, error) {
	e.req = req
	return ApplyPatchResult{Output: "applied", FilesChanged: 1}, nil
}

func TestExecuteViewFallsBackToLegacyExecutor(t *testing.T) {
	exec := &legacyViewRecorder{}

	got, err := ExecuteView(context.Background(), exec, ViewRequest{
		Path:      "/workspaces/repo/main.go",
		ViewRange: []int{2, 4},
	})
	if err != nil {
		t.Fatalf("ExecuteView() error = %v", err)
	}

	if exec.path != "/workspaces/repo/main.go" {
		t.Fatalf("ViewFile path = %q", exec.path)
	}
	if !reflect.DeepEqual(exec.viewRange, []int{2, 4}) {
		t.Fatalf("ViewFile range = %v, want [2 4]", exec.viewRange)
	}
	if got.Kind != ViewKindFile || got.Content != "1. package main\n" {
		t.Fatalf("ExecuteView() = %+v", got)
	}
}

func TestExecuteGrepFallsBackToLegacyExecutor(t *testing.T) {
	exec := &legacyGrepRecorder{}

	got, err := ExecuteGrep(context.Background(), exec, GrepRequest{
		Pattern: "needle",
		Paths:   []string{"cmd"},
		Glob:    "*.go",
		Cwd:     "/workspaces/repo",
	})
	if err != nil {
		t.Fatalf("ExecuteGrep() error = %v", err)
	}

	if exec.pattern != "needle" || exec.path != "cmd" || exec.glob != "*.go" || exec.cwd != "/workspaces/repo" {
		t.Fatalf("legacy grep args = %+v", exec)
	}
	if got.Output != "cmd/main.go:3:needle\n" {
		t.Fatalf("Output = %q", got.Output)
	}
	if got.OutputMode != GrepOutputModeContent {
		t.Fatalf("OutputMode = %q, want %q", got.OutputMode, GrepOutputModeContent)
	}
	if !reflect.DeepEqual(got.Paths, []string{"cmd"}) {
		t.Fatalf("Paths = %v, want [cmd]", got.Paths)
	}
}

func TestExecuteGlobFallsBackToLegacyExecutor(t *testing.T) {
	exec := &legacyGlobRecorder{}

	got, err := ExecuteGlob(context.Background(), exec, GlobRequest{
		Pattern: "**/*.go",
		Paths:   []string{"internal"},
		Cwd:     "/workspaces/repo",
	})
	if err != nil {
		t.Fatalf("ExecuteGlob() error = %v", err)
	}

	if exec.pattern != "**/*.go" || exec.path != "internal" || exec.cwd != "/workspaces/repo" {
		t.Fatalf("legacy glob args = %+v", exec)
	}
	if got.Output != "internal/ssh/client.go\n" {
		t.Fatalf("Output = %q", got.Output)
	}
	if !reflect.DeepEqual(got.Paths, []string{"internal"}) {
		t.Fatalf("Paths = %v, want [internal]", got.Paths)
	}
}

func TestExecuteApplyPatchRequiresOptionalExecutor(t *testing.T) {
	_, err := ExecuteApplyPatch(context.Background(), struct{}{}, ApplyPatchRequest{
		Patch: "*** Begin Patch\n*** End Patch\n",
	})

	if !errors.Is(err, ErrApplyPatchUnsupported) {
		t.Fatalf("ExecuteApplyPatch() error = %v, want %v", err, ErrApplyPatchUnsupported)
	}
}

func TestExecuteApplyPatchUsesOptionalExecutor(t *testing.T) {
	exec := &patchExecutor{}

	got, err := ExecuteApplyPatch(context.Background(), exec, ApplyPatchRequest{
		Patch: "*** Begin Patch\n*** End Patch\n",
		Cwd:   "/workspaces/repo",
	})
	if err != nil {
		t.Fatalf("ExecuteApplyPatch() error = %v", err)
	}

	if exec.req.Patch == "" || exec.req.Cwd != "/workspaces/repo" {
		t.Fatalf("ApplyPatch req = %+v", exec.req)
	}
	if got.Output != "applied" || got.FilesChanged != 1 {
		t.Fatalf("ExecuteApplyPatch() = %+v", got)
	}
}
