package daemonclient

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ekroon/gh-copilot-codespace/internal/daemonproto"
	"github.com/ekroon/gh-copilot-codespace/internal/ssh"
)

func TestExecutorViewStructuredRoundTrip(t *testing.T) {
	e := dialDaemon(t)
	dir := testDir(t)
	if err := os.WriteFile(filepath.Join(dir, "root.txt"), []byte("root\n"), 0o644); err != nil {
		t.Fatalf("WriteFile root.txt: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatalf("Mkdir sub: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "child.txt"), []byte("child\n"), 0o644); err != nil {
		t.Fatalf("WriteFile sub/child.txt: %v", err)
	}

	dirResult, err := e.View(testContext(t), ssh.ViewRequest{Path: dir})
	if err != nil {
		t.Fatalf("View(dir): %v", err)
	}
	if dirResult.Kind != ssh.ViewKindDirectory {
		t.Fatalf("Kind = %q, want %q", dirResult.Kind, ssh.ViewKindDirectory)
	}
	wantEntries := []string{"root.txt", "sub/", "sub/child.txt"}
	if !reflect.DeepEqual(dirResult.Entries, wantEntries) {
		t.Fatalf("Entries = %v, want %v", dirResult.Entries, wantEntries)
	}

	textResult, err := e.View(testContext(t), ssh.ViewRequest{Path: filepath.Join(dir, "root.txt")})
	if err != nil {
		t.Fatalf("View(text): %v", err)
	}
	if textResult.Content != "1. root\n" {
		t.Fatalf("text content = %q, want legacy plain output", textResult.Content)
	}
	if textResult.Kind != ssh.ViewKindFile || textResult.MimeType != "text/plain" {
		t.Fatalf("text metadata = %+v, want file/text fields separate from content", textResult)
	}
	if strings.Contains(textResult.Content, `"kind"`) || strings.Contains(textResult.Content, `"mime_type"`) {
		t.Fatalf("text content includes structured metadata: %q", textResult.Content)
	}

	pngData, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO7+4xoAAAAASUVORK5CYII=")
	if err != nil {
		t.Fatalf("DecodeString png: %v", err)
	}
	imagePath := filepath.Join(dir, "pixel.png")
	if err := os.WriteFile(imagePath, pngData, 0o644); err != nil {
		t.Fatalf("WriteFile pixel.png: %v", err)
	}
	imageResult, err := e.View(testContext(t), ssh.ViewRequest{Path: imagePath})
	if err != nil {
		t.Fatalf("View(image): %v", err)
	}
	if imageResult.Kind != ssh.ViewKindImage || imageResult.MimeType != "image/png" || imageResult.Base64Data == "" {
		t.Fatalf("imageResult = %+v", imageResult)
	}
}

func TestExecutorWriteFileRejectsOversizedPayloadBeforeTransmission(t *testing.T) {
	e := &Executor{}
	err := e.WriteFileRooted(context.Background(), ssh.RootedWriteRequest{
		Path: "large.bin",
		Root: ".",
		Data: make([]byte, daemonproto.MaxFileTransferBytes+1),
	})
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("WriteFileRooted() error = %v, want size rejection", err)
	}
	if !errors.Is(err, daemonproto.ErrFileTransferTooLarge) {
		t.Fatalf("WriteFileRooted() error = %v, want ErrFileTransferTooLarge", err)
	}
}

func TestExecutorStructuredGrepAndGlobFallbackRoundTrip(t *testing.T) {
	t.Setenv("PATH", "")
	e := dialDaemon(t)
	root := testDir(t)
	dirOne := filepath.Join(root, "one")
	dirTwo := filepath.Join(root, "two")
	for _, dir := range []string{filepath.Join(dirOne, "sub"), filepath.Join(dirTwo, "sub"), filepath.Join(dirTwo, ".git")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dirOne, "sub", "match.go"), []byte("Alpha\nBeta\n"), 0o644); err != nil {
		t.Fatalf("WriteFile dirOne match.go: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dirOne, "sub", "skip.txt"), []byte("Alpha\nBeta\n"), 0o644); err != nil {
		t.Fatalf("WriteFile dirOne skip.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dirTwo, "sub", "other.go"), []byte("ALPHA\nBETA\n"), 0o644); err != nil {
		t.Fatalf("WriteFile dirTwo other.go: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dirTwo, ".git", "ignored.go"), []byte("ignored\n"), 0o644); err != nil {
		t.Fatalf("WriteFile ignored.go: %v", err)
	}

	grepResult, err := e.GrepFiles(testContext(t), ssh.GrepRequest{
		Pattern:         "alpha\\nbeta",
		Paths:           []string{dirOne, dirTwo},
		Glob:            "*.go",
		OutputMode:      ssh.GrepOutputModeFilesWithMatches,
		CaseInsensitive: true,
		HeadLimit:       1,
		Multiline:       true,
	})
	if err != nil {
		t.Fatalf("GrepFiles: %v", err)
	}
	if grepResult.OutputMode != ssh.GrepOutputModeFilesWithMatches {
		t.Fatalf("OutputMode = %q, want %q", grepResult.OutputMode, ssh.GrepOutputModeFilesWithMatches)
	}
	if len(strings.Fields(strings.TrimSpace(grepResult.Output))) != 1 {
		t.Fatalf("grep output = %q, want 1 match due to head_limit", grepResult.Output)
	}

	globResult, err := e.GlobFiles(testContext(t), ssh.GlobRequest{
		Pattern: "**/*.{go,txt}",
		Paths:   []string{dirOne, dirTwo},
	})
	if err != nil {
		t.Fatalf("GlobFiles: %v", err)
	}
	gotGlob := strings.Fields(strings.TrimSpace(globResult.Output))
	wantGlob := []string{
		filepath.Join(dirOne, "sub", "match.go"),
		filepath.Join(dirOne, "sub", "skip.txt"),
		filepath.Join(dirTwo, "sub", "other.go"),
	}
	if !reflect.DeepEqual(gotGlob, wantGlob) {
		t.Fatalf("glob matches = %v, want %v", gotGlob, wantGlob)
	}
}

func TestExecutorApplyPatchStructuredUsesDefaultWorkdir(t *testing.T) {
	e := dialDaemon(t)
	dir := testDir(t)
	e.SetWorkdir(dir)
	if err := os.WriteFile(filepath.Join(dir, "old.txt"), []byte("before\nvalue\nafter\n"), 0o644); err != nil {
		t.Fatalf("WriteFile old.txt: %v", err)
	}

	patch := strings.Join([]string{
		"*** Begin Patch",
		"*** Add File: added.txt",
		"+hello",
		"*** Update File: old.txt",
		"@@",
		" before",
		"-value",
		"+VALUE",
		" after",
		"*** End Patch",
		"",
	}, "\n")

	got, err := e.ApplyPatch(context.Background(), ssh.ApplyPatchRequest{Patch: patch})
	if err != nil {
		t.Fatalf("ApplyPatch: %v", err)
	}
	if got.FilesChanged != 2 {
		t.Fatalf("FilesChanged = %d, want 2", got.FilesChanged)
	}
	if !strings.Contains(got.Output, "added.txt") || !strings.Contains(got.Output, "old.txt") {
		t.Fatalf("Output = %q", got.Output)
	}
	updated, err := os.ReadFile(filepath.Join(dir, "old.txt"))
	if err != nil {
		t.Fatalf("ReadFile old.txt: %v", err)
	}
	if string(updated) != "before\nVALUE\nafter\n" {
		t.Fatalf("old.txt = %q", string(updated))
	}
	added, err := os.ReadFile(filepath.Join(dir, "added.txt"))
	if err != nil {
		t.Fatalf("ReadFile added.txt: %v", err)
	}
	if string(added) != "hello\n" {
		t.Fatalf("added.txt = %q", string(added))
	}
}

func TestExecutorRelativeFilesystemPathsUseUpdatedWorkdir(t *testing.T) {
	e := dialDaemon(t)
	first := testDir(t)
	second := testDir(t)

	e.SetWorkdir(first)
	if err := e.CreateFile(testContext(t), "nested/file.txt", "first\n"); err != nil {
		t.Fatalf("CreateFile(first): %v", err)
	}

	e.SetWorkdir(second)
	if err := e.CreateFile(testContext(t), "nested/file.txt", "before\n"); err != nil {
		t.Fatalf("CreateFile(second): %v", err)
	}
	if err := e.EditFile(testContext(t), "nested/file.txt", "before", "after"); err != nil {
		t.Fatalf("EditFile(second): %v", err)
	}
	result, err := e.View(testContext(t), ssh.ViewRequest{Path: "nested/file.txt"})
	if err != nil {
		t.Fatalf("View(second): %v", err)
	}
	if result.Content != "1. after\n" {
		t.Fatalf("View(second) content = %q, want %q", result.Content, "1. after\n")
	}

	firstContent, err := os.ReadFile(filepath.Join(first, "nested", "file.txt"))
	if err != nil {
		t.Fatalf("ReadFile(first): %v", err)
	}
	if string(firstContent) != "first\n" {
		t.Fatalf("first file = %q, want unchanged", firstContent)
	}
}
