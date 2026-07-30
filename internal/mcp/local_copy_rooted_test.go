//go:build darwin || linux

package mcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadLocalCopySourcePinsParentAgainstSymlinkSwap(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	parent := filepath.Join(root, "parent")
	movedParent := filepath.Join(root, "moved-parent")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	if err := os.WriteFile(filepath.Join(parent, "source.txt"), []byte("inside"), 0o600); err != nil {
		t.Fatalf("write inside source: %v", err)
	}
	outsideSource := filepath.Join(outside, "source.txt")
	if err := os.WriteFile(outsideSource, []byte("outside"), 0o600); err != nil {
		t.Fatalf("write outside source: %v", err)
	}

	content, err := readLocalCopySourceRootedWithHooks(context.Background(), root, "parent/source.txt", localCopyReadHooks{
		afterParentOpen: func() error {
			if err := os.Rename(parent, movedParent); err != nil {
				return err
			}
			return os.Symlink(outside, parent)
		},
	})
	if err != nil {
		t.Fatalf("read rooted source: %v", err)
	}
	if got := string(content); got != "inside" {
		t.Fatalf("content = %q, want pinned inside source", got)
	}
	gotOutside, err := os.ReadFile(outsideSource)
	if err != nil {
		t.Fatalf("read outside source: %v", err)
	}
	if got := string(gotOutside); got != "outside" {
		t.Fatalf("outside source = %q, want untouched", got)
	}
}

func TestWriteLocalCopyDestinationPinsParentAgainstSymlinkSwap(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	parent := filepath.Join(root, "parent")
	movedParent := filepath.Join(root, "moved-parent")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	if err := os.WriteFile(filepath.Join(parent, "destination.txt"), []byte("old inside"), 0o600); err != nil {
		t.Fatalf("write inside destination: %v", err)
	}
	outsideDestination := filepath.Join(outside, "destination.txt")
	if err := os.WriteFile(outsideDestination, []byte("outside"), 0o600); err != nil {
		t.Fatalf("write outside destination: %v", err)
	}

	err := writeLocalFileAtomicRootedWithHooks(
		context.Background(),
		root,
		"parent/destination.txt",
		[]byte("new inside"),
		true,
		localCopyWriteHooks{
			afterParentOpen: func() error {
				if err := os.Rename(parent, movedParent); err != nil {
					return err
				}
				return os.Symlink(outside, parent)
			},
		},
	)
	if err != nil {
		t.Fatalf("write rooted destination: %v", err)
	}
	gotInside, err := os.ReadFile(filepath.Join(movedParent, "destination.txt"))
	if err != nil {
		t.Fatalf("read pinned destination: %v", err)
	}
	if got := string(gotInside); got != "new inside" {
		t.Fatalf("pinned destination = %q, want replacement", got)
	}
	gotOutside, err := os.ReadFile(outsideDestination)
	if err != nil {
		t.Fatalf("read outside destination: %v", err)
	}
	if got := string(gotOutside); got != "outside" {
		t.Fatalf("outside destination = %q, want untouched", got)
	}
}

func TestWriteLocalCopyOverwriteRejectsDestinationReplacementAfterTempCreated(t *testing.T) {
	root := t.TempDir()
	destination := filepath.Join(root, "destination.txt")
	replacement := filepath.Join(root, "replacement.txt")
	if err := os.WriteFile(destination, []byte("old"), 0o600); err != nil {
		t.Fatalf("write destination: %v", err)
	}
	if err := os.WriteFile(replacement, []byte("concurrent"), 0o600); err != nil {
		t.Fatalf("write replacement: %v", err)
	}

	err := writeLocalFileAtomicRootedWithHooks(
		context.Background(),
		root,
		"destination.txt",
		[]byte("copied"),
		true,
		localCopyWriteHooks{
			afterTempCreated: func() error {
				entries, err := os.ReadDir(root)
				if err != nil {
					return err
				}
				tempFound := false
				for _, entry := range entries {
					if strings.HasPrefix(entry.Name(), ".copilot-") && strings.HasSuffix(entry.Name(), ".tmp") {
						tempFound = true
						break
					}
				}
				if !tempFound {
					return os.ErrNotExist
				}
				return os.Rename(replacement, destination)
			},
		},
	)
	if err == nil || !strings.Contains(err.Error(), "changed while staging") {
		t.Fatalf("writeLocalFileAtomicRootedWithHooks() error = %v, want destination change rejection", err)
	}
	got, err := os.ReadFile(destination)
	if err != nil {
		t.Fatalf("read concurrent destination: %v", err)
	}
	if string(got) != "concurrent" {
		t.Fatalf("destination = %q, want concurrent content preserved", got)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read root: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".copilot-") && strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatalf("staged temp %q was not cleaned up", entry.Name())
		}
	}
}

func TestWriteLocalCopyOverwriteTransaction(t *testing.T) {
	assertNoCopyArtifacts := func(t *testing.T, root string) {
		t.Helper()
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatalf("read root: %v", err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".copilot-") {
				t.Fatalf("copy artifact %q was not cleaned up", entry.Name())
			}
		}
	}

	t.Run("rejects in-place same-inode content change", func(t *testing.T) {
		root := t.TempDir()
		destination := filepath.Join(root, "destination.txt")
		if err := os.WriteFile(destination, []byte("original"), 0o640); err != nil {
			t.Fatalf("write destination: %v", err)
		}
		before, err := os.Stat(destination)
		if err != nil {
			t.Fatalf("stat destination: %v", err)
		}

		err = writeLocalFileAtomicRootedWithHooks(
			context.Background(),
			root,
			"destination.txt",
			[]byte("staged"),
			true,
			localCopyWriteHooks{
				afterTempCreated: func() error {
					file, openErr := os.OpenFile(destination, os.O_WRONLY|os.O_TRUNC, 0)
					if openErr != nil {
						return openErr
					}
					if _, writeErr := file.Write([]byte("modified")); writeErr != nil {
						_ = file.Close()
						return writeErr
					}
					return file.Close()
				},
			},
		)
		if err == nil || !strings.Contains(err.Error(), "changed while staging") {
			t.Fatalf("writeLocalFileAtomicRootedWithHooks() error = %v, want in-place change rejection", err)
		}
		content, readErr := os.ReadFile(destination)
		if readErr != nil {
			t.Fatalf("read destination: %v", readErr)
		}
		if got := string(content); got != "modified" {
			t.Fatalf("destination content = %q, want modified", got)
		}
		after, statErr := os.Stat(destination)
		if statErr != nil {
			t.Fatalf("stat destination after change: %v", statErr)
		}
		if !os.SameFile(before, after) {
			t.Fatal("destination inode changed, want deterministic in-place mutation")
		}
		assertNoCopyArtifacts(t, root)
	})

	t.Run("rejects destination appearing before install", func(t *testing.T) {
		root := t.TempDir()
		destination := filepath.Join(root, "destination.txt")

		err := writeLocalFileAtomicRootedWithHooks(
			context.Background(),
			root,
			"destination.txt",
			[]byte("staged"),
			true,
			localCopyWriteHooks{
				beforeInstall: func() error {
					return os.WriteFile(destination, []byte("concurrent"), 0o600)
				},
			},
		)
		if err == nil || !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("writeLocalFileAtomicRootedWithHooks() error = %v, want destination conflict", err)
		}
		content, readErr := os.ReadFile(destination)
		if readErr != nil {
			t.Fatalf("read destination: %v", readErr)
		}
		if got := string(content); got != "concurrent" {
			t.Fatalf("destination content = %q, want concurrent", got)
		}
		assertNoCopyArtifacts(t, root)
	})

	t.Run("rejects replacement between staging and capture", func(t *testing.T) {
		root := t.TempDir()
		destination := filepath.Join(root, "destination.txt")
		replacement := filepath.Join(root, "replacement.txt")
		if err := os.WriteFile(destination, []byte("original"), 0o640); err != nil {
			t.Fatalf("write destination: %v", err)
		}
		if err := os.WriteFile(replacement, []byte("concurrent"), 0o600); err != nil {
			t.Fatalf("write replacement: %v", err)
		}

		err := writeLocalFileAtomicRootedWithHooks(
			context.Background(),
			root,
			"destination.txt",
			[]byte("staged"),
			true,
			localCopyWriteHooks{
				afterTempCreated: func() error {
					return os.Rename(replacement, destination)
				},
			},
		)
		if err == nil || !strings.Contains(err.Error(), "changed while staging") {
			t.Fatalf("writeLocalFileAtomicRootedWithHooks() error = %v, want replacement rejection", err)
		}
		content, readErr := os.ReadFile(destination)
		if readErr != nil {
			t.Fatalf("read destination: %v", readErr)
		}
		if got := string(content); got != "concurrent" {
			t.Fatalf("destination content = %q, want concurrent", got)
		}
		assertNoCopyArtifacts(t, root)
	})

	t.Run("preserves replacement and recovery after capture", func(t *testing.T) {
		root := t.TempDir()
		destination := filepath.Join(root, "destination.txt")
		if err := os.WriteFile(destination, []byte("original"), 0o640); err != nil {
			t.Fatalf("write destination: %v", err)
		}

		err := writeLocalFileAtomicRootedWithHooks(
			context.Background(),
			root,
			"destination.txt",
			[]byte("staged"),
			true,
			localCopyWriteHooks{
				beforeInstall: func() error {
					return os.WriteFile(destination, []byte("concurrent"), 0o600)
				},
			},
		)
		if err == nil || !strings.Contains(err.Error(), "recovery preserved at") {
			t.Fatalf("writeLocalFileAtomicRootedWithHooks() error = %v, want preserved recovery error", err)
		}
		content, readErr := os.ReadFile(destination)
		if readErr != nil {
			t.Fatalf("read destination: %v", readErr)
		}
		if got := string(content); got != "concurrent" {
			t.Fatalf("destination content = %q, want concurrent", got)
		}
		recoveries, globErr := filepath.Glob(filepath.Join(root, ".copilot-*.recover"))
		if globErr != nil {
			t.Fatalf("glob recovery: %v", globErr)
		}
		if len(recoveries) != 1 {
			t.Fatalf("recovery files = %v, want one", recoveries)
		}
		recovery, readErr := os.ReadFile(recoveries[0])
		if readErr != nil {
			t.Fatalf("read recovery: %v", readErr)
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
		destination := filepath.Join(root, "destination.txt")
		if err := os.WriteFile(destination, []byte("original"), 0o640); err != nil {
			t.Fatalf("write destination: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())

		err := writeLocalFileAtomicRootedWithHooks(
			ctx,
			root,
			"destination.txt",
			[]byte("staged"),
			true,
			localCopyWriteHooks{
				beforeInstall: func() error {
					cancel()
					return nil
				},
			},
		)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("writeLocalFileAtomicRootedWithHooks() error = %v, want cancellation", err)
		}
		content, readErr := os.ReadFile(destination)
		if readErr != nil {
			t.Fatalf("read destination: %v", readErr)
		}
		if got := string(content); got != "original" {
			t.Fatalf("destination content = %q, want original", got)
		}
		assertNoCopyArtifacts(t, root)
	})

	t.Run("commits validated overwrite", func(t *testing.T) {
		root := t.TempDir()
		destination := filepath.Join(root, "destination.txt")
		if err := os.WriteFile(destination, []byte("original"), 0o600); err != nil {
			t.Fatalf("write destination: %v", err)
		}

		if err := writeLocalFileAtomicRootedWithHooks(
			context.Background(),
			root,
			"destination.txt",
			[]byte("staged"),
			true,
			localCopyWriteHooks{
				beforeInstall: func() error {
					for check := 0; check < 100; check++ {
						if _, err := os.Lstat(destination); err != nil {
							return err
						}
					}
					return nil
				},
				afterInstall: func() error {
					for check := 0; check < 100; check++ {
						if _, err := os.Lstat(destination); err != nil {
							return err
						}
					}
					return nil
				},
			},
		); err != nil {
			t.Fatalf("writeLocalFileAtomicRootedWithHooks() error = %v", err)
		}
		content, err := os.ReadFile(destination)
		if err != nil {
			t.Fatalf("read destination: %v", err)
		}
		if got := string(content); got != "staged" {
			t.Fatalf("destination content = %q, want staged", got)
		}
		info, err := os.Stat(destination)
		if err != nil {
			t.Fatalf("stat destination: %v", err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("destination mode = %04o, want 0600", got)
		}
		assertNoCopyArtifacts(t, root)
	})

	t.Run("preserves concurrent replacement after atomic install", func(t *testing.T) {
		root := t.TempDir()
		destination := filepath.Join(root, "destination.txt")
		if err := os.WriteFile(destination, []byte("original"), 0o640); err != nil {
			t.Fatalf("write destination: %v", err)
		}

		err := writeLocalFileAtomicRootedWithHooks(
			context.Background(),
			root,
			"destination.txt",
			[]byte("staged"),
			true,
			localCopyWriteHooks{
				afterInstall: func() error {
					replacement := filepath.Join(root, "replacement.txt")
					if err := os.WriteFile(replacement, []byte("concurrent"), 0o600); err != nil {
						return err
					}
					return os.Rename(replacement, destination)
				},
			},
		)
		if err == nil || !strings.Contains(err.Error(), "recovery preserved at") {
			t.Fatalf("writeLocalFileAtomicRootedWithHooks() error = %v, want atomic replacement conflict", err)
		}
		content, readErr := os.ReadFile(destination)
		if readErr != nil {
			t.Fatalf("read destination: %v", readErr)
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

func TestLocalCopyRejectsSymlinkAndNonDirectoryParents(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatalf("create parent symlink: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write parent file: %v", err)
	}

	for _, path := range []string{"link/source.txt", "file/source.txt"} {
		t.Run(path, func(t *testing.T) {
			_, _, err := openLocalCopySourceRooted(root, path, localCopyReadHooks{})
			if err == nil || !strings.Contains(err.Error(), "symbolic link or non-directory parent") {
				t.Fatalf("openLocalCopySourceRooted() error = %v, want parent rejection", err)
			}
			err = writeLocalFileAtomicRootedWithHooks(
				context.Background(),
				root,
				path,
				[]byte("content"),
				false,
				localCopyWriteHooks{},
			)
			if err == nil || !strings.Contains(err.Error(), "symbolic link or non-directory parent") {
				t.Fatalf("writeLocalFileAtomicRootedWithHooks() error = %v, want parent rejection", err)
			}
		})
	}
}
