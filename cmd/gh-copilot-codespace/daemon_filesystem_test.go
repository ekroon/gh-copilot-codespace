package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ekroon/gh-copilot-codespace/internal/daemonproto"
	"github.com/ekroon/gh-copilot-codespace/internal/helperinfo"
	"github.com/ekroon/gh-copilot-codespace/internal/ssh"
)

func TestDaemonViewDirectoryListing(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "root.txt"), []byte("root\n"), 0o644); err != nil {
		t.Fatalf("WriteFile root.txt: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatalf("Mkdir sub: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "child.txt"), []byte("child\n"), 0o644); err != nil {
		t.Fatalf("WriteFile sub/child.txt: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub", "deeper"), 0o755); err != nil {
		t.Fatalf("Mkdir sub/deeper: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "deeper", "too-deep.txt"), []byte("skip me\n"), 0o644); err != nil {
		t.Fatalf("WriteFile sub/deeper/too-deep.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".hidden"), []byte("hidden\n"), 0o644); err != nil {
		t.Fatalf("WriteFile .hidden: %v", err)
	}

	got, err := daemonView(context.Background(), ssh.ViewRequest{Path: dir})
	if err != nil {
		t.Fatalf("daemonView() error = %v", err)
	}

	wantEntries := []string{
		"root.txt",
		"sub/",
		"sub/child.txt",
		"sub/deeper/",
	}
	if got.Kind != ssh.ViewKindDirectory {
		t.Fatalf("Kind = %q, want %q", got.Kind, ssh.ViewKindDirectory)
	}
	if !reflect.DeepEqual(got.Entries, wantEntries) {
		t.Fatalf("Entries = %v, want %v", got.Entries, wantEntries)
	}
	if got.Content != strings.Join(wantEntries, "\n")+"\n" {
		t.Fatalf("Content = %q", got.Content)
	}
	if got.Truncated {
		t.Fatal("Truncated = true, want false")
	}
}

func TestDaemonViewDirectoryListingHasGlobalEntryAndByteLimits(t *testing.T) {
	t.Run("entries", func(t *testing.T) {
		dir := t.TempDir()
		for _, child := range []string{"a", "b"} {
			childDir := filepath.Join(dir, child)
			if err := os.Mkdir(childDir, 0o755); err != nil {
				t.Fatalf("Mkdir(%s): %v", child, err)
			}
			for index := 0; index < daemonDirectoryMaxEntries/2+10; index++ {
				name := fmt.Sprintf("%04d", index)
				if err := os.WriteFile(filepath.Join(childDir, name), nil, 0o644); err != nil {
					t.Fatalf("WriteFile(%s/%s): %v", child, name, err)
				}
			}
		}

		got, err := daemonView(context.Background(), ssh.ViewRequest{Path: dir})
		if err != nil {
			t.Fatalf("daemonView() error = %v", err)
		}
		if !got.Truncated {
			t.Fatal("Truncated = false, want true")
		}
		if len(got.Entries) > daemonDirectoryMaxEntries {
			t.Fatalf("entries = %d, want <= %d", len(got.Entries), daemonDirectoryMaxEntries)
		}
		assertStructuredLimitMetadata(t, got, daemonDirectoryMaxEntries, daemonDirectoryMaxBytes)
	})

	t.Run("exact entry limit", func(t *testing.T) {
		dir := t.TempDir()
		for index := 0; index < daemonDirectoryMaxEntries; index++ {
			name := fmt.Sprintf("%04d", index)
			if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
				t.Fatalf("WriteFile(%s): %v", name, err)
			}
		}

		got, err := daemonView(context.Background(), ssh.ViewRequest{Path: dir})
		if err != nil {
			t.Fatalf("daemonView() error = %v", err)
		}
		if got.Truncated {
			t.Fatal("Truncated = true, want false for exactly the entry limit")
		}
		if len(got.Entries) != daemonDirectoryMaxEntries {
			t.Fatalf("entries = %d, want %d", len(got.Entries), daemonDirectoryMaxEntries)
		}
		assertStructuredLimitMetadata(t, got, daemonDirectoryMaxEntries, daemonDirectoryMaxBytes)
	})

	t.Run("bytes", func(t *testing.T) {
		dir := t.TempDir()
		for index := 0; index < 200; index++ {
			name := fmt.Sprintf("%03d-%s", index, strings.Repeat("x", 180))
			if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
				t.Fatalf("WriteFile(%s): %v", name, err)
			}
		}

		got, err := daemonView(context.Background(), ssh.ViewRequest{Path: dir})
		if err != nil {
			t.Fatalf("daemonView() error = %v", err)
		}
		if !got.Truncated {
			t.Fatal("Truncated = false, want true")
		}
		if len([]byte(got.Content)) > daemonDirectoryMaxBytes {
			t.Fatalf("content bytes = %d, want <= %d", len([]byte(got.Content)), daemonDirectoryMaxBytes)
		}
		if got.Content != strings.Join(got.Entries, "\n")+"\n" {
			t.Fatalf("content and entries diverged")
		}
		assertStructuredLimitMetadata(t, got, daemonDirectoryMaxEntries, daemonDirectoryMaxBytes)
	})
}

func assertStructuredLimitMetadata(t *testing.T, value any, wantLimit, wantByteLimit int) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	var metadata map[string]any
	if err := json.Unmarshal(data, &metadata); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	delete(metadata, "content")
	delete(metadata, "entries")
	delete(metadata, "output")
	limit, ok := metadata["limit"].(float64)
	if !ok {
		t.Fatalf("limit metadata missing or invalid: %v", metadata)
	}
	if got := int(limit); got != wantLimit {
		t.Fatalf("limit = %d, want %d; metadata=%v", got, wantLimit, metadata)
	}
	byteLimit, ok := metadata["byte_limit"].(float64)
	if !ok {
		t.Fatalf("byte_limit metadata missing or invalid: %v", metadata)
	}
	if got := int(byteLimit); got != wantByteLimit {
		t.Fatalf("byte_limit = %d, want %d; metadata=%v", got, wantByteLimit, metadata)
	}
}

func TestDaemonViewLargeFileTruncatesUnlessForced(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.txt")
	var body strings.Builder
	for i := 0; i < 4000; i++ {
		body.WriteString("line ")
		body.WriteString(strings.Repeat("x", 8))
		body.WriteString("\n")
	}
	body.WriteString("THE LAST LINE\n")
	if err := os.WriteFile(path, []byte(body.String()), 0o644); err != nil {
		t.Fatalf("WriteFile large.txt: %v", err)
	}

	truncated, err := daemonView(context.Background(), ssh.ViewRequest{Path: path})
	if err != nil {
		t.Fatalf("daemonView() error = %v", err)
	}
	if truncated.Kind != ssh.ViewKindFile {
		t.Fatalf("Kind = %q, want %q", truncated.Kind, ssh.ViewKindFile)
	}
	if !truncated.Truncated {
		t.Fatal("Truncated = false, want true")
	}
	if strings.Contains(truncated.Content, "THE LAST LINE") {
		t.Fatalf("truncated content unexpectedly contains last line")
	}

	ranged, err := daemonView(context.Background(), ssh.ViewRequest{Path: path, ViewRange: []int{4001, 4001}})
	if err != nil {
		t.Fatalf("daemonView(range) error = %v", err)
	}
	if ranged.Truncated {
		t.Fatal("ranged.Truncated = true, want false")
	}
	if !strings.Contains(ranged.Content, "4001. THE LAST LINE") {
		t.Fatalf("ranged content = %q, want last line", ranged.Content)
	}

	full, err := daemonView(context.Background(), ssh.ViewRequest{Path: path, ForceReadLargeFiles: true})
	if err != nil {
		t.Fatalf("daemonView(force) error = %v", err)
	}
	if full.Truncated {
		t.Fatal("full.Truncated = true, want false")
	}
	if !strings.Contains(full.Content, "4001. THE LAST LINE") {
		t.Fatalf("full content missing last line")
	}
}

func TestDaemonViewHugeSingleLineIsBoundedAndUTF8Safe(t *testing.T) {
	tests := []struct {
		name string
		line string
	}{
		{name: "ASCII", line: strings.Repeat("x", 25*1024)},
		{name: "multibyte UTF-8", line: strings.Repeat("界", 10*1024)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "single-line")
			if err := os.WriteFile(path, []byte(tt.line), 0o644); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}

			var truncatedResults []ssh.ViewResult
			for _, viewRange := range [][]int{nil, {1, 1}} {
				got, err := daemonView(context.Background(), ssh.ViewRequest{
					Path:      path,
					ViewRange: viewRange,
				})
				if err != nil {
					t.Fatalf("daemonView(range=%v) error = %v", viewRange, err)
				}
				if !got.Truncated {
					t.Fatalf("daemonView(range=%v) Truncated = false, want true", viewRange)
				}
				if !strings.HasPrefix(got.Content, "1. ") || len(got.Content) <= len("1. \n") {
					t.Fatalf("daemonView(range=%v) content is not a useful partial line: %q", viewRange, got.Content)
				}
				if len([]byte(got.Content)) > daemonViewMaxBytes {
					t.Fatalf("daemonView(range=%v) content size = %d, want <= %d", viewRange, len([]byte(got.Content)), daemonViewMaxBytes)
				}
				if !utf8.ValidString(got.Content) {
					t.Fatalf("daemonView(range=%v) returned invalid UTF-8", viewRange)
				}
				truncatedResults = append(truncatedResults, got)
			}
			if truncatedResults[0].Content != truncatedResults[1].Content {
				t.Fatal("ranged and unranged truncation differ")
			}

			wantFull := "1. " + tt.line + "\n"
			for _, viewRange := range [][]int{nil, {1, 1}} {
				got, err := daemonView(context.Background(), ssh.ViewRequest{
					Path:                path,
					ViewRange:           viewRange,
					ForceReadLargeFiles: true,
				})
				if err != nil {
					t.Fatalf("daemonView(force, range=%v) error = %v", viewRange, err)
				}
				if got.Truncated || got.Content != wantFull {
					t.Fatalf("daemonView(force, range=%v) = {Truncated:%v ContentBytes:%d}, want complete %d-byte content",
						viewRange, got.Truncated, len([]byte(got.Content)), len([]byte(wantFull)))
				}
			}
		})
	}
}

func TestDaemonViewReturnsImageAndBinaryMetadata(t *testing.T) {
	dir := t.TempDir()
	pngData, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO7+4xoAAAAASUVORK5CYII=")
	if err != nil {
		t.Fatalf("DecodeString png: %v", err)
	}
	imagePath := filepath.Join(dir, "pixel.png")
	if err := os.WriteFile(imagePath, pngData, 0o644); err != nil {
		t.Fatalf("WriteFile pixel.png: %v", err)
	}
	imageResult, err := daemonView(context.Background(), ssh.ViewRequest{Path: imagePath})
	if err != nil {
		t.Fatalf("daemonView(image) error = %v", err)
	}
	if imageResult.Kind != ssh.ViewKindImage {
		t.Fatalf("image Kind = %q, want %q", imageResult.Kind, ssh.ViewKindImage)
	}
	if imageResult.MimeType != "image/png" {
		t.Fatalf("image MimeType = %q, want image/png", imageResult.MimeType)
	}
	if !strings.Contains(imageResult.Content, "Image file (image/png)") {
		t.Fatalf("image Content = %q, want summary", imageResult.Content)
	}
	decodedImage, err := base64.StdEncoding.DecodeString(imageResult.Base64Data)
	if err != nil {
		t.Fatalf("DecodeString imageResult.Base64Data: %v", err)
	}
	if string(decodedImage) != string(pngData) {
		t.Fatalf("decoded image bytes changed")
	}

	binaryPath := filepath.Join(dir, "blob.bin")
	if err := os.WriteFile(binaryPath, []byte{0x00, 0x01, 0x02, 0xff, 0x7f}, 0o644); err != nil {
		t.Fatalf("WriteFile blob.bin: %v", err)
	}
	binaryResult, err := daemonView(context.Background(), ssh.ViewRequest{Path: binaryPath})
	if err != nil {
		t.Fatalf("daemonView(binary) error = %v", err)
	}
	if binaryResult.Kind != ssh.ViewKindFile {
		t.Fatalf("binary Kind = %q, want %q", binaryResult.Kind, ssh.ViewKindFile)
	}
	if binaryResult.MimeType == "" {
		t.Fatal("binary MimeType is empty")
	}
	if binaryResult.Base64Data != "" || !binaryResult.Truncated || !strings.Contains(binaryResult.Content, "Binary file") {
		t.Fatalf("binaryResult = %+v", binaryResult)
	}
}

func TestDaemonViewClassifiesContentBeforeFilenameExtension(t *testing.T) {
	dir := t.TempDir()
	pngData, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO7+4xoAAAAASUVORK5CYII=")
	if err != nil {
		t.Fatalf("DecodeString png: %v", err)
	}

	tests := []struct {
		name     string
		path     string
		data     []byte
		wantKind ssh.ViewKind
		wantMIME string
		wantText string
	}{
		{
			name:     "PNG signature with text extension",
			path:     filepath.Join(dir, "pixel.txt"),
			data:     pngData,
			wantKind: ssh.ViewKindImage,
			wantMIME: "image/png",
			wantText: "Image file",
		},
		{
			name:     "NUL byte with text extension",
			path:     filepath.Join(dir, "nul.txt"),
			data:     []byte("text\x00data"),
			wantKind: ssh.ViewKindFile,
			wantText: "Binary file",
		},
		{
			name:     "invalid UTF-8 with text extension",
			path:     filepath.Join(dir, "invalid.txt"),
			data:     []byte{'t', 'e', 'x', 't', 0xff},
			wantKind: ssh.ViewKindFile,
			wantText: "Binary file",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := os.WriteFile(test.path, test.data, 0o644); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}

			got, err := daemonView(context.Background(), ssh.ViewRequest{Path: test.path})
			if err != nil {
				t.Fatalf("daemonView() error = %v", err)
			}
			if got.Kind != test.wantKind {
				t.Fatalf("Kind = %q, want %q", got.Kind, test.wantKind)
			}
			if test.wantMIME != "" && got.MimeType != test.wantMIME {
				t.Fatalf("MimeType = %q, want %q", got.MimeType, test.wantMIME)
			}
			if !strings.Contains(got.Content, test.wantText) {
				t.Fatalf("Content = %q, want %q summary", got.Content, test.wantText)
			}
		})
	}
}

func TestDaemonViewClassifiesLateBinaryContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "late.txt")
	data := append(bytes.Repeat([]byte("a"), 600), 0)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	got, err := daemonView(context.Background(), ssh.ViewRequest{Path: path})
	if err != nil {
		t.Fatalf("daemonView() error = %v", err)
	}
	if got.Kind != ssh.ViewKindFile || got.MimeType == "" || !got.Truncated {
		t.Fatalf("daemonView() = %+v, want structured binary metadata", got)
	}
	if got.Base64Data != "" || !strings.Contains(got.Content, "Binary file") {
		t.Fatalf("daemonView() = %+v, want binary summary without payload", got)
	}
}

func TestDaemonViewRejectsNonRegularPathsBeforeReading(t *testing.T) {
	dir := t.TempDir()
	fifoPath := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(fifoPath, 0o600); err != nil {
		t.Fatalf("Mkfifo() error = %v", err)
	}
	fifo, err := os.OpenFile(fifoPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("OpenFile(fifo) error = %v", err)
	}
	defer fifo.Close()

	socketPath := filepath.Join("..", "..", fmt.Sprintf(".daemon-view-%d.sock", time.Now().UnixNano()))
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("Listen(unix) error = %v", err)
	}
	defer listener.Close()
	t.Cleanup(func() { _ = os.Remove(socketPath) })

	for name, path := range map[string]string{
		"FIFO":        fifoPath,
		"device":      os.DevNull,
		"Unix socket": socketPath,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := daemonView(context.Background(), ssh.ViewRequest{Path: path})
			if err == nil || !strings.Contains(err.Error(), "not a regular file") {
				t.Fatalf("daemonView() error = %v, want non-regular rejection", err)
			}
		})
	}
}

func TestDaemonViewStopsReadingAfterRangeOrResultCap(t *testing.T) {
	tests := []struct {
		name      string
		content   string
		viewRange []int
	}{
		{
			name:      "requested range",
			content:   "first\n" + strings.Repeat("x", 8<<20),
			viewRange: []int{1, 1},
		},
		{
			name:    "result cap",
			content: strings.Repeat("x", 8<<20),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "huge.txt")
			if err := os.WriteFile(path, []byte(test.content), 0o644); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}

			bytesRead := 0
			got, err := daemonViewWithHooks(context.Background(), ssh.ViewRequest{
				Path:      path,
				ViewRange: test.viewRange,
			}, daemonViewHooks{
				afterRead: func(n int) {
					bytesRead += n
				},
			})
			if err != nil {
				t.Fatalf("daemonViewWithHooks() error = %v", err)
			}
			if bytesRead >= 128*1024 {
				t.Fatalf("bytes read = %d, want bounded early stop", bytesRead)
			}
			if test.viewRange == nil && !got.Truncated {
				t.Fatal("Truncated = false, want true at result cap")
			}
		})
	}
}

func TestDaemonViewHonorsCancellationDuringRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.txt")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 1<<20), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	_, err := daemonViewWithHooks(ctx, ssh.ViewRequest{Path: path}, daemonViewHooks{
		afterRead: func(int) {
			cancel()
		},
	})
	if !errors.Is(err, errDaemonCanceled) {
		t.Fatalf("daemonViewWithHooks() error = %v, want cancellation", err)
	}
}

func TestDaemonViewLargeImageRequiresForceForBinaryData(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "large.png")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 25*1024)), 0o644); err != nil {
		t.Fatalf("WriteFile large.png: %v", err)
	}

	truncated, err := daemonView(context.Background(), ssh.ViewRequest{Path: path})
	if err != nil {
		t.Fatalf("daemonView() error = %v", err)
	}
	if truncated.Kind != ssh.ViewKindImage || !truncated.Truncated || truncated.Base64Data != "" {
		t.Fatalf("truncated = %+v", truncated)
	}

	full, err := daemonView(context.Background(), ssh.ViewRequest{
		Path:                path,
		ForceReadLargeFiles: true,
	})
	if err != nil {
		t.Fatalf("daemonView(force) error = %v", err)
	}
	if full.Kind != ssh.ViewKindImage || full.Truncated || full.Base64Data == "" {
		t.Fatalf("full = %+v", full)
	}
}

func TestDaemonGrepFilesFallbackSupportsStructuredOptions(t *testing.T) {
	t.Setenv("PATH", "")

	root := t.TempDir()
	dirOne := filepath.Join(root, "one")
	dirTwo := filepath.Join(root, "two")
	for _, dir := range []string{dirOne, dirTwo} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dirOne, "match.go"), []byte("Alpha\nBeta\n"), 0o644); err != nil {
		t.Fatalf("WriteFile dirOne match.go: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dirOne, "skip.txt"), []byte("Alpha\nBeta\n"), 0o644); err != nil {
		t.Fatalf("WriteFile dirOne skip.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dirTwo, "match.go"), []byte("ALPHA\nBETA\n"), 0o644); err != nil {
		t.Fatalf("WriteFile dirTwo match.go: %v", err)
	}

	got, err := daemonGrepFiles(context.Background(), ssh.GrepRequest{
		Pattern:         "alpha\\nbeta",
		Paths:           []string{dirOne, dirTwo},
		Glob:            "*.go",
		OutputMode:      ssh.GrepOutputModeFilesWithMatches,
		CaseInsensitive: true,
		HeadLimit:       1,
		Multiline:       true,
	})
	if err != nil {
		t.Fatalf("daemonGrepFiles() error = %v", err)
	}
	if got.OutputMode != ssh.GrepOutputModeFilesWithMatches {
		t.Fatalf("OutputMode = %q, want %q", got.OutputMode, ssh.GrepOutputModeFilesWithMatches)
	}
	if !reflect.DeepEqual(got.Paths, []string{dirOne, dirTwo}) {
		t.Fatalf("Paths = %v, want %v", got.Paths, []string{dirOne, dirTwo})
	}
	lines := strings.Fields(strings.TrimSpace(got.Output))
	if len(lines) != 1 {
		t.Fatalf("matched files = %v, want 1 because head_limit should apply", lines)
	}
	if lines[0] != filepath.Join(dirOne, "match.go") && lines[0] != filepath.Join(dirTwo, "match.go") {
		t.Fatalf("matched file = %q, want one of the go files", lines[0])
	}
}

func TestDaemonGrepHeadLimitTruncatesOnlyWhenOutputIsOmitted(t *testing.T) {
	modes := []ssh.GrepOutputMode{
		ssh.GrepOutputModeContent,
		ssh.GrepOutputModeFilesWithMatches,
		ssh.GrepOutputModeCount,
	}
	for _, mode := range modes {
		for _, overflow := range []bool{false, true} {
			name := string(mode) + "/exact"
			if overflow {
				name = string(mode) + "/overflow"
			}
			t.Run("fallback/"+name, func(t *testing.T) {
				t.Setenv("PATH", "")
				root := t.TempDir()
				if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("match\n"), 0o644); err != nil {
					t.Fatalf("WriteFile(a.txt) error = %v", err)
				}
				second := "other\n"
				if overflow {
					second = "match\n"
				}
				if err := os.WriteFile(filepath.Join(root, "b.txt"), []byte(second), 0o644); err != nil {
					t.Fatalf("WriteFile(b.txt) error = %v", err)
				}

				got, err := daemonGrepFiles(context.Background(), ssh.GrepRequest{
					Pattern:    "match",
					Paths:      []string{root},
					OutputMode: mode,
					HeadLimit:  1,
				})
				if err != nil {
					t.Fatalf("daemonGrepFiles() error = %v", err)
				}
				if got.Truncated != overflow {
					t.Fatalf("Truncated = %v, want %v; output=%q", got.Truncated, overflow, got.Output)
				}
				if lines := strings.Split(strings.TrimSuffix(got.Output, "\n"), "\n"); len(lines) != 1 {
					t.Fatalf("output lines = %d, want 1; output=%q", len(lines), got.Output)
				}
			})
		}
	}

	for _, overflow := range []bool{false, true} {
		name := "exact"
		if overflow {
			name = "overflow"
		}
		t.Run("fallback/context/"+name, func(t *testing.T) {
			t.Setenv("PATH", "")
			path := filepath.Join(t.TempDir(), "context.txt")
			content := "before\nmatch\nafter\n"
			if overflow {
				content += "gap\nmatch\n"
			}
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}

			got, err := daemonGrepFiles(context.Background(), ssh.GrepRequest{
				Pattern:       "match",
				Paths:         []string{path},
				BeforeContext: 1,
				AfterContext:  1,
				HeadLimit:     3,
			})
			if err != nil {
				t.Fatalf("daemonGrepFiles() error = %v", err)
			}
			if got.Truncated != overflow {
				t.Fatalf("Truncated = %v, want %v; output=%q", got.Truncated, overflow, got.Output)
			}
			if lines := strings.Split(strings.TrimSuffix(got.Output, "\n"), "\n"); len(lines) != 3 {
				t.Fatalf("output lines = %d, want 3; output=%q", len(lines), got.Output)
			}
		})
	}

	rgCases := []struct {
		name     string
		head     int
		exact    string
		overflow string
	}{
		{name: "content", head: 1, exact: "a.txt:1:match\n", overflow: "a.txt:1:match\nb.txt:1:match\n"},
		{name: "files", head: 1, exact: "a.txt\n", overflow: "a.txt\nb.txt\n"},
		{name: "count", head: 1, exact: "a.txt:1\n", overflow: "a.txt:1\nb.txt:1\n"},
		{name: "context", head: 3, exact: "a.txt-1-before\na.txt:2:match\na.txt-3-after\n", overflow: "a.txt-1-before\na.txt:2:match\na.txt-3-after\n--\n"},
	}
	for _, tc := range rgCases {
		for _, overflow := range []bool{false, true} {
			name := tc.name + "/exact"
			output := tc.exact
			if overflow {
				name = tc.name + "/overflow"
				output = tc.overflow
			}
			t.Run("ripgrep/"+name, func(t *testing.T) {
				binDir := t.TempDir()
				if err := os.WriteFile(filepath.Join(binDir, "rg"), []byte("#!/bin/sh\nprintf '%s' \"$RG_TEST_OUTPUT\"\n"), 0o755); err != nil {
					t.Fatalf("WriteFile(rg) error = %v", err)
				}
				t.Setenv("PATH", binDir)
				t.Setenv("RG_TEST_OUTPUT", output)

				got, err := daemonGrepFiles(context.Background(), ssh.GrepRequest{
					Pattern:   "match",
					HeadLimit: tc.head,
				})
				if err != nil {
					t.Fatalf("daemonGrepFiles() error = %v", err)
				}
				if got.Truncated != overflow {
					t.Fatalf("Truncated = %v, want %v; output=%q", got.Truncated, overflow, got.Output)
				}
				if got.Output != tc.exact {
					t.Fatalf("Output = %q, want %q", got.Output, tc.exact)
				}
			})
		}
	}
}

func TestDaemonGrepByteLimitTruncatesOnlyOnOverflow(t *testing.T) {
	for _, overflow := range []bool{false, true} {
		name := "exact"
		byteCount := daemonGrepMaxOutputBytes
		if overflow {
			name = "overflow"
			byteCount++
		}

		t.Run("ripgrep/"+name, func(t *testing.T) {
			python3, err := exec.LookPath("python3")
			if err != nil {
				t.Fatalf("LookPath(python3) error = %v", err)
			}
			binDir := t.TempDir()
			script := fmt.Sprintf(`#!%s
import os
import sys
size = int(os.environ["RG_TEST_BYTES"])
sys.stdout.write("x" * (size - 1) + "\n")
`, python3)
			if err := os.WriteFile(filepath.Join(binDir, "rg"), []byte(script), 0o755); err != nil {
				t.Fatalf("WriteFile(rg) error = %v", err)
			}
			t.Setenv("PATH", binDir)
			t.Setenv("RG_TEST_BYTES", fmt.Sprintf("%d", byteCount))

			got, err := daemonGrepFiles(context.Background(), ssh.GrepRequest{Pattern: "match"})
			if err != nil {
				t.Fatalf("daemonGrepFiles() error = %v", err)
			}
			if got.Truncated != overflow {
				t.Fatalf("Truncated = %v, want %v", got.Truncated, overflow)
			}
			if len([]byte(got.Output)) != daemonGrepMaxOutputBytes {
				t.Fatalf("output bytes = %d, want %d", len([]byte(got.Output)), daemonGrepMaxOutputBytes)
			}
		})

		t.Run("fallback/"+name, func(t *testing.T) {
			t.Setenv("PATH", "")
			path := filepath.Join(t.TempDir(), "match.txt")
			prefix := path + ":1:"
			contentBytes := daemonGrepMaxOutputBytes - len(prefix) - 1
			if overflow {
				contentBytes++
			}
			if err := os.WriteFile(path, []byte(strings.Repeat("x", contentBytes)+"\n"), 0o644); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}

			got, err := daemonGrepFiles(context.Background(), ssh.GrepRequest{
				Pattern: "x",
				Paths:   []string{path},
			})
			if err != nil {
				t.Fatalf("daemonGrepFiles() error = %v", err)
			}
			if got.Truncated != overflow {
				t.Fatalf("Truncated = %v, want %v", got.Truncated, overflow)
			}
			if len([]byte(got.Output)) != daemonGrepMaxOutputBytes {
				t.Fatalf("output bytes = %d, want %d", len([]byte(got.Output)), daemonGrepMaxOutputBytes)
			}
		})
	}
}

func TestDaemonGrepFallbackSkipsFIFOWithoutBlocking(t *testing.T) {
	t.Setenv("PATH", "")
	root := t.TempDir()
	fifoPath := filepath.Join(root, "000-pipe")
	if err := syscall.Mkfifo(fifoPath, 0o600); err != nil {
		t.Fatalf("Mkfifo() error = %v", err)
	}
	matchPath := filepath.Join(root, "100-match.txt")
	if err := os.WriteFile(matchPath, []byte("match\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	release := make(chan struct{})
	go func() {
		time.Sleep(600 * time.Millisecond)
		fifo, _ := os.OpenFile(fifoPath, os.O_RDWR, 0)
		if fifo != nil {
			_ = fifo.Close()
		}
		close(release)
	}()

	started := time.Now()
	got, err := daemonGrepFilesFallback(context.Background(), ssh.GrepRequest{
		Pattern: "match",
		Paths:   []string{root},
	})
	elapsed := time.Since(started)
	<-release
	if err != nil {
		t.Fatalf("daemonGrepFilesFallback() error = %v", err)
	}
	if elapsed >= 400*time.Millisecond {
		t.Fatalf("daemonGrepFilesFallback() took %v, want FIFO skipped", elapsed)
	}
	if !strings.Contains(got.Output, matchPath+":1:match") {
		t.Fatalf("Output = %q, want regular-file match", got.Output)
	}
}

func TestDaemonGrepFallbackHonorsCancellationWhileReading(t *testing.T) {
	t.Setenv("PATH", "")
	path := filepath.Join(t.TempDir(), "large.txt")
	if err := os.WriteFile(path, bytes.Repeat([]byte("text without the needle\n"), 4096), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	_, err := daemonGrepFilesFallbackWithHooks(ctx, ssh.GrepRequest{
		Pattern: "missing",
		Paths:   []string{path},
	}, daemonGrepHooks{
		afterRead: func() {
			cancel()
		},
	})
	if !errors.Is(err, errDaemonCanceled) {
		t.Fatalf("daemonGrepFilesFallbackWithHooks() error = %v, want cancellation", err)
	}
}

func TestDaemonGrepFallbackMultilineSkipsHugeSparseFileWithoutReading(t *testing.T) {
	t.Setenv("PATH", "")
	path := filepath.Join(t.TempDir(), "huge.txt")
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if _, err := file.Write(bytes.Repeat([]byte("text"), 128)); err != nil {
		_ = file.Close()
		t.Fatalf("Write() error = %v", err)
	}
	if err := file.Truncate(1 << 40); err != nil {
		_ = file.Close()
		t.Fatalf("Truncate() error = %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reads := 0
	started := time.Now()
	got, err := daemonGrepFilesFallbackWithHooks(ctx, ssh.GrepRequest{
		Pattern:   "text",
		Paths:     []string{path},
		Multiline: true,
	}, daemonGrepHooks{
		afterRead: func() {
			reads++
			if reads >= 4 {
				cancel()
			}
		},
	})
	if err != nil {
		t.Fatalf("daemonGrepFilesFallbackWithHooks() error = %v", err)
	}
	if elapsed := time.Since(started); elapsed >= time.Second {
		t.Fatalf("daemonGrepFilesFallbackWithHooks() took %v, want prompt size rejection", elapsed)
	}
	if reads != 0 {
		t.Fatalf("file reads = %d, want 0 for file above %d-byte input limit", reads, daemonGrepMaxInputBytes)
	}
	if got.Output != "" || !got.Truncated {
		t.Fatalf("GrepResult = %+v, want skipped file with truncation metadata", got)
	}
	if got.SkippedFiles != 1 || got.InputByteLimit != ssh.MaxGrepInputBytes {
		t.Fatalf("GrepResult limits = %+v", got)
	}
}

func TestDaemonGrepFallbackSkipsOverlongLineAndContinues(t *testing.T) {
	t.Setenv("PATH", "")
	path := filepath.Join(t.TempDir(), "long-line.txt")
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	chunk := bytes.Repeat([]byte("x"), 32*1024)
	remaining := daemonGrepMaxLineBytes + 1
	for remaining > 0 {
		write := len(chunk)
		if write > remaining {
			write = remaining
		}
		if _, err := file.Write(chunk[:write]); err != nil {
			_ = file.Close()
			t.Fatalf("Write() error = %v", err)
		}
		remaining -= write
	}
	if _, err := file.WriteString("\nmatch\n"); err != nil {
		_ = file.Close()
		t.Fatalf("WriteString() error = %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	got, err := daemonGrepFilesFallback(context.Background(), ssh.GrepRequest{
		Pattern: "match",
		Paths:   []string{path},
	})
	if err != nil {
		t.Fatalf("daemonGrepFilesFallback() error = %v", err)
	}
	if got.Output != path+":2:match\n" {
		t.Fatalf("Output = %q, want match after overlong line", got.Output)
	}
	if !got.Truncated {
		t.Fatal("Truncated = false, want warning metadata for skipped overlong line")
	}
	if got.SkippedFiles != 1 || got.InputByteLimit != ssh.MaxGrepInputBytes {
		t.Fatalf("GrepResult limits = %+v", got)
	}
}

func TestDaemonGrepFallbackCancelsPromptlyWithinLongLine(t *testing.T) {
	t.Setenv("PATH", "")
	path := filepath.Join(t.TempDir(), "long-line.txt")
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	chunk := bytes.Repeat([]byte("x"), 32*1024)
	for remaining := daemonGrepMaxLineBytes + 1; remaining > 0; {
		write := len(chunk)
		if write > remaining {
			write = remaining
		}
		if _, err := file.Write(chunk[:write]); err != nil {
			_ = file.Close()
			t.Fatalf("Write() error = %v", err)
		}
		remaining -= write
	}
	if err := file.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	reads := 0
	started := time.Now()
	_, err = daemonGrepFilesFallbackWithHooks(ctx, ssh.GrepRequest{
		Pattern: "missing",
		Paths:   []string{path},
	}, daemonGrepHooks{
		afterRead: func() {
			reads++
			if reads == 2 {
				cancel()
			}
		},
	})
	if !errors.Is(err, errDaemonCanceled) {
		t.Fatalf("daemonGrepFilesFallbackWithHooks() error = %v, want cancellation", err)
	}
	if elapsed := time.Since(started); elapsed >= time.Second {
		t.Fatalf("cancellation took %v, want prompt stop within long line", elapsed)
	}
}

func TestDaemonGrepHeadLimitStopsRipgrepProducer(t *testing.T) {
	binDir := t.TempDir()
	rgPath := filepath.Join(binDir, "rg")
	stoppedPath := filepath.Join(t.TempDir(), "stopped")
	script := `#!/bin/sh
trap 'printf stopped > "$RG_STOP_FILE"; exit 0' TERM INT
i=0
while :; do
	printf 'file.txt:%d:match\n' "$i"
	i=$((i + 1))
done
`
	if err := os.WriteFile(rgPath, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile(rg) error = %v", err)
	}
	t.Setenv("PATH", binDir)
	t.Setenv("RG_STOP_FILE", stoppedPath)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	started := time.Now()
	got, err := daemonGrepFiles(ctx, ssh.GrepRequest{
		Pattern:   "match",
		HeadLimit: 5,
	})
	if err != nil {
		t.Fatalf("daemonGrepFiles() error = %v", err)
	}
	if elapsed := time.Since(started); elapsed >= 2*time.Second {
		t.Fatalf("daemonGrepFiles() took %v, want producer stopped promptly", elapsed)
	}
	if lines := strings.Split(strings.TrimSpace(got.Output), "\n"); len(lines) != 5 {
		t.Fatalf("output lines = %d, want 5; output=%q", len(lines), got.Output)
	}
	if stopped, err := os.ReadFile(stoppedPath); err != nil || string(stopped) != "stopped" {
		t.Fatalf("producer stop marker = %q, %v", stopped, err)
	}
}

func TestDaemonGrepByteLimitStopsRipgrepProducerWithUTF8SafeOutput(t *testing.T) {
	binDir := t.TempDir()
	rgPath := filepath.Join(binDir, "rg")
	stoppedPath := filepath.Join(t.TempDir(), "stopped")
	script := `#!/bin/sh
trap 'printf stopped > "$RG_STOP_FILE"; exit 0' TERM INT
while :; do
	printf 'file.txt:1:😀😀😀😀😀😀😀😀\n'
done
`
	if err := os.WriteFile(rgPath, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile(rg) error = %v", err)
	}
	t.Setenv("PATH", binDir)
	t.Setenv("RG_STOP_FILE", stoppedPath)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	got, err := daemonGrepFiles(ctx, ssh.GrepRequest{Pattern: "match"})
	if err != nil {
		t.Fatalf("daemonGrepFiles() error = %v", err)
	}
	if len([]byte(got.Output)) > daemonGrepMaxOutputBytes {
		t.Fatalf("output bytes = %d, want <= %d", len([]byte(got.Output)), daemonGrepMaxOutputBytes)
	}
	if !utf8.ValidString(got.Output) {
		t.Fatalf("output is not valid UTF-8")
	}
	assertStructuredLimitMetadata(t, got, daemonGrepMaxOutputBytes, daemonGrepMaxOutputBytes)
	if stopped, err := os.ReadFile(stoppedPath); err != nil || string(stopped) != "stopped" {
		t.Fatalf("producer stop marker = %q, %v", stopped, err)
	}
}

func TestDaemonGrepFallbackByteLimitStopsTraversal(t *testing.T) {
	t.Setenv("PATH", "")
	root := t.TempDir()
	longMatch := strings.Repeat("😀", daemonGrepMaxOutputBytes/4+100)
	if err := os.WriteFile(filepath.Join(root, "000-match.txt"), []byte(longMatch+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "999-after.txt"), []byte("😀\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	got, err := daemonGrepFilesFallbackWithHooks(context.Background(), ssh.GrepRequest{
		Pattern: "😀",
		Paths:   []string{root},
	}, daemonGrepHooks{
		beforeFile: func(file daemonSearchFile) error {
			if filepath.Base(file.resolved) == "999-after.txt" {
				return errors.New("walked past grep byte limit")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("daemonGrepFiles() error = %v, want traversal stopped at byte limit", err)
	}
	if len([]byte(got.Output)) > daemonGrepMaxOutputBytes {
		t.Fatalf("output bytes = %d, want <= %d", len([]byte(got.Output)), daemonGrepMaxOutputBytes)
	}
	if !utf8.ValidString(got.Output) {
		t.Fatal("output is not valid UTF-8")
	}
	assertStructuredLimitMetadata(t, got, daemonGrepMaxOutputBytes, daemonGrepMaxOutputBytes)
}

func TestDaemonGrepFallbackHeadLimitStopsTraversal(t *testing.T) {
	t.Setenv("PATH", "")
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "000-match.txt"), []byte("match\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "100-match.txt"), []byte("match\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "999-after.txt"), []byte("match\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	got, err := daemonGrepFilesFallbackWithHooks(context.Background(), ssh.GrepRequest{
		Pattern:    "match",
		Paths:      []string{root},
		OutputMode: ssh.GrepOutputModeFilesWithMatches,
		HeadLimit:  1,
	}, daemonGrepHooks{
		beforeFile: func(file daemonSearchFile) error {
			if filepath.Base(file.resolved) == "999-after.txt" {
				return errors.New("walked past grep truncation probe")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("daemonGrepFilesFallbackWithHooks() error = %v", err)
	}
	if got.Output != filepath.Join(root, "000-match.txt")+"\n" || !got.Truncated {
		t.Fatalf("GrepResult = %+v, want first match and truncation", got)
	}
}

func TestDaemonGrepFallbackHeadLimitPreservesContextUnits(t *testing.T) {
	t.Setenv("PATH", "")
	path := filepath.Join(t.TempDir(), "context.txt")
	if err := os.WriteFile(path, []byte("one\nbefore\nmatch\nafter\ngap\nbefore2\nmatch\nafter2\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	got, err := daemonGrepFiles(context.Background(), ssh.GrepRequest{
		Pattern:       "match",
		Paths:         []string{path},
		BeforeContext: 1,
		AfterContext:  1,
		HeadLimit:     4,
	})
	if err != nil {
		t.Fatalf("daemonGrepFiles() error = %v", err)
	}
	want := strings.Join([]string{
		path + "-2-before",
		path + ":3:match",
		path + "-4-after",
		"--",
		"",
	}, "\n")
	if got.Output != want {
		t.Fatalf("Output = %q, want %q", got.Output, want)
	}
}

func TestDaemonGrepDashPrefixedPatternUsesArgumentSeparator(t *testing.T) {
	t.Run("ripgrep", func(t *testing.T) {
		binDir := t.TempDir()
		rgPath := filepath.Join(binDir, "rg")
		script := `#!/bin/sh
while [ "$#" -gt 0 ] && [ "$1" != "--" ]; do
	shift
done
[ "$#" -eq 3 ] || exit 2
shift
[ "$1" = "--force" ] || exit 3
[ "$2" = "--search-root" ] || exit 4
printf '%s\n' "$2/file.txt:1:$1"
`
		if err := os.WriteFile(rgPath, []byte(script), 0o755); err != nil {
			t.Fatalf("WriteFile(rg) error = %v", err)
		}
		t.Setenv("PATH", binDir)

		got, err := daemonGrepFiles(context.Background(), ssh.GrepRequest{
			Pattern: "--force",
			Paths:   []string{"--search-root"},
			Cwd:     t.TempDir(),
		})
		if err != nil {
			t.Fatalf("daemonGrepFiles() error = %v", err)
		}
		if got.Output != "--search-root/file.txt:1:--force\n" {
			t.Fatalf("Output = %q", got.Output)
		}
	})

	t.Run("fallback", func(t *testing.T) {
		t.Setenv("PATH", "")
		root := t.TempDir()
		path := filepath.Join(root, "flags.txt")
		if err := os.WriteFile(path, []byte("before\n--force\nafter\n"), 0o644); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}

		got, err := daemonGrepFiles(context.Background(), ssh.GrepRequest{
			Pattern: "--force",
			Paths:   []string{root},
		})
		if err != nil {
			t.Fatalf("daemonGrepFiles() error = %v", err)
		}
		if !strings.Contains(got.Output, path+":2:--force") {
			t.Fatalf("Output = %q, want literal dash-prefixed match", got.Output)
		}
	})
}

func TestDaemonGlobFilesFallbackSupportsMultiplePaths(t *testing.T) {
	t.Setenv("PATH", "")

	root := t.TempDir()
	dirOne := filepath.Join(root, "one")
	dirTwo := filepath.Join(root, "two")
	for _, dir := range []string{filepath.Join(dirOne, "sub"), filepath.Join(dirTwo, "sub"), filepath.Join(dirTwo, ".git")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll %s: %v", dir, err)
		}
	}
	want := []string{
		filepath.Join(dirOne, "sub", "a.go"),
		filepath.Join(dirOne, "sub", "b.txt"),
		filepath.Join(dirTwo, "sub", "c.go"),
	}
	for _, path := range want {
		if err := os.WriteFile(path, []byte("x\n"), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", path, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dirTwo, ".git", "ignored.go"), []byte("ignore\n"), 0o644); err != nil {
		t.Fatalf("WriteFile ignored.go: %v", err)
	}

	got, err := daemonGlobFiles(context.Background(), ssh.GlobRequest{
		Pattern: "**/*.{go,txt}",
		Paths:   []string{dirOne, dirTwo},
	})
	if err != nil {
		t.Fatalf("daemonGlobFiles() error = %v", err)
	}
	if !reflect.DeepEqual(got.Paths, []string{dirOne, dirTwo}) {
		t.Fatalf("Paths = %v, want %v", got.Paths, []string{dirOne, dirTwo})
	}
	lines := strings.Fields(strings.TrimSpace(got.Output))
	if !reflect.DeepEqual(lines, want) {
		t.Fatalf("matches = %v, want %v", lines, want)
	}
}

func TestDaemonGlobFDFastPathMatchesWalkerForHiddenIgnoredAndDashPattern(t *testing.T) {
	root := t.TempDir()
	hiddenDir := filepath.Join(root, ".hidden")
	gitDir := filepath.Join(root, ".git")
	for _, dir := range []string{hiddenDir, gitDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll %s: %v", dir, err)
		}
	}
	hiddenPath := filepath.Join(hiddenDir, "-hidden.go")
	ignoredPath := filepath.Join(root, "-ignored.go")
	for _, path := range []string{hiddenPath, ignoredPath, filepath.Join(gitDir, "-excluded.go")} {
		if err := os.WriteFile(path, []byte("package test\n"), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", path, err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("-ignored.go\n"), 0o644); err != nil {
		t.Fatalf("WriteFile .gitignore: %v", err)
	}

	t.Setenv("PATH", "")
	fallback, err := daemonGlobFiles(context.Background(), ssh.GlobRequest{
		Pattern: "-*.go",
		Paths:   []string{root},
	})
	if err != nil {
		t.Fatalf("daemonGlobFiles(fallback) error = %v", err)
	}
	wantOutput := ignoredPath + "\n" + hiddenPath + "\n"
	if fallback.Output != wantOutput {
		t.Fatalf("fallback Output = %q, want %q", fallback.Output, wantOutput)
	}

	binDir := t.TempDir()
	fdPath := filepath.Join(binDir, "fd")
	script := `#!/bin/sh
[ "$#" -eq 12 ] || exit 12
[ "$1" = "--type" ] && [ "$2" = "f" ] || exit 21
[ "$3" = "--color" ] && [ "$4" = "never" ] || exit 22
[ "$5" = "--hidden" ] || exit 23
[ "$6" = "--no-ignore" ] || exit 24
[ "$7" = "--glob" ] || exit 25
[ "$8" = "--exclude" ] && [ "$9" = ".git" ] || exit 26
[ "${10}" = "--" ] || exit 27
[ "${11}" = "-*.go" ] || exit 28
printf '%s\n' "${12}/-ignored.go" "${12}/.hidden/-hidden.go"
`
	if err := os.WriteFile(fdPath, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile fd: %v", err)
	}
	t.Setenv("PATH", binDir)

	fast, err := daemonGlobFiles(context.Background(), ssh.GlobRequest{
		Pattern: "-*.go",
		Paths:   []string{root},
	})
	if err != nil {
		t.Fatalf("daemonGlobFiles(fd) error = %v", err)
	}
	if fast.Output != fallback.Output {
		t.Fatalf("fd Output = %q, want walker output %q", fast.Output, fallback.Output)
	}
}

func TestDaemonGlobDefaultLimitTruncatesAndStopsWalker(t *testing.T) {
	t.Setenv("PATH", "")
	root := t.TempDir()
	for index := 0; index <= ssh.DefaultGlobLimit; index++ {
		path := filepath.Join(root, fmt.Sprintf("%04d.go", index))
		if err := os.WriteFile(path, []byte("package test\n"), 0o644); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", path, err)
		}
	}

	got, err := daemonGlobFiles(context.Background(), ssh.GlobRequest{
		Pattern: "*.go",
		Paths:   []string{root, filepath.Join(root, "missing")},
	})
	if err != nil {
		t.Fatalf("daemonGlobFiles() error = %v, want walker stopped before missing root", err)
	}
	if !got.Truncated || got.Limit != ssh.DefaultGlobLimit {
		t.Fatalf("glob metadata = %+v, want default-limit truncation", got)
	}
	if lines := strings.Split(strings.TrimSpace(got.Output), "\n"); len(lines) != ssh.DefaultGlobLimit {
		t.Fatalf("output lines = %d, want %d", len(lines), ssh.DefaultGlobLimit)
	}
}

func TestDaemonGlobLimitStopsFDProducer(t *testing.T) {
	binDir := t.TempDir()
	fdPath := filepath.Join(binDir, "fd")
	stoppedPath := filepath.Join(t.TempDir(), "stopped")
	script := `#!/bin/sh
trap 'printf stopped > "$FD_STOP_FILE"; exit 0' TERM INT
i=0
while :; do
	printf 'file-%d.go\n' "$i"
	i=$((i + 1))
done
`
	if err := os.WriteFile(fdPath, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile(fd) error = %v", err)
	}
	t.Setenv("PATH", binDir)
	t.Setenv("FD_STOP_FILE", stoppedPath)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	got, err := daemonGlobFiles(ctx, ssh.GlobRequest{Pattern: "*.go", Limit: 5})
	if err != nil {
		t.Fatalf("daemonGlobFiles() error = %v", err)
	}
	if !got.Truncated || got.Limit != 5 {
		t.Fatalf("glob metadata = %+v, want explicit-limit truncation", got)
	}
	if lines := strings.Split(strings.TrimSpace(got.Output), "\n"); len(lines) != 5 {
		t.Fatalf("output lines = %d, want 5; output=%q", len(lines), got.Output)
	}
	if stopped, err := os.ReadFile(stoppedPath); err != nil || string(stopped) != "stopped" {
		t.Fatalf("producer stop marker = %q, %v", stopped, err)
	}
}

func TestDaemonCreateFileDoesNotOverwriteExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "existing.txt")
	if err := os.WriteFile(path, []byte("keep\n"), 0o644); err != nil {
		t.Fatalf("WriteFile existing.txt: %v", err)
	}

	err := daemonCreateFile(context.Background(), path, "replace\n", false)
	if err == nil {
		t.Fatal("daemonCreateFile() error = nil, want error")
	}
	content, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("ReadFile existing.txt: %v", readErr)
	}
	if string(content) != "keep\n" {
		t.Fatalf("content = %q, want keep", string(content))
	}
}

func TestDaemonWriteFileRejectsInvalidBase64WithoutPartialDestination(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "blob.bin")
	if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
		t.Fatalf("WriteFile existing: %v", err)
	}
	params, err := json.Marshal(map[string]any{
		"path":      path,
		"data":      "not-base64%%%",
		"overwrite": true,
		"root":      dir,
	})
	if err != nil {
		t.Fatalf("Marshal params: %v", err)
	}

	h := startDaemonTestHarness(t)
	defer h.close()
	requireHello(t, readFrameWithTimeout(t, h.dec))
	writeFrame(t, h.enc, daemonproto.Frame{
		Version: daemonproto.ProtocolVersion,
		Type:    daemonproto.TypeRequest,
		ID:      1,
		Verb:    daemonproto.VerbWriteFile,
		Params:  params,
	})

	resp := readFrameWithTimeout(t, h.dec)
	if resp.Error == nil || resp.Error.Code != daemonproto.ErrCodeBadRequest {
		t.Fatalf("resp.Error = %+v, want bad request", resp.Error)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile existing: %v", err)
	}
	if string(content) != "keep" {
		t.Fatalf("content = %q, want unchanged", content)
	}
}

func TestDaemonRootedWriteMatchesSSHModeSemanticsUnderRestrictiveUmask(t *testing.T) {
	binDir := t.TempDir()
	ghPath := filepath.Join(binDir, "gh")
	ghScript := `#!/bin/sh
while [ "$#" -gt 0 ] && [ "$1" != "--" ]; do
	shift
done
if [ "$#" -eq 0 ]; then
	exit 2
fi
shift
exec /bin/sh -c "$1"
`
	if err := os.WriteFile(ghPath, []byte(ghScript), 0o755); err != nil {
		t.Fatalf("WriteFile(fake gh) error = %v", err)
	}
	if err := os.Chmod(ghPath, 0o755); err != nil {
		t.Fatalf("Chmod(fake gh) error = %v", err)
	}
	filesystemHelper := filesystemHelperTestScript(t, binDir)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	oldUmask := syscall.Umask(0o077)
	defer syscall.Umask(oldUmask)

	writers := []struct {
		name  string
		write func(context.Context, string, string, []byte, bool) error
	}{
		{
			name: "daemon",
			write: func(ctx context.Context, root, path string, content []byte, overwrite bool) error {
				return daemonWriteFile(ctx, path, content, overwrite, root)
			},
		},
		{
			name: "SSH",
			write: func(ctx context.Context, root, path string, content []byte, overwrite bool) error {
				client := ssh.NewClient("local-test")
				if err := client.SelectFilesystemHelper(filesystemHelper, helperinfo.Current()); err != nil {
					return err
				}
				return client.WriteFileRooted(ctx, ssh.RootedWriteRequest{
					Path:      path,
					Root:      root,
					Data:      content,
					Overwrite: overwrite,
				})
			},
		},
	}
	tests := []struct {
		name      string
		existing  bool
		mode      os.FileMode
		wantMode  os.FileMode
		overwrite bool
	}{
		{name: "overwrite 0666", existing: true, mode: 0o666, wantMode: 0o666, overwrite: true},
		{name: "overwrite other mode", existing: true, mode: 0o751, wantMode: 0o751, overwrite: true},
		{name: "new file defaults to 0644", wantMode: 0o644},
	}

	for _, test := range tests {
		for _, writer := range writers {
			t.Run(test.name+"/"+writer.name, func(t *testing.T) {
				root := t.TempDir()
				path := filepath.Join(root, "file.bin")
				if test.existing {
					if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
						t.Fatalf("WriteFile(existing) error = %v", err)
					}
					if err := os.Chmod(path, test.mode); err != nil {
						t.Fatalf("Chmod(existing) error = %v", err)
					}
				}

				if err := writer.write(context.Background(), root, path, []byte("new"), test.overwrite); err != nil {
					t.Fatalf("%s write error = %v", writer.name, err)
				}
				info, err := os.Stat(path)
				if err != nil {
					t.Fatalf("Stat() error = %v", err)
				}
				if got := info.Mode().Perm(); got != test.wantMode {
					t.Fatalf("mode = %04o, want %04o", got, test.wantMode)
				}
			})
		}
	}
}

func TestRunDaemonEditFileRejectsSymlinkPath(t *testing.T) {
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "target.txt")
	linkPath := filepath.Join(dir, "link.txt")
	if err := os.WriteFile(targetPath, []byte("old\n"), 0o644); err != nil {
		t.Fatalf("WriteFile target.txt: %v", err)
	}
	if err := os.Symlink(targetPath, linkPath); err != nil {
		t.Fatalf("Symlink link.txt: %v", err)
	}

	h := startDaemonTestHarness(t)
	defer h.close()
	requireHello(t, readFrameWithTimeout(t, h.dec))
	writeFrame(t, h.enc, mustRequest(t, 1, daemonproto.VerbEditFile, daemonproto.EditFileParams{
		Path:   linkPath,
		OldStr: "old",
		NewStr: "new",
	}))

	resp := readFrameWithTimeout(t, h.dec)
	if resp.Error == nil {
		t.Fatal("resp.Error = nil, want symbolic link failure")
	}
	if resp.Error.Code != daemonproto.ErrCodeExecFailed || !strings.Contains(resp.Error.Message, "symbolic link") {
		t.Fatalf("resp.Error = %+v, want clear symbolic link tool failure", resp.Error)
	}

	info, err := os.Lstat(linkPath)
	if err != nil {
		t.Fatalf("Lstat link.txt: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link.txt mode = %v, want symlink", info.Mode())
	}
	content, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("ReadFile target.txt: %v", err)
	}
	if string(content) != "old\n" {
		t.Fatalf("target.txt = %q, want unchanged", string(content))
	}
}

func TestDaemonEditFilePreservesConcurrentReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.txt")
	if err := os.WriteFile(path, []byte("before\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(source) error = %v", err)
	}

	err := daemonEditFileWithHook(context.Background(), path, "before", "after", func() error {
		if err := os.Remove(path); err != nil {
			return err
		}
		return os.WriteFile(path, []byte("concurrent\n"), 0o600)
	})
	if err == nil || !strings.Contains(err.Error(), "changed during edit") {
		t.Fatalf("daemonEditFileWithHook() error = %v, want concurrent change", err)
	}
	content, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("ReadFile(source) error = %v", readErr)
	}
	if string(content) != "concurrent\n" {
		t.Fatalf("source content = %q, want concurrent replacement", content)
	}
	info, statErr := os.Stat(path)
	if statErr != nil {
		t.Fatalf("Stat(source) error = %v", statErr)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("source mode = %o, want 600", info.Mode().Perm())
	}
}

func TestDaemonRootedReadPinsParentDirectoryAcrossSymlinkSwap(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "parent")
	held := filepath.Join(root, "held")
	outside := t.TempDir()
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatalf("Mkdir(parent) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(parent, "file.bin"), []byte("inside"), 0o644); err != nil {
		t.Fatalf("WriteFile(inside) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(outside, "file.bin"), []byte("outside"), 0o644); err != nil {
		t.Fatalf("WriteFile(outside) error = %v", err)
	}

	data, err := daemonReadFileWithHooks(context.Background(), filepath.Join(parent, "file.bin"), root, daemonRootedFileHooks{
		afterParentOpen: func() error {
			if err := os.Rename(parent, held); err != nil {
				return err
			}
			return os.Symlink(outside, parent)
		},
	})
	if err != nil {
		t.Fatalf("daemonReadFileWithHooks() error = %v", err)
	}
	if string(data) != "inside" {
		t.Fatalf("data = %q, want pinned inside file", data)
	}
}

func TestDaemonRootedWritePinsParentDirectoryAcrossSymlinkSwap(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "parent")
	held := filepath.Join(root, "held")
	outside := t.TempDir()
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatalf("Mkdir(parent) error = %v", err)
	}

	err := daemonWriteFileWithHooks(context.Background(), filepath.Join(parent, "file.bin"), []byte("inside"), false, root, daemonRootedFileHooks{
		afterParentOpen: func() error {
			if err := os.Rename(parent, held); err != nil {
				return err
			}
			return os.Symlink(outside, parent)
		},
	})
	if err != nil {
		t.Fatalf("daemonWriteFileWithHooks() error = %v", err)
	}
	data, readErr := os.ReadFile(filepath.Join(held, "file.bin"))
	if readErr != nil {
		t.Fatalf("ReadFile(pinned destination) error = %v", readErr)
	}
	if string(data) != "inside" {
		t.Fatalf("pinned destination = %q, want inside", data)
	}
	if _, statErr := os.Lstat(filepath.Join(outside, "file.bin")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("outside destination Lstat error = %v, want absent", statErr)
	}
}

func TestDaemonRootedReadWriteRejectSymlinkRoot(t *testing.T) {
	realRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(realRoot, "source.bin"), []byte("inside"), 0o644); err != nil {
		t.Fatalf("WriteFile(source) error = %v", err)
	}
	linkRoot := filepath.Join(t.TempDir(), "root")
	if err := os.Symlink(realRoot, linkRoot); err != nil {
		t.Fatalf("Symlink(root) error = %v", err)
	}

	if _, err := daemonReadFile(context.Background(), "source.bin", linkRoot); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("daemonReadFile() error = %v, want symlink root rejection", err)
	}
	if err := daemonWriteFile(context.Background(), "source.bin", []byte("outside"), true, linkRoot); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("daemonWriteFile() error = %v, want symlink root rejection", err)
	}
	content, err := os.ReadFile(filepath.Join(realRoot, "source.bin"))
	if err != nil {
		t.Fatalf("ReadFile(source) error = %v", err)
	}
	if got := string(content); got != "inside" {
		t.Fatalf("source content = %q, want unchanged", got)
	}
}

func TestDaemonRootedOverwriteRejectsConcurrentDestinationChange(t *testing.T) {
	tests := []struct {
		name        string
		change      func(t *testing.T, path string)
		wantContent string
		wantMode    os.FileMode
	}{
		{
			name: "replacement",
			change: func(t *testing.T, path string) {
				t.Helper()
				replacement := filepath.Join(filepath.Dir(path), "replacement.bin")
				if err := os.WriteFile(replacement, []byte("concurrent"), 0o600); err != nil {
					t.Fatalf("WriteFile(replacement) error = %v", err)
				}
				if err := os.Rename(replacement, path); err != nil {
					t.Fatalf("Rename(replacement) error = %v", err)
				}
			},
			wantContent: "concurrent",
			wantMode:    0o600,
		},
		{
			name: "mode",
			change: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Chmod(path, 0o600); err != nil {
					t.Fatalf("Chmod(destination) error = %v", err)
				}
			},
			wantContent: "original",
			wantMode:    0o600,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "destination.bin")
			if err := os.WriteFile(path, []byte("original"), 0o640); err != nil {
				t.Fatalf("WriteFile(destination) error = %v", err)
			}
			if err := os.Chmod(path, 0o640); err != nil {
				t.Fatalf("Chmod(destination) error = %v", err)
			}

			err := daemonWriteFileWithHooks(context.Background(), path, []byte("staged"), true, root, daemonRootedFileHooks{
				beforeCommit: func() error {
					test.change(t, path)
					return nil
				},
			})
			if err == nil || !strings.Contains(err.Error(), "changed during write") {
				t.Fatalf("daemonWriteFileWithHooks() error = %v, want concurrent change rejection", err)
			}
			content, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatalf("ReadFile(destination) error = %v", readErr)
			}
			if got := string(content); got != test.wantContent {
				t.Fatalf("destination content = %q, want %q", got, test.wantContent)
			}
			info, statErr := os.Stat(path)
			if statErr != nil {
				t.Fatalf("Stat(destination) error = %v", statErr)
			}
			if got := info.Mode().Perm(); got != test.wantMode {
				t.Fatalf("destination mode = %04o, want %04o", got, test.wantMode)
			}
			entries, readDirErr := os.ReadDir(root)
			if readDirErr != nil {
				t.Fatalf("ReadDir(root) error = %v", readDirErr)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".copilot-") {
					t.Fatalf("staged file %q was not cleaned up", entry.Name())
				}
			}
		})
	}
}

func TestDaemonRootedOverwriteTransaction(t *testing.T) {
	assertNoCopyArtifacts := func(t *testing.T, root string) {
		t.Helper()
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatalf("ReadDir(root) error = %v", err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".copilot-") {
				t.Fatalf("copy artifact %q was not cleaned up", entry.Name())
			}
		}
	}

	t.Run("rejects in-place same-inode content change", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "destination.bin")
		if err := os.WriteFile(path, []byte("original"), 0o640); err != nil {
			t.Fatalf("WriteFile(destination) error = %v", err)
		}
		before, err := os.Stat(path)
		if err != nil {
			t.Fatalf("Stat(destination) error = %v", err)
		}

		err = daemonWriteFileWithHooks(context.Background(), path, []byte("staged"), true, root, daemonRootedFileHooks{
			beforeCommit: func() error {
				file, openErr := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
				if openErr != nil {
					return openErr
				}
				if _, writeErr := file.Write([]byte("modified")); writeErr != nil {
					_ = file.Close()
					return writeErr
				}
				return file.Close()
			},
		})
		if err == nil || !strings.Contains(err.Error(), "changed during write") {
			t.Fatalf("daemonWriteFileWithHooks() error = %v, want in-place change rejection", err)
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("ReadFile(destination) error = %v", readErr)
		}
		if got := string(content); got != "modified" {
			t.Fatalf("destination content = %q, want modified", got)
		}
		after, statErr := os.Stat(path)
		if statErr != nil {
			t.Fatalf("Stat(destination after change) error = %v", statErr)
		}
		if !os.SameFile(before, after) {
			t.Fatal("destination inode changed, want deterministic in-place mutation")
		}
		assertNoCopyArtifacts(t, root)
	})

	t.Run("rejects destination appearing before install", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "destination.bin")

		err := daemonWriteFileWithHooks(context.Background(), path, []byte("staged"), true, root, daemonRootedFileHooks{
			beforeInstall: func() error {
				return os.WriteFile(path, []byte("concurrent"), 0o600)
			},
		})
		if err == nil || !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("daemonWriteFileWithHooks() error = %v, want destination conflict", err)
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("ReadFile(destination) error = %v", readErr)
		}
		if got := string(content); got != "concurrent" {
			t.Fatalf("destination content = %q, want concurrent", got)
		}
		assertNoCopyArtifacts(t, root)
	})

	t.Run("rejects replacement between staging and capture", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "destination.bin")
		replacement := filepath.Join(root, "replacement.bin")
		if err := os.WriteFile(path, []byte("original"), 0o640); err != nil {
			t.Fatalf("WriteFile(destination) error = %v", err)
		}
		if err := os.WriteFile(replacement, []byte("concurrent"), 0o600); err != nil {
			t.Fatalf("WriteFile(replacement) error = %v", err)
		}

		err := daemonWriteFileWithHooks(context.Background(), path, []byte("staged"), true, root, daemonRootedFileHooks{
			beforeCommit: func() error {
				return os.Rename(replacement, path)
			},
		})
		if err == nil || !strings.Contains(err.Error(), "changed during write") {
			t.Fatalf("daemonWriteFileWithHooks() error = %v, want replacement rejection", err)
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("ReadFile(destination) error = %v", readErr)
		}
		if got := string(content); got != "concurrent" {
			t.Fatalf("destination content = %q, want concurrent", got)
		}
		assertNoCopyArtifacts(t, root)
	})

	t.Run("preserves replacement and recovery after capture", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "destination.bin")
		if err := os.WriteFile(path, []byte("original"), 0o640); err != nil {
			t.Fatalf("WriteFile(destination) error = %v", err)
		}

		err := daemonWriteFileWithHooks(context.Background(), path, []byte("staged"), true, root, daemonRootedFileHooks{
			beforeInstall: func() error {
				return os.WriteFile(path, []byte("concurrent"), 0o600)
			},
		})
		if err == nil || !strings.Contains(err.Error(), "recovery preserved at") {
			t.Fatalf("daemonWriteFileWithHooks() error = %v, want preserved recovery error", err)
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("ReadFile(destination) error = %v", readErr)
		}
		if got := string(content); got != "concurrent" {
			t.Fatalf("destination content = %q, want concurrent", got)
		}
		recoveries, globErr := filepath.Glob(filepath.Join(root, ".copilot-*.recover"))
		if globErr != nil {
			t.Fatalf("Glob(recovery) error = %v", globErr)
		}
		if len(recoveries) != 1 {
			t.Fatalf("recovery files = %v, want one", recoveries)
		}
		recovery, readErr := os.ReadFile(recoveries[0])
		if readErr != nil {
			t.Fatalf("ReadFile(recovery) error = %v", readErr)
		}
		if got := string(recovery); got != "original" {
			t.Fatalf("recovery content = %q, want original", got)
		}
		if temps, globErr := filepath.Glob(filepath.Join(root, ".copilot-*.tmp")); globErr != nil || len(temps) != 0 {
			t.Fatalf("staged temp files = %v, error = %v", temps, globErr)
		}
	})

	t.Run("rolls back cancellation after capture", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "destination.bin")
		if err := os.WriteFile(path, []byte("original"), 0o640); err != nil {
			t.Fatalf("WriteFile(destination) error = %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())

		err := daemonWriteFileWithHooks(ctx, path, []byte("staged"), true, root, daemonRootedFileHooks{
			beforeInstall: func() error {
				cancel()
				return nil
			},
		})
		if !errors.Is(err, errDaemonCanceled) {
			t.Fatalf("daemonWriteFileWithHooks() error = %v, want cancellation", err)
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("ReadFile(destination) error = %v", readErr)
		}
		if got := string(content); got != "original" {
			t.Fatalf("destination content = %q, want original", got)
		}
		assertNoCopyArtifacts(t, root)
	})

	t.Run("commits validated overwrite", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "destination.bin")
		if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
			t.Fatalf("WriteFile(destination) error = %v", err)
		}

		if err := daemonWriteFileWithHooks(context.Background(), path, []byte("staged"), true, root, daemonRootedFileHooks{
			beforeInstall: func() error {
				for check := 0; check < 100; check++ {
					if _, err := os.Lstat(path); err != nil {
						return err
					}
				}
				return nil
			},
			afterInstall: func() error {
				for check := 0; check < 100; check++ {
					if _, err := os.Lstat(path); err != nil {
						return err
					}
				}
				return nil
			},
		}); err != nil {
			t.Fatalf("daemonWriteFileWithHooks() error = %v", err)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(destination) error = %v", err)
		}
		if got := string(content); got != "staged" {
			t.Fatalf("destination content = %q, want staged", got)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("Stat(destination) error = %v", err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("destination mode = %04o, want 0600", got)
		}
		assertNoCopyArtifacts(t, root)
	})

	t.Run("preserves concurrent replacement after atomic install", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "destination.bin")
		if err := os.WriteFile(path, []byte("original"), 0o640); err != nil {
			t.Fatalf("WriteFile(destination) error = %v", err)
		}

		err := daemonWriteFileWithHooks(context.Background(), path, []byte("staged"), true, root, daemonRootedFileHooks{
			afterInstall: func() error {
				replacement := filepath.Join(root, "replacement.bin")
				if err := os.WriteFile(replacement, []byte("concurrent"), 0o600); err != nil {
					return err
				}
				return os.Rename(replacement, path)
			},
		})
		if err == nil || !strings.Contains(err.Error(), "recovery preserved at") {
			t.Fatalf("daemonWriteFileWithHooks() error = %v, want atomic replacement conflict", err)
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("ReadFile(destination) error = %v", readErr)
		}
		if got := string(content); got != "concurrent" {
			t.Fatalf("destination content = %q, want concurrent", got)
		}
		recoveries, globErr := filepath.Glob(filepath.Join(root, ".copilot-*.recover"))
		if globErr != nil || len(recoveries) != 1 {
			t.Fatalf("recovery files = %v, error = %v, want one", recoveries, globErr)
		}
	})
}

func TestDaemonApplyPatchUpdateKeepsSourcePresentBeforeInstall(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "source.txt")
	if err := os.WriteFile(path, []byte("before\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(source) error = %v", err)
	}

	_, err := daemonApplyPatchWithHooks(context.Background(), ssh.ApplyPatchRequest{
		Cwd: dir,
		Patch: strings.Join([]string{
			"*** Begin Patch",
			"*** Update File: source.txt",
			"@@",
			"-before",
			"+after",
			"*** End Patch",
			"",
		}, "\n"),
	}, daemonPatchHooks{
		beforeActionInstall: func(index int) error {
			if index != 0 {
				return fmt.Errorf("install action index = %d, want 0", index)
			}
			for check := 0; check < 100; check++ {
				if _, err := os.Lstat(path); err != nil {
					return err
				}
			}
			return nil
		},
		afterActionInstall: func(index int) error {
			if index != 0 {
				return fmt.Errorf("installed action index = %d, want 0", index)
			}
			for check := 0; check < 100; check++ {
				if _, err := os.Lstat(path); err != nil {
					return err
				}
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("daemonApplyPatchWithHooks() error = %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(source) error = %v", err)
	}
	if got := string(content); got != "after\n" {
		t.Fatalf("source content = %q, want after", got)
	}
}

func TestDaemonApplyPatchUpdatePreservesConcurrentReplacementAfterInstall(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "source.txt")
	if err := os.WriteFile(path, []byte("before\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(source) error = %v", err)
	}

	_, err := daemonApplyPatchWithHooks(context.Background(), ssh.ApplyPatchRequest{
		Cwd: dir,
		Patch: strings.Join([]string{
			"*** Begin Patch",
			"*** Update File: source.txt",
			"@@",
			"-before",
			"+after",
			"*** End Patch",
			"",
		}, "\n"),
	}, daemonPatchHooks{
		afterActionInstall: func(index int) error {
			replacement := filepath.Join(dir, "replacement.txt")
			if err := os.WriteFile(replacement, []byte("concurrent\n"), 0o600); err != nil {
				return err
			}
			return os.Rename(replacement, path)
		},
	})
	if err == nil || !strings.Contains(err.Error(), "recovery backup preserved") {
		t.Fatalf("daemonApplyPatchWithHooks() error = %v, want atomic replacement conflict", err)
	}
	content, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("ReadFile(source) error = %v", readErr)
	}
	if got := string(content); got != "concurrent\n" {
		t.Fatalf("source content = %q, want concurrent", got)
	}
	backups, globErr := filepath.Glob(filepath.Join(dir, ".source.txt.backup.*.tmp"))
	if globErr != nil || len(backups) != 1 {
		t.Fatalf("backup files = %v, error = %v, want one", backups, globErr)
	}
}

func TestDaemonReadFileRejectsOversizedSparseFileBeforeRead(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "large.bin")
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := file.Truncate(int64(daemonproto.MaxFileTransferBytes + 1)); err != nil {
		_ = file.Close()
		t.Fatalf("Truncate() error = %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	bytesRead := 0
	_, err = daemonReadFileWithHooks(context.Background(), path, root, daemonRootedFileHooks{
		afterRead: func(n int) {
			bytesRead += n
		},
	})
	if !errors.Is(err, daemonproto.ErrFileTransferTooLarge) {
		t.Fatalf("daemonReadFileWithHooks() error = %v, want ErrFileTransferTooLarge", err)
	}
	if bytesRead != 0 {
		t.Fatalf("bytes read = %d, want rejection before read", bytesRead)
	}
}

func TestDaemonApplyPatchAddUpdateMoveDelete(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "old.txt"), []byte("one\ntwo\nthree\nfour\n"), 0o644); err != nil {
		t.Fatalf("WriteFile old.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "remove.txt"), []byte("gone\n"), 0o644); err != nil {
		t.Fatalf("WriteFile remove.txt: %v", err)
	}

	patch := strings.Join([]string{
		"*** Begin Patch",
		"*** Add File: added.txt",
		"+alpha",
		"+beta",
		"*** Update File: old.txt",
		"*** Move to: moved.txt",
		"@@",
		" one",
		"-two",
		"+TWO",
		" three",
		"@@",
		" four",
		"+five",
		"*** Delete File: remove.txt",
		"*** End Patch",
		"",
	}, "\n")

	got, err := daemonApplyPatch(context.Background(), ssh.ApplyPatchRequest{Patch: patch, Cwd: dir})
	if err != nil {
		t.Fatalf("daemonApplyPatch() error = %v", err)
	}
	if got.FilesChanged != 3 {
		t.Fatalf("FilesChanged = %d, want 3", got.FilesChanged)
	}
	for _, want := range []string{"added.txt", "moved.txt", "remove.txt"} {
		if !strings.Contains(got.Output, want) {
			t.Fatalf("Output = %q, want to mention %s", got.Output, want)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "old.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old.txt stat error = %v, want not exist", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "remove.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("remove.txt stat error = %v, want not exist", err)
	}
	added, err := os.ReadFile(filepath.Join(dir, "added.txt"))
	if err != nil {
		t.Fatalf("ReadFile added.txt: %v", err)
	}
	if string(added) != "alpha\nbeta\n" {
		t.Fatalf("added.txt = %q", string(added))
	}
	moved, err := os.ReadFile(filepath.Join(dir, "moved.txt"))
	if err != nil {
		t.Fatalf("ReadFile moved.txt: %v", err)
	}
	if string(moved) != "one\nTWO\nthree\nfour\nfive\n" {
		t.Fatalf("moved.txt = %q", string(moved))
	}
}

func TestDaemonApplyPatchContextHeaderAnchorsDuplicateContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "duplicate.txt")
	original := "section one\nvalue=old\nsection two\nvalue=old\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatalf("WriteFile duplicate.txt: %v", err)
	}

	patch := strings.Join([]string{
		"*** Begin Patch",
		"*** Update File: duplicate.txt",
		"@@ section two",
		"-value=old",
		"+value=new",
		"*** End Patch",
		"",
	}, "\n")

	if _, err := daemonApplyPatch(context.Background(), ssh.ApplyPatchRequest{Patch: patch, Cwd: dir}); err != nil {
		t.Fatalf("daemonApplyPatch() error = %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile duplicate.txt: %v", err)
	}
	want := "section one\nvalue=old\nsection two\nvalue=new\n"
	if string(content) != want {
		t.Fatalf("duplicate.txt = %q, want %q", content, want)
	}
}

func TestDaemonApplyPatchRejectsSymlinkSources(t *testing.T) {
	tests := []struct {
		name  string
		patch []string
	}{
		{
			name: "update",
			patch: []string{
				"*** Update File: link.txt",
				"@@",
				"-before",
				"+after",
			},
		},
		{
			name: "delete",
			patch: []string{
				"*** Delete File: link.txt",
			},
		},
		{
			name: "move",
			patch: []string{
				"*** Update File: link.txt",
				"*** Move to: moved.txt",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			targetPath := filepath.Join(dir, "target.txt")
			linkPath := filepath.Join(dir, "link.txt")
			if err := os.WriteFile(targetPath, []byte("before\n"), 0o644); err != nil {
				t.Fatalf("WriteFile target.txt: %v", err)
			}
			if err := os.Symlink(targetPath, linkPath); err != nil {
				t.Fatalf("Symlink link.txt: %v", err)
			}

			patchLines := append([]string{"*** Begin Patch"}, test.patch...)
			patchLines = append(patchLines, "*** End Patch", "")
			_, err := daemonApplyPatch(context.Background(), ssh.ApplyPatchRequest{
				Patch: strings.Join(patchLines, "\n"),
				Cwd:   dir,
			})
			if err == nil || !strings.Contains(err.Error(), "symbolic link") {
				t.Fatalf("daemonApplyPatch() error = %v, want symbolic link rejection", err)
			}

			info, err := os.Lstat(linkPath)
			if err != nil {
				t.Fatalf("Lstat link.txt: %v", err)
			}
			if info.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("link.txt mode = %v, want symlink", info.Mode())
			}
			content, err := os.ReadFile(targetPath)
			if err != nil {
				t.Fatalf("ReadFile target.txt: %v", err)
			}
			if string(content) != "before\n" {
				t.Fatalf("target.txt = %q, want unchanged", content)
			}
			if _, err := os.Lstat(filepath.Join(dir, "moved.txt")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("moved.txt Lstat error = %v, want not exist", err)
			}
		})
	}
}

func TestDaemonApplyPatchRejectsAliasedSourceConflicts(t *testing.T) {
	tests := []struct {
		name       string
		makeAlias  func(t *testing.T, dir, sourcePath string) string
		aliasPatch string
	}{
		{
			name: "symlinked parent component",
			makeAlias: func(t *testing.T, dir, sourcePath string) string {
				t.Helper()
				realDir := filepath.Dir(sourcePath)
				aliasDir := filepath.Join(dir, "alias")
				if err := os.Symlink(realDir, aliasDir); err != nil {
					t.Fatalf("Symlink alias: %v", err)
				}
				return filepath.Join("alias", filepath.Base(sourcePath))
			},
		},
		{
			name: "hard link inode alias",
			makeAlias: func(t *testing.T, dir, sourcePath string) string {
				t.Helper()
				aliasPath := filepath.Join(dir, "hardlink.txt")
				if err := os.Link(sourcePath, aliasPath); err != nil {
					t.Fatalf("Link hardlink.txt: %v", err)
				}
				return filepath.Base(aliasPath)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			realDir := filepath.Join(dir, "real")
			if err := os.Mkdir(realDir, 0o755); err != nil {
				t.Fatalf("Mkdir real: %v", err)
			}
			sourcePath := filepath.Join(realDir, "source.txt")
			if err := os.WriteFile(sourcePath, []byte("before\n"), 0o644); err != nil {
				t.Fatalf("WriteFile source.txt: %v", err)
			}
			aliasPath := test.makeAlias(t, dir, sourcePath)
			realPath, err := filepath.Rel(dir, sourcePath)
			if err != nil {
				t.Fatalf("Rel source.txt: %v", err)
			}

			patch := strings.Join([]string{
				"*** Begin Patch",
				"*** Update File: " + aliasPath,
				"@@",
				"-before",
				"+after",
				"*** Delete File: " + realPath,
				"*** End Patch",
				"",
			}, "\n")

			_, err = daemonApplyPatch(context.Background(), ssh.ApplyPatchRequest{Patch: patch, Cwd: dir})
			if err == nil || !strings.Contains(err.Error(), "conflicts with") {
				t.Fatalf("daemonApplyPatch() error = %v, want alias conflict", err)
			}
			content, readErr := os.ReadFile(sourcePath)
			if readErr != nil {
				t.Fatalf("ReadFile source.txt: %v", readErr)
			}
			if string(content) != "before\n" {
				t.Fatalf("source.txt = %q, want unchanged", content)
			}
		})
	}
}

func TestDaemonApplyPatchPreservesUpdatedSourceFinalNewline(t *testing.T) {
	tests := []struct {
		name     string
		original string
		moveTo   string
		want     string
	}{
		{
			name:     "with final newline",
			original: "alpha\nbeta\n",
			want:     "alpha\nBETA\n",
		},
		{
			name:     "without final newline",
			original: "alpha\nbeta",
			want:     "alpha\nBETA",
		},
		{
			name:     "move and update without final newline",
			original: "alpha\nbeta",
			moveTo:   "moved.txt",
			want:     "alpha\nBETA",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			sourcePath := filepath.Join(dir, "source.txt")
			if err := os.WriteFile(sourcePath, []byte(test.original), 0o644); err != nil {
				t.Fatalf("WriteFile source.txt: %v", err)
			}

			patchLines := []string{
				"*** Begin Patch",
				"*** Update File: source.txt",
			}
			if test.moveTo != "" {
				patchLines = append(patchLines, "*** Move to: "+test.moveTo)
			}
			patchLines = append(patchLines,
				"@@",
				"-beta",
				"+BETA",
				"*** End Patch",
				"",
			)

			if _, err := daemonApplyPatch(context.Background(), ssh.ApplyPatchRequest{
				Patch: strings.Join(patchLines, "\n"),
				Cwd:   dir,
			}); err != nil {
				t.Fatalf("daemonApplyPatch() error = %v", err)
			}

			resultPath := sourcePath
			if test.moveTo != "" {
				resultPath = filepath.Join(dir, test.moveTo)
				if _, err := os.Lstat(sourcePath); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("source.txt Lstat error = %v, want not exist", err)
				}
			}
			content, err := os.ReadFile(resultPath)
			if err != nil {
				t.Fatalf("ReadFile result: %v", err)
			}
			if string(content) != test.want {
				t.Fatalf("content = %q, want %q", content, test.want)
			}
		})
	}
}

func TestDaemonApplyPatchDeletingEveryLineProducesEmptyFile(t *testing.T) {
	got, err := daemonApplyPatchHunks(context.Background(), "only\n", []daemonPatchHunk{{
		lines: []daemonPatchLine{{op: '-', text: "only"}},
	}})
	if err != nil {
		t.Fatalf("daemonApplyPatchHunks() error = %v", err)
	}
	if got != "" {
		t.Fatalf("result = %q, want empty file", got)
	}
}

func TestDaemonApplyPatchPreservesCRLFLineEndings(t *testing.T) {
	tests := []struct {
		name     string
		original string
		want     string
	}{
		{
			name:     "with final newline",
			original: "alpha\r\nbeta\r\n",
			want:     "alpha\r\nBETA\r\n",
		},
		{
			name:     "without final newline",
			original: "alpha\r\nbeta",
			want:     "alpha\r\nBETA",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "source.txt")
			if err := os.WriteFile(path, []byte(test.original), 0o644); err != nil {
				t.Fatalf("WriteFile source.txt: %v", err)
			}
			patch := strings.Join([]string{
				"*** Begin Patch",
				"*** Update File: source.txt",
				"@@",
				"-beta",
				"+BETA",
				"*** End Patch",
				"",
			}, "\n")

			if _, err := daemonApplyPatch(context.Background(), ssh.ApplyPatchRequest{Patch: patch, Cwd: dir}); err != nil {
				t.Fatalf("daemonApplyPatch() error = %v", err)
			}
			content, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile source.txt: %v", err)
			}
			if string(content) != test.want {
				t.Fatalf("content = %q, want %q", content, test.want)
			}
		})
	}
}

func TestDaemonApplyPatchRejectsMixedLineEndingsWithoutMutation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "source.txt")
	original := "alpha\r\nbeta\ngamma\r\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatalf("WriteFile source.txt: %v", err)
	}
	patch := strings.Join([]string{
		"*** Begin Patch",
		"*** Update File: source.txt",
		"@@",
		"-beta",
		"+BETA",
		"*** End Patch",
		"",
	}, "\n")

	_, err := daemonApplyPatch(context.Background(), ssh.ApplyPatchRequest{Patch: patch, Cwd: dir})
	if err == nil || !strings.Contains(err.Error(), "mixed line endings") {
		t.Fatalf("daemonApplyPatch() error = %v, want mixed line ending rejection", err)
	}
	content, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("ReadFile source.txt: %v", readErr)
	}
	if string(content) != original {
		t.Fatalf("source.txt = %q, want unchanged", content)
	}
}

func TestDaemonApplyPatchMoveOnlyDoesNotOverwriteConcurrentDestination(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.txt")
	targetPath := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(sourcePath, []byte("source\n"), 0o644); err != nil {
		t.Fatalf("WriteFile source.txt: %v", err)
	}
	patch := strings.Join([]string{
		"*** Begin Patch",
		"*** Add File: earlier.txt",
		"+earlier",
		"*** Update File: source.txt",
		"*** Move to: target.txt",
		"*** End Patch",
		"",
	}, "\n")

	_, err := daemonApplyPatchWithHooks(context.Background(), ssh.ApplyPatchRequest{
		Patch: patch,
		Cwd:   dir,
	}, daemonPatchHooks{
		beforeMoveOnlyCommit: func(source, target string) error {
			if filepath.Base(source) != filepath.Base(sourcePath) || filepath.Base(target) != filepath.Base(targetPath) {
				return fmt.Errorf("move hook paths = %q -> %q", source, target)
			}
			return os.WriteFile(target, []byte("concurrent\n"), 0o644)
		},
	})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("daemonApplyPatchWithHooks() error = %v, want destination conflict", err)
	}
	for path, want := range map[string]string{
		sourcePath: "source\n",
		targetPath: "concurrent\n",
	} {
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("ReadFile %s: %v", path, readErr)
		}
		if string(content) != want {
			t.Fatalf("%s = %q, want %q", path, content, want)
		}
	}
	if _, err := os.Lstat(filepath.Join(dir, "earlier.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("earlier.txt Lstat error = %v, want rollback", err)
	}
}

func TestDaemonApplyPatchMoveAndDeleteLargeFilesDoNotMaterializeContent(t *testing.T) {
	dir := t.TempDir()
	const largeFileSize = 16 << 20

	for _, name := range []string{"move.bin", "delete.bin"} {
		path := filepath.Join(dir, name)
		file, err := os.Create(path)
		if err != nil {
			t.Fatalf("Create %s: %v", name, err)
		}
		if err := file.Truncate(largeFileSize); err != nil {
			_ = file.Close()
			t.Fatalf("Truncate %s: %v", name, err)
		}
		if err := file.Close(); err != nil {
			t.Fatalf("Close %s: %v", name, err)
		}
	}

	patch := strings.Join([]string{
		"*** Begin Patch",
		"*** Update File: move.bin",
		"*** Move to: moved.bin",
		"*** Delete File: delete.bin",
		"*** End Patch",
		"",
	}, "\n")

	materializeCalls := 0
	_, err := daemonApplyPatchWithHooks(context.Background(), ssh.ApplyPatchRequest{
		Patch: patch,
		Cwd:   dir,
	}, daemonPatchHooks{
		readFileContent: func(context.Context, io.Reader) ([]byte, error) {
			materializeCalls++
			return nil, errors.New("unexpected full content read")
		},
	})
	if err != nil {
		t.Fatalf("daemonApplyPatchWithHooks() error = %v", err)
	}
	if materializeCalls != 0 {
		t.Fatalf("materialize calls = %d, want 0", materializeCalls)
	}
	movedInfo, err := os.Stat(filepath.Join(dir, "moved.bin"))
	if err != nil {
		t.Fatalf("Stat moved.bin: %v", err)
	}
	if movedInfo.Size() != largeFileSize {
		t.Fatalf("moved.bin size = %d, want %d", movedInfo.Size(), largeFileSize)
	}
	for _, path := range []string{
		filepath.Join(dir, "move.bin"),
		filepath.Join(dir, "delete.bin"),
	} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("Lstat %s error = %v, want not exist", path, err)
		}
	}
}

func TestDaemonApplyPatchRevalidatesLaterSourceAfterEarlierCommit(t *testing.T) {
	dir := t.TempDir()
	firstPath := filepath.Join(dir, "first.txt")
	laterPath := filepath.Join(dir, "later.txt")
	if err := os.WriteFile(firstPath, []byte("before\n"), 0o644); err != nil {
		t.Fatalf("WriteFile first.txt: %v", err)
	}

	if err := os.WriteFile(laterPath, []byte("later\n"), 0o644); err != nil {
		t.Fatalf("WriteFile later.txt: %v", err)
	}
	laterInfo, err := os.Stat(laterPath)
	if err != nil {
		t.Fatalf("Stat later.txt: %v", err)
	}

	patch := strings.Join([]string{
		"*** Begin Patch",
		"*** Update File: first.txt",
		"@@",
		"-before",
		"+after",
		"*** Delete File: later.txt",
		"*** End Patch",
		"",
	}, "\n")

	hookCalls := 0
	_, err = daemonApplyPatchWithHooks(context.Background(), ssh.ApplyPatchRequest{
		Patch: patch,
		Cwd:   dir,
	}, daemonPatchHooks{
		afterActionCommit: func(index int) error {
			hookCalls++
			if index != 0 {
				return nil
			}
			if err := os.WriteFile(laterPath, []byte("other\n"), laterInfo.Mode().Perm()); err != nil {
				return err
			}
			if err := os.Chtimes(laterPath, laterInfo.ModTime(), laterInfo.ModTime()); err != nil {
				return err
			}
			currentInfo, err := os.Stat(laterPath)
			if err != nil {
				return err
			}
			if !daemonSamePatchFileMetadata(laterInfo, currentInfo) {
				return fmt.Errorf("failed to preserve later source metadata")
			}
			return nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "file changed during patch: later.txt") {
		t.Fatalf("daemonApplyPatchWithHooks() error = %v, want later source change", err)
	}
	if hookCalls != 1 {
		t.Fatalf("afterActionCommit calls = %d, want 1", hookCalls)
	}
	for path, want := range map[string]string{
		firstPath: "before\n",
		laterPath: "other\n",
	} {
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("ReadFile %s: %v", path, readErr)
		}
		if string(content) != want {
			t.Fatalf("%s = %q, want %q", path, content, want)
		}
	}
}

func TestDaemonApplyPatchCapturesSourceBeforeUpdateDeleteOrMove(t *testing.T) {
	tests := []struct {
		name       string
		patchLines []string
		target     string
	}{
		{
			name: "update",
			patchLines: []string{
				"*** Update File: source.txt",
				"@@",
				"-before",
				"+after",
			},
		},
		{
			name:       "delete",
			patchLines: []string{"*** Delete File: source.txt"},
		},
		{
			name: "move",
			patchLines: []string{
				"*** Update File: source.txt",
				"*** Move to: moved.txt",
			},
			target: "moved.txt",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			sourcePath := filepath.Join(dir, "source.txt")
			if err := os.WriteFile(sourcePath, []byte("before\n"), 0o644); err != nil {
				t.Fatalf("WriteFile(source) error = %v", err)
			}
			patchLines := append([]string{"*** Begin Patch"}, test.patchLines...)
			patchLines = append(patchLines, "*** End Patch", "")

			_, err := daemonApplyPatchWithHooks(context.Background(), ssh.ApplyPatchRequest{
				Patch: strings.Join(patchLines, "\n"),
				Cwd:   dir,
			}, daemonPatchHooks{
				afterActionValidate: func(index int) error {
					if index != 0 {
						return fmt.Errorf("validated action index = %d, want 0", index)
					}
					if err := os.Remove(sourcePath); err != nil {
						return err
					}
					return os.WriteFile(sourcePath, []byte("concurrent\n"), 0o600)
				},
			})
			if err == nil || !strings.Contains(err.Error(), "file changed during patch") {
				t.Fatalf("daemonApplyPatchWithHooks() error = %v, want captured-source mismatch", err)
			}
			content, readErr := os.ReadFile(sourcePath)
			if readErr != nil {
				t.Fatalf("ReadFile(source) error = %v", readErr)
			}
			if string(content) != "concurrent\n" {
				t.Fatalf("source content = %q, want concurrent replacement", content)
			}
			info, statErr := os.Stat(sourcePath)
			if statErr != nil {
				t.Fatalf("Stat(source) error = %v", statErr)
			}
			if info.Mode().Perm() != 0o600 {
				t.Fatalf("source mode = %o, want 600", info.Mode().Perm())
			}
			if test.target != "" {
				if _, statErr := os.Lstat(filepath.Join(dir, test.target)); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("target Lstat error = %v, want absent", statErr)
				}
			}
		})
	}
}

func TestDaemonApplyPatchRejectsSourceChangeAfterStagingWithoutPatchMutation(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.txt")
	deletePath := filepath.Join(dir, "delete.txt")
	if err := os.WriteFile(sourcePath, []byte("before\n"), 0o644); err != nil {
		t.Fatalf("WriteFile source.txt: %v", err)
	}
	if err := os.WriteFile(deletePath, []byte("delete\n"), 0o644); err != nil {
		t.Fatalf("WriteFile delete.txt: %v", err)
	}
	sourceInfo, err := os.Stat(sourcePath)
	if err != nil {
		t.Fatalf("Stat source.txt: %v", err)
	}

	patch := strings.Join([]string{
		"*** Begin Patch",
		"*** Add File: nested/new.txt",
		"+new",
		"*** Update File: source.txt",
		"@@",
		"-before",
		"+after",
		"*** Delete File: delete.txt",
		"*** End Patch",
		"",
	}, "\n")

	_, err = daemonApplyPatchWithHook(context.Background(), ssh.ApplyPatchRequest{
		Patch: patch,
		Cwd:   dir,
	}, func() error {
		if _, err := os.Stat(filepath.Join(dir, "nested")); err != nil {
			return fmt.Errorf("staged target directory missing: %w", err)
		}
		if err := os.WriteFile(sourcePath, []byte("alter!\n"), 0o644); err != nil {
			return err
		}
		if err := os.Chtimes(sourcePath, sourceInfo.ModTime(), sourceInfo.ModTime()); err != nil {
			return err
		}
		currentInfo, err := os.Lstat(sourcePath)
		if err != nil {
			return err
		}
		if !daemonSamePatchFileMetadata(sourceInfo, currentInfo) {
			return fmt.Errorf("failed to restore source metadata after concurrent write")
		}
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "file changed during patch") {
		t.Fatalf("daemonApplyPatchWithHook() error = %v, want concurrent change failure", err)
	}

	content, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatalf("ReadFile source.txt: %v", err)
	}
	if string(content) != "alter!\n" {
		t.Fatalf("source.txt = %q, want concurrent content preserved", content)
	}
	content, err = os.ReadFile(deletePath)
	if err != nil {
		t.Fatalf("ReadFile delete.txt: %v", err)
	}
	if string(content) != "delete\n" {
		t.Fatalf("delete.txt = %q, want unchanged", content)
	}
	if _, err := os.Lstat(filepath.Join(dir, "nested", "new.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("nested/new.txt Lstat error = %v, want not exist", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "nested")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("nested Lstat error = %v, want staging directory cleaned up", err)
	}
}

func TestRunDaemonApplyPatchMalformedInputReturnsBadRequest(t *testing.T) {
	h := startDaemonTestHarness(t)
	defer h.close()
	requireHello(t, readFrameWithTimeout(t, h.dec))

	writeFrame(t, h.enc, mustRequest(t, 1, daemonproto.VerbApplyPatch, daemonproto.ApplyPatchParams{
		Patch: "*** Begin Patch\n*** Update File: broken.txt\nnot-a-patch-line\n*** End Patch\n",
		Cwd:   t.TempDir(),
	}))

	resp := readFrameWithTimeout(t, h.dec)
	if resp.Error == nil {
		t.Fatal("resp.Error = nil, want bad request")
	}
	if resp.Error.Code != daemonproto.ErrCodeBadRequest {
		t.Fatalf("resp.Error.Code = %q, want %q", resp.Error.Code, daemonproto.ErrCodeBadRequest)
	}
}

func TestDaemonApplyPatchCheckFailureIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "old.txt")
	if err := os.WriteFile(path, []byte("keep\n"), 0o644); err != nil {
		t.Fatalf("WriteFile old.txt: %v", err)
	}

	patch := strings.Join([]string{
		"*** Begin Patch",
		"*** Add File: new.txt",
		"+new",
		"*** Update File: old.txt",
		"@@",
		"-missing",
		"+changed",
		"*** End Patch",
		"",
	}, "\n")

	if _, err := daemonApplyPatch(context.Background(), ssh.ApplyPatchRequest{Patch: patch, Cwd: dir}); err == nil {
		t.Fatal("daemonApplyPatch() error = nil, want error")
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile old.txt: %v", err)
	}
	if string(content) != "keep\n" {
		t.Fatalf("old.txt = %q, want keep", string(content))
	}
	if _, err := os.Stat(filepath.Join(dir, "new.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("new.txt stat error = %v, want not exist", err)
	}
}

func TestDaemonApplyPatchPreflightsLaterWriteTargetsBeforeMutating(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "old.txt")
	if err := os.WriteFile(path, []byte("keep\n"), 0o644); err != nil {
		t.Fatalf("WriteFile old.txt: %v", err)
	}
	lockedDir := filepath.Join(dir, "locked")
	if err := os.Mkdir(lockedDir, 0o500); err != nil {
		t.Fatalf("Mkdir locked: %v", err)
	}

	patch := strings.Join([]string{
		"*** Begin Patch",
		"*** Update File: old.txt",
		"@@",
		"-keep",
		"+changed",
		"*** Add File: locked/new.txt",
		"+new",
		"*** End Patch",
		"",
	}, "\n")

	if _, err := daemonApplyPatch(context.Background(), ssh.ApplyPatchRequest{Patch: patch, Cwd: dir}); err == nil {
		t.Fatal("daemonApplyPatch() error = nil, want error")
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile old.txt: %v", err)
	}
	if string(content) != "keep\n" {
		t.Fatalf("old.txt = %q, want keep", string(content))
	}
	if _, err := os.Stat(filepath.Join(lockedDir, "new.txt")); err == nil {
		t.Fatal("locked/new.txt exists, want failure before mutation")
	}
}

func TestDaemonApplyPatchCommitFailureRollsBack(t *testing.T) {
	dir := t.TempDir()
	updatePath := filepath.Join(dir, "update.txt")
	deletePath := filepath.Join(dir, "delete.txt")
	if err := os.WriteFile(updatePath, []byte("before\n"), 0o644); err != nil {
		t.Fatalf("WriteFile update.txt: %v", err)
	}
	if err := os.WriteFile(deletePath, []byte("keep\n"), 0o644); err != nil {
		t.Fatalf("WriteFile delete.txt: %v", err)
	}
	patch := strings.Join([]string{
		"*** Begin Patch",
		"*** Update File: update.txt",
		"@@",
		"-before",
		"+after",
		"*** Delete File: delete.txt",
		"*** Add File: a/b.txt",
		"+nested",
		"*** Add File: a",
		"+conflict",
		"*** End Patch",
		"",
	}, "\n")

	if _, err := daemonApplyPatch(context.Background(), ssh.ApplyPatchRequest{Patch: patch, Cwd: dir}); err == nil {
		t.Fatal("daemonApplyPatch() error = nil, want commit-time topology failure")
	}
	if _, err := os.Stat(filepath.Join(dir, "a")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a stat error = %v, want no partial filesystem changes", err)
	}
	for path, want := range map[string]string{
		updatePath: "before\n",
		deletePath: "keep\n",
	} {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile %s: %v", path, err)
		}
		if string(content) != want {
			t.Fatalf("%s = %q, want %q", path, string(content), want)
		}
	}
}

func TestDaemonApplyPatchRollbackPreservesConcurrentReplacements(t *testing.T) {
	tests := []struct {
		name          string
		setup         map[string]string
		firstPatch    []string
		watched       string
		restored      map[string]string
		wantBackupFor string
	}{
		{
			name: "add",
			firstPatch: []string{
				"*** Add File: watched.txt",
				"+patch",
			},
			watched: "watched.txt",
		},
		{
			name:  "update",
			setup: map[string]string{"watched.txt": "before\n"},
			firstPatch: []string{
				"*** Update File: watched.txt",
				"@@",
				"-before",
				"+after",
			},
			watched:       "watched.txt",
			wantBackupFor: "watched.txt",
		},
		{
			name:  "delete",
			setup: map[string]string{"watched.txt": "before\n"},
			firstPatch: []string{
				"*** Delete File: watched.txt",
			},
			watched:       "watched.txt",
			wantBackupFor: "watched.txt",
		},
		{
			name:  "move",
			setup: map[string]string{"source.txt": "before\n"},
			firstPatch: []string{
				"*** Update File: source.txt",
				"*** Move to: watched.txt",
			},
			watched:       "watched.txt",
			restored:      map[string]string{"source.txt": "before\n"},
			wantBackupFor: "source.txt",
		},
		{
			name:  "move with update",
			setup: map[string]string{"source.txt": "before\n"},
			firstPatch: []string{
				"*** Update File: source.txt",
				"*** Move to: watched.txt",
				"@@",
				"-before",
				"+after",
			},
			watched:       "watched.txt",
			restored:      map[string]string{"source.txt": "before\n"},
			wantBackupFor: "source.txt",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, content := range test.setup {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
					t.Fatalf("WriteFile(%s) error = %v", name, err)
				}
			}
			patchLines := append([]string{"*** Begin Patch"}, test.firstPatch...)
			patchLines = append(patchLines,
				"*** Add File: later.txt",
				"+later",
				"*** End Patch",
				"",
			)

			_, err := daemonApplyPatchWithHooks(context.Background(), ssh.ApplyPatchRequest{
				Patch: strings.Join(patchLines, "\n"),
				Cwd:   dir,
			}, daemonPatchHooks{
				afterActionCommit: func(index int) error {
					if index == 0 {
						watchedPath := filepath.Join(dir, test.watched)
						if removeErr := os.Remove(watchedPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
							return removeErr
						}
						return os.WriteFile(watchedPath, []byte("watcher\n"), 0o644)
					}
					return errors.New("injected later failure")
				},
			})
			if err == nil || !strings.Contains(err.Error(), "rollback") {
				t.Fatalf("daemonApplyPatchWithHooks() error = %v, want rollback conflict", err)
			}

			content, readErr := os.ReadFile(filepath.Join(dir, test.watched))
			if readErr != nil {
				t.Fatalf("ReadFile(watched) error = %v", readErr)
			}
			if string(content) != "watcher\n" {
				t.Fatalf("watched content = %q, want concurrent replacement", content)
			}
			for name, want := range test.restored {
				content, readErr := os.ReadFile(filepath.Join(dir, name))
				if readErr != nil {
					t.Fatalf("ReadFile(%s) error = %v", name, readErr)
				}
				if string(content) != want {
					t.Fatalf("%s content = %q, want %q", name, content, want)
				}
			}
			if _, statErr := os.Lstat(filepath.Join(dir, "later.txt")); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("later.txt Lstat error = %v, want rolled back", statErr)
			}

			backups, globErr := filepath.Glob(filepath.Join(dir, "."+test.wantBackupFor+".backup.*.tmp"))
			if globErr != nil {
				t.Fatalf("Glob(backups) error = %v", globErr)
			}
			if test.wantBackupFor == "" {
				if len(backups) != 0 {
					t.Fatalf("backups = %v, want none", backups)
				}
				return
			}
			if len(backups) != 1 {
				t.Fatalf("backups = %v, want one preserved recovery backup; error=%v", backups, err)
			}
			if !strings.Contains(err.Error(), backups[0]) {
				t.Fatalf("error = %v, want recovery backup path %q", err, backups[0])
			}
			backupContent, readErr := os.ReadFile(backups[0])
			if readErr != nil {
				t.Fatalf("ReadFile(backup) error = %v", readErr)
			}
			if string(backupContent) != "before\n" {
				t.Fatalf("backup content = %q, want original", backupContent)
			}
		})
	}
}

func TestDaemonApplyPatchRollbackUsesInstalledDigest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "watched.txt")
	if err := os.WriteFile(path, []byte("before\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	patch := strings.Join([]string{
		"*** Begin Patch",
		"*** Update File: watched.txt",
		"@@",
		"-before",
		"+after!",
		"*** Add File: later.txt",
		"+later",
		"*** End Patch",
		"",
	}, "\n")

	_, err := daemonApplyPatchWithHooks(context.Background(), ssh.ApplyPatchRequest{
		Patch: patch,
		Cwd:   dir,
	}, daemonPatchHooks{
		afterActionCommit: func(index int) error {
			if index == 0 {
				installedInfo, statErr := os.Stat(path)
				if statErr != nil {
					return statErr
				}
				if writeErr := os.WriteFile(path, []byte("watch!\n"), installedInfo.Mode().Perm()); writeErr != nil {
					return writeErr
				}
				return os.Chtimes(path, installedInfo.ModTime(), installedInfo.ModTime())
			}
			return errors.New("injected later failure")
		},
	})
	if err == nil || !strings.Contains(err.Error(), "rollback") {
		t.Fatalf("daemonApplyPatchWithHooks() error = %v, want digest conflict", err)
	}
	content, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("ReadFile() error = %v", readErr)
	}
	if string(content) != "watch!\n" {
		t.Fatalf("content = %q, want in-place concurrent write preserved", content)
	}
}

type daemonCancelAfterMissingPathContext struct {
	context.Context
	path string
}

func (c daemonCancelAfterMissingPathContext) Err() error {
	if _, err := os.Stat(c.path); errors.Is(err, os.ErrNotExist) {
		return context.Canceled
	}
	return c.Context.Err()
}

func TestDaemonApplyPatchCancellationRollsBack(t *testing.T) {
	dir := t.TempDir()
	firstPath := filepath.Join(dir, "first.txt")
	if err := os.WriteFile(firstPath, []byte("keep\n"), 0o644); err != nil {
		t.Fatalf("WriteFile first.txt: %v", err)
	}
	ctx := daemonCancelAfterMissingPathContext{
		Context: context.Background(),
		path:    firstPath,
	}
	patch := strings.Join([]string{
		"*** Begin Patch",
		"*** Delete File: first.txt",
		"*** Add File: second.txt",
		"+second",
		"*** End Patch",
		"",
	}, "\n")

	if _, err := daemonApplyPatch(ctx, ssh.ApplyPatchRequest{Patch: patch, Cwd: dir}); !errors.Is(err, errDaemonCanceled) {
		t.Fatalf("daemonApplyPatch() error = %v, want cancellation", err)
	}
	content, err := os.ReadFile(firstPath)
	if err != nil {
		t.Fatalf("ReadFile first.txt: %v", err)
	}
	if string(content) != "keep\n" {
		t.Fatalf("first.txt = %q, want restored content", string(content))
	}
	if _, err := os.Stat(filepath.Join(dir, "second.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("second.txt stat error = %v, want no partial filesystem changes", err)
	}
}

func TestDaemonApplyPatchHonorsCanceledContext(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	patch := strings.Join([]string{
		"*** Begin Patch",
		"*** Add File: new.txt",
		"+new",
		"*** End Patch",
		"",
	}, "\n")

	if _, err := daemonApplyPatch(ctx, ssh.ApplyPatchRequest{Patch: patch, Cwd: dir}); !errors.Is(err, errDaemonCanceled) && !errors.Is(err, context.Canceled) {
		t.Fatalf("daemonApplyPatch() error = %v, want cancellation", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "new.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("new.txt stat error = %v, want not exist", err)
	}
}
