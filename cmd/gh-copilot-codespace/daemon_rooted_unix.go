package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ekroon/gh-copilot-codespace/internal/daemonproto"
	"golang.org/x/sys/unix"
)

type daemonRootedParent struct {
	dir  *os.File
	name string
}

type daemonRootedDestinationState struct {
	exists bool
	device uint64
	inode  uint64
	mode   uint32
	size   int64
	digest [sha256.Size]byte
}

func daemonOpenRootedParent(path, root, operation string, create bool) (*daemonRootedParent, error) {
	cleanRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("%s file: resolve workdir: %w", operation, err)
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(cleanRoot, path)
	}
	cleanPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("%s file: resolve path: %w", operation, err)
	}
	rel, err := filepath.Rel(cleanRoot, cleanPath)
	if err != nil {
		return nil, fmt.Errorf("%s file: resolve path: %w", operation, err)
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return nil, fmt.Errorf("%s file: path %s escapes workdir %s", operation, path, root)
	}

	parts := strings.Split(rel, string(filepath.Separator))
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, fmt.Errorf("%s file: invalid path %s", operation, path)
		}
	}

	fd, err := unix.Open(cleanRoot, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
			return nil, fmt.Errorf("%s file: workdir %s is a symbolic link or not a directory", operation, root)
		}
		return nil, fmt.Errorf("%s file: open workdir: %w", operation, err)
	}
	current := os.NewFile(uintptr(fd), cleanRoot)
	if current == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("%s file: open workdir", operation)
	}

	for _, part := range parts[:len(parts)-1] {
		nextFD, openErr := unix.Openat(int(current.Fd()), part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if openErr != nil && create && errors.Is(openErr, unix.ENOENT) {
			if mkdirErr := unix.Mkdirat(int(current.Fd()), part, 0o755); mkdirErr != nil && !errors.Is(mkdirErr, unix.EEXIST) {
				_ = current.Close()
				return nil, fmt.Errorf("%s file: create parent %s: %w", operation, part, mkdirErr)
			}
			nextFD, openErr = unix.Openat(int(current.Fd()), part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		}
		if openErr != nil {
			_ = current.Close()
			if errors.Is(openErr, unix.ELOOP) || errors.Is(openErr, unix.ENOTDIR) {
				return nil, fmt.Errorf("%s file: path %s escapes workdir %s through symbolic link", operation, path, root)
			}
			return nil, fmt.Errorf("%s file: open parent %s: %w", operation, part, openErr)
		}
		next := os.NewFile(uintptr(nextFD), part)
		if next == nil {
			_ = unix.Close(nextFD)
			_ = current.Close()
			return nil, fmt.Errorf("%s file: open parent %s", operation, part)
		}
		_ = current.Close()
		current = next
	}

	return &daemonRootedParent{dir: current, name: parts[len(parts)-1]}, nil
}

func daemonInspectRootedDestination(ctx context.Context, parent *daemonRootedParent, name, path string) (daemonRootedDestinationState, error) {
	fd, err := unix.Openat(int(parent.dir.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ENOENT) {
		return daemonRootedDestinationState{}, nil
	}
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return daemonRootedDestinationState{}, fmt.Errorf("write file: refusing to overwrite symbolic link path %s", path)
		}
		return daemonRootedDestinationState{}, fmt.Errorf("write file: inspect destination: %w", err)
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		_ = unix.Close(fd)
		return daemonRootedDestinationState{}, fmt.Errorf("write file: inspect destination")
	}
	defer file.Close()

	var before unix.Stat_t
	if err := unix.Fstat(fd, &before); err != nil {
		return daemonRootedDestinationState{}, fmt.Errorf("write file: inspect destination: %w", err)
	}
	if before.Mode&unix.S_IFMT != unix.S_IFREG {
		return daemonRootedDestinationState{}, fmt.Errorf("write file: destination %s is not a regular file", path)
	}

	digest := sha256.New()
	buffer := make([]byte, 64*1024)
	for {
		if err := ctx.Err(); err != nil {
			return daemonRootedDestinationState{}, errDaemonCanceled
		}
		n, readErr := file.Read(buffer)
		if n > 0 {
			_, _ = digest.Write(buffer[:n])
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return daemonRootedDestinationState{}, fmt.Errorf("write file: inspect destination content: %w", readErr)
		}
	}

	var after unix.Stat_t
	if err := unix.Fstat(fd, &after); err != nil {
		return daemonRootedDestinationState{}, fmt.Errorf("write file: inspect destination: %w", err)
	}
	if before.Dev != after.Dev || before.Ino != after.Ino || before.Mode != after.Mode || before.Size != after.Size {
		return daemonRootedDestinationState{}, fmt.Errorf("write file: destination %s changed during write", path)
	}
	state := daemonRootedDestinationState{
		exists: true,
		device: uint64(after.Dev),
		inode:  uint64(after.Ino),
		mode:   uint32(after.Mode),
		size:   after.Size,
	}
	copy(state.digest[:], digest.Sum(nil))
	return state, nil
}

func daemonCreateRootedRecovery(
	ctx context.Context,
	parent *daemonRootedParent,
	path string,
	expected daemonRootedDestinationState,
) (string, error) {
	name, recovery, err := daemonCreateRootedNamedTemp(parent.dir, os.FileMode(expected.mode&0o777), ".recover")
	if err != nil {
		return "", fmt.Errorf("write file: create recovery snapshot: %w", err)
	}
	cleanup := true
	defer func() {
		_ = recovery.Close()
		if cleanup {
			_ = unix.Unlinkat(int(parent.dir.Fd()), name, 0)
		}
	}()

	fd, err := unix.Openat(int(parent.dir.Fd()), parent.name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
	if err != nil {
		return "", fmt.Errorf("write file: destination %s changed during write: %w", path, err)
	}
	source := os.NewFile(uintptr(fd), parent.name)
	if source == nil {
		_ = unix.Close(fd)
		return "", fmt.Errorf("write file: create recovery snapshot")
	}
	defer source.Close()

	var before unix.Stat_t
	if err := unix.Fstat(fd, &before); err != nil {
		return "", fmt.Errorf("write file: inspect destination: %w", err)
	}
	if before.Mode&unix.S_IFMT != unix.S_IFREG {
		return "", fmt.Errorf("write file: destination %s is not a regular file", path)
	}

	digest := sha256.New()
	buffer := make([]byte, 64*1024)
	for {
		if err := ctx.Err(); err != nil {
			return "", errDaemonCanceled
		}
		n, readErr := source.Read(buffer)
		if n > 0 {
			if _, err := recovery.Write(buffer[:n]); err != nil {
				return "", fmt.Errorf("write file: create recovery snapshot: %w", err)
			}
			_, _ = digest.Write(buffer[:n])
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return "", fmt.Errorf("write file: create recovery snapshot: %w", readErr)
		}
	}

	var after unix.Stat_t
	if err := unix.Fstat(fd, &after); err != nil {
		return "", fmt.Errorf("write file: inspect destination: %w", err)
	}
	actual := daemonRootedDestinationState{
		exists: true,
		device: uint64(after.Dev),
		inode:  uint64(after.Ino),
		mode:   uint32(after.Mode),
		size:   after.Size,
	}
	copy(actual.digest[:], digest.Sum(nil))
	if before.Dev != after.Dev || before.Ino != after.Ino || before.Mode != after.Mode || before.Size != after.Size || actual != expected {
		return "", fmt.Errorf("write file: destination %s changed during write", path)
	}
	if err := recovery.Chmod(os.FileMode(expected.mode & 0o777)); err != nil {
		return "", fmt.Errorf("write file: create recovery snapshot: %w", err)
	}
	if err := recovery.Close(); err != nil {
		return "", fmt.Errorf("write file: create recovery snapshot: %w", err)
	}
	cleanup = false
	return name, nil
}

func daemonReadRootedFile(ctx context.Context, path, root string, hooks daemonRootedFileHooks) ([]byte, error) {
	parent, err := daemonOpenRootedParent(path, root, "read", false)
	if err != nil {
		return nil, err
	}
	defer parent.dir.Close()
	if hooks.afterParentOpen != nil {
		if err := hooks.afterParentOpen(); err != nil {
			return nil, fmt.Errorf("read file: after opening parent: %w", err)
		}
	}

	fd, err := unix.Openat(int(parent.dir.Fd()), parent.name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, fmt.Errorf("read file: refusing symbolic link path %s", path)
		}
		return nil, fmt.Errorf("read file: %w", err)
	}
	file := os.NewFile(uintptr(fd), parent.name)
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("read file: open %s", path)
	}
	defer file.Close()
	return daemonReadBoundedFile(ctx, file, hooks)
}

func daemonReadBoundedFile(ctx context.Context, file *os.File, hooks daemonRootedFileHooks) ([]byte, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("read file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("read file: source %s is not a regular file", file.Name())
	}
	if info.Size() > daemonproto.MaxFileTransferBytes {
		return nil, fmt.Errorf("%w: %d bytes exceeds %d", daemonproto.ErrFileTransferTooLarge, info.Size(), daemonproto.MaxFileTransferBytes)
	}
	if err := ctx.Err(); err != nil {
		return nil, errDaemonCanceled
	}

	reader := &daemonRootedReader{ctx: ctx, src: file, hooks: hooks}
	data, err := io.ReadAll(io.LimitReader(reader, daemonproto.MaxFileTransferBytes+1))
	if err != nil {
		if errors.Is(err, errDaemonCanceled) {
			return nil, errDaemonCanceled
		}
		return nil, fmt.Errorf("read file: %w", err)
	}
	if len(data) > daemonproto.MaxFileTransferBytes {
		return nil, fmt.Errorf("%w: file grew beyond %d bytes", daemonproto.ErrFileTransferTooLarge, daemonproto.MaxFileTransferBytes)
	}
	return data, nil
}

type daemonRootedReader struct {
	ctx   context.Context
	src   io.Reader
	hooks daemonRootedFileHooks
}

func (r *daemonRootedReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, errDaemonCanceled
	}
	n, err := r.src.Read(p)
	if n > 0 && r.hooks.afterRead != nil {
		r.hooks.afterRead(n)
	}
	if r.ctx.Err() != nil {
		return n, errDaemonCanceled
	}
	return n, err
}

func daemonWriteRootedFile(ctx context.Context, path string, content []byte, overwrite bool, root string, hooks daemonRootedFileHooks) error {
	if len(content) > daemonproto.MaxFileTransferBytes {
		return fmt.Errorf("%w: %d bytes exceeds %d", daemonproto.ErrFileTransferTooLarge, len(content), daemonproto.MaxFileTransferBytes)
	}
	parent, err := daemonOpenRootedParent(path, root, "write", true)
	if err != nil {
		return err
	}
	defer parent.dir.Close()
	if hooks.afterParentOpen != nil {
		if err := hooks.afterParentOpen(); err != nil {
			return fmt.Errorf("write file: after opening parent: %w", err)
		}
	}

	mode := os.FileMode(0o644)
	destination, err := daemonInspectRootedDestination(ctx, parent, parent.name, path)
	if err != nil {
		return err
	}
	if destination.exists {
		if !overwrite {
			return fmt.Errorf("write file: %s already exists", path)
		}
		mode = os.FileMode(destination.mode & 0o777)
	}

	tempName, temp, err := daemonCreateRootedTemp(parent.dir, mode)
	if err != nil {
		return fmt.Errorf("write file: %w", err)
	}
	committed := false
	defer func() {
		_ = temp.Close()
		if !committed {
			_ = unix.Unlinkat(int(parent.dir.Fd()), tempName, 0)
		}
	}()
	if err := daemonWriteContext(ctx, temp, content); err != nil {
		return fmt.Errorf("write file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("write file: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return errDaemonCanceled
	}
	if hooks.beforeCommit != nil {
		if err := hooks.beforeCommit(); err != nil {
			return fmt.Errorf("write file: before commit: %w", err)
		}
	}

	recoveryName := ""
	preserveRecovery := false
	defer func() {
		if recoveryName != "" && !preserveRecovery {
			_ = unix.Unlinkat(int(parent.dir.Fd()), recoveryName, 0)
		}
	}()
	if destination.exists {
		recoveryName, err = daemonCreateRootedRecovery(ctx, parent, path, destination)
		if err != nil {
			return err
		}
	}

	if err := ctx.Err(); err != nil {
		return errDaemonCanceled
	}
	if hooks.beforeInstall != nil {
		if err := hooks.beforeInstall(); err != nil {
			return fmt.Errorf("write file: before install: %w", err)
		}
	}
	if err := ctx.Err(); err != nil {
		return errDaemonCanceled
	}

	if !destination.exists {
		err = daemonRenameAtNoReplace(int(parent.dir.Fd()), tempName, parent.name)
		if err != nil {
			if errors.Is(err, unix.EEXIST) {
				return fmt.Errorf("write file: %s already exists", path)
			}
			return fmt.Errorf("write file: commit: %w", err)
		}
		committed = true
		return nil
	}

	current, err := daemonInspectRootedDestination(ctx, parent, parent.name, path)
	if err != nil || current != destination {
		preserveRecovery = true
		recoveryPath := filepath.Join(filepath.Dir(path), recoveryName)
		if err != nil {
			return fmt.Errorf("write file: destination %s changed during write: %v; recovery preserved at %s", path, err, recoveryPath)
		}
		return fmt.Errorf("write file: destination %s changed during write; recovery preserved at %s", path, recoveryPath)
	}
	staged, err := daemonInspectRootedDestination(ctx, parent, tempName, path)
	if err != nil {
		return fmt.Errorf("write file: inspect staged content: %w", err)
	}
	if err := daemonRenameAtExchange(int(parent.dir.Fd()), tempName, parent.name); err != nil {
		return fmt.Errorf("write file: commit: %w", err)
	}

	var afterInstallErr error
	if hooks.afterInstall != nil {
		afterInstallErr = hooks.afterInstall()
	}
	displaced, inspectErr := daemonInspectRootedDestination(ctx, parent, tempName, path)
	live, liveErr := daemonInspectRootedDestination(ctx, parent, parent.name, path)
	if afterInstallErr != nil || inspectErr != nil || liveErr != nil || displaced != destination || live != staged {
		preserveRecovery = true
		recoveryPath := filepath.Join(filepath.Dir(path), recoveryName)
		if liveErr == nil && live == staged {
			if swapErr := daemonRenameAtExchange(int(parent.dir.Fd()), tempName, parent.name); swapErr == nil {
				if afterInstallErr != nil && inspectErr == nil && displaced == destination {
					preserveRecovery = false
					return fmt.Errorf("write file: after install: %w", afterInstallErr)
				}
				if inspectErr != nil {
					return fmt.Errorf("write file: destination %s changed during atomic replacement: %v; recovery preserved at %s", path, inspectErr, recoveryPath)
				}
				return fmt.Errorf("write file: destination %s changed during atomic replacement; recovery preserved at %s", path, recoveryPath)
			}
		}
		committed = true
		displacedPath := filepath.Join(filepath.Dir(path), tempName)
		return fmt.Errorf(
			"write file: destination %s changed during atomic replacement; recovery preserved at %s and displaced path preserved at %s",
			path,
			recoveryPath,
			displacedPath,
		)
	}

	committed = true
	if err := unix.Unlinkat(int(parent.dir.Fd()), tempName, 0); err != nil {
		preserveRecovery = true
		recoveryPath := filepath.Join(filepath.Dir(path), recoveryName)
		displacedPath := filepath.Join(filepath.Dir(path), tempName)
		return fmt.Errorf("write file: installed staged content but displaced destination cleanup failed; recovery preserved at %s and displaced path preserved at %s: %w", recoveryPath, displacedPath, err)
	}
	if err := unix.Unlinkat(int(parent.dir.Fd()), recoveryName, 0); err != nil {
		preserveRecovery = true
		recoveryPath := filepath.Join(filepath.Dir(path), recoveryName)
		return fmt.Errorf("write file: installed staged content but recovery cleanup failed; recovery preserved at %s: %w", recoveryPath, err)
	}
	recoveryName = ""
	return nil
}

func daemonCreateRootedTemp(parent *os.File, mode os.FileMode) (string, *os.File, error) {
	return daemonCreateRootedNamedTemp(parent, mode, ".tmp")
}

func daemonCreateRootedNamedTemp(parent *os.File, mode os.FileMode, suffix string) (string, *os.File, error) {
	for attempt := 0; attempt < 100; attempt++ {
		name, err := daemonRandomRootedName(suffix)
		if err != nil {
			return "", nil, err
		}
		fd, err := unix.Openat(int(parent.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, uint32(mode.Perm()))
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return "", nil, err
		}
		if err := unix.Fchmod(fd, uint32(mode.Perm())); err != nil {
			_ = unix.Close(fd)
			_ = unix.Unlinkat(int(parent.Fd()), name, 0)
			return "", nil, fmt.Errorf("set staged file permissions: %w", err)
		}
		file := os.NewFile(uintptr(fd), name)
		if file == nil {
			_ = unix.Close(fd)
			_ = unix.Unlinkat(int(parent.Fd()), name, 0)
			return "", nil, errors.New("open staged file")
		}
		return name, file, nil
	}
	return "", nil, errors.New("could not reserve staged file")
}

func daemonRandomRootedName(suffix string) (string, error) {
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return ".copilot-" + hex.EncodeToString(random[:]) + suffix, nil
}

func daemonWriteContext(ctx context.Context, writer io.Writer, content []byte) error {
	for len(content) > 0 {
		if err := ctx.Err(); err != nil {
			return errDaemonCanceled
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
	return nil
}
