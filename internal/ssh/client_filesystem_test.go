package ssh

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ekroon/gh-copilot-codespace/internal/helperinfo"
)

func runFilesystemRunner(t *testing.T, op string, payload any) (stdout string, stderr string, exitCode int) {
	t.Helper()
	return runFilesystemRunnerScript(t, filesystemRunnerScript, op, payload)
}

func runFilesystemRunnerScript(t *testing.T, script, op string, payload any) (stdout string, stderr string, exitCode int) {
	t.Helper()
	stdout, stderr, exitCode, err := runFilesystemRunnerScriptContext(context.Background(), script, op, payload)
	if err != nil {
		t.Fatalf("run filesystem runner: %v", err)
	}
	return stdout, stderr, exitCode
}

func runFilesystemRunnerScriptContext(ctx context.Context, script, op string, payload any) (stdout string, stderr string, exitCode int, runErr error) {
	input, err := json.Marshal(payload)
	if err != nil {
		return "", "", -1, fmt.Errorf("marshal payload: %w", err)
	}

	cmd := exec.CommandContext(ctx, "python3", "-c", script, op)
	cmd.Stdin = strings.NewReader(string(input))
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return "", string(output), -1, ctx.Err()
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		return "", string(output), exitErr.ExitCode(), nil
	}
	if err != nil {
		return "", "", -1, err
	}
	return string(output), "", 0, nil
}

func forceFilesystemRunnerFallback(t *testing.T) {
	t.Helper()

	python3, err := exec.LookPath("python3")
	if err != nil {
		t.Fatalf("LookPath(python3) error = %v", err)
	}
	binDir := t.TempDir()
	if err := os.Symlink(python3, filepath.Join(binDir, "python3")); err != nil {
		t.Fatalf("Symlink(python3) error = %v", err)
	}
	t.Setenv("PATH", binDir)
}

func normalizePythonSnippet(script string) string {
	return strings.ReplaceAll(script, "\t", "")
}

func decodeRunnerResult[T any](t *testing.T, stdout string) T {
	t.Helper()

	var out T
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("Unmarshal(stdout) error = %v\nstdout=%s", err, stdout)
	}
	return out
}

func readJSONFile(t *testing.T, path string, out any) {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		t.Fatalf("Unmarshal(%q) error = %v", path, err)
	}
}

func boolValue(t *testing.T, v *bool) bool {
	t.Helper()
	if v == nil {
		t.Fatal("bool pointer is nil")
	}
	return *v
}

func newVerifiedFilesystemClient(t *testing.T) *Client {
	t.Helper()
	client := NewClient("demo")
	if err := client.SelectFilesystemHelper("/remote/gh-copilot-codespace", helperinfo.Current()); err != nil {
		t.Fatalf("SelectFilesystemHelper() error = %v", err)
	}
	return client
}

func TestFilesystemRunnerViewListsDirectoriesAndSkipsHiddenEntries(t *testing.T) {
	root := t.TempDir()
	for path, content := range map[string]string{
		"a.txt":                 "alpha\n",
		".hidden.txt":           "skip\n",
		"dir/b.txt":             "beta\n",
		"dir/.hidden/note.txt":  "skip\n",
		"dir/nested/c.txt":      "gamma\n",
		"dir/nested/deep/d.txt": "delta\n",
	} {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll(%q) error = %v", full, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile(%q) error = %v", full, err)
		}
	}

	stdout, stderr, exitCode := runFilesystemRunner(t, "view", ViewRequest{Path: root})
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr)
	}

	got := decodeRunnerResult[ViewResult](t, stdout)
	wantEntries := []string{"a.txt", "dir/", "dir/b.txt", "dir/nested/"}
	if got.Kind != ViewKindDirectory {
		t.Fatalf("Kind = %q, want %q", got.Kind, ViewKindDirectory)
	}
	if !reflect.DeepEqual(got.Entries, wantEntries) {
		t.Fatalf("Entries = %v, want %v", got.Entries, wantEntries)
	}
	if got.Content != strings.Join(wantEntries, "\n")+"\n" || got.Base64Data != "" || got.Truncated {
		t.Fatalf("unexpected directory payload: %+v", got)
	}
}

func TestFilesystemRunnerViewDirectoryGlobalBounds(t *testing.T) {
	t.Run("entry limit", func(t *testing.T) {
		root := t.TempDir()
		for index := 0; index < MaxDirectoryEntries+1; index++ {
			path := filepath.Join(root, fmt.Sprintf("%04d", index))
			if err := os.WriteFile(path, nil, 0o644); err != nil {
				t.Fatalf("WriteFile(%q) error = %v", path, err)
			}
		}

		stdout, stderr, exitCode := runFilesystemRunner(t, "view", ViewRequest{Path: root})
		if exitCode != 0 {
			t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr)
		}
		got := decodeRunnerResult[ViewResult](t, stdout)
		if !got.Truncated {
			t.Fatal("Truncated = false, want true")
		}
		if len(got.Entries) != MaxDirectoryEntries {
			t.Fatalf("entries = %d, want %d", len(got.Entries), MaxDirectoryEntries)
		}
		if got.Limit != MaxDirectoryEntries || got.ByteLimit != MaxViewBytes {
			t.Fatalf("limits = (%d, %d), want (%d, %d)", got.Limit, got.ByteLimit, MaxDirectoryEntries, MaxViewBytes)
		}
		if len([]byte(got.Content)) > MaxViewBytes {
			t.Fatalf("content bytes = %d, want <= %d", len([]byte(got.Content)), MaxViewBytes)
		}
	})

	t.Run("byte limit", func(t *testing.T) {
		root := t.TempDir()
		for index := 0; index < MaxDirectoryEntries; index++ {
			name := fmt.Sprintf("%04d-%s", index, strings.Repeat("x", 80))
			if err := os.WriteFile(filepath.Join(root, name), nil, 0o644); err != nil {
				t.Fatalf("WriteFile(%q) error = %v", name, err)
			}
		}

		stdout, stderr, exitCode := runFilesystemRunner(t, "view", ViewRequest{Path: root})
		if exitCode != 0 {
			t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr)
		}
		got := decodeRunnerResult[ViewResult](t, stdout)
		if !got.Truncated {
			t.Fatal("Truncated = false, want true")
		}
		if got.Limit != MaxDirectoryEntries || got.ByteLimit != MaxViewBytes {
			t.Fatalf("limits = (%d, %d), want (%d, %d)", got.Limit, got.ByteLimit, MaxDirectoryEntries, MaxViewBytes)
		}
		if len([]byte(got.Content)) > MaxViewBytes {
			t.Fatalf("content bytes = %d, want <= %d", len([]byte(got.Content)), MaxViewBytes)
		}
		if got.Content != strings.Join(got.Entries, "\n")+"\n" {
			t.Fatal("content and entries diverged")
		}
	})

	t.Run("exact entry limit is complete", func(t *testing.T) {
		root := t.TempDir()
		for index := 0; index < MaxDirectoryEntries; index++ {
			if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("%04d", index)), nil, 0o644); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
		}

		stdout, stderr, exitCode := runFilesystemRunner(t, "view", ViewRequest{Path: root})
		if exitCode != 0 {
			t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr)
		}
		got := decodeRunnerResult[ViewResult](t, stdout)
		if got.Truncated {
			t.Fatal("Truncated = true, want false")
		}
		if len(got.Entries) != MaxDirectoryEntries {
			t.Fatalf("entries = %d, want %d", len(got.Entries), MaxDirectoryEntries)
		}
	})
}

func TestFilesystemRunnerViewDirectoryBoundsSyntheticEnumerationWork(t *testing.T) {
	root := t.TempDir()
	const handleView = "def handle_view(req):"
	instrumentation := fmt.Sprintf(`synthetic_view_nsmallest = heapq.nsmallest
synthetic_view_selection_calls = 0

def bounded_view_nsmallest(limit, iterable, key=None):
    global synthetic_view_selection_calls
    synthetic_view_selection_calls += 1
    if synthetic_view_selection_calls > 1:
        raise OSError("view re-enumerated its synthetic directory")
    if limit > %d:
        raise OSError("view selection exceeded its entry bound")
    return synthetic_view_nsmallest(limit, iterable, key=key)

heapq.nsmallest = bounded_view_nsmallest

class SyntheticViewEntry:
    def __init__(self, index):
        self.name = "file-%%06d.txt" %% index
        self.path = os.path.join(%q, self.name)

    def is_dir(self, follow_symlinks=False):
        global synthetic_view_metadata_calls
        synthetic_view_metadata_calls += 1
        if synthetic_view_metadata_calls > %d:
            raise OSError("view re-enumerated synthetic entry metadata")
        return False

class SyntheticViewScan:
    def __enter__(self):
        return self

    def __exit__(self, exc_type, exc_value, traceback):
        if synthetic_view_selection_calls != 1:
            raise OSError("view did not use bounded selection")
        return False

    def __iter__(self):
        for index in range(100000, -1, -1):
            yield SyntheticViewEntry(index)

synthetic_view_metadata_calls = 0
os.scandir = lambda path: SyntheticViewScan()

`, MaxDirectoryEntries+1, root, 100001)
	script := strings.Replace(filesystemRunnerScript, handleView, instrumentation+handleView, 1)
	if script == filesystemRunnerScript {
		t.Fatal("failed to inject synthetic view enumeration")
	}

	stdout, stderr, exitCode := runFilesystemRunnerScript(t, script, "view", ViewRequest{Path: root})
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr)
	}
	got := decodeRunnerResult[ViewResult](t, stdout)
	if len(got.Entries) != MaxDirectoryEntries || !got.Truncated {
		t.Fatalf("entries = %d, truncated = %v, want %d and true", len(got.Entries), got.Truncated, MaxDirectoryEntries)
	}
	if got.Entries[0] != "file-000000.txt" || got.Entries[len(got.Entries)-1] != "file-000999.txt" {
		t.Fatalf("entry bounds = %q ... %q, want deterministic smallest entries", got.Entries[0], got.Entries[len(got.Entries)-1])
	}
}

func TestFilesystemRunnerDirectorySearchOrderingUsesRenderedPaths(t *testing.T) {
	root := t.TempDir()
	for rel := range map[string]struct{}{
		"a.go":   {},
		"a/x.go": {},
	} {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("MkdirAll() error = %v", err)
		}
		if err := os.WriteFile(path, []byte("needle\n"), 0o644); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}
	}

	stdout, stderr, exitCode := runFilesystemRunner(t, "view", ViewRequest{Path: root})
	if exitCode != 0 {
		t.Fatalf("view exitCode = %d, stderr = %q", exitCode, stderr)
	}
	view := decodeRunnerResult[ViewResult](t, stdout)
	if want := []string{"a.go", "a/", "a/x.go"}; !reflect.DeepEqual(view.Entries, want) {
		t.Fatalf("view entries = %v, want %v", view.Entries, want)
	}

	stdout, stderr, exitCode = runFilesystemRunner(t, "glob", GlobRequest{
		Pattern: "**/*.go",
		Paths:   []string{"."},
		Cwd:     root,
		Limit:   1,
	})
	if exitCode != 0 {
		t.Fatalf("glob exitCode = %d, stderr = %q", exitCode, stderr)
	}
	glob := decodeRunnerResult[GlobResult](t, stdout)
	if glob.Output != "a.go\n" || !glob.Truncated {
		t.Fatalf("GlobResult = %+v, want rendered-path first result", glob)
	}

	stdout, stderr, exitCode = runFilesystemRunner(t, "grep", filesystemGrepCommand{
		Pattern:    "needle",
		Paths:      []string{"."},
		OutputMode: GrepOutputModeContent,
		HeadLimit:  1,
		Cwd:        root,
	})
	if exitCode != 0 {
		t.Fatalf("grep exitCode = %d, stderr = %q", exitCode, stderr)
	}
	grep := decodeRunnerResult[GrepResult](t, stdout)
	if grep.Output != "a.go:1:needle\n" || !grep.Truncated {
		t.Fatalf("GrepResult = %+v, want rendered-path first result", grep)
	}
}

func TestFilesystemRunnerViewTruncatesLargeFilesUnlessForced(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "large.txt")

	var builder strings.Builder
	for i := 1; i <= 500; i++ {
		builder.WriteString(strings.Repeat("x", 60))
		builder.WriteString("\n")
	}
	if err := os.WriteFile(path, []byte(builder.String()), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	stdout, stderr, exitCode := runFilesystemRunner(t, "view", ViewRequest{Path: path})
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr)
	}
	truncated := decodeRunnerResult[ViewResult](t, stdout)
	if truncated.Kind != ViewKindFile {
		t.Fatalf("Kind = %q, want %q", truncated.Kind, ViewKindFile)
	}
	if !truncated.Truncated {
		t.Fatal("Truncated = false, want true")
	}
	if strings.Contains(truncated.Content, "500.") {
		t.Fatalf("Content unexpectedly includes final line:\n%s", truncated.Content)
	}

	stdout, stderr, exitCode = runFilesystemRunner(t, "view", ViewRequest{
		Path:                path,
		ForceReadLargeFiles: true,
	})
	if exitCode != 0 {
		t.Fatalf("forced exitCode = %d, stderr = %q", exitCode, stderr)
	}
	forced := decodeRunnerResult[ViewResult](t, stdout)
	if forced.Truncated {
		t.Fatal("Truncated = true, want false when forceReadLargeFiles is set")
	}
	if !strings.Contains(forced.Content, "500. ") {
		t.Fatalf("Content missing final line:\n%s", forced.Content)
	}
}

func TestFilesystemRunnerViewReturnsImageAndBinaryMetadata(t *testing.T) {
	root := t.TempDir()

	pngPath := filepath.Join(root, "pixel.png")
	pngData, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO5W0r8AAAAASUVORK5CYII=")
	if err != nil {
		t.Fatalf("DecodeString() error = %v", err)
	}
	if err := os.WriteFile(pngPath, pngData, 0o644); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", pngPath, err)
	}

	stdout, stderr, exitCode := runFilesystemRunner(t, "view", ViewRequest{Path: pngPath})
	if exitCode != 0 {
		t.Fatalf("image exitCode = %d, stderr = %q", exitCode, stderr)
	}
	imageResult := decodeRunnerResult[ViewResult](t, stdout)
	if imageResult.Kind != ViewKindImage {
		t.Fatalf("Kind = %q, want %q", imageResult.Kind, ViewKindImage)
	}
	if imageResult.MimeType != "image/png" {
		t.Fatalf("MimeType = %q, want %q", imageResult.MimeType, "image/png")
	}
	if imageResult.Base64Data == "" || !strings.Contains(imageResult.Content, "Image file (image/png)") {
		t.Fatalf("unexpected image payload: %+v", imageResult)
	}

	binaryPath := filepath.Join(root, "blob.bin")
	if err := os.WriteFile(binaryPath, []byte{0x00, 0x01, 0x02, 0x03}, 0o644); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", binaryPath, err)
	}

	stdout, stderr, exitCode = runFilesystemRunner(t, "view", ViewRequest{Path: binaryPath})
	if exitCode != 0 {
		t.Fatalf("binary exitCode = %d, stderr = %q", exitCode, stderr)
	}
	binaryResult := decodeRunnerResult[ViewResult](t, stdout)
	if binaryResult.Kind != ViewKindFile {
		t.Fatalf("Kind = %q, want %q", binaryResult.Kind, ViewKindFile)
	}
	if !strings.Contains(binaryResult.Content, "Binary file") || binaryResult.Base64Data != "" || binaryResult.MimeType == "" || !binaryResult.Truncated {
		t.Fatalf("unexpected binary payload: %+v", binaryResult)
	}
}

func TestFilesystemRunnerViewLargeImageRequiresForceForBinaryData(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "large.png")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 25*1024)), 0o644); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}

	stdout, stderr, exitCode := runFilesystemRunner(t, "view", ViewRequest{Path: path})
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr)
	}
	truncated := decodeRunnerResult[ViewResult](t, stdout)
	if truncated.Kind != ViewKindImage || !truncated.Truncated || truncated.Base64Data != "" {
		t.Fatalf("truncated image result = %+v", truncated)
	}

	stdout, stderr, exitCode = runFilesystemRunner(t, "view", ViewRequest{
		Path:                path,
		ForceReadLargeFiles: true,
	})
	if exitCode != 0 {
		t.Fatalf("forced exitCode = %d, stderr = %q", exitCode, stderr)
	}
	forced := decodeRunnerResult[ViewResult](t, stdout)
	if forced.Kind != ViewKindImage || forced.Truncated || forced.Base64Data == "" {
		t.Fatalf("forced image result = %+v", forced)
	}
}

func TestFilesystemRunnerCreateRefusesOverwrite(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "existing.txt")
	if err := os.WriteFile(path, []byte("original\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	_, stderr, exitCode := runFilesystemRunner(t, "create", map[string]any{
		"path":    path,
		"content": "new content\n",
	})
	if exitCode == 0 {
		t.Fatal("exitCode = 0, want failure")
	}
	if !strings.Contains(stderr, "already exists") {
		t.Fatalf("stderr = %q, want overwrite refusal", stderr)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(content) != "original\n" {
		t.Fatalf("file content = %q, want original", string(content))
	}
}

func TestFilesystemRunnerCreateUsesDefaultFilePermissions(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "new.txt")

	_, stderr, exitCode := runFilesystemRunner(t, "create", map[string]any{
		"path":    path,
		"content": "hello\n",
	})
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%q) error = %v", path, err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Fatalf("mode = %v, want 0644", got)
	}
}

func TestFilesystemRunnerGrepFallsBackWithoutRipgrep(t *testing.T) {
	root := t.TempDir()
	python3, err := exec.LookPath("python3")
	if err != nil {
		t.Fatalf("LookPath(python3) error = %v", err)
	}
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", binDir, err)
	}
	if err := os.Symlink(python3, filepath.Join(binDir, "python3")); err != nil {
		t.Fatalf("Symlink(python3) error = %v", err)
	}
	t.Setenv("PATH", binDir)

	for rel, content := range map[string]string{
		"src/component.tsx": "Needle\n",
		"src/ignore.go":     "other\n",
	} {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll(%q) error = %v", full, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile(%q) error = %v", full, err)
		}
	}

	t.Run("dash-prefixed pattern uses argument separator", func(t *testing.T) {
		t.Run("ripgrep", func(t *testing.T) {
			root := t.TempDir()
			python3, err := exec.LookPath("python3")
			if err != nil {
				t.Fatalf("LookPath(python3) error = %v", err)
			}
			binDir := filepath.Join(root, "bin")
			if err := os.MkdirAll(binDir, 0o755); err != nil {
				t.Fatalf("MkdirAll() error = %v", err)
			}
			if err := os.Symlink(python3, filepath.Join(binDir, "python3")); err != nil {
				t.Fatalf("Symlink(python3) error = %v", err)
			}
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

			client := NewClient("demo")
			req, err := client.buildFilesystemGrepRequest(GrepRequest{
				Pattern:    "--force",
				Paths:      []string{"--search-root"},
				OutputMode: GrepOutputModeContent,
				Cwd:        root,
			})
			if err != nil {
				t.Fatalf("buildFilesystemGrepRequest() error = %v", err)
			}
			stdout, stderr, exitCode := runFilesystemRunner(t, "grep", req)
			if exitCode != 0 {
				t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr)
			}
			got := decodeRunnerResult[struct {
				Output string `json:"output"`
			}](t, stdout)
			if got.Output != "--search-root/file.txt:1:--force\n" {
				t.Fatalf("Output = %q", got.Output)
			}
		})

		t.Run("fallback", func(t *testing.T) {
			root := t.TempDir()
			python3, err := exec.LookPath("python3")
			if err != nil {
				t.Fatalf("LookPath(python3) error = %v", err)
			}
			binDir := filepath.Join(root, "bin")
			if err := os.MkdirAll(binDir, 0o755); err != nil {
				t.Fatalf("MkdirAll() error = %v", err)
			}
			if err := os.Symlink(python3, filepath.Join(binDir, "python3")); err != nil {
				t.Fatalf("Symlink(python3) error = %v", err)
			}
			t.Setenv("PATH", binDir)

			searchRoot := filepath.Join(root, "search-root")
			if err := os.MkdirAll(searchRoot, 0o755); err != nil {
				t.Fatalf("MkdirAll(searchRoot) error = %v", err)
			}
			if err := os.WriteFile(filepath.Join(searchRoot, "flags.txt"), []byte("before\n--force\nafter\n"), 0o644); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}

			client := NewClient("demo")
			req, err := client.buildFilesystemGrepRequest(GrepRequest{
				Pattern:    "--force",
				Paths:      []string{"search-root"},
				OutputMode: GrepOutputModeContent,
				Cwd:        root,
			})
			if err != nil {
				t.Fatalf("buildFilesystemGrepRequest() error = %v", err)
			}
			stdout, stderr, exitCode := runFilesystemRunner(t, "grep", req)
			if exitCode != 0 {
				t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr)
			}
			got := decodeRunnerResult[struct {
				Output string `json:"output"`
			}](t, stdout)
			if got.Output != "search-root/flags.txt:2:--force\n" {
				t.Fatalf("Output = %q", got.Output)
			}
		})
	})

	client := NewClient("demo")
	req, err := client.buildFilesystemGrepRequest(GrepRequest{
		Pattern:         "needle",
		Paths:           []string{"src"},
		OutputMode:      GrepOutputModeFilesWithMatches,
		Type:            "tsx",
		CaseInsensitive: true,
		Cwd:             root,
	})
	if err != nil {
		t.Fatalf("buildFilesystemGrepRequest() error = %v", err)
	}

	stdout, stderr, exitCode := runFilesystemRunner(t, "grep", req)
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr)
	}
	got := decodeRunnerResult[struct {
		Output string `json:"output"`
	}](t, stdout)
	if got.Output != "src/component.tsx\n" {
		t.Fatalf("Output = %q, want %q", got.Output, "src/component.tsx\n")
	}
}

func TestFilesystemRunnerFallbackGrepReadsRegularFile(t *testing.T) {
	forceFilesystemRunnerFallback(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "regular.txt"), []byte("needle\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	stdout, stderr, exitCode := runFilesystemRunner(t, "grep", filesystemGrepCommand{
		Pattern:    "needle",
		Paths:      []string{"."},
		OutputMode: GrepOutputModeFilesWithMatches,
		Cwd:        root,
	})
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr)
	}
	got := decodeRunnerResult[GrepResult](t, stdout)
	if got.Output != "regular.txt\n" {
		t.Fatalf("Output = %q, want regular file only", got.Output)
	}
}

func TestFilesystemRunnerFallbackGrepSkipsSymlinkOutsideRoot(t *testing.T) {
	forceFilesystemRunnerFallback(t)
	root := t.TempDir()
	outside := t.TempDir()
	outsidePath := filepath.Join(outside, "outside.txt")
	if err := os.WriteFile(outsidePath, []byte("needle\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(outside) error = %v", err)
	}
	if err := os.Symlink(outsidePath, filepath.Join(root, "outside-link.txt")); err != nil {
		t.Fatalf("Symlink(outside) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "regular.txt"), []byte("needle\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(regular) error = %v", err)
	}

	stdout, stderr, exitCode := runFilesystemRunner(t, "grep", filesystemGrepCommand{
		Pattern:    "needle",
		Paths:      []string{"."},
		OutputMode: GrepOutputModeFilesWithMatches,
		Cwd:        root,
	})
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr)
	}
	got := decodeRunnerResult[GrepResult](t, stdout)
	if got.Output != "regular.txt\n" {
		t.Fatalf("Output = %q, want external symlink skipped", got.Output)
	}
}

func TestFilesystemRunnerFallbackGrepSkipsFIFOWithoutHanging(t *testing.T) {
	forceFilesystemRunnerFallback(t)
	root := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(root, "a-fifo"), 0o600); err != nil {
		t.Fatalf("Mkfifo() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "regular.txt"), []byte("needle\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(regular) error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	stdout, stderr, exitCode, err := runFilesystemRunnerScriptContext(ctx, filesystemRunnerScript, "grep", filesystemGrepCommand{
		Pattern:    "needle",
		Paths:      []string{"."},
		OutputMode: GrepOutputModeFilesWithMatches,
		Cwd:        root,
	})
	if err != nil {
		t.Fatalf("fallback grep did not finish before timeout: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr)
	}
	got := decodeRunnerResult[GrepResult](t, stdout)
	if got.Output != "regular.txt\n" {
		t.Fatalf("Output = %q, want FIFO skipped", got.Output)
	}
}

func TestFilesystemRunnerFallbackGrepSkipsUnixSocket(t *testing.T) {
	forceFilesystemRunnerFallback(t)
	root := t.TempDir()
	shortRoot := fmt.Sprintf(".ssh-grep-socket-%d-%d", os.Getpid(), time.Now().UnixNano())
	if err := os.Symlink(root, shortRoot); err != nil {
		t.Fatalf("Symlink(short socket root) error = %v", err)
	}
	defer os.Remove(shortRoot)
	listener, err := net.Listen("unix", filepath.Join(shortRoot, "a-socket"))
	if err != nil {
		t.Fatalf("Listen(unix) error = %v", err)
	}
	defer listener.Close()
	if err := os.WriteFile(filepath.Join(root, "regular.txt"), []byte("needle\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(regular) error = %v", err)
	}

	stdout, stderr, exitCode := runFilesystemRunner(t, "grep", filesystemGrepCommand{
		Pattern:    "needle",
		Paths:      []string{"."},
		OutputMode: GrepOutputModeFilesWithMatches,
		Cwd:        root,
	})
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr)
	}
	got := decodeRunnerResult[GrepResult](t, stdout)
	if got.Output != "regular.txt\n" {
		t.Fatalf("Output = %q, want socket skipped", got.Output)
	}
}

func TestFilesystemRunnerGrepHeadLimitTruncatesOnlyWhenOutputIsOmitted(t *testing.T) {
	modes := []GrepOutputMode{
		GrepOutputModeContent,
		GrepOutputModeFilesWithMatches,
		GrepOutputModeCount,
	}
	for _, mode := range modes {
		for _, overflow := range []bool{false, true} {
			name := string(mode) + "/exact"
			if overflow {
				name = string(mode) + "/overflow"
			}
			t.Run("fallback/"+name, func(t *testing.T) {
				root := t.TempDir()
				python3, err := exec.LookPath("python3")
				if err != nil {
					t.Fatalf("LookPath(python3) error = %v", err)
				}
				binDir := filepath.Join(root, "bin")
				if err := os.MkdirAll(binDir, 0o755); err != nil {
					t.Fatalf("MkdirAll() error = %v", err)
				}
				if err := os.Symlink(python3, filepath.Join(binDir, "python3")); err != nil {
					t.Fatalf("Symlink(python3) error = %v", err)
				}
				t.Setenv("PATH", binDir)

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

				stdout, stderr, exitCode := runFilesystemRunner(t, "grep", filesystemGrepCommand{
					Pattern:    "match",
					Paths:      []string{"."},
					OutputMode: mode,
					HeadLimit:  1,
					Cwd:        root,
				})
				if exitCode != 0 {
					t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr)
				}
				got := decodeRunnerResult[GrepResult](t, stdout)
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
			root := t.TempDir()
			python3, err := exec.LookPath("python3")
			if err != nil {
				t.Fatalf("LookPath(python3) error = %v", err)
			}
			binDir := filepath.Join(root, "bin")
			if err := os.MkdirAll(binDir, 0o755); err != nil {
				t.Fatalf("MkdirAll() error = %v", err)
			}
			if err := os.Symlink(python3, filepath.Join(binDir, "python3")); err != nil {
				t.Fatalf("Symlink(python3) error = %v", err)
			}
			t.Setenv("PATH", binDir)

			content := "before\nmatch\nafter\n"
			if overflow {
				content += "gap\nmatch\n"
			}
			if err := os.WriteFile(filepath.Join(root, "context.txt"), []byte(content), 0o644); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}

			stdout, stderr, exitCode := runFilesystemRunner(t, "grep", filesystemGrepCommand{
				Pattern:       "match",
				Paths:         []string{"."},
				OutputMode:    GrepOutputModeContent,
				BeforeContext: 1,
				AfterContext:  1,
				HeadLimit:     3,
				Cwd:           root,
			})
			if exitCode != 0 {
				t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr)
			}
			got := decodeRunnerResult[GrepResult](t, stdout)
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
				root := t.TempDir()
				python3, err := exec.LookPath("python3")
				if err != nil {
					t.Fatalf("LookPath(python3) error = %v", err)
				}
				binDir := filepath.Join(root, "bin")
				if err := os.MkdirAll(binDir, 0o755); err != nil {
					t.Fatalf("MkdirAll() error = %v", err)
				}
				if err := os.Symlink(python3, filepath.Join(binDir, "python3")); err != nil {
					t.Fatalf("Symlink(python3) error = %v", err)
				}
				if err := os.WriteFile(filepath.Join(binDir, "rg"), []byte("#!/bin/sh\nprintf '%s' \"$RG_TEST_OUTPUT\"\n"), 0o755); err != nil {
					t.Fatalf("WriteFile(rg) error = %v", err)
				}
				t.Setenv("PATH", binDir)
				t.Setenv("RG_TEST_OUTPUT", output)

				stdout, stderr, exitCode := runFilesystemRunner(t, "grep", filesystemGrepCommand{
					Args:       []string{"rg"},
					HeadLimit:  tc.head,
					Cwd:        root,
					AllowExit1: true,
				})
				if exitCode != 0 {
					t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr)
				}
				got := decodeRunnerResult[GrepResult](t, stdout)
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

func TestFilesystemRunnerGrepByteLimitTruncatesOnlyOnOverflow(t *testing.T) {
	for _, overflow := range []bool{false, true} {
		name := "exact"
		byteCount := MaxGrepOutputBytes
		if overflow {
			name = "overflow"
			byteCount++
		}

		t.Run("ripgrep/"+name, func(t *testing.T) {
			root := t.TempDir()
			python3, err := exec.LookPath("python3")
			if err != nil {
				t.Fatalf("LookPath(python3) error = %v", err)
			}
			binDir := filepath.Join(root, "bin")
			if err := os.MkdirAll(binDir, 0o755); err != nil {
				t.Fatalf("MkdirAll() error = %v", err)
			}
			if err := os.Symlink(python3, filepath.Join(binDir, "python3")); err != nil {
				t.Fatalf("Symlink(python3) error = %v", err)
			}
			producer := fmt.Sprintf(`#!%s
import os
import sys
size = int(os.environ["RG_TEST_BYTES"])
sys.stdout.write("x" * (size - 1) + "\n")
`, python3)
			if err := os.WriteFile(filepath.Join(binDir, "rg"), []byte(producer), 0o755); err != nil {
				t.Fatalf("WriteFile(rg) error = %v", err)
			}
			t.Setenv("PATH", binDir)
			t.Setenv("RG_TEST_BYTES", strconv.Itoa(byteCount))

			stdout, stderr, exitCode := runFilesystemRunner(t, "grep", filesystemGrepCommand{
				Args:       []string{"rg"},
				Cwd:        root,
				AllowExit1: true,
			})
			if exitCode != 0 {
				t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr)
			}
			got := decodeRunnerResult[GrepResult](t, stdout)
			if got.Truncated != overflow {
				t.Fatalf("Truncated = %v, want %v", got.Truncated, overflow)
			}
			if len([]byte(got.Output)) != MaxGrepOutputBytes {
				t.Fatalf("output bytes = %d, want %d", len([]byte(got.Output)), MaxGrepOutputBytes)
			}
		})

		t.Run("fallback/"+name, func(t *testing.T) {
			root := t.TempDir()
			python3, err := exec.LookPath("python3")
			if err != nil {
				t.Fatalf("LookPath(python3) error = %v", err)
			}
			binDir := filepath.Join(root, "bin")
			if err := os.MkdirAll(binDir, 0o755); err != nil {
				t.Fatalf("MkdirAll() error = %v", err)
			}
			if err := os.Symlink(python3, filepath.Join(binDir, "python3")); err != nil {
				t.Fatalf("Symlink(python3) error = %v", err)
			}
			t.Setenv("PATH", binDir)

			const display = "match.txt"
			prefix := display + ":1:"
			contentBytes := MaxGrepOutputBytes - len(prefix) - 1
			if overflow {
				contentBytes++
			}
			if err := os.WriteFile(filepath.Join(root, display), []byte(strings.Repeat("x", contentBytes)+"\n"), 0o644); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}

			stdout, stderr, exitCode := runFilesystemRunner(t, "grep", filesystemGrepCommand{
				Pattern:    "x",
				Paths:      []string{"."},
				OutputMode: GrepOutputModeContent,
				Cwd:        root,
			})
			if exitCode != 0 {
				t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr)
			}
			got := decodeRunnerResult[GrepResult](t, stdout)
			if got.Truncated != overflow {
				t.Fatalf("Truncated = %v, want %v", got.Truncated, overflow)
			}
			if len([]byte(got.Output)) != MaxGrepOutputBytes {
				t.Fatalf("output bytes = %d, want %d", len([]byte(got.Output)), MaxGrepOutputBytes)
			}
		})
	}
}

func TestFilesystemRunnerGrepHeadLimitTerminatesProducer(t *testing.T) {
	root := t.TempDir()
	python3, err := exec.LookPath("python3")
	if err != nil {
		t.Fatalf("LookPath(python3) error = %v", err)
	}
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", binDir, err)
	}
	if err := os.Symlink(python3, filepath.Join(binDir, "python3")); err != nil {
		t.Fatalf("Symlink(python3) error = %v", err)
	}

	t.Run("byte limit terminates producer", func(t *testing.T) {
		root := t.TempDir()
		python3, err := exec.LookPath("python3")
		if err != nil {
			t.Fatalf("LookPath(python3) error = %v", err)
		}
		binDir := filepath.Join(root, "bin")
		if err := os.MkdirAll(binDir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%q) error = %v", binDir, err)
		}
		if err := os.Symlink(python3, filepath.Join(binDir, "python3")); err != nil {
			t.Fatalf("Symlink(python3) error = %v", err)
		}

		statusPath := filepath.Join(root, "producer-status.json")
		rgPath := filepath.Join(binDir, "rg")
		producer := normalizePythonSnippet(`#!` + python3 + `
	import json
	import os
	import signal
	import sys

	status_path = os.environ["RG_PRODUCER_STATUS"]
	produced = 0

	def finish(status):
	    with open(status_path, "w", encoding="utf-8") as handle:
	        json.dump({"status": status, "produced": produced}, handle)
	    raise SystemExit(0)

	signal.signal(signal.SIGTERM, lambda signum, frame: finish("terminated"))

	try:
	    for index in range(100000):
	        sys.stdout.write("%05d:%s\n" % (index, "x" * 1024))
	        sys.stdout.flush()
	        produced += 1
	except BrokenPipeError:
	    finish("broken_pipe")

	finish("completed")
	`)
		if err := os.WriteFile(rgPath, []byte(producer), 0o755); err != nil {
			t.Fatalf("WriteFile(rg) error = %v", err)
		}
		t.Setenv("PATH", binDir)
		t.Setenv("RG_PRODUCER_STATUS", statusPath)

		stdout, stderr, exitCode := runFilesystemRunner(t, "grep", filesystemGrepCommand{
			Args:       []string{"rg", "--", "needle", "."},
			Cwd:        root,
			AllowExit1: true,
		})
		if exitCode != 0 {
			t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr)
		}
		got := decodeRunnerResult[GrepResult](t, stdout)
		if !got.Truncated {
			t.Fatal("Truncated = false, want true")
		}
		if got.Limit != MaxGrepOutputBytes || got.ByteLimit != MaxGrepOutputBytes {
			t.Fatalf("limits = (%d, %d), want %d", got.Limit, got.ByteLimit, MaxGrepOutputBytes)
		}
		if len([]byte(got.Output)) > MaxGrepOutputBytes {
			t.Fatalf("output bytes = %d, want <= %d", len([]byte(got.Output)), MaxGrepOutputBytes)
		}

		var status struct {
			Status   string `json:"status"`
			Produced int    `json:"produced"`
		}
		readJSONFile(t, statusPath, &status)
		if status.Status == "completed" {
			t.Fatalf("producer status = %q, want early termination", status.Status)
		}
		if status.Produced >= 10000 {
			t.Fatalf("producer emitted %d records, want bounded consumption", status.Produced)
		}
	})

	t.Run("fallback byte limit stops walking", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("needle"+strings.Repeat("x", MaxGrepOutputBytes)+"\n"), 0o644); err != nil {
			t.Fatalf("WriteFile(a.txt) error = %v", err)
		}
		if err := os.WriteFile(filepath.Join(root, "z.txt"), []byte("needle\n"), 0o644); err != nil {
			t.Fatalf("WriteFile(z.txt) error = %v", err)
		}

		walkLoop := normalizePythonSnippet(`    for file in walk_search_files(paths, cwd, False, directory_batch_limit):
	        rel = file["rel"]`)
		injectedWalk := normalizePythonSnippet(`    for file in walk_search_files(paths, cwd, False, directory_batch_limit):
	        if file["rel"] == "z.txt":
	            raise OSError("fallback grep walked past byte limit")
	        rel = file["rel"]`)
		script := strings.Replace(filesystemRunnerScript, walkLoop, injectedWalk, 1)
		if script == filesystemRunnerScript {
			t.Fatal("failed to inject fallback walk guard")
		}

		stdout, stderr, exitCode := runFilesystemRunnerScript(t, script, "grep", filesystemGrepCommand{
			Pattern:    "needle",
			Paths:      []string{"."},
			OutputMode: GrepOutputModeContent,
			Cwd:        root,
		})
		if exitCode != 0 {
			t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr)
		}
		got := decodeRunnerResult[GrepResult](t, stdout)
		if !got.Truncated {
			t.Fatal("Truncated = false, want true")
		}
		if len([]byte(got.Output)) > MaxGrepOutputBytes {
			t.Fatalf("output bytes = %d, want <= %d", len([]byte(got.Output)), MaxGrepOutputBytes)
		}
	})

	statusPath := filepath.Join(root, "producer-status.json")
	rgPath := filepath.Join(binDir, "rg")
	producer := `#!` + python3 + `
import json
import os
import signal
import sys

status_path = os.environ["RG_PRODUCER_STATUS"]
produced = 0

def finish(status):
    with open(status_path, "w", encoding="utf-8") as handle:
        json.dump({"status": status, "produced": produced}, handle)
    raise SystemExit(0)

signal.signal(signal.SIGTERM, lambda signum, frame: finish("terminated"))

try:
    for index in range(10000):
        sys.stdout.write("%05d:%s\n" % (index, "x" * 1024))
        sys.stdout.flush()
        produced += 1
except BrokenPipeError:
    finish("broken_pipe")

finish("completed")
`
	if err := os.WriteFile(rgPath, []byte(producer), 0o755); err != nil {
		t.Fatalf("WriteFile(rg) error = %v", err)
	}
	t.Setenv("PATH", binDir)
	t.Setenv("RG_PRODUCER_STATUS", statusPath)

	stdout, stderr, exitCode := runFilesystemRunner(t, "grep", filesystemGrepCommand{
		Args:       []string{"rg", "--", "needle", "."},
		Cwd:        root,
		HeadLimit:  3,
		AllowExit1: true,
	})
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr)
	}
	got := decodeRunnerResult[GrepResult](t, stdout)
	if lines := strings.Split(strings.TrimSuffix(got.Output, "\n"), "\n"); len(lines) != 3 {
		t.Fatalf("output line count = %d, want 3", len(lines))
	}
	if !got.Truncated {
		t.Fatal("Truncated = false, want true")
	}

	var status struct {
		Status   string `json:"status"`
		Produced int    `json:"produced"`
	}
	readJSONFile(t, statusPath, &status)
	if status.Status == "completed" {
		t.Fatalf("producer status = %q, want early termination", status.Status)
	}
	if status.Produced >= 1000 {
		t.Fatalf("producer emitted %d records, want bounded consumption", status.Produced)
	}
}

func TestFilesystemRunnerFallbackGrepHeadLimitStopsWalking(t *testing.T) {
	root := t.TempDir()
	for rel, content := range map[string]string{
		"a.txt": "needle\n",
		"b.txt": "needle\n",
		"z.txt": "other\n",
	} {
		if err := os.WriteFile(filepath.Join(root, rel), []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile(%q) error = %v", rel, err)
		}
	}

	const walkLoop = `    for file in walk_search_files(paths, cwd, False, directory_batch_limit):
        rel = file["rel"]`
	injectedWalk := `    for file in walk_search_files(paths, cwd, False, directory_batch_limit):
        if file["rel"] == "z.txt":
            raise OSError("fallback grep walked past head limit")
        rel = file["rel"]`
	script := strings.Replace(filesystemRunnerScript, walkLoop, injectedWalk, 1)
	if script == filesystemRunnerScript {
		t.Fatal("failed to inject fallback walk guard")
	}

	stdout, stderr, exitCode := runFilesystemRunnerScript(t, script, "grep", filesystemGrepCommand{
		Pattern:    "needle",
		Paths:      []string{"."},
		OutputMode: GrepOutputModeContent,
		LineNumbers: func() *bool {
			value := true
			return &value
		}(),
		HeadLimit: 1,
		Cwd:       root,
	})
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr)
	}
	got := decodeRunnerResult[GrepResult](t, stdout)
	if got.Output != "a.txt:1:needle\n" || !got.Truncated {
		t.Fatalf("GrepResult = %+v, want first match and truncation", got)
	}
}

func TestFilesystemRunnerFallbackGrepBoundsSyntheticDirectoryWork(t *testing.T) {
	root := t.TempDir()
	contentPath := filepath.Join(root, "content.txt")
	if err := os.WriteFile(contentPath, []byte("needle\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	const fallbackGrep = "def fallback_grep(req):"
	instrumentation := fmt.Sprintf(`synthetic_grep_nsmallest = heapq.nsmallest
synthetic_grep_selection_calls = 0

def bounded_grep_nsmallest(limit, iterable, key=None):
    global synthetic_grep_selection_calls
    synthetic_grep_selection_calls += 1
    if synthetic_grep_selection_calls > 1:
        raise OSError("grep re-enumerated after satisfying its head limit")
    if limit > 2:
        raise OSError("grep selection exceeded its limit probe")
    return synthetic_grep_nsmallest(limit, iterable, key=key)

heapq.nsmallest = bounded_grep_nsmallest

class SyntheticGrepEntry:
    def __init__(self, index):
        self.name = "file-%%06d.txt" %% index
        self.path = %q

    def is_dir(self, follow_symlinks=False):
        global synthetic_grep_metadata_calls
        synthetic_grep_metadata_calls += 1
        if synthetic_grep_metadata_calls > 100001:
            raise OSError("grep re-enumerated synthetic entry metadata")
        return False

class SyntheticGrepScan:
    def __enter__(self):
        return self

    def __exit__(self, exc_type, exc_value, traceback):
        if synthetic_grep_selection_calls != 1:
            raise OSError("grep did not use bounded selection")
        return False

    def __iter__(self):
        for index in range(100000, -1, -1):
            yield SyntheticGrepEntry(index)

synthetic_grep_metadata_calls = 0
os.scandir = lambda path: SyntheticGrepScan()

`, contentPath)
	script := strings.Replace(filesystemRunnerScript, fallbackGrep, instrumentation+fallbackGrep, 1)
	if script == filesystemRunnerScript {
		t.Fatal("failed to inject synthetic grep enumeration")
	}

	stdout, stderr, exitCode := runFilesystemRunnerScript(t, script, "grep", filesystemGrepCommand{
		Pattern:    "needle",
		Paths:      []string{"."},
		OutputMode: GrepOutputModeContent,
		HeadLimit:  1,
		Cwd:        root,
	})
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr)
	}
	got := decodeRunnerResult[GrepResult](t, stdout)
	if got.Output != "file-000000.txt:1:needle\n" || !got.Truncated {
		t.Fatalf("GrepResult = %+v, want deterministic first match and truncation", got)
	}
}

func TestFilesystemRunnerGlobMatchesAcrossMultipleRootsAndReturnsNoMatches(t *testing.T) {
	root := t.TempDir()
	for path := range map[string]struct{}{
		"src/main.go":          {},
		"src/sub/component.ts": {},
		"pkg/lib.go":           {},
		"pkg/ignore.txt":       {},
		".git/config":          {},
	} {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll(%q) error = %v", full, err)
		}
		if err := os.WriteFile(full, []byte("x\n"), 0o644); err != nil {
			t.Fatalf("WriteFile(%q) error = %v", full, err)
		}
	}

	stdout, stderr, exitCode := runFilesystemRunner(t, "glob", GlobRequest{
		Pattern: "**/*.{go,ts}",
		Paths:   []string{"src", "pkg"},
		Cwd:     root,
	})
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr)
	}
	got := decodeRunnerResult[GlobResult](t, stdout)
	want := "pkg/lib.go\nsrc/main.go\nsrc/sub/component.ts\n"
	if got.Output != want {
		t.Fatalf("Output = %q, want %q", got.Output, want)
	}

	stdout, stderr, exitCode = runFilesystemRunner(t, "glob", GlobRequest{
		Pattern: "**/*.py",
		Paths:   []string{"src", "pkg"},
		Cwd:     root,
	})
	if exitCode != 0 {
		t.Fatalf("no-match exitCode = %d, stderr = %q", exitCode, stderr)
	}
	noMatches := decodeRunnerResult[GlobResult](t, stdout)
	if noMatches.Output != "" {
		t.Fatalf("Output = %q, want empty", noMatches.Output)
	}
}

func TestFilesystemRunnerGlobIncludesHiddenFilesExceptGit(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{
		"README.md",
		"docs/guide.md",
		".github/copilot.md",
		".github/workflows/check.md",
		".hidden/note.md",
		".git/ignored.md",
	} {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll(%q) error = %v", full, err)
		}
		if err := os.WriteFile(full, []byte("needle\n"), 0o644); err != nil {
			t.Fatalf("WriteFile(%q) error = %v", full, err)
		}
	}

	tests := []struct {
		name    string
		pattern string
		want    string
	}{
		{
			name:    "recursive wildcard includes hidden paths",
			pattern: "**/*.md",
			want:    ".github/copilot.md\n.github/workflows/check.md\n.hidden/note.md\nREADME.md\ndocs/guide.md\n",
		},
		{
			name:    "explicit hidden pattern",
			pattern: ".github/**/*.md",
			want:    ".github/copilot.md\n.github/workflows/check.md\n",
		},
		{
			name:    "git remains excluded",
			pattern: ".git/**/*.md",
			want:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdout, stderr, exitCode := runFilesystemRunner(t, "glob", GlobRequest{
				Pattern: tt.pattern,
				Paths:   []string{"."},
				Cwd:     root,
			})
			if exitCode != 0 {
				t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr)
			}
			got := decodeRunnerResult[GlobResult](t, stdout)
			if got.Output != tt.want {
				t.Fatalf("Output = %q, want %q", got.Output, tt.want)
			}
		})
	}
}

func TestFilesystemRunnerGlobLimitUsesDeterministicHiddenOrdering(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{
		"visible.txt",
		".hidden/first.txt",
		".git/ignored.txt",
	} {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll(%q) error = %v", full, err)
		}
		if err := os.WriteFile(full, []byte("x\n"), 0o644); err != nil {
			t.Fatalf("WriteFile(%q) error = %v", full, err)
		}
	}

	stdout, stderr, exitCode := runFilesystemRunner(t, "glob", GlobRequest{
		Pattern: "**/*.txt",
		Paths:   []string{"."},
		Cwd:     root,
		Limit:   1,
	})
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr)
	}
	got := decodeRunnerResult[GlobResult](t, stdout)
	if got.Output != ".hidden/first.txt\n" || got.Limit != 1 || !got.Truncated {
		t.Fatalf("GlobResult = %+v, want first hidden path with limit 1 and truncation", got)
	}

	stdout, stderr, exitCode = runFilesystemRunner(t, "glob", GlobRequest{
		Pattern: ".hidden/**/*.txt",
		Paths:   []string{"."},
		Cwd:     root,
		Limit:   1,
	})
	if exitCode != 0 {
		t.Fatalf("exact-limit exitCode = %d, stderr = %q", exitCode, stderr)
	}
	got = decodeRunnerResult[GlobResult](t, stdout)
	if got.Output != ".hidden/first.txt\n" || got.Limit != 1 || got.Truncated {
		t.Fatalf("exact-limit GlobResult = %+v, want one complete result", got)
	}
}

func TestFilesystemRunnerGlobNormalizesDefaultAndMaximumLimits(t *testing.T) {
	tests := []struct {
		name          string
		requested     int
		wantLimit     int
		producerCount int
	}{
		{
			name:          "default",
			wantLimit:     DefaultGlobLimit,
			producerCount: DefaultGlobLimit + 1,
		},
		{
			name:          "maximum",
			requested:     MaxGlobLimit + 1,
			wantLimit:     MaxGlobLimit,
			producerCount: MaxGlobLimit + 1,
		},
	}

	const handleGlob = "def handle_glob(req):"
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			producer := `def walk_glob_files(paths, cwd, directory_batch_limit):
    for idx in range(` + strconv.Itoa(tt.producerCount) + `):
        rendered = "file-%05d.go" % idx
        yield {"resolved": rendered, "display": rendered, "rel": rendered}

`
			script := strings.Replace(filesystemRunnerScript, handleGlob, producer+handleGlob, 1)
			if script == filesystemRunnerScript {
				t.Fatal("failed to inject glob producer")
			}

			stdout, stderr, exitCode := runFilesystemRunnerScript(t, script, "glob", GlobRequest{
				Pattern: "**/*.go",
				Limit:   tt.requested,
			})
			if exitCode != 0 {
				t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr)
			}
			got := decodeRunnerResult[GlobResult](t, stdout)
			lines := strings.Split(strings.TrimSuffix(got.Output, "\n"), "\n")
			if got.Limit != tt.wantLimit || !got.Truncated || len(lines) != tt.wantLimit {
				t.Fatalf("GlobResult limit=%d truncated=%v lines=%d, want limit=%d truncated=true", got.Limit, got.Truncated, len(lines), tt.wantLimit)
			}
			if lines[0] != "file-00000.go" || lines[len(lines)-1] != fmt.Sprintf("file-%05d.go", tt.wantLimit-1) {
				t.Fatalf("output bounds = %q ... %q, want deterministic order", lines[0], lines[len(lines)-1])
			}
		})
	}
}

func TestFilesystemRunnerGlobLimitStopsProducerEarly(t *testing.T) {
	const handleGlob = "def handle_glob(req):"
	producer := `def walk_glob_files(paths, cwd, directory_batch_limit):
    for rendered in ("a.go", "b.go"):
        yield {"resolved": rendered, "display": rendered, "rel": rendered}
    raise OSError("glob producer was consumed past limit plus truncation probe")

`
	script := strings.Replace(filesystemRunnerScript, handleGlob, producer+handleGlob, 1)
	if script == filesystemRunnerScript {
		t.Fatal("failed to inject bounded glob producer")
	}

	stdout, stderr, exitCode := runFilesystemRunnerScript(t, script, "glob", GlobRequest{
		Pattern: "**/*.go",
		Limit:   1,
	})
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr)
	}
	got := decodeRunnerResult[GlobResult](t, stdout)
	if got.Output != "a.go\n" || got.Limit != 1 || !got.Truncated {
		t.Fatalf("GlobResult = %+v, want one result and accurate truncation", got)
	}
}

func TestFilesystemRunnerGlobBoundsSyntheticDirectoryWork(t *testing.T) {
	root := t.TempDir()
	const handleGlob = "def handle_glob(req):"
	instrumentation := fmt.Sprintf(`synthetic_glob_nsmallest = heapq.nsmallest
synthetic_glob_selection_calls = 0

def bounded_glob_nsmallest(limit, iterable, key=None):
    global synthetic_glob_selection_calls
    synthetic_glob_selection_calls += 1
    if synthetic_glob_selection_calls > 1:
        raise OSError("glob re-enumerated after its truncation probe")
    if limit > 2:
        raise OSError("glob selection exceeded its limit probe")
    return synthetic_glob_nsmallest(limit, iterable, key=key)

heapq.nsmallest = bounded_glob_nsmallest

class SyntheticGlobEntry:
    def __init__(self, index):
        self.name = "file-%%06d.go" %% index
        self.path = os.path.join(%q, self.name)

    def is_dir(self, follow_symlinks=False):
        global synthetic_glob_metadata_calls
        synthetic_glob_metadata_calls += 1
        if synthetic_glob_metadata_calls > 100001:
            raise OSError("glob re-enumerated synthetic entry metadata")
        return False

class SyntheticGlobScan:
    def __enter__(self):
        return self

    def __exit__(self, exc_type, exc_value, traceback):
        if synthetic_glob_selection_calls != 1:
            raise OSError("glob did not use bounded selection")
        return False

    def __iter__(self):
        for index in range(100000, -1, -1):
            yield SyntheticGlobEntry(index)

synthetic_glob_metadata_calls = 0
os.scandir = lambda path: SyntheticGlobScan()

`, root)
	script := strings.Replace(filesystemRunnerScript, handleGlob, instrumentation+handleGlob, 1)
	if script == filesystemRunnerScript {
		t.Fatal("failed to inject synthetic glob enumeration")
	}

	stdout, stderr, exitCode := runFilesystemRunnerScript(t, script, "glob", GlobRequest{
		Pattern: "**/*.go",
		Paths:   []string{"."},
		Cwd:     root,
		Limit:   1,
	})
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr)
	}
	got := decodeRunnerResult[GlobResult](t, stdout)
	if got.Output != "file-000000.go\n" || got.Limit != 1 || !got.Truncated {
		t.Fatalf("GlobResult = %+v, want deterministic first match and truncation", got)
	}
}

func TestFilesystemRunnerGrepStillExcludesHiddenFiles(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{"visible.md", ".github/hidden.md"} {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll(%q) error = %v", full, err)
		}
		if err := os.WriteFile(full, []byte("needle\n"), 0o644); err != nil {
			t.Fatalf("WriteFile(%q) error = %v", full, err)
		}
	}

	stdout, stderr, exitCode := runFilesystemRunner(t, "grep", filesystemGrepCommand{
		Pattern:    "needle",
		Paths:      []string{"."},
		Glob:       "**/*.md",
		OutputMode: GrepOutputModeFilesWithMatches,
		Cwd:        root,
	})
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr)
	}
	got := decodeRunnerResult[struct {
		Output string `json:"output"`
	}](t, stdout)
	if got.Output != "visible.md\n" {
		t.Fatalf("Output = %q, want visible file only", got.Output)
	}
}

func TestFilesystemRunnerApplyPatchRejectsMalformedInputWithoutMutatingFiles(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "sample.txt")
	if err := os.WriteFile(path, []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	_, stderr, exitCode := runFilesystemRunner(t, "apply_patch", ApplyPatchRequest{
		Cwd: root,
		Patch: strings.Join([]string{
			"*** Begin Patch",
			"*** Update File: sample.txt",
			"@@",
			"invalid line",
			"*** End Patch",
			"",
		}, "\n"),
	})
	if exitCode == 0 {
		t.Fatal("exitCode = 0, want malformed patch failure")
	}
	if !strings.Contains(stderr, "malformed patch") {
		t.Fatalf("stderr = %q, want malformed patch error", stderr)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(content) != "one\ntwo\n" {
		t.Fatalf("file content = %q, want original", string(content))
	}
}

func TestFilesystemRunnerApplyPatchRejectsEmptyAddBodies(t *testing.T) {
	root := t.TempDir()

	_, stderr, exitCode := runFilesystemRunner(t, "apply_patch", ApplyPatchRequest{
		Cwd: root,
		Patch: strings.Join([]string{
			"*** Begin Patch",
			"*** Add File: empty.txt",
			"*** End Patch",
			"",
		}, "\n"),
	})
	if exitCode == 0 {
		t.Fatal("exitCode = 0, want malformed patch failure")
	}
	if !strings.Contains(stderr, "malformed patch") {
		t.Fatalf("stderr = %q, want malformed patch error", stderr)
	}
	if _, err := os.Stat(filepath.Join(root, "empty.txt")); err == nil {
		t.Fatal("empty.txt exists, want add failure")
	}
}

func TestFilesystemRunnerApplyPatchRejectsUpdateWithoutHunks(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "sample.txt")
	if err := os.WriteFile(path, []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	_, stderr, exitCode := runFilesystemRunner(t, "apply_patch", ApplyPatchRequest{
		Cwd: root,
		Patch: strings.Join([]string{
			"*** Begin Patch",
			"*** Update File: sample.txt",
			"*** End Patch",
			"",
		}, "\n"),
	})
	if exitCode == 0 {
		t.Fatal("exitCode = 0, want malformed patch failure")
	}
	if !strings.Contains(stderr, "malformed patch") {
		t.Fatalf("stderr = %q, want malformed patch error", stderr)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(content) != "one\ntwo\n" {
		t.Fatalf("file content = %q, want original", string(content))
	}
}

func TestFilesystemRunnerApplyPatchFailureDoesNotPartiallyMutateFiles(t *testing.T) {
	root := t.TempDir()
	aPath := filepath.Join(root, "a.txt")
	bPath := filepath.Join(root, "b.txt")
	if err := os.WriteFile(aPath, []byte("alpha\nbeta\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(a) error = %v", err)
	}
	if err := os.WriteFile(bPath, []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(b) error = %v", err)
	}

	_, stderr, exitCode := runFilesystemRunner(t, "apply_patch", ApplyPatchRequest{
		Cwd: root,
		Patch: strings.Join([]string{
			"*** Begin Patch",
			"*** Update File: a.txt",
			"@@",
			"-alpha",
			"+ALPHA",
			"*** Update File: b.txt",
			"@@",
			"-three",
			"+THREE",
			"*** End Patch",
			"",
		}, "\n"),
	})
	if exitCode == 0 {
		t.Fatal("exitCode = 0, want patch failure")
	}
	if !strings.Contains(stderr, "could not apply patch") {
		t.Fatalf("stderr = %q, want patch failure", stderr)
	}

	aContent, err := os.ReadFile(aPath)
	if err != nil {
		t.Fatalf("ReadFile(a) error = %v", err)
	}
	if string(aContent) != "alpha\nbeta\n" {
		t.Fatalf("a.txt = %q, want original", string(aContent))
	}

	bContent, err := os.ReadFile(bPath)
	if err != nil {
		t.Fatalf("ReadFile(b) error = %v", err)
	}
	if string(bContent) != "one\ntwo\n" {
		t.Fatalf("b.txt = %q, want original", string(bContent))
	}
}

func TestFilesystemRunnerApplyPatchUpdatesAndAddsFiles(t *testing.T) {
	root := t.TempDir()
	samplePath := filepath.Join(root, "sample.txt")
	if err := os.WriteFile(samplePath, []byte("alpha\nbeta\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	stdout, stderr, exitCode := runFilesystemRunner(t, "apply_patch", ApplyPatchRequest{
		Cwd: root,
		Patch: strings.Join([]string{
			"*** Begin Patch",
			"*** Update File: sample.txt",
			"@@",
			"-beta",
			"+BETA",
			"*** Add File: nested/new.txt",
			"+hello",
			"+world",
			"*** End Patch",
			"",
		}, "\n"),
	})
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr)
	}

	got := decodeRunnerResult[ApplyPatchResult](t, stdout)
	if got.FilesChanged != 2 || !strings.Contains(got.Output, "Updated sample.txt") || !strings.Contains(got.Output, "Added nested/new.txt") {
		t.Fatalf("ApplyPatchResult = %+v", got)
	}

	sampleContent, err := os.ReadFile(samplePath)
	if err != nil {
		t.Fatalf("ReadFile(sample) error = %v", err)
	}
	if string(sampleContent) != "alpha\nBETA\n" {
		t.Fatalf("sample.txt = %q, want updated content", string(sampleContent))
	}

	newContent, err := os.ReadFile(filepath.Join(root, "nested", "new.txt"))
	if err != nil {
		t.Fatalf("ReadFile(new) error = %v", err)
	}
	if string(newContent) != "hello\nworld\n" {
		t.Fatalf("new.txt = %q, want added content", string(newContent))
	}
	info, err := os.Stat(filepath.Join(root, "nested", "new.txt"))
	if err != nil {
		t.Fatalf("Stat(new) error = %v", err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Fatalf("new.txt mode = %v, want 0644", got)
	}
	tmpLinks, err := filepath.Glob(filepath.Join(root, ".sample.txt.*.tmp"))
	if err != nil {
		t.Fatalf("Glob(tmp links) error = %v", err)
	}
	if len(tmpLinks) != 0 {
		t.Fatalf("obsolete patch temp hardlinks remain after success: %v", tmpLinks)
	}
}

func TestFilesystemRunnerApplyPatchMoveOnlyPreservesBinaryIdentityAndMetadata(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "blob.bin")
	hardlinkPath := filepath.Join(root, "blob-hardlink.bin")
	targetPath := filepath.Join(root, "nested", "moved.bin")
	content := []byte{0x00, 0x01, 0xfe, 0xff, 0x7f}
	if err := os.WriteFile(sourcePath, content, 0o751); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	modTime := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(sourcePath, modTime, modTime); err != nil {
		t.Fatalf("Chtimes() error = %v", err)
	}
	if err := os.Link(sourcePath, hardlinkPath); err != nil {
		t.Fatalf("Link() error = %v", err)
	}
	before, err := os.Stat(sourcePath)
	if err != nil {
		t.Fatalf("Stat(source) error = %v", err)
	}

	xattrName := "user.copilot-move-test"
	xattrValue := "preserved"
	xattrSupported := exec.Command("python3", "-c", "import os,sys; os.setxattr(sys.argv[1], sys.argv[2].encode(), sys.argv[3].encode())", sourcePath, xattrName, xattrValue).Run() == nil

	stdout, stderr, exitCode := runFilesystemRunner(t, "apply_patch", ApplyPatchRequest{
		Cwd: root,
		Patch: strings.Join([]string{
			"*** Begin Patch",
			"*** Update File: blob.bin",
			"*** Move to: nested/moved.bin",
			"*** End Patch",
			"",
		}, "\n"),
	})
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr)
	}
	got := decodeRunnerResult[ApplyPatchResult](t, stdout)
	if got.FilesChanged != 1 || got.Output != "Updated blob.bin -> nested/moved.bin" {
		t.Fatalf("ApplyPatchResult = %+v", got)
	}
	if _, err := os.Lstat(sourcePath); !os.IsNotExist(err) {
		t.Fatalf("source Lstat error = %v, want not exist", err)
	}
	movedContent, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("ReadFile(target) error = %v", err)
	}
	if !reflect.DeepEqual(movedContent, content) {
		t.Fatalf("target content = %v, want %v", movedContent, content)
	}
	after, err := os.Stat(targetPath)
	if err != nil {
		t.Fatalf("Stat(target) error = %v", err)
	}
	hardlink, err := os.Stat(hardlinkPath)
	if err != nil {
		t.Fatalf("Stat(hardlink) error = %v", err)
	}
	if !os.SameFile(before, after) || !os.SameFile(after, hardlink) {
		t.Fatal("move did not preserve file identity and hardlinks")
	}
	if after.Mode().Perm() != before.Mode().Perm() || !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("metadata changed: before=%v %v after=%v %v", before.Mode().Perm(), before.ModTime(), after.Mode().Perm(), after.ModTime())
	}
	if xattrSupported {
		output, err := exec.Command("python3", "-c", "import os,sys; sys.stdout.buffer.write(os.getxattr(sys.argv[1], sys.argv[2].encode()))", targetPath, xattrName).Output()
		if err != nil {
			t.Fatalf("getxattr() error = %v", err)
		}
		if string(output) != xattrValue {
			t.Fatalf("xattr = %q, want %q", output, xattrValue)
		}
	}
}

func TestFilesystemRunnerApplyPatchMoveOnlyRejectsDestinationAndTopologyConflicts(t *testing.T) {
	tests := []struct {
		name       string
		targetPath string
		setup      func(t *testing.T, root string)
		wantError  string
	}{
		{
			name:       "destination exists",
			targetPath: "target.bin",
			setup: func(t *testing.T, root string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(root, "target.bin"), []byte("target"), 0o644); err != nil {
					t.Fatalf("WriteFile(target) error = %v", err)
				}
			},
			wantError: "already exists",
		},
		{
			name:       "parent is a file",
			targetPath: "parent/target.bin",
			setup: func(t *testing.T, root string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(root, "parent"), []byte("parent"), 0o644); err != nil {
					t.Fatalf("WriteFile(parent) error = %v", err)
				}
			},
			wantError: "parent path",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			sourcePath := filepath.Join(root, "source.bin")
			sourceContent := []byte{0x00, 0x01, 0x02}
			if err := os.WriteFile(sourcePath, sourceContent, 0o640); err != nil {
				t.Fatalf("WriteFile(source) error = %v", err)
			}
			before, err := os.Stat(sourcePath)
			if err != nil {
				t.Fatalf("Stat(source) error = %v", err)
			}
			tt.setup(t, root)

			_, stderr, exitCode := runFilesystemRunner(t, "apply_patch", ApplyPatchRequest{
				Cwd: root,
				Patch: strings.Join([]string{
					"*** Begin Patch",
					"*** Update File: source.bin",
					"*** Move to: " + tt.targetPath,
					"*** End Patch",
					"",
				}, "\n"),
			})
			if exitCode == 0 {
				t.Fatal("exitCode = 0, want conflict failure")
			}
			if !strings.Contains(stderr, tt.wantError) {
				t.Fatalf("stderr = %q, want %q", stderr, tt.wantError)
			}
			content, err := os.ReadFile(sourcePath)
			if err != nil {
				t.Fatalf("ReadFile(source) error = %v", err)
			}
			after, err := os.Stat(sourcePath)
			if err != nil {
				t.Fatalf("Stat(source) error = %v", err)
			}
			if !reflect.DeepEqual(content, sourceContent) || !os.SameFile(before, after) {
				t.Fatalf("source changed after conflict: content=%v sameFile=%v", content, os.SameFile(before, after))
			}
		})
	}
}

func TestFilesystemRunnerApplyPatchMoveOnlyRejectsSymlinkSource(t *testing.T) {
	root := t.TempDir()
	targetPath := filepath.Join(root, "target.bin")
	sourcePath := filepath.Join(root, "source.bin")
	if err := os.WriteFile(targetPath, []byte{0x00, 0x01}, 0o644); err != nil {
		t.Fatalf("WriteFile(target) error = %v", err)
	}
	if err := os.Symlink(targetPath, sourcePath); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}

	_, stderr, exitCode := runFilesystemRunner(t, "apply_patch", ApplyPatchRequest{
		Cwd: root,
		Patch: strings.Join([]string{
			"*** Begin Patch",
			"*** Update File: source.bin",
			"*** Move to: moved.bin",
			"*** End Patch",
			"",
		}, "\n"),
	})
	if exitCode == 0 {
		t.Fatal("exitCode = 0, want symlink rejection")
	}
	if !strings.Contains(stderr, "symbolic link") {
		t.Fatalf("stderr = %q, want symbolic link error", stderr)
	}
	if _, err := os.Lstat(sourcePath); err != nil {
		t.Fatalf("Lstat(source) error = %v", err)
	}
}

func TestFilesystemRunnerApplyPatchMoveOnlyRejectsConflictingPatchTopology(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "source.txt")
	if err := os.WriteFile(sourcePath, []byte("source\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(source) error = %v", err)
	}

	_, stderr, exitCode := runFilesystemRunner(t, "apply_patch", ApplyPatchRequest{
		Cwd: root,
		Patch: strings.Join([]string{
			"*** Begin Patch",
			"*** Update File: source.txt",
			"*** Move to: nested/moved.txt",
			"*** Add File: nested/moved.txt/child.txt",
			"+child",
			"*** End Patch",
			"",
		}, "\n"),
	})
	if exitCode == 0 {
		t.Fatal("exitCode = 0, want topology conflict")
	}
	if !strings.Contains(stderr, "conflicts with") {
		t.Fatalf("stderr = %q, want topology conflict", stderr)
	}
	content, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatalf("ReadFile(source) error = %v", err)
	}
	if string(content) != "source\n" {
		t.Fatalf("source = %q, want unchanged", content)
	}
	if _, err := os.Stat(filepath.Join(root, "nested")); !os.IsNotExist(err) {
		t.Fatalf("nested Stat error = %v, want not exist", err)
	}
}

func TestFilesystemRunnerApplyPatchMoveOnlyLateDestinationConflictDoesNotOverwrite(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "source.txt")
	targetPath := filepath.Join(root, "target.txt")
	if err := os.WriteFile(sourcePath, []byte("source\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(source) error = %v", err)
	}

	const commitLoop = `        for entry in plan:
            if entry["kind"] == "delete":`
	injectedConflict := `        for entry in plan:
            if entry["kind"] in ("move", "write") and entry.get("target") != entry["path"]:
                with open(entry["target"], "xb") as handle:
                    handle.write(b"racer\n")
            if entry["kind"] == "delete":`
	script := strings.Replace(filesystemRunnerScript, commitLoop, injectedConflict, 1)
	if script == filesystemRunnerScript {
		t.Fatal("failed to inject late destination conflict")
	}

	_, stderr, exitCode := runFilesystemRunnerScript(t, script, "apply_patch", ApplyPatchRequest{
		Cwd: root,
		Patch: strings.Join([]string{
			"*** Begin Patch",
			"*** Update File: source.txt",
			"*** Move to: target.txt",
			"*** End Patch",
			"",
		}, "\n"),
	})
	if exitCode == 0 {
		t.Fatal("exitCode = 0, want destination conflict")
	}
	if !strings.Contains(stderr, "already exists") {
		t.Fatalf("stderr = %q, want destination conflict", stderr)
	}
	sourceContent, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatalf("ReadFile(source) error = %v", err)
	}
	if string(sourceContent) != "source\n" {
		t.Fatalf("source = %q, want unchanged", sourceContent)
	}
	targetContent, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("ReadFile(target) error = %v", err)
	}
	if string(targetContent) != "racer\n" {
		t.Fatalf("target = %q, want racer content", targetContent)
	}
}

func TestClientFilesystemFallbackUsesVerifiedHelperWithoutDaemon(t *testing.T) {
	client := NewClient("demo")
	if err := client.SelectFilesystemHelper("/remote/gh-copilot-codespace", helperinfo.Current()); err != nil {
		t.Fatalf("SelectFilesystemHelper() error = %v", err)
	}
	stdinPath := filepath.Join(t.TempDir(), "view.json")

	var calls []fakeExecCall
	client.commandContext = fakeCommandContext(t, &calls, []fakeExecResponse{
		{stdout: `{"kind":"file","content":"2. beta\n","truncated":false}` + "\n", stdinPath: stdinPath},
	})

	got, err := client.View(context.Background(), ViewRequest{
		Path:      "/workspaces/repo/sample.txt",
		ViewRange: []int{2, 2},
	})
	if err != nil {
		t.Fatalf("View() error = %v", err)
	}
	if got.Kind != ViewKindFile || got.Content != "2. beta\n" {
		t.Fatalf("View() = %+v", got)
	}

	var payload ViewRequest
	readJSONFile(t, stdinPath, &payload)
	if payload.Path != "/workspaces/repo/sample.txt" || !reflect.DeepEqual(payload.ViewRange, []int{2, 2}) {
		t.Fatalf("payload = %+v", payload)
	}
	if len(calls) != 1 {
		t.Fatalf("unexpected calls: %#v", calls)
	}
	command := strings.Join(calls[0].args, " ")
	if calls[0].name != "gh" || !strings.Contains(command, "/remote/gh-copilot-codespace") || !strings.Contains(command, "filesystem view") {
		t.Fatalf("unexpected calls: %#v", calls)
	}
	if strings.Contains(command, "python") {
		t.Fatalf("filesystem command unexpectedly depends on Python: %q", command)
	}
}

func TestClientFilesystemFallbackHasNoLegacyDefault(t *testing.T) {
	client := NewClient("demo")

	_, err := client.View(context.Background(), ViewRequest{Path: "/workspaces/repo/sample.txt"})
	if err == nil || !strings.Contains(err.Error(), "compatible deployed filesystem helper is unavailable") {
		t.Fatalf("View() error = %v, want unavailable compatible helper", err)
	}
}

func TestClientCreateFileTransfersContentOverStdin(t *testing.T) {
	client := newVerifiedFilesystemClient(t)
	stdinPath := filepath.Join(t.TempDir(), "create.json")

	var calls []fakeExecCall
	client.commandContext = fakeCommandContext(t, &calls, []fakeExecResponse{
		{stdinPath: stdinPath},
	})

	if err := client.CreateFile(context.Background(), "/workspaces/repo/new.txt", "hello\nworld\n"); err != nil {
		t.Fatalf("CreateFile() error = %v", err)
	}

	var payload map[string]string
	readJSONFile(t, stdinPath, &payload)
	if payload["path"] != "/workspaces/repo/new.txt" || payload["content"] != "hello\nworld\n" {
		t.Fatalf("payload = %#v", payload)
	}
	if len(calls) != 1 || !strings.Contains(strings.Join(calls[0].args, " "), "filesystem create") {
		t.Fatalf("unexpected calls: %#v", calls)
	}
	if strings.Contains(strings.Join(calls[0].args, " "), "hello\nworld\n") {
		t.Fatalf("command unexpectedly inlined file content: %#v", calls)
	}
}

func TestFilesystemRunnerCreateFileStillRefusesOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "existing.txt")
	if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	_, stderr, exitCode := runFilesystemRunner(t, "create", map[string]any{
		"path":    path,
		"content": "replace",
	})
	if exitCode == 0 || !strings.Contains(stderr, "already exists") {
		t.Fatalf("exitCode = %d, stderr = %q, want overwrite refusal", exitCode, stderr)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(content) != "keep" {
		t.Fatalf("content = %q, want unchanged", content)
	}
}

func TestFilesystemRunnerWriteFileBinaryOverwriteAndPermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "blob.bin")
	first := []byte{0x00, 0xff, 'a'}
	_, stderr, exitCode := runFilesystemRunner(t, "write", map[string]any{
		"path":      path,
		"data":      base64.StdEncoding.EncodeToString(first),
		"overwrite": false,
		"root":      dir,
	})
	if exitCode != 0 {
		t.Fatalf("write new exitCode = %d, stderr = %q", exitCode, stderr)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(new) error = %v", err)
	}
	if !reflect.DeepEqual(got, first) {
		t.Fatalf("new bytes = %v, want %v", got, first)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(new) error = %v", err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Fatalf("new mode = %v, want 0644", got)
	}

	second := []byte{'n', 0x00, 0xfe, 'w'}
	_, stderr, exitCode = runFilesystemRunner(t, "write", map[string]any{
		"path":      path,
		"data":      base64.StdEncoding.EncodeToString(second),
		"overwrite": false,
		"root":      dir,
	})
	if exitCode == 0 || !strings.Contains(stderr, "already exists") {
		t.Fatalf("write refusal exitCode = %d, stderr = %q", exitCode, stderr)
	}
	got, err = os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(refused) error = %v", err)
	}
	if !reflect.DeepEqual(got, first) {
		t.Fatalf("refused bytes = %v, want unchanged %v", got, first)
	}

	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}
	_, stderr, exitCode = runFilesystemRunner(t, "write", map[string]any{
		"path":      path,
		"data":      base64.StdEncoding.EncodeToString(second),
		"overwrite": true,
		"root":      dir,
	})
	if exitCode != 0 {
		t.Fatalf("write overwrite exitCode = %d, stderr = %q", exitCode, stderr)
	}
	got, err = os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(overwrite) error = %v", err)
	}
	if !reflect.DeepEqual(got, second) {
		t.Fatalf("overwrite bytes = %v, want %v", got, second)
	}
	info, err = os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(overwrite) error = %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("overwrite mode = %v, want 0600", got)
	}
}

func TestFilesystemRunnerRootedOverwriteTransaction(t *testing.T) {
	beforeCaptureHook := normalizePythonSnippet(`def rooted_write_before_capture(parent_fd, name, path):
	    pass`)
	beforeInstallHook := normalizePythonSnippet(`def rooted_write_before_install(parent_fd, name, path):
	    pass`)
	afterInstallHook := normalizePythonSnippet(`def rooted_write_after_install(parent_fd, name, path):
	    pass`)
	injectHook := func(t *testing.T, hook, replacement string) string {
		t.Helper()
		script := strings.Replace(filesystemRunnerScript, hook, normalizePythonSnippet(replacement), 1)
		if script == filesystemRunnerScript {
			t.Fatal("failed to inject rooted write transaction hook")
		}
		return script
	}
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
	runWrite := func(t *testing.T, script, root string) (string, int) {
		t.Helper()
		_, stderr, exitCode := runFilesystemRunnerScript(t, script, "write", map[string]any{
			"path":      "destination.bin",
			"root":      root,
			"data":      base64.StdEncoding.EncodeToString([]byte("staged")),
			"overwrite": true,
		})
		return stderr, exitCode
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
		script := injectHook(t, beforeCaptureHook, `def rooted_write_before_capture(parent_fd, name, path):
	    fd = os.open(name, os.O_WRONLY | os.O_TRUNC, dir_fd=parent_fd)
	    try:
	        os.write(fd, b"modified")
	    finally:
	        os.close(fd)`)

		stderr, exitCode := runWrite(t, script, root)
		if exitCode == 0 || !strings.Contains(stderr, "changed during write") {
			t.Fatalf("exitCode = %d, stderr = %q, want in-place change rejection", exitCode, stderr)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(destination) error = %v", err)
		}
		if got := string(content); got != "modified" {
			t.Fatalf("destination content = %q, want modified", got)
		}
		after, err := os.Stat(path)
		if err != nil {
			t.Fatalf("Stat(destination after change) error = %v", err)
		}
		if !os.SameFile(before, after) {
			t.Fatal("destination inode changed, want deterministic in-place mutation")
		}
		assertNoCopyArtifacts(t, root)
	})

	t.Run("rejects destination appearing before install", func(t *testing.T) {
		root := t.TempDir()
		script := injectHook(t, beforeInstallHook, `def rooted_write_before_install(parent_fd, name, path):
	    fd = os.open(name, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600, dir_fd=parent_fd)
	    try:
	        os.write(fd, b"concurrent")
	    finally:
	        os.close(fd)`)

		stderr, exitCode := runWrite(t, script, root)
		if exitCode == 0 || !strings.Contains(stderr, "already exists") {
			t.Fatalf("exitCode = %d, stderr = %q, want destination conflict", exitCode, stderr)
		}
		content, err := os.ReadFile(filepath.Join(root, "destination.bin"))
		if err != nil {
			t.Fatalf("ReadFile(destination) error = %v", err)
		}
		if got := string(content); got != "concurrent" {
			t.Fatalf("destination content = %q, want concurrent", got)
		}
		assertNoCopyArtifacts(t, root)
	})

	t.Run("rejects replacement between staging and capture", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "destination.bin")
		if err := os.WriteFile(path, []byte("original"), 0o640); err != nil {
			t.Fatalf("WriteFile(destination) error = %v", err)
		}
		script := injectHook(t, beforeCaptureHook, `def rooted_write_before_capture(parent_fd, name, path):
	    replacement = name + ".concurrent"
	    fd = os.open(replacement, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600, dir_fd=parent_fd)
	    try:
	        os.write(fd, b"concurrent")
	    finally:
	        os.close(fd)
	    os.replace(replacement, name, src_dir_fd=parent_fd, dst_dir_fd=parent_fd)`)

		stderr, exitCode := runWrite(t, script, root)
		if exitCode == 0 || !strings.Contains(stderr, "changed during write") {
			t.Fatalf("exitCode = %d, stderr = %q, want replacement rejection", exitCode, stderr)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(destination) error = %v", err)
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
		script := injectHook(t, beforeInstallHook, `def rooted_write_before_install(parent_fd, name, path):
	    replacement = name + ".concurrent"
	    fd = os.open(replacement, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600, dir_fd=parent_fd)
	    try:
	        os.write(fd, b"concurrent")
	    finally:
	        os.close(fd)
	    os.replace(replacement, name, src_dir_fd=parent_fd, dst_dir_fd=parent_fd)`)

		stderr, exitCode := runWrite(t, script, root)
		if exitCode == 0 || !strings.Contains(stderr, "recovery preserved at") {
			t.Fatalf("exitCode = %d, stderr = %q, want preserved recovery error", exitCode, stderr)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(destination) error = %v", err)
		}
		if got := string(content); got != "concurrent" {
			t.Fatalf("destination content = %q, want concurrent", got)
		}
		recoveries, err := filepath.Glob(filepath.Join(root, ".copilot-*.recover"))
		if err != nil {
			t.Fatalf("Glob(recovery) error = %v", err)
		}
		if len(recoveries) != 1 {
			t.Fatalf("recovery files = %v, want one", recoveries)
		}
		recovery, err := os.ReadFile(recoveries[0])
		if err != nil {
			t.Fatalf("ReadFile(recovery) error = %v", err)
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
		script := injectHook(t, beforeInstallHook, `def rooted_write_before_install(parent_fd, name, path):
	    raise PatchCanceled("write canceled")`)

		stderr, exitCode := runWrite(t, script, root)
		if exitCode == 0 || !strings.Contains(stderr, "write canceled") {
			t.Fatalf("exitCode = %d, stderr = %q, want cancellation", exitCode, stderr)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(destination) error = %v", err)
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

		script := injectHook(t, beforeInstallHook, `def rooted_write_before_install(parent_fd, name, path):
	    for _ in range(100):
	        os.stat(name, dir_fd=parent_fd, follow_symlinks=False)`)
		beforeAfterInstallInjection := script
		script = strings.Replace(script, afterInstallHook, normalizePythonSnippet(`def rooted_write_after_install(parent_fd, name, path):
	    for _ in range(100):
	        os.stat(name, dir_fd=parent_fd, follow_symlinks=False)`), 1)
		if script == beforeAfterInstallInjection {
			t.Fatal("failed to inject rooted post-install observer")
		}
		stderr, exitCode := runWrite(t, script, root)
		if exitCode != 0 {
			t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr)
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
		script := injectHook(t, afterInstallHook, `def rooted_write_after_install(parent_fd, name, path):
	    replacement = name + ".concurrent"
	    fd = os.open(replacement, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600, dir_fd=parent_fd)
	    try:
	        os.write(fd, b"concurrent")
	    finally:
	        os.close(fd)
	    os.replace(replacement, name, src_dir_fd=parent_fd, dst_dir_fd=parent_fd)`)

		stderr, exitCode := runWrite(t, script, root)
		if exitCode == 0 || !strings.Contains(stderr, "recovery preserved at") {
			t.Fatalf("exitCode = %d, stderr = %q, want atomic replacement conflict", exitCode, stderr)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(destination) error = %v", err)
		}
		if got := string(content); got != "concurrent" {
			t.Fatalf("destination content = %q, want concurrent", got)
		}
		recoveries, err := filepath.Glob(filepath.Join(root, ".copilot-*.recover"))
		if err != nil || len(recoveries) != 1 {
			t.Fatalf("recovery files = %v, error = %v, want one", recoveries, err)
		}
	})
}

func TestFilesystemRunnerApplyPatchUpdateKeepsSourcePresentBeforeInstall(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "source.txt")
	if err := os.WriteFile(path, []byte("before\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(source) error = %v", err)
	}

	const hook = `def patch_before_install(entry):
    pass`
	injectedHook := `def patch_before_install(entry):
    for _ in range(100):
        if not os.path.lexists(entry["path"]):
            raise PatchError("source disappeared before install")`
	script := strings.Replace(filesystemRunnerScript, hook, injectedHook, 1)
	if script == filesystemRunnerScript {
		t.Fatal("failed to inject patch install observer")
	}
	const afterHook = `def patch_after_install(entry):
    pass`
	injectedAfterHook := `def patch_after_install(entry):
    for _ in range(100):
        if not os.path.lexists(entry["path"]):
            raise PatchError("source disappeared after install")`
	beforeAfterHook := script
	script = strings.Replace(script, afterHook, injectedAfterHook, 1)
	if script == beforeAfterHook {
		t.Fatal("failed to inject patch post-install observer")
	}

	_, stderr, exitCode := runFilesystemRunnerScript(t, script, "apply_patch", ApplyPatchRequest{
		Cwd: root,
		Patch: strings.Join([]string{
			"*** Begin Patch",
			"*** Update File: source.txt",
			"@@",
			"-before",
			"+after",
			"*** End Patch",
			"",
		}, "\n"),
	})
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(source) error = %v", err)
	}
	if got := string(content); got != "after\n" {
		t.Fatalf("source content = %q, want after", got)
	}
}

func TestFilesystemRunnerApplyPatchUpdatePreservesConcurrentReplacementAfterInstall(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "source.txt")
	if err := os.WriteFile(path, []byte("before\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(source) error = %v", err)
	}

	const hook = `def patch_after_install(entry):
    pass`
	injectedHook := `def patch_after_install(entry):
    replacement = entry["path"] + ".concurrent"
    with open(replacement, "w", encoding="utf-8") as handle:
        handle.write("concurrent\n")
    os.replace(replacement, entry["path"])`
	script := strings.Replace(filesystemRunnerScript, hook, injectedHook, 1)
	if script == filesystemRunnerScript {
		t.Fatal("failed to inject patch concurrent replacement")
	}

	_, stderr, exitCode := runFilesystemRunnerScript(t, script, "apply_patch", ApplyPatchRequest{
		Cwd: root,
		Patch: strings.Join([]string{
			"*** Begin Patch",
			"*** Update File: source.txt",
			"@@",
			"-before",
			"+after",
			"*** End Patch",
			"",
		}, "\n"),
	})
	if exitCode == 0 || !strings.Contains(stderr, "recovery backup preserved") {
		t.Fatalf("exitCode = %d, stderr = %q, want atomic replacement conflict", exitCode, stderr)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(source) error = %v", err)
	}
	if got := string(content); got != "concurrent\n" {
		t.Fatalf("source content = %q, want concurrent", got)
	}
	backups, err := filepath.Glob(filepath.Join(root, ".source.txt.*.bak"))
	if err != nil || len(backups) != 1 {
		t.Fatalf("backup files = %v, error = %v, want one", backups, err)
	}
}

func TestFilesystemRunnerRootedReadReturnsBinaryAndRejectsEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	content := []byte{0x00, 0xff, 'r', 'o', 'o', 't'}
	path := filepath.Join(root, "root.bin")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("WriteFile(root): %v", err)
	}

	t.Run("pins parent across symlink swap", func(t *testing.T) {
		hook := normalizePythonSnippet(`def rooted_parent_opened(operation, parent_fd, path):
	    pass`)
		injectedHook := normalizePythonSnippet(`def rooted_parent_opened(operation, parent_fd, path):
	    parent = os.environ["SSH_SWAP_PARENT"]
	    moved = os.environ["SSH_SWAP_MOVED"]
	    outside = os.environ["SSH_SWAP_OUTSIDE"]
	    if not os.path.lexists(moved):
	        os.rename(parent, moved)
	        os.symlink(outside, parent)`)
		script := strings.Replace(filesystemRunnerScript, hook, injectedHook, 1)
		if script == filesystemRunnerScript {
			t.Fatal("failed to inject rooted parent swap hook")
		}

		t.Run("read", func(t *testing.T) {
			root := t.TempDir()
			parent := filepath.Join(root, "parent")
			moved := filepath.Join(root, "pinned")
			outside := t.TempDir()
			if err := os.Mkdir(parent, 0o755); err != nil {
				t.Fatalf("Mkdir(parent) error = %v", err)
			}
			if err := os.WriteFile(filepath.Join(parent, "source"), []byte("inside"), 0o644); err != nil {
				t.Fatalf("WriteFile(inside) error = %v", err)
			}
			if err := os.WriteFile(filepath.Join(outside, "source"), []byte("outside"), 0o644); err != nil {
				t.Fatalf("WriteFile(outside) error = %v", err)
			}
			t.Setenv("SSH_SWAP_PARENT", parent)
			t.Setenv("SSH_SWAP_MOVED", moved)
			t.Setenv("SSH_SWAP_OUTSIDE", outside)

			stdout, stderr, exitCode := runFilesystemRunnerScript(t, script, "read", RootedReadRequest{
				Path: filepath.Join(parent, "source"),
				Root: root,
			})
			if exitCode != 0 {
				t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr)
			}
			var got struct {
				Data []byte `json:"data"`
			}
			if err := json.Unmarshal([]byte(stdout), &got); err != nil {
				t.Fatalf("Unmarshal() error = %v", err)
			}
			if string(got.Data) != "inside" {
				t.Fatalf("data = %q, want pinned source", got.Data)
			}
		})

		t.Run("write", func(t *testing.T) {
			root := t.TempDir()
			parent := filepath.Join(root, "parent")
			moved := filepath.Join(root, "pinned")
			outside := t.TempDir()
			if err := os.Mkdir(parent, 0o755); err != nil {
				t.Fatalf("Mkdir(parent) error = %v", err)
			}
			if err := os.WriteFile(filepath.Join(parent, "target"), []byte("inside"), 0o600); err != nil {
				t.Fatalf("WriteFile(inside) error = %v", err)
			}
			if err := os.WriteFile(filepath.Join(outside, "target"), []byte("outside"), 0o644); err != nil {
				t.Fatalf("WriteFile(outside) error = %v", err)
			}
			t.Setenv("SSH_SWAP_PARENT", parent)
			t.Setenv("SSH_SWAP_MOVED", moved)
			t.Setenv("SSH_SWAP_OUTSIDE", outside)

			_, stderr, exitCode := runFilesystemRunnerScript(t, script, "write", RootedWriteRequest{
				Path:      filepath.Join(parent, "target"),
				Root:      root,
				Data:      []byte("updated"),
				Overwrite: true,
			})
			if exitCode != 0 {
				t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr)
			}
			pinned, err := os.ReadFile(filepath.Join(moved, "target"))
			if err != nil {
				t.Fatalf("ReadFile(pinned) error = %v", err)
			}
			if string(pinned) != "updated" {
				t.Fatalf("pinned = %q, want updated", pinned)
			}
			outsideContent, err := os.ReadFile(filepath.Join(outside, "target"))
			if err != nil {
				t.Fatalf("ReadFile(outside) error = %v", err)
			}
			if string(outsideContent) != "outside" {
				t.Fatalf("outside = %q, want unchanged", outsideContent)
			}
			info, err := os.Stat(filepath.Join(moved, "target"))
			if err != nil {
				t.Fatalf("Stat(pinned) error = %v", err)
			}
			if info.Mode().Perm() != 0o600 {
				t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
			}
		})
	})

	t.Run("bounds copies and rejects nonregular files", func(t *testing.T) {
		root := t.TempDir()

		t.Run("oversized source", func(t *testing.T) {
			path := filepath.Join(root, "large")
			if err := os.WriteFile(path, nil, 0o644); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
			if err := os.Truncate(path, MaxFileTransferBytes+1); err != nil {
				t.Fatalf("Truncate() error = %v", err)
			}
			_, stderr, exitCode := runFilesystemRunner(t, "read", RootedReadRequest{Path: path, Root: root})
			if exitCode == 0 || !strings.Contains(stderr, "exceeds") {
				t.Fatalf("exitCode = %d, stderr = %q, want copy limit", exitCode, stderr)
			}
		})

		t.Run("oversized encoded destination", func(t *testing.T) {
			path := filepath.Join(root, "oversized")
			encodedBytes := ((MaxFileTransferBytes + 2) / 3 * 4) + 4
			_, stderr, exitCode := runFilesystemRunner(t, "write", map[string]any{
				"path":      path,
				"root":      root,
				"data":      strings.Repeat("A", encodedBytes),
				"overwrite": true,
			})
			if exitCode == 0 || !strings.Contains(stderr, "exceeds") {
				t.Fatalf("exitCode = %d, stderr = %q, want copy limit", exitCode, stderr)
			}
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				t.Fatalf("destination Lstat error = %v, want absent", err)
			}
		})

		t.Run("nonregular source and destination", func(t *testing.T) {
			source := filepath.Join(root, "source-fifo")
			if err := syscall.Mkfifo(source, 0o600); err != nil {
				t.Fatalf("Mkfifo() error = %v", err)
			}
			_, stderr, exitCode := runFilesystemRunner(t, "read", RootedReadRequest{Path: source, Root: root})
			if exitCode == 0 || !strings.Contains(stderr, "not a regular file") {
				t.Fatalf("read exitCode = %d, stderr = %q", exitCode, stderr)
			}

			destination := filepath.Join(root, "destination")
			if err := os.Mkdir(destination, 0o755); err != nil {
				t.Fatalf("Mkdir(destination) error = %v", err)
			}
			_, stderr, exitCode = runFilesystemRunner(t, "write", RootedWriteRequest{
				Path:      destination,
				Root:      root,
				Data:      []byte("data"),
				Overwrite: true,
			})
			if exitCode == 0 || !strings.Contains(stderr, "not a regular file") {
				t.Fatalf("write exitCode = %d, stderr = %q", exitCode, stderr)
			}
		})
	})

	t.Run("client rejects oversized content before SSH", func(t *testing.T) {
		client := NewClient("demo")
		var calls []fakeExecCall
		client.commandContext = fakeCommandContext(t, &calls, nil)

		err := client.WriteFileRooted(context.Background(), RootedWriteRequest{
			Path: "/workspaces/repo/large",
			Root: "/workspaces/repo",
			Data: make([]byte, MaxFileTransferBytes+1),
		})
		if err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("WriteFileRooted() error = %v, want copy limit", err)
		}
		if len(calls) != 0 {
			t.Fatalf("SSH calls = %d, want zero", len(calls))
		}
	})

	stdout, stderr, exitCode := runFilesystemRunner(t, "read", map[string]any{
		"path": path,
		"root": root,
	})
	if exitCode != 0 {
		t.Fatalf("read exitCode = %d, stderr = %q", exitCode, stderr)
	}
	var result struct {
		Data []byte `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("Unmarshal(read result): %v", err)
	}
	if !bytes.Equal(result.Data, content) {
		t.Fatalf("read bytes = %v, want %v", result.Data, content)
	}

	_, stderr, exitCode = runFilesystemRunner(t, "read", map[string]any{
		"path": filepath.Join(outside, "escaped.bin"),
		"root": root,
	})
	if exitCode == 0 || !strings.Contains(stderr, "escapes workdir") {
		t.Fatalf("escape exitCode = %d, stderr = %q", exitCode, stderr)
	}
}

func TestFilesystemRunnerWriteFileRejectsUnsafeDestinationsWithoutPartialWrites(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.bin")
	link := filepath.Join(dir, "link.bin")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatalf("WriteFile(target) error = %v", err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}

	_, stderr, exitCode := runFilesystemRunner(t, "write", map[string]any{
		"path":      link,
		"data":      base64.StdEncoding.EncodeToString([]byte("replace")),
		"overwrite": true,
		"root":      dir,
	})
	if exitCode == 0 || !strings.Contains(stderr, "symbolic link") {
		t.Fatalf("symlink write exitCode = %d, stderr = %q", exitCode, stderr)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile(target) error = %v", err)
	}
	if string(got) != "keep" {
		t.Fatalf("target = %q, want unchanged", got)
	}

	missing := filepath.Join(dir, "missing.bin")
	_, stderr, exitCode = runFilesystemRunner(t, "write", map[string]any{
		"path":      missing,
		"data":      "not-base64%%%",
		"overwrite": true,
		"root":      dir,
	})
	if exitCode == 0 || !strings.Contains(stderr, "base64") {
		t.Fatalf("invalid base64 exitCode = %d, stderr = %q", exitCode, stderr)
	}
	if _, err := os.Lstat(missing); !os.IsNotExist(err) {
		t.Fatalf("missing destination exists or Lstat failed: %v", err)
	}

	outside := t.TempDir()
	parentLink := filepath.Join(dir, "outside")
	if err := os.Symlink(outside, parentLink); err != nil {
		t.Fatalf("Symlink(parent) error = %v", err)
	}
	escaped := filepath.Join(parentLink, "escaped.bin")
	_, stderr, exitCode = runFilesystemRunner(t, "write", map[string]any{
		"path":      escaped,
		"data":      base64.StdEncoding.EncodeToString([]byte("escape")),
		"overwrite": true,
		"root":      dir,
	})
	if exitCode == 0 || !strings.Contains(stderr, "escapes workdir") {
		t.Fatalf("parent symlink exitCode = %d, stderr = %q", exitCode, stderr)
	}
	if _, err := os.Lstat(filepath.Join(outside, "escaped.bin")); !os.IsNotExist(err) {
		t.Fatalf("escaped destination exists or Lstat failed: %v", err)
	}
}

func TestClientWriteFileTransfersBase64AndOverwriteOverStdin(t *testing.T) {
	client := newVerifiedFilesystemClient(t)
	client.SetWorkdir("/workspaces/repo")
	stdinPath := filepath.Join(t.TempDir(), "write.json")

	var calls []fakeExecCall
	client.commandContext = fakeCommandContext(t, &calls, []fakeExecResponse{
		{stdinPath: stdinPath},
	})

	content := []byte{0x00, 0xff, 'x'}
	if err := client.WriteFile(context.Background(), "blob.bin", content, true); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	var payload map[string]any
	readJSONFile(t, stdinPath, &payload)
	if payload["path"] != "/workspaces/repo/blob.bin" {
		t.Fatalf("payload path = %#v", payload["path"])
	}
	if payload["data"] != base64.StdEncoding.EncodeToString(content) {
		t.Fatalf("payload data = %#v", payload["data"])
	}
	if payload["overwrite"] != true || payload["root"] != "/workspaces/repo" {
		t.Fatalf("payload flags = %#v", payload)
	}
	if len(calls) != 1 || !strings.Contains(strings.Join(calls[0].args, " "), "filesystem write") {
		t.Fatalf("unexpected calls: %#v", calls)
	}
}

func TestClientRootedCopyRequestsIgnoreMutableWorkdir(t *testing.T) {
	client := newVerifiedFilesystemClient(t)
	client.SetWorkdir("/workspaces/repo/internal/ssh")
	readInput := filepath.Join(t.TempDir(), "read.json")
	writeInput := filepath.Join(t.TempDir(), "write.json")
	content := []byte{0x00, 0xff, 'x'}

	var calls []fakeExecCall
	client.commandContext = fakeCommandContext(t, &calls, []fakeExecResponse{
		{stdout: `{"data":"AP94"}` + "\n", stdinPath: readInput},
		{stdinPath: writeInput},
	})

	read, err := client.ReadFileRooted(context.Background(), RootedReadRequest{
		Path: "/workspaces/repo/root.bin",
		Root: "/workspaces/repo",
	})
	if err != nil {
		t.Fatalf("ReadFileRooted() error = %v", err)
	}
	if !bytes.Equal(read, content) {
		t.Fatalf("read bytes = %v, want %v", read, content)
	}
	if err := client.WriteFileRooted(context.Background(), RootedWriteRequest{
		Path:      "/workspaces/repo/root.bin",
		Root:      "/workspaces/repo",
		Data:      content,
		Overwrite: true,
	}); err != nil {
		t.Fatalf("WriteFileRooted() error = %v", err)
	}

	var readPayload, writePayload map[string]any
	readJSONFile(t, readInput, &readPayload)
	readJSONFile(t, writeInput, &writePayload)
	for name, payload := range map[string]map[string]any{"read": readPayload, "write": writePayload} {
		if payload["path"] != "/workspaces/repo/root.bin" || payload["root"] != "/workspaces/repo" {
			t.Fatalf("%s payload = %#v, want immutable root", name, payload)
		}
	}
	if len(calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(calls))
	}
}

func TestClientGrepFilesMapsOptionsAndResolvedCwd(t *testing.T) {
	client := NewClient("demo")
	if err := client.SelectFilesystemHelper("/remote/gh-copilot-codespace", helperinfo.Current()); err != nil {
		t.Fatalf("SelectFilesystemHelper() error = %v", err)
	}
	client.SetWorkdir("/workspaces/repo")
	stdinPath := filepath.Join(t.TempDir(), "grep.json")

	var calls []fakeExecCall
	client.commandContext = fakeCommandContext(t, &calls, []fakeExecResponse{
		{stdout: `{"output":"src/main.go:7:Needle\n","truncated":true,"skipped_files":2,"input_byte_limit":16777216}` + "\n", stdinPath: stdinPath},
	})

	lineNumbers := false
	got, err := client.GrepFiles(context.Background(), GrepRequest{
		Pattern:         "Needle",
		Paths:           []string{"src", "pkg"},
		Glob:            "*.go",
		OutputMode:      GrepOutputModeContent,
		Type:            "go",
		CaseInsensitive: true,
		Context:         3,
		LineNumbers:     &lineNumbers,
		HeadLimit:       5,
		Multiline:       true,
	})
	if err != nil {
		t.Fatalf("GrepFiles() error = %v", err)
	}
	if got.Output != "src/main.go:7:Needle\n" || got.OutputMode != GrepOutputModeContent {
		t.Fatalf("GrepFiles() = %+v", got)
	}
	if !got.Truncated {
		t.Fatal("Truncated = false, want true")
	}
	if got.SkippedFiles != 2 || got.InputByteLimit != MaxGrepInputBytes {
		t.Fatalf("grep input metadata = %+v", got)
	}
	if !reflect.DeepEqual(got.Paths, []string{"src", "pkg"}) {
		t.Fatalf("Paths = %v, want [src pkg]", got.Paths)
	}

	var payload map[string]any
	readJSONFile(t, stdinPath, &payload)
	if payload["cwd"] != "/workspaces/repo" {
		t.Fatalf("cwd = %v, want /workspaces/repo", payload["cwd"])
	}
	if int(payload["head_limit"].(float64)) != 5 {
		t.Fatalf("head_limit = %v, want 5", payload["head_limit"])
	}
	if _, ok := payload["args"]; ok {
		t.Fatalf("payload contains legacy runner args: %#v", payload["args"])
	}
}

func TestClientGrepFilesCountForcesFilenameWithoutHeading(t *testing.T) {
	client := NewClient("demo")
	req, err := client.buildFilesystemGrepRequest(GrepRequest{
		Pattern:    "needle",
		Paths:      []string{"single.txt"},
		OutputMode: GrepOutputModeCount,
		Cwd:        "/workspaces/repo",
	})
	if err != nil {
		t.Fatalf("buildFilesystemGrepRequest() error = %v", err)
	}
	want := []string{
		"rg", "--color=never", "--with-filename", "--no-heading", "--count", "--", "needle", "single.txt",
	}
	if !reflect.DeepEqual(req.Args, want) {
		t.Fatalf("Args = %v, want %v", req.Args, want)
	}
}

func TestClientGlobFilesSupportsMultipleRoots(t *testing.T) {
	client := newVerifiedFilesystemClient(t)
	stdinPath := filepath.Join(t.TempDir(), "glob.json")

	var calls []fakeExecCall
	client.commandContext = fakeCommandContext(t, &calls, []fakeExecResponse{
		{stdout: `{"output":"pkg/foo.go\nsrc/bar.go\n"}` + "\n", stdinPath: stdinPath},
	})

	got, err := client.GlobFiles(context.Background(), GlobRequest{
		Pattern: "**/*.go",
		Paths:   []string{"pkg", "src"},
		Cwd:     "/workspaces/repo",
	})
	if err != nil {
		t.Fatalf("GlobFiles() error = %v", err)
	}
	if got.Output != "pkg/foo.go\nsrc/bar.go\n" {
		t.Fatalf("Output = %q", got.Output)
	}
	if !reflect.DeepEqual(got.Paths, []string{"pkg", "src"}) {
		t.Fatalf("Paths = %v, want [pkg src]", got.Paths)
	}

	var payload GlobRequest
	readJSONFile(t, stdinPath, &payload)
	if payload.Pattern != "**/*.go" || payload.Cwd != "/workspaces/repo" || !reflect.DeepEqual(payload.Paths, []string{"pkg", "src"}) {
		t.Fatalf("payload = %+v", payload)
	}
	if len(calls) != 1 || !strings.Contains(strings.Join(calls[0].args, " "), "filesystem glob") {
		t.Fatalf("unexpected calls: %#v", calls)
	}
}

func TestClientApplyPatchTransfersPatchOverStdin(t *testing.T) {
	client := newVerifiedFilesystemClient(t)
	stdinPath := filepath.Join(t.TempDir(), "patch.json")

	var calls []fakeExecCall
	client.commandContext = fakeCommandContext(t, &calls, []fakeExecResponse{
		{stdout: `{"output":"Patch applied.","files_changed":1}` + "\n", stdinPath: stdinPath},
	})

	got, err := client.ApplyPatch(context.Background(), ApplyPatchRequest{
		Patch: "*** Begin Patch\n*** End Patch\n",
		Cwd:   "/workspaces/repo",
	})
	if err != nil {
		t.Fatalf("ApplyPatch() error = %v", err)
	}
	if got.Output != "Patch applied." || got.FilesChanged != 1 {
		t.Fatalf("ApplyPatch() = %+v", got)
	}

	var payload ApplyPatchRequest
	readJSONFile(t, stdinPath, &payload)
	if payload.Patch != "*** Begin Patch\n*** End Patch\n" || payload.Cwd != "/workspaces/repo" {
		t.Fatalf("payload = %+v", payload)
	}
	if len(calls) != 1 || !strings.Contains(strings.Join(calls[0].args, " "), "filesystem apply_patch") {
		t.Fatalf("unexpected calls: %#v", calls)
	}
}

func TestFilesystemRunnerViewHugeSingleLineIsBoundedAndUTF8Safe(t *testing.T) {
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

			var truncatedResults []ViewResult
			for _, viewRange := range [][]int{nil, {1, 1}} {
				stdout, stderr, exitCode := runFilesystemRunner(t, "view", ViewRequest{
					Path:      path,
					ViewRange: viewRange,
				})
				if exitCode != 0 {
					t.Fatalf("range=%v exitCode = %d, stderr = %q", viewRange, exitCode, stderr)
				}
				got := decodeRunnerResult[ViewResult](t, stdout)
				if !got.Truncated {
					t.Fatalf("range=%v Truncated = false, want true", viewRange)
				}
				if !strings.HasPrefix(got.Content, "1. ") || len(got.Content) <= len("1. \n") {
					t.Fatalf("range=%v content is not a useful partial line: %q", viewRange, got.Content)
				}
				if len([]byte(got.Content)) > 20*1024 {
					t.Fatalf("range=%v content size = %d, want <= 20KB", viewRange, len([]byte(got.Content)))
				}
				if !utf8.ValidString(got.Content) {
					t.Fatalf("range=%v returned invalid UTF-8", viewRange)
				}
				truncatedResults = append(truncatedResults, got)
			}
			if truncatedResults[0].Content != truncatedResults[1].Content {
				t.Fatal("ranged and unranged truncation differ")
			}

			wantFull := "1. " + tt.line + "\n"
			for _, viewRange := range [][]int{nil, {1, 1}} {
				stdout, stderr, exitCode := runFilesystemRunner(t, "view", ViewRequest{
					Path:                path,
					ViewRange:           viewRange,
					ForceReadLargeFiles: true,
				})
				if exitCode != 0 {
					t.Fatalf("force range=%v exitCode = %d, stderr = %q", viewRange, exitCode, stderr)
				}
				got := decodeRunnerResult[ViewResult](t, stdout)
				if got.Truncated || got.Content != wantFull {
					t.Fatalf("force range=%v = {Truncated:%v ContentBytes:%d}, want complete %d-byte content",
						viewRange, got.Truncated, len([]byte(got.Content)), len([]byte(wantFull)))
				}
			}
		})
	}
}

func TestFilesystemRunnerEditRejectsSymlinkPath(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.txt")
	link := filepath.Join(root, "link.txt")
	if err := os.WriteFile(target, []byte("before\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}

	t.Run("transactional edit", func(t *testing.T) {
		t.Run("preserves mode", func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "sample.txt")
			if err := os.WriteFile(path, []byte("before\n"), 0o751); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}

			_, stderr, exitCode := runFilesystemRunner(t, "edit", map[string]any{
				"path":    path,
				"old_str": "before",
				"new_str": "after",
			})
			if exitCode != 0 {
				t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatalf("Stat() error = %v", err)
			}
			if info.Mode().Perm() != 0o751 {
				t.Fatalf("mode = %v, want 0751", info.Mode().Perm())
			}
		})

		t.Run("detects digest conflict", func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "sample.txt")
			if err := os.WriteFile(path, []byte("before\n"), 0o644); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}

			preflightSource := normalizePythonSnippet(`def preflight_patch_source(entry):
	    info = require_patch_file(entry["path"], entry["path"])
	    if patch_path_identity(entry["path"], info) != entry["identity"]:
	        patch_error("could not apply patch: file changed during patch: %s" % entry["path"])`)
			injectedConflict := preflightSource + normalizePythonSnippet(`
	    entry["_test_preflight_count"] = entry.get("_test_preflight_count", 0) + 1
	    if entry.get("_edit") and entry["_test_preflight_count"] == 3:
	        mtime_ns = entry["identity"][5]
	        with open(entry["path"], "w", encoding="utf-8") as handle:
	            handle.write("changed\n")
	        os.utime(entry["path"], ns=(mtime_ns, mtime_ns))`)
			script := strings.Replace(filesystemRunnerScript, preflightSource, injectedConflict, 1)
			if script == filesystemRunnerScript {
				t.Fatal("failed to inject edit conflict")
			}

			_, stderr, exitCode := runFilesystemRunnerScript(t, script, "edit", map[string]any{
				"path":    path,
				"old_str": "before",
				"new_str": "after",
			})
			if exitCode == 0 || !strings.Contains(stderr, "file changed") {
				t.Fatalf("exitCode = %d, stderr = %q, want conflict", exitCode, stderr)
			}
			content, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile() error = %v", err)
			}
			if string(content) != "changed\n" {
				t.Fatalf("content = %q, want concurrent content", content)
			}
		})

		t.Run("cancellation rolls back", func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "sample.txt")
			if err := os.WriteFile(path, []byte("before\n"), 0o640); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}

			replace := normalizePythonSnippet(`def patch_after_install(entry):
	    pass`)
			injectedCancel := normalizePythonSnippet(`def patch_after_install(entry):
	    if entry.get("_edit"):
	        os.kill(os.getpid(), signal.SIGTERM)`)
			script := strings.Replace(filesystemRunnerScript, replace, injectedCancel, 1)
			if script == filesystemRunnerScript {
				t.Fatal("failed to inject edit cancellation")
			}

			_, stderr, exitCode := runFilesystemRunnerScript(t, script, "edit", map[string]any{
				"path":    path,
				"old_str": "before",
				"new_str": "after",
			})
			if exitCode == 0 || !strings.Contains(stderr, "canceled") {
				t.Fatalf("exitCode = %d, stderr = %q, want cancellation", exitCode, stderr)
			}
			content, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile() error = %v", err)
			}
			if string(content) != "before\n" {
				t.Fatalf("content = %q, want rollback", content)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatalf("Stat() error = %v", err)
			}
			if info.Mode().Perm() != 0o640 {
				t.Fatalf("mode = %v, want 0640", info.Mode().Perm())
			}
		})

		t.Run("rollback conflict preserves concurrent replacement and backup", func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "sample.txt")
			if err := os.WriteFile(path, []byte("before\n"), 0o644); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}

			replace := normalizePythonSnippet(`def patch_after_install(entry):
	    pass`)
			injectedConflict := normalizePythonSnippet(`def patch_after_install(entry):
	    if entry.get("_edit"):
	        replacement = entry["target"] + ".concurrent"
	        with open(replacement, "w", encoding="utf-8") as handle:
	            handle.write("concurrent\n")
	        os.replace(replacement, entry["target"])
	        raise OSError("injected edit failure")`)
			script := strings.Replace(filesystemRunnerScript, replace, injectedConflict, 1)
			if script == filesystemRunnerScript {
				t.Fatal("failed to inject edit rollback conflict")
			}

			_, stderr, exitCode := runFilesystemRunnerScript(t, script, "edit", map[string]any{
				"path":    path,
				"old_str": "before",
				"new_str": "after",
			})
			if exitCode == 0 || !strings.Contains(stderr, "rollback") {
				t.Fatalf("exitCode = %d, stderr = %q, want rollback conflict", exitCode, stderr)
			}
			content, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile() error = %v", err)
			}
			if string(content) != "concurrent\n" {
				t.Fatalf("content = %q, want concurrent replacement", content)
			}
			backups, err := filepath.Glob(filepath.Join(root, ".sample.txt.*.bak"))
			if err != nil {
				t.Fatalf("Glob(backups) error = %v", err)
			}
			if len(backups) != 1 {
				t.Fatalf("backups = %v, want one; stderr=%q", backups, stderr)
			}
			original, err := os.ReadFile(backups[0])
			if err != nil {
				t.Fatalf("ReadFile(backup) error = %v", err)
			}
			if string(original) != "before\n" {
				t.Fatalf("backup = %q, want original", original)
			}
		})
	})

	_, stderr, exitCode := runFilesystemRunner(t, "edit", map[string]any{
		"path":    link,
		"old_str": "before",
		"new_str": "after",
	})
	if exitCode == 0 {
		t.Fatal("exitCode = 0, want symlink rejection")
	}
	if !strings.Contains(stderr, "symbolic link") {
		t.Fatalf("stderr = %q, want explicit symlink error", stderr)
	}
	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(content) != "before\n" {
		t.Fatalf("target content = %q, want unchanged", content)
	}
}

func TestFilesystemRunnerGlobSlashlessPatternMatchesBasenamesRecursively(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{"root.go", "nested/child.go", "nested/skip.txt"} {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll(%q) error = %v", full, err)
		}
		if err := os.WriteFile(full, []byte("x\n"), 0o644); err != nil {
			t.Fatalf("WriteFile(%q) error = %v", full, err)
		}
	}

	stdout, stderr, exitCode := runFilesystemRunner(t, "glob", GlobRequest{
		Pattern: "*.go",
		Paths:   []string{"."},
		Cwd:     root,
	})
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr)
	}
	got := decodeRunnerResult[GlobResult](t, stdout)
	if got.Output != "nested/child.go\nroot.go\n" {
		t.Fatalf("Output = %q, want recursive basename matches", got.Output)
	}
}

func TestFilesystemRunnerApplyPatchRejectsTopologyConflictWithoutMutation(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "sample.txt")
	if err := os.WriteFile(path, []byte("alpha\nbeta\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	_, stderr, exitCode := runFilesystemRunner(t, "apply_patch", ApplyPatchRequest{
		Cwd: root,
		Patch: strings.Join([]string{
			"*** Begin Patch",
			"*** Update File: sample.txt",
			"@@",
			"-alpha",
			"+ALPHA",
			"*** Update File: sample.txt",
			"@@",
			"-beta",
			"+BETA",
			"*** End Patch",
			"",
		}, "\n"),
	})
	if exitCode == 0 {
		t.Fatal("exitCode = 0, want topology conflict")
	}
	if !strings.Contains(stderr, "conflicts with") {
		t.Fatalf("stderr = %q, want topology conflict", stderr)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(content) != "alpha\nbeta\n" {
		t.Fatalf("content = %q, want unchanged", content)
	}
}

func TestFilesystemRunnerApplyPatchCommitFailureRollsBack(t *testing.T) {
	testFilesystemRunnerApplyPatchInterruptionRollsBack(t, `raise OSError("injected commit failure")`)
}

func TestFilesystemRunnerApplyPatchCommitFailureRollsBackAddsAndDeletes(t *testing.T) {
	root := t.TempDir()
	deletedPath := filepath.Join(root, "deleted.txt")
	finalPath := filepath.Join(root, "final.txt")
	if err := os.WriteFile(deletedPath, []byte("deleted\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(deleted) error = %v", err)
	}
	if err := os.WriteFile(finalPath, []byte("final\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(final) error = %v", err)
	}

	const commitDelete = `            if entry["kind"] == "delete":
                tracked_remove(entry["path"], entry)`
	injectedDelete := `            if entry["kind"] == "delete":
                if entry["path"].endswith("final.txt"):
                    raise OSError("injected commit failure")
                tracked_remove(entry["path"], entry)`
	script := strings.Replace(filesystemRunnerScript, commitDelete, injectedDelete, 1)
	if script == filesystemRunnerScript {
		t.Fatal("failed to inject commit interruption")
	}

	_, _, exitCode := runFilesystemRunnerScript(t, script, "apply_patch", ApplyPatchRequest{
		Cwd: root,
		Patch: strings.Join([]string{
			"*** Begin Patch",
			"*** Delete File: deleted.txt",
			"*** Add File: added.txt",
			"+added",
			"*** Delete File: final.txt",
			"*** End Patch",
			"",
		}, "\n"),
	})
	if exitCode == 0 {
		t.Fatal("exitCode = 0, want commit failure")
	}
	for path, want := range map[string]string{
		deletedPath: "deleted\n",
		finalPath:   "final\n",
	} {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(%q) error = %v", path, err)
		}
		if string(content) != want {
			t.Fatalf("%s = %q, want %q", filepath.Base(path), content, want)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "added.txt")); !os.IsNotExist(err) {
		t.Fatalf("added.txt stat error = %v, want not exist", err)
	}
}

func TestFilesystemRunnerApplyPatchRollbackPreservesConcurrentReplacements(t *testing.T) {
	tests := []struct {
		name              string
		patchOperation    []string
		replacementPath   string
		wantFiles         map[string]string
		wantBackupPattern string
	}{
		{
			name: "add",
			patchOperation: []string{
				"*** Add File: subject.txt",
				"+added",
			},
			replacementPath: "subject.txt",
			wantFiles: map[string]string{
				"subject.txt": "concurrent\n",
				"final.txt":   "final\n",
			},
		},
		{
			name: "update",
			patchOperation: []string{
				"*** Update File: subject.txt",
				"@@",
				"-original",
				"+updated",
			},
			replacementPath: "subject.txt",
			wantFiles: map[string]string{
				"subject.txt": "concurrent\n",
				"final.txt":   "final\n",
			},
			wantBackupPattern: ".subject.txt.*.bak",
		},
		{
			name: "delete",
			patchOperation: []string{
				"*** Delete File: subject.txt",
			},
			replacementPath: "subject.txt",
			wantFiles: map[string]string{
				"subject.txt": "concurrent\n",
				"final.txt":   "final\n",
			},
			wantBackupPattern: ".subject.txt.*.bak",
		},
		{
			name: "move",
			patchOperation: []string{
				"*** Update File: subject.txt",
				"*** Move to: moved.txt",
			},
			replacementPath: "subject.txt",
			wantFiles: map[string]string{
				"subject.txt": "concurrent\n",
				"moved.txt":   "original\n",
				"final.txt":   "final\n",
			},
			wantBackupPattern: ".subject.txt.*.bak",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			if tt.name != "add" {
				if err := os.WriteFile(filepath.Join(root, "subject.txt"), []byte("original\n"), 0o644); err != nil {
					t.Fatalf("WriteFile(subject) error = %v", err)
				}
			}
			if err := os.WriteFile(filepath.Join(root, "final.txt"), []byte("final\n"), 0o644); err != nil {
				t.Fatalf("WriteFile(final) error = %v", err)
			}

			const commitDelete = `            if entry["kind"] == "delete":
                tracked_remove(entry["path"], entry)`
			injectedFailure := `            if entry["kind"] == "delete":
                if entry["path"].endswith("final.txt"):
                    replacement_path = os.path.join(os.path.dirname(entry["path"]), ` + strconv.Quote(tt.replacementPath) + `)
                    replacement_tmp = replacement_path + ".concurrent"
                    with open(replacement_tmp, "w", encoding="utf-8") as handle:
                        handle.write("concurrent\n")
                    os.replace(replacement_tmp, replacement_path)
                    raise OSError("injected later commit failure")
                tracked_remove(entry["path"], entry)`
			script := strings.Replace(filesystemRunnerScript, commitDelete, injectedFailure, 1)
			if script == filesystemRunnerScript {
				t.Fatal("failed to inject concurrent replacement")
			}

			patchLines := append([]string{"*** Begin Patch"}, tt.patchOperation...)
			patchLines = append(patchLines,
				"*** Delete File: final.txt",
				"*** End Patch",
				"",
			)
			_, stderr, exitCode := runFilesystemRunnerScript(t, script, "apply_patch", ApplyPatchRequest{
				Cwd:   root,
				Patch: strings.Join(patchLines, "\n"),
			})
			if exitCode == 0 {
				t.Fatal("exitCode = 0, want later commit failure")
			}
			if !strings.Contains(stderr, "rollback") {
				t.Fatalf("stderr = %q, want rollback conflict", stderr)
			}
			for rel, want := range tt.wantFiles {
				content, err := os.ReadFile(filepath.Join(root, rel))
				if err != nil {
					t.Fatalf("ReadFile(%q) error = %v", rel, err)
				}
				if string(content) != want {
					t.Fatalf("%s = %q, want %q", rel, content, want)
				}
			}

			if tt.wantBackupPattern == "" {
				return
			}
			backups, err := filepath.Glob(filepath.Join(root, tt.wantBackupPattern))
			if err != nil {
				t.Fatalf("Glob(backups) error = %v", err)
			}
			if len(backups) != 1 {
				t.Fatalf("backups = %v, want one; stderr=%q", backups, stderr)
			}
			if !strings.Contains(stderr, backups[0]) {
				t.Fatalf("stderr = %q, want backup path %q", stderr, backups[0])
			}
			content, err := os.ReadFile(backups[0])
			if err != nil {
				t.Fatalf("ReadFile(backup) error = %v", err)
			}
			if string(content) != "original\n" {
				t.Fatalf("backup content = %q, want original", content)
			}
		})
	}
}

func TestFilesystemRunnerApplyPatchCancellationRollsBack(t *testing.T) {
	testFilesystemRunnerApplyPatchInterruptionRollsBack(t, `os.kill(os.getpid(), signal.SIGTERM)`)
}

func TestFilesystemRunnerApplyPatchMoveOnlyLaterFailureRollsBack(t *testing.T) {
	testFilesystemRunnerApplyPatchMoveOnlyInterruptionRollsBack(t, `raise OSError("injected commit failure")`, "injected commit failure")
}

func TestFilesystemRunnerApplyPatchMoveOnlyCancellationRollsBack(t *testing.T) {
	testFilesystemRunnerApplyPatchMoveOnlyInterruptionRollsBack(t, `os.kill(os.getpid(), signal.SIGTERM)`, "apply patch canceled")
}

func testFilesystemRunnerApplyPatchMoveOnlyInterruptionRollsBack(t *testing.T, interruption, wantError string) {
	t.Helper()
	root := t.TempDir()
	sourcePath := filepath.Join(root, "source.bin")
	laterPath := filepath.Join(root, "later.txt")
	sourceContent := []byte{0x00, 0xfe, 0xff}
	if err := os.WriteFile(sourcePath, sourceContent, 0o751); err != nil {
		t.Fatalf("WriteFile(source) error = %v", err)
	}
	if err := os.WriteFile(laterPath, []byte("later\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(later) error = %v", err)
	}
	before, err := os.Stat(sourcePath)
	if err != nil {
		t.Fatalf("Stat(source) error = %v", err)
	}

	const commitDelete = `            if entry["kind"] == "delete":
                tracked_remove(entry["path"], entry)`
	injectedDelete := `            if entry["kind"] == "delete":
                ` + interruption + `
                tracked_remove(entry["path"], entry)`
	script := strings.Replace(filesystemRunnerScript, commitDelete, injectedDelete, 1)
	if script == filesystemRunnerScript {
		t.Fatal("failed to inject commit interruption")
	}

	_, stderr, exitCode := runFilesystemRunnerScript(t, script, "apply_patch", ApplyPatchRequest{
		Cwd: root,
		Patch: strings.Join([]string{
			"*** Begin Patch",
			"*** Update File: source.bin",
			"*** Move to: nested/moved.bin",
			"*** Delete File: later.txt",
			"*** End Patch",
			"",
		}, "\n"),
	})
	if exitCode == 0 {
		t.Fatal("exitCode = 0, want interruption failure")
	}
	if !strings.Contains(stderr, wantError) {
		t.Fatalf("stderr = %q, want %q", stderr, wantError)
	}
	content, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatalf("ReadFile(source) error = %v", err)
	}
	after, err := os.Stat(sourcePath)
	if err != nil {
		t.Fatalf("Stat(source) error = %v", err)
	}
	if !reflect.DeepEqual(content, sourceContent) || !os.SameFile(before, after) {
		t.Fatalf("source not restored: content=%v sameFile=%v", content, os.SameFile(before, after))
	}
	if _, err := os.Lstat(filepath.Join(root, "nested", "moved.bin")); !os.IsNotExist(err) {
		t.Fatalf("moved target Lstat error = %v, want not exist", err)
	}
	laterContent, err := os.ReadFile(laterPath)
	if err != nil {
		t.Fatalf("ReadFile(later) error = %v", err)
	}
	if string(laterContent) != "later\n" {
		t.Fatalf("later.txt = %q, want unchanged", laterContent)
	}
	if _, err := os.Stat(filepath.Join(root, "nested")); !os.IsNotExist(err) {
		t.Fatalf("created directory Stat error = %v, want not exist", err)
	}
}

func testFilesystemRunnerApplyPatchInterruptionRollsBack(t *testing.T, interruption string) {
	t.Helper()
	root := t.TempDir()
	aPath := filepath.Join(root, "a.txt")
	bPath := filepath.Join(root, "b.txt")
	if err := os.WriteFile(aPath, []byte("alpha\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(a) error = %v", err)
	}
	if err := os.WriteFile(bPath, []byte("beta\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(b) error = %v", err)
	}

	const commitDelete = `            if entry["kind"] == "delete":
                tracked_remove(entry["path"], entry)`
	injectedDelete := `            if entry["kind"] == "delete":
                ` + interruption + `
                tracked_remove(entry["path"], entry)`
	script := strings.Replace(filesystemRunnerScript, commitDelete, injectedDelete, 1)
	if script == filesystemRunnerScript {
		t.Fatal("failed to inject commit interruption")
	}

	_, _, exitCode := runFilesystemRunnerScript(t, script, "apply_patch", ApplyPatchRequest{
		Cwd: root,
		Patch: strings.Join([]string{
			"*** Begin Patch",
			"*** Update File: a.txt",
			"@@",
			"-alpha",
			"+ALPHA",
			"*** Delete File: b.txt",
			"*** End Patch",
			"",
		}, "\n"),
	})
	if exitCode == 0 {
		t.Fatal("exitCode = 0, want interruption failure")
	}
	for path, want := range map[string]string{
		aPath: "alpha\n",
		bPath: "beta\n",
	} {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(%q) error = %v", path, err)
		}
		if string(content) != want {
			t.Fatalf("%s = %q, want %q", filepath.Base(path), content, want)
		}
	}
}

func TestFilesystemRunnerApplyPatchEndOfFileMarkerTargetsFinalMatch(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "sample.txt")
	if err := os.WriteFile(path, []byte("value\nmiddle\nvalue\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	_, stderr, exitCode := runFilesystemRunner(t, "apply_patch", ApplyPatchRequest{
		Cwd: root,
		Patch: strings.Join([]string{
			"*** Begin Patch",
			"*** Update File: sample.txt",
			"@@",
			"-value",
			"+VALUE",
			"*** End of File",
			"*** End Patch",
			"",
		}, "\n"),
	})
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(content) != "value\nmiddle\nVALUE\n" {
		t.Fatalf("content = %q, want final match updated", content)
	}
}

func TestFilesystemRunnerApplyPatchHunkAnchorTargetsDuplicateSequence(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "sample.txt")
	if err := os.WriteFile(path, []byte("first section\nvalue\nsecond section\nvalue\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	_, stderr, exitCode := runFilesystemRunner(t, "apply_patch", ApplyPatchRequest{
		Cwd: root,
		Patch: strings.Join([]string{
			"*** Begin Patch",
			"*** Update File: sample.txt",
			"@@ second section",
			"-value",
			"+VALUE",
			"*** End Patch",
			"",
		}, "\n"),
	})
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(content) != "first section\nvalue\nsecond section\nVALUE\n" {
		t.Fatalf("content = %q, want anchored section updated", content)
	}
}

func TestFilesystemRunnerApplyPatchRejectsMissingHunkAnchor(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "sample.txt")
	if err := os.WriteFile(path, []byte("section\nvalue\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	_, stderr, exitCode := runFilesystemRunner(t, "apply_patch", ApplyPatchRequest{
		Cwd: root,
		Patch: strings.Join([]string{
			"*** Begin Patch",
			"*** Update File: sample.txt",
			"@@ missing section",
			"-value",
			"+VALUE",
			"*** End Patch",
			"",
		}, "\n"),
	})
	if exitCode == 0 {
		t.Fatal("exitCode = 0, want missing anchor failure")
	}
	if !strings.Contains(stderr, "could not apply patch") {
		t.Fatalf("stderr = %q, want patch failure", stderr)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(content) != "section\nvalue\n" {
		t.Fatalf("content = %q, want unchanged", content)
	}
}

func TestFilesystemRunnerApplyPatchPreservesLineEndingsAndFinalNewline(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "crlf with final newline", in: "alpha\r\nbeta\r\n", want: "alpha\r\nBETA\r\n"},
		{name: "crlf without final newline", in: "alpha\r\nbeta", want: "alpha\r\nBETA"},
		{name: "lf without final newline", in: "alpha\nbeta", want: "alpha\nBETA"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "sample.txt")
			if err := os.WriteFile(path, []byte(tt.in), 0o644); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}

			_, stderr, exitCode := runFilesystemRunner(t, "apply_patch", ApplyPatchRequest{
				Cwd: root,
				Patch: strings.Join([]string{
					"*** Begin Patch",
					"*** Update File: sample.txt",
					"@@",
					"-beta",
					"+BETA",
					"*** End Patch",
					"",
				}, "\n"),
			})
			if exitCode != 0 {
				t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr)
			}
			content, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile() error = %v", err)
			}
			if string(content) != tt.want {
				t.Fatalf("content = %q, want %q", content, tt.want)
			}
		})
	}
}

func TestFilesystemRunnerApplyPatchDeletingAllLinesProducesEmptyFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "sample.txt")
	if err := os.WriteFile(path, []byte("only line\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	_, stderr, exitCode := runFilesystemRunner(t, "apply_patch", ApplyPatchRequest{
		Cwd: root,
		Patch: strings.Join([]string{
			"*** Begin Patch",
			"*** Update File: sample.txt",
			"@@",
			"-only line",
			"*** End Patch",
			"",
		}, "\n"),
	})
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, stderr = %q", exitCode, stderr)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if len(content) != 0 {
		t.Fatalf("content = %q, want zero bytes", content)
	}
}

func TestFilesystemRunnerApplyPatchRejectsUnsupportedLineEndingsWithoutMutation(t *testing.T) {
	tests := []struct {
		name     string
		original string
	}{
		{
			name:     "mixed LF and CRLF",
			original: "alpha\r\nbeta\ngamma\r\n",
		},
		{
			name:     "bare CR",
			original: "alpha\rbeta\rgamma\r",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "sample.txt")
			if err := os.WriteFile(path, []byte(tt.original), 0o644); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}

			_, stderr, exitCode := runFilesystemRunner(t, "apply_patch", ApplyPatchRequest{
				Cwd: root,
				Patch: strings.Join([]string{
					"*** Begin Patch",
					"*** Update File: sample.txt",
					"@@",
					"-beta",
					"+BETA",
					"*** End Patch",
					"",
				}, "\n"),
			})
			if exitCode == 0 {
				t.Fatal("exitCode = 0, want line-ending rejection")
			}
			if !strings.Contains(stderr, "mixed line endings are not supported") {
				t.Fatalf("stderr = %q, want clear line-ending rejection", stderr)
			}
			content, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile() error = %v", err)
			}
			if string(content) != tt.original {
				t.Fatalf("content = %q, want unchanged %q", content, tt.original)
			}
		})
	}
}

func TestFilesystemRunnerApplyPatchRejectsSymlinkAliasOfSameFile(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatalf("Mkdir(real) error = %v", err)
	}
	path := filepath.Join(realDir, "sample.txt")
	if err := os.WriteFile(path, []byte("value\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := os.Symlink(realDir, filepath.Join(root, "alias")); err != nil {
		t.Fatalf("Symlink(alias) error = %v", err)
	}

	_, stderr, exitCode := runFilesystemRunner(t, "apply_patch", ApplyPatchRequest{
		Cwd: root,
		Patch: strings.Join([]string{
			"*** Begin Patch",
			"*** Update File: real/sample.txt",
			"@@",
			"-value",
			"+REAL",
			"*** Update File: alias/sample.txt",
			"@@",
			"-value",
			"+ALIAS",
			"*** End Patch",
			"",
		}, "\n"),
	})
	if exitCode == 0 {
		t.Fatal("exitCode = 0, want alias conflict")
	}
	if !strings.Contains(stderr, "conflicts with") {
		t.Fatalf("stderr = %q, want canonical alias conflict", stderr)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(content) != "value\n" {
		t.Fatalf("content = %q, want unchanged", content)
	}
}

func TestFilesystemRunnerApplyPatchDetectsSameMetadataContentChangeBeforeCommit(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "sample.txt")
	if err := os.WriteFile(path, []byte("alpha\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	const commitLoop = `        for entry in plan:
            if entry["kind"] == "delete":`
	injectedChange := `        for entry in plan:
            if entry["path"].endswith("sample.txt"):
                before = os.stat(entry["path"])
                with open(entry["path"], "r+b") as handle:
                    handle.write(b"racer\n")
                os.utime(entry["path"], ns=(before.st_atime_ns, before.st_mtime_ns))
            if entry["kind"] == "delete":`
	script := strings.Replace(filesystemRunnerScript, commitLoop, injectedChange, 1)
	if script == filesystemRunnerScript {
		t.Fatal("failed to inject concurrent content change")
	}

	_, stderr, exitCode := runFilesystemRunnerScript(t, script, "apply_patch", ApplyPatchRequest{
		Cwd: root,
		Patch: strings.Join([]string{
			"*** Begin Patch",
			"*** Update File: sample.txt",
			"@@",
			"-alpha",
			"+ALPHA",
			"*** End Patch",
			"",
		}, "\n"),
	})
	if exitCode == 0 {
		t.Fatal("exitCode = 0, want concurrent-change failure")
	}
	if !strings.Contains(stderr, "file changed during patch") {
		t.Fatalf("stderr = %q, want concurrent-change error", stderr)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(content) != "racer\n" {
		t.Fatalf("content = %q, want concurrent content preserved", content)
	}
}

func TestFilesystemRunnerApplyPatchPreservesConcurrentSourceReplacement(t *testing.T) {
	tests := []struct {
		name      string
		operation []string
	}{
		{
			name: "update",
			operation: []string{
				"*** Update File: sample.txt",
				"@@",
				"-original",
				"+updated",
			},
		},
		{
			name: "delete",
			operation: []string{
				"*** Delete File: sample.txt",
			},
		},
		{
			name: "move",
			operation: []string{
				"*** Update File: sample.txt",
				"*** Move to: moved.txt",
			},
		},
	}

	const preflightSource = `def preflight_patch_source(entry):
    info = require_patch_file(entry["path"], entry["path"])
    if patch_path_identity(entry["path"], info) != entry["identity"]:
        patch_error("could not apply patch: file changed during patch: %s" % entry["path"])`

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "sample.txt")
			if err := os.WriteFile(path, []byte("original\n"), 0o644); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}

			injectedReplacement := preflightSource + `
    entry["_test_preflight_count"] = entry.get("_test_preflight_count", 0) + 1
    if entry["_test_preflight_count"] == 3:
        replacement_path = entry["path"] + ".concurrent"
        with open(replacement_path, "w", encoding="utf-8") as handle:
            handle.write("concurrent\n")
        os.replace(replacement_path, entry["path"])`
			script := strings.Replace(filesystemRunnerScript, preflightSource, injectedReplacement, 1)
			if script == filesystemRunnerScript {
				t.Fatal("failed to inject concurrent source replacement")
			}

			patchLines := append([]string{"*** Begin Patch"}, tt.operation...)
			patchLines = append(patchLines, "*** End Patch", "")
			_, stderr, exitCode := runFilesystemRunnerScript(t, script, "apply_patch", ApplyPatchRequest{
				Cwd:   root,
				Patch: strings.Join(patchLines, "\n"),
			})
			if exitCode == 0 {
				t.Fatal("exitCode = 0, want concurrent-change failure")
			}
			if !strings.Contains(stderr, "file changed during patch") {
				t.Fatalf("stderr = %q, want concurrent-change error", stderr)
			}
			content, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile(source) error = %v", err)
			}
			if string(content) != "concurrent\n" {
				t.Fatalf("source = %q, want concurrent replacement preserved", content)
			}
			if _, err := os.Lstat(filepath.Join(root, "moved.txt")); !os.IsNotExist(err) {
				t.Fatalf("moved target Lstat error = %v, want not exist", err)
			}
		})
	}
}

func TestFilesystemRunnerApplyPatchRejectsConcurrentChmod(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "sample.txt")
	if err := os.WriteFile(path, []byte("original\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	const preflightSource = `def preflight_patch_source(entry):
    info = require_patch_file(entry["path"], entry["path"])
    if patch_path_identity(entry["path"], info) != entry["identity"]:
        patch_error("could not apply patch: file changed during patch: %s" % entry["path"])`
	injectedChmod := preflightSource + `
    entry["_test_preflight_count"] = entry.get("_test_preflight_count", 0) + 1
    if entry["_test_preflight_count"] == 3:
        os.chmod(entry["path"], 0o600)`
	script := strings.Replace(filesystemRunnerScript, preflightSource, injectedChmod, 1)
	if script == filesystemRunnerScript {
		t.Fatal("failed to inject concurrent chmod")
	}

	_, stderr, exitCode := runFilesystemRunnerScript(t, script, "apply_patch", ApplyPatchRequest{
		Cwd: root,
		Patch: strings.Join([]string{
			"*** Begin Patch",
			"*** Update File: sample.txt",
			"@@",
			"-original",
			"+updated",
			"*** End Patch",
			"",
		}, "\n"),
	})
	if exitCode == 0 {
		t.Fatal("exitCode = 0, want concurrent-change failure")
	}
	if !strings.Contains(stderr, "file changed during patch") {
		t.Fatalf("stderr = %q, want concurrent-change error", stderr)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(content) != "original\n" {
		t.Fatalf("content = %q, want unchanged", content)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %v, want concurrent 0600", got)
	}
}

func TestFilesystemRunnerApplyPatchRollbackFailurePreservesRecoveryBackup(t *testing.T) {
	root := t.TempDir()
	aPath := filepath.Join(root, "a.txt")
	bPath := filepath.Join(root, "b.txt")
	if err := os.WriteFile(aPath, []byte("alpha\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(a) error = %v", err)
	}
	if err := os.WriteFile(bPath, []byte("beta\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(b) error = %v", err)
	}

	const rollbackRestore = `def rollback_replace_backup(entry, path):
    backup_path = entry.get("backup_path")
    if not backup_path:
        return "rollback recovery backup is unavailable for %s" % path`
	injectedRollbackFailure := rollbackRestore + `
    if path.endswith("a.txt"):
        return "injected rollback restore failure; recovery backup preserved at %s" % backup_path`
	script := strings.Replace(filesystemRunnerScript, rollbackRestore, injectedRollbackFailure, 1)
	if script == filesystemRunnerScript {
		t.Fatal("failed to inject rollback restoration failure")
	}

	const commitDelete = `            if entry["kind"] == "delete":
                tracked_remove(entry["path"], entry)`
	injectedCommitFailure := `            if entry["kind"] == "delete":
                raise OSError("injected commit failure")`
	beforeCommitInjection := script
	script = strings.Replace(script, commitDelete, injectedCommitFailure, 1)
	if script == beforeCommitInjection {
		t.Fatal("failed to inject commit failure")
	}

	_, stderr, exitCode := runFilesystemRunnerScript(t, script, "apply_patch", ApplyPatchRequest{
		Cwd: root,
		Patch: strings.Join([]string{
			"*** Begin Patch",
			"*** Update File: a.txt",
			"@@",
			"-alpha",
			"+ALPHA",
			"*** Delete File: b.txt",
			"*** End Patch",
			"",
		}, "\n"),
	})
	if exitCode == 0 {
		t.Fatal("exitCode = 0, want rollback failure")
	}

	backups, err := filepath.Glob(filepath.Join(root, ".a.txt.*.bak"))
	if err != nil {
		t.Fatalf("Glob(backups) error = %v", err)
	}
	if len(backups) != 1 {
		t.Fatalf("backups = %v, want one preserved recovery backup; stderr=%q", backups, stderr)
	}
	if !strings.Contains(stderr, backups[0]) {
		t.Fatalf("stderr = %q, want preserved backup path %q", stderr, backups[0])
	}
	content, err := os.ReadFile(backups[0])
	if err != nil {
		t.Fatalf("ReadFile(backup) error = %v", err)
	}
	if string(content) != "alpha\n" {
		t.Fatalf("backup content = %q, want original", content)
	}
	tmpLinks, err := filepath.Glob(filepath.Join(root, ".a.txt.*.tmp"))
	if err != nil {
		t.Fatalf("Glob(tmp links) error = %v", err)
	}
	if len(tmpLinks) != 0 {
		t.Fatalf("obsolete patch temp hardlinks remain after rollback failure: %v", tmpLinks)
	}
}

func TestFilesystemRunnerApplyPatchRejectsSymlinkUpdate(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.txt")
	link := filepath.Join(root, "link.txt")
	if err := os.WriteFile(target, []byte("before\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}

	_, stderr, exitCode := runFilesystemRunner(t, "apply_patch", ApplyPatchRequest{
		Cwd: root,
		Patch: strings.Join([]string{
			"*** Begin Patch",
			"*** Update File: link.txt",
			"@@",
			"-before",
			"+after",
			"*** End Patch",
			"",
		}, "\n"),
	})
	if exitCode == 0 {
		t.Fatal("exitCode = 0, want symlink rejection")
	}
	if !strings.Contains(stderr, "symbolic link") {
		t.Fatalf("stderr = %q, want explicit symlink error", stderr)
	}
	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(content) != "before\n" {
		t.Fatalf("target content = %q, want unchanged", content)
	}
}

func TestClientFilesystemPathsResolveAgainstCurrentWorkdir(t *testing.T) {
	client := newVerifiedFilesystemClient(t)
	client.SetWorkdir("/workspaces/repo/after-cd")
	root := t.TempDir()
	viewInput := filepath.Join(root, "view.json")
	editInput := filepath.Join(root, "edit.json")
	createInput := filepath.Join(root, "create.json")

	var calls []fakeExecCall
	client.commandContext = fakeCommandContext(t, &calls, []fakeExecResponse{
		{stdout: `{"kind":"file","content":"1. before\n"}` + "\n", stdinPath: viewInput},
		{stdinPath: editInput},
		{stdinPath: createInput},
	})

	if _, err := client.View(context.Background(), ViewRequest{Path: "view.txt"}); err != nil {
		t.Fatalf("View() error = %v", err)
	}
	if err := client.EditFile(context.Background(), "edit.txt", "before", "after"); err != nil {
		t.Fatalf("EditFile() error = %v", err)
	}
	if err := client.CreateFile(context.Background(), "create.txt", "created\n"); err != nil {
		t.Fatalf("CreateFile() error = %v", err)
	}

	var viewPayload ViewRequest
	readJSONFile(t, viewInput, &viewPayload)
	if viewPayload.Path != "/workspaces/repo/after-cd/view.txt" {
		t.Fatalf("view path = %q", viewPayload.Path)
	}
	var editPayload map[string]string
	readJSONFile(t, editInput, &editPayload)
	if editPayload["path"] != "/workspaces/repo/after-cd/edit.txt" {
		t.Fatalf("edit path = %q", editPayload["path"])
	}
	var createPayload map[string]string
	readJSONFile(t, createInput, &createPayload)
	if createPayload["path"] != "/workspaces/repo/after-cd/create.txt" {
		t.Fatalf("create path = %q", createPayload["path"])
	}
}
