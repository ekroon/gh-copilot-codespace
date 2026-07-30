package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ekroon/gh-copilot-codespace/internal/daemonproto"
	"github.com/ekroon/gh-copilot-codespace/internal/helperinfo"
	"github.com/ekroon/gh-copilot-codespace/internal/ssh"
)

const filesystemHelperProcessEnv = "GH_COPILOT_CODESPACE_FILESYSTEM_HELPER_PROCESS"

func filesystemHelperTestScript(t *testing.T, dir string) string {
	t.Helper()

	path := filepath.Join(dir, "filesystem-helper")
	script := `#!/bin/sh
exec "$` + filesystemHelperProcessEnv + `" -test.run '^TestFilesystemHelperProcess$' -- "$@"
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile(filesystem helper) error = %v", err)
	}
	t.Setenv(filesystemHelperProcessEnv, os.Args[0])
	return path
}

func TestFilesystemHelperProcess(t *testing.T) {
	if os.Getenv(filesystemHelperProcessEnv) == "" {
		return
	}
	separator := -1
	for i, arg := range os.Args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 || len(os.Args) != separator+3 || os.Args[separator+1] != "filesystem" {
		fmt.Fprintln(os.Stderr, "invalid filesystem helper test invocation")
		os.Exit(2)
	}
	if err := runFilesystemOperation(context.Background(), os.Args[separator+2], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func runFilesystemTestRequest[T any](t *testing.T, op string, request any) T {
	t.Helper()

	input, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("Marshal(%s) error = %v", op, err)
	}
	var output bytes.Buffer
	if err := runFilesystemOperation(context.Background(), op, bytes.NewReader(input), &output); err != nil {
		t.Fatalf("runFilesystemOperation(%s) error = %v", op, err)
	}

	var result T
	if output.Len() > 0 {
		if err := json.Unmarshal(output.Bytes(), &result); err != nil {
			t.Fatalf("Unmarshal(%s) error = %v; output=%q", op, err, output.String())
		}
	}
	return result
}

func noDaemonFilesystemClient(t *testing.T, workdir string) *ssh.Client {
	t.Helper()

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
	filesystemHelper := filesystemHelperTestScript(t, binDir)
	t.Setenv("PATH", binDir)

	client := ssh.NewClient("local-test")
	if err := client.SelectFilesystemHelper(filesystemHelper, helperinfo.Current()); err != nil {
		t.Fatalf("SelectFilesystemHelper() error = %v", err)
	}
	client.SetWorkdir(workdir)
	return client
}

func TestNoDaemonFilesystemHelperGrepDecodesOptions(t *testing.T) {
	t.Run("case insensitive", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "case.txt"), []byte("before\nNeedle\nafter\n"), 0o644); err != nil {
			t.Fatalf("WriteFile(case.txt) error = %v", err)
		}

		got, err := noDaemonFilesystemClient(t, root).GrepFiles(context.Background(), ssh.GrepRequest{
			Pattern:         "needle",
			Paths:           []string{"case.txt"},
			OutputMode:      ssh.GrepOutputModeContent,
			CaseInsensitive: true,
		})
		if err != nil {
			t.Fatalf("GrepFiles() error = %v", err)
		}
		if got.Output != "case.txt:2:Needle\n" {
			t.Fatalf("Output = %q, want case-insensitive match", got.Output)
		}
	})

	t.Run("contexts", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "context.txt"), []byte("zero\nbefore\nmatch\nafter\nafter2\ntail\n"), 0o644); err != nil {
			t.Fatalf("WriteFile(context.txt) error = %v", err)
		}
		client := noDaemonFilesystemClient(t, root)

		asymmetric, err := client.GrepFiles(context.Background(), ssh.GrepRequest{
			Pattern:       "match",
			Paths:         []string{"context.txt"},
			OutputMode:    ssh.GrepOutputModeContent,
			BeforeContext: 1,
			AfterContext:  2,
		})
		if err != nil {
			t.Fatalf("GrepFiles(asymmetric) error = %v", err)
		}
		if asymmetric.Output != "context.txt-2-before\ncontext.txt:3:match\ncontext.txt-4-after\ncontext.txt-5-after2\n" {
			t.Fatalf("asymmetric Output = %q", asymmetric.Output)
		}

		shared, err := client.GrepFiles(context.Background(), ssh.GrepRequest{
			Pattern:    "match",
			Paths:      []string{"context.txt"},
			OutputMode: ssh.GrepOutputModeContent,
			Context:    1,
		})
		if err != nil {
			t.Fatalf("GrepFiles(shared) error = %v", err)
		}
		if shared.Output != "context.txt-2-before\ncontext.txt:3:match\ncontext.txt-4-after\n" {
			t.Fatalf("shared Output = %q", shared.Output)
		}
	})

	t.Run("line numbers", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "lines.txt"), []byte("before\nmatch\nafter\n"), 0o644); err != nil {
			t.Fatalf("WriteFile(lines.txt) error = %v", err)
		}
		lineNumbers := false

		got, err := noDaemonFilesystemClient(t, root).GrepFiles(context.Background(), ssh.GrepRequest{
			Pattern:     "match",
			Paths:       []string{"lines.txt"},
			OutputMode:  ssh.GrepOutputModeContent,
			LineNumbers: &lineNumbers,
		})
		if err != nil {
			t.Fatalf("GrepFiles() error = %v", err)
		}
		if got.Output != "lines.txt:match\n" {
			t.Fatalf("Output = %q, want no line number", got.Output)
		}
	})

	t.Run("combined options", func(t *testing.T) {
		root := t.TempDir()
		for _, rel := range []string{"one/target.go", "two/target.go", "one/ignored.txt"} {
			path := filepath.Join(root, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatalf("MkdirAll(%q) error = %v", path, err)
			}
			if err := os.WriteFile(path, []byte("ALPHA\nBETA\n"), 0o644); err != nil {
				t.Fatalf("WriteFile(%q) error = %v", path, err)
			}
		}

		got, err := noDaemonFilesystemClient(t, root).GrepFiles(context.Background(), ssh.GrepRequest{
			Pattern:         "alpha\\nbeta",
			Paths:           []string{"one", "two"},
			Glob:            "target.*",
			OutputMode:      ssh.GrepOutputModeFilesWithMatches,
			Type:            "go",
			CaseInsensitive: true,
			HeadLimit:       1,
			Multiline:       true,
		})
		if err != nil {
			t.Fatalf("GrepFiles() error = %v", err)
		}
		if got.Output != "one/target.go\n" || !got.Truncated {
			t.Fatalf("GrepFiles() = %+v, want first matching Go file with truncation", got)
		}
	})
}

func TestFilesystemOperationWorksWithoutPython(t *testing.T) {
	root := t.TempDir()
	binDir := t.TempDir()
	t.Setenv("PATH", binDir)
	if _, err := exec.LookPath("python3"); err == nil {
		t.Fatal("python3 unexpectedly available in isolated PATH")
	}

	source := filepath.Join(root, "source.txt")
	runFilesystemTestRequest[struct{}](t, "create", daemonproto.CreateFileParams{
		Path:    source,
		Content: "alpha\n",
	})

	view := runFilesystemTestRequest[ssh.ViewResult](t, "view", ssh.ViewRequest{Path: source})
	if view.Kind != ssh.ViewKindFile || view.Content != "1. alpha\n" {
		t.Fatalf("view = %+v", view)
	}

	runFilesystemTestRequest[struct{}](t, "edit", daemonproto.EditFileParams{
		Path:   source,
		OldStr: "alpha",
		NewStr: "beta",
	})

	read := runFilesystemTestRequest[daemonproto.ReadFileResult](t, "read", ssh.RootedReadRequest{
		Path: source,
		Root: root,
	})
	destination := filepath.Join(root, "copy.txt")
	runFilesystemTestRequest[struct{}](t, "write", ssh.RootedWriteRequest{
		Path: destination,
		Root: root,
		Data: read.Data,
	})
	copied, err := os.ReadFile(destination)
	if err != nil {
		t.Fatalf("ReadFile(copy) error = %v", err)
	}
	if string(copied) != "beta\n" {
		t.Fatalf("copy content = %q, want beta", copied)
	}

	glob := runFilesystemTestRequest[ssh.GlobResult](t, "glob", ssh.GlobRequest{
		Pattern: "*.txt",
		Path:    root,
		Cwd:     root,
	})
	if !strings.Contains(glob.Output, source) || !strings.Contains(glob.Output, destination) {
		t.Fatalf("glob output = %q, want source and copy", glob.Output)
	}

	patch := runFilesystemTestRequest[ssh.ApplyPatchResult](t, "apply_patch", ssh.ApplyPatchRequest{
		Cwd: root,
		Patch: strings.Join([]string{
			"*** Begin Patch",
			"*** Add File: patched.txt",
			"+patched",
			"*** End Patch",
			"",
		}, "\n"),
	})
	if patch.FilesChanged != 1 {
		t.Fatalf("patch = %+v, want one changed file", patch)
	}
	patched, err := os.ReadFile(filepath.Join(root, "patched.txt"))
	if err != nil {
		t.Fatalf("ReadFile(patched) error = %v", err)
	}
	if string(patched) != "patched\n" {
		t.Fatalf("patched content = %q", patched)
	}
}

func TestFilesystemOperationFallbackGrepSkipsOversizedFiles(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PATH", t.TempDir())

	for _, multiline := range []bool{false, true} {
		t.Run(map[bool]string{false: "huge single line", true: "multiline"}[multiline], func(t *testing.T) {
			path := filepath.Join(root, map[bool]string{false: "single.txt", true: "multi.txt"}[multiline])
			file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				t.Fatalf("OpenFile() error = %v", err)
			}
			if _, err := file.WriteString("needle"); err != nil {
				_ = file.Close()
				t.Fatalf("WriteString() error = %v", err)
			}
			if err := file.Truncate(ssh.MaxGrepInputBytes + 1); err != nil {
				_ = file.Close()
				t.Fatalf("Truncate() error = %v", err)
			}
			if err := file.Close(); err != nil {
				t.Fatalf("Close() error = %v", err)
			}

			got := runFilesystemTestRequest[ssh.GrepResult](t, "grep", ssh.GrepRequest{
				Pattern:    "needle",
				Path:       path,
				Cwd:        root,
				OutputMode: ssh.GrepOutputModeContent,
				Multiline:  multiline,
			})
			if got.Output != "" || !got.Truncated {
				t.Fatalf("grep = %+v, want skipped and truncated", got)
			}
			if got.SkippedFiles != 1 || got.InputByteLimit != ssh.MaxGrepInputBytes {
				t.Fatalf("grep limits = %+v", got)
			}
		})
	}
}
