//go:build !darwin && !linux

package mcp

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/ekroon/gh-copilot-codespace/internal/ssh"
)

func openLocalCopySourceRooted(root, value string, hooks localCopyReadHooks) (*os.File, string, error) {
	path, err := resolveLocalCopySource(root, value)
	if err != nil {
		return nil, "", err
	}
	before, err := os.Lstat(path)
	if err != nil {
		return nil, "", fmt.Errorf("reading local source: %w", err)
	}
	if before.Mode()&os.ModeSymlink != 0 {
		return nil, "", fmt.Errorf("reading local source: %s is a symbolic link", path)
	}
	if !before.Mode().IsRegular() {
		return nil, "", fmt.Errorf("reading local source: %s is not a regular file", path)
	}
	if hooks.afterParentOpen != nil {
		if err := hooks.afterParentOpen(); err != nil {
			return nil, "", fmt.Errorf("reading local source: after opening parent: %w", err)
		}
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, "", fmt.Errorf("reading local source: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, "", fmt.Errorf("reading local source: %w", err)
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, "", fmt.Errorf("reading local source: %s is not a regular file", path)
	}
	if !os.SameFile(before, info) {
		_ = file.Close()
		return nil, "", fmt.Errorf("reading local source: %s changed while opening", path)
	}
	if info.Size() > ssh.MaxFileTransferBytes {
		_ = file.Close()
		return nil, "", fmt.Errorf("%w: local source has %d bytes, maximum is %d",
			ssh.ErrFileTransferTooLarge, info.Size(), ssh.MaxFileTransferBytes)
	}
	return file, path, nil
}

func inspectLocalCopyDestinationRooted(root, value string) (string, bool, error) {
	path, err := resolveLocalCopyDestination(root, value)
	if err != nil {
		return "", false, err
	}
	info, err := os.Lstat(path)
	switch {
	case os.IsNotExist(err):
		return path, false, nil
	case err != nil:
		return "", false, fmt.Errorf("checking local destination: %w", err)
	case info.Mode()&os.ModeSymlink != 0:
		return "", true, fmt.Errorf("local destination %q is a symbolic link", value)
	case !info.Mode().IsRegular():
		return "", true, fmt.Errorf("destination %s is not a regular file", path)
	default:
		return path, true, nil
	}
}

func writeLocalFileAtomicRootedWithHooks(
	ctx context.Context,
	root, value string,
	content []byte,
	overwrite bool,
	hooks localCopyWriteHooks,
) error {
	if len(content) > ssh.MaxFileTransferBytes {
		return fmt.Errorf("%w: local destination has %d bytes, maximum is %d",
			ssh.ErrFileTransferTooLarge, len(content), ssh.MaxFileTransferBytes)
	}
	path, err := resolveLocalCopyDestination(root, value)
	if err != nil {
		return err
	}
	if hooks.afterParentOpen != nil {
		if err := hooks.afterParentOpen(); err != nil {
			return fmt.Errorf("after opening local destination parent: %w", err)
		}
	}

	mode := os.FileMode(0o644)
	info, err := os.Lstat(path)
	switch {
	case err == nil:
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to overwrite symbolic link path %s", path)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("destination %s is not a regular file", path)
		}
		if !overwrite {
			return fmt.Errorf("destination %s already exists", path)
		}
		mode = info.Mode().Perm()
	case !os.IsNotExist(err):
		return err
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer func() {
		_ = temp.Close()
		_ = os.Remove(tempName)
	}()
	if err := writeLocalCopyContent(ctx, temp, content); err != nil {
		return err
	}
	if err := temp.Chmod(mode); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if overwrite {
		return os.Rename(tempName, path)
	}
	if err := os.Link(tempName, path); err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("destination %s already exists", path)
		}
		return err
	}
	return nil
}

func writeLocalCopyContent(ctx context.Context, writer io.Writer, content []byte) error {
	for len(content) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		chunk := content
		if len(chunk) > 64*1024 {
			chunk = chunk[:64*1024]
		}
		written, err := writer.Write(chunk)
		if err != nil {
			return err
		}
		if written != len(chunk) {
			return io.ErrShortWrite
		}
		content = content[written:]
	}
	return ctx.Err()
}
