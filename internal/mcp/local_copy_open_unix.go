//go:build darwin || linux

package mcp

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

	"github.com/ekroon/gh-copilot-codespace/internal/ssh"
	"golang.org/x/sys/unix"
)

type localCopyRootedParent struct {
	dir     *os.File
	name    string
	display string
}

type localCopyDestinationState struct {
	exists bool
	device uint64
	inode  uint64
	mode   uint32
	size   int64
	digest [sha256.Size]byte
}

func (s localCopyDestinationState) permissions() os.FileMode {
	if !s.exists {
		return 0o644
	}
	return os.FileMode(s.mode & 0o777)
}

func openLocalCopyRootedParent(root, value string, create bool) (*localCopyRootedParent, bool, error) {
	display, err := localCopyCandidate(root, value)
	if err != nil {
		return nil, false, err
	}
	rel, err := filepath.Rel(root, display)
	if err != nil {
		return nil, false, fmt.Errorf("resolving local path: %w", err)
	}
	parts := strings.Split(rel, string(filepath.Separator))
	for _, part := range parts {
		if part == "" || part == ".." {
			return nil, false, fmt.Errorf("local path %q is invalid", value)
		}
	}

	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, false, fmt.Errorf("opening local workdir %q: %w", root, err)
	}
	current := os.NewFile(uintptr(fd), root)
	if current == nil {
		_ = unix.Close(fd)
		return nil, false, fmt.Errorf("opening local workdir %q", root)
	}

	for _, part := range parts[:len(parts)-1] {
		nextFD, openErr := unix.Openat(int(current.Fd()), part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if openErr != nil && create && errors.Is(openErr, unix.ENOENT) {
			if mkdirErr := unix.Mkdirat(int(current.Fd()), part, 0o755); mkdirErr != nil && !errors.Is(mkdirErr, unix.EEXIST) {
				_ = current.Close()
				return nil, false, fmt.Errorf("creating local destination parent %q: %w", part, mkdirErr)
			}
			nextFD, openErr = unix.Openat(int(current.Fd()), part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		}
		if openErr != nil {
			_ = current.Close()
			if !create && errors.Is(openErr, unix.ENOENT) {
				return nil, true, nil
			}
			if errors.Is(openErr, unix.ELOOP) || errors.Is(openErr, unix.ENOTDIR) {
				return nil, false, fmt.Errorf("local path %q escapes local workdir %q through a symbolic link or non-directory parent", value, root)
			}
			return nil, false, fmt.Errorf("opening local path parent %q: %w", part, openErr)
		}
		next := os.NewFile(uintptr(nextFD), part)
		if next == nil {
			_ = unix.Close(nextFD)
			_ = current.Close()
			return nil, false, fmt.Errorf("opening local path parent %q", part)
		}
		_ = current.Close()
		current = next
	}

	return &localCopyRootedParent{
		dir:     current,
		name:    parts[len(parts)-1],
		display: display,
	}, false, nil
}

func openLocalCopySourceRooted(root, value string, hooks localCopyReadHooks) (*os.File, string, error) {
	parent, missing, err := openLocalCopyRootedParent(root, value, false)
	if err != nil {
		return nil, "", fmt.Errorf("reading local source: %w", err)
	}
	if missing {
		return nil, "", fmt.Errorf("reading local source: %s does not exist", filepath.Join(root, value))
	}
	defer parent.dir.Close()
	if hooks.afterParentOpen != nil {
		if err := hooks.afterParentOpen(); err != nil {
			return nil, "", fmt.Errorf("reading local source: after opening parent: %w", err)
		}
	}

	fd, err := unix.Openat(int(parent.dir.Fd()), parent.name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, "", fmt.Errorf("reading local source: %s is a symbolic link", parent.display)
		}
		return nil, "", fmt.Errorf("reading local source: %w", err)
	}
	file := os.NewFile(uintptr(fd), parent.display)
	if file == nil {
		_ = unix.Close(fd)
		return nil, "", fmt.Errorf("reading local source: opening %s", parent.display)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, "", fmt.Errorf("reading local source: %w", err)
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, "", fmt.Errorf("reading local source: %s is not a regular file", parent.display)
	}
	if info.Size() > ssh.MaxFileTransferBytes {
		_ = file.Close()
		return nil, "", fmt.Errorf("%w: local source has %d bytes, maximum is %d",
			ssh.ErrFileTransferTooLarge, info.Size(), ssh.MaxFileTransferBytes)
	}
	return file, parent.display, nil
}

func inspectLocalCopyDestinationRooted(root, value string) (string, bool, error) {
	parent, missing, err := openLocalCopyRootedParent(root, value, false)
	if err != nil {
		return "", false, err
	}
	if missing {
		display, candidateErr := localCopyCandidate(root, value)
		return display, false, candidateErr
	}
	defer parent.dir.Close()
	state, err := inspectLocalCopyDestinationAt(parent)
	return parent.display, state.exists, err
}

func inspectLocalCopyDestinationAt(parent *localCopyRootedParent) (localCopyDestinationState, error) {
	var stat unix.Stat_t
	err := unix.Fstatat(int(parent.dir.Fd()), parent.name, &stat, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		return localCopyDestinationState{}, nil
	}
	if err != nil {
		return localCopyDestinationState{}, fmt.Errorf("checking local destination %q: %w", parent.display, err)
	}
	switch stat.Mode & unix.S_IFMT {
	case unix.S_IFLNK:
		return localCopyDestinationState{}, fmt.Errorf("local destination %q is a symbolic link", parent.display)
	case unix.S_IFREG:
		return localCopyDestinationState{
			exists: true,
			device: uint64(stat.Dev),
			inode:  uint64(stat.Ino),
			mode:   uint32(stat.Mode),
		}, nil
	default:
		return localCopyDestinationState{}, fmt.Errorf("destination %s is not a regular file", parent.display)
	}
}

func snapshotLocalCopyDestinationAt(
	ctx context.Context,
	parent *localCopyRootedParent,
	name string,
) (localCopyDestinationState, error) {
	fd, err := unix.Openat(int(parent.dir.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ENOENT) {
		return localCopyDestinationState{}, nil
	}
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return localCopyDestinationState{}, fmt.Errorf("local destination %q is a symbolic link", parent.display)
		}
		return localCopyDestinationState{}, fmt.Errorf("checking local destination %q: %w", parent.display, err)
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		_ = unix.Close(fd)
		return localCopyDestinationState{}, fmt.Errorf("checking local destination %q", parent.display)
	}
	defer file.Close()

	var before unix.Stat_t
	if err := unix.Fstat(fd, &before); err != nil {
		return localCopyDestinationState{}, fmt.Errorf("checking local destination %q: %w", parent.display, err)
	}
	if before.Mode&unix.S_IFMT != unix.S_IFREG {
		return localCopyDestinationState{}, fmt.Errorf("destination %s is not a regular file", parent.display)
	}

	digest := sha256.New()
	buffer := make([]byte, 64*1024)
	for {
		if err := ctx.Err(); err != nil {
			return localCopyDestinationState{}, err
		}
		n, readErr := file.Read(buffer)
		if n > 0 {
			_, _ = digest.Write(buffer[:n])
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return localCopyDestinationState{}, fmt.Errorf("reading local destination %q: %w", parent.display, readErr)
		}
	}

	var after unix.Stat_t
	if err := unix.Fstat(fd, &after); err != nil {
		return localCopyDestinationState{}, fmt.Errorf("checking local destination %q: %w", parent.display, err)
	}
	if before.Dev != after.Dev || before.Ino != after.Ino || before.Mode != after.Mode || before.Size != after.Size {
		return localCopyDestinationState{}, fmt.Errorf("local destination %q changed while staging", parent.display)
	}
	state := localCopyDestinationState{
		exists: true,
		device: uint64(after.Dev),
		inode:  uint64(after.Ino),
		mode:   uint32(after.Mode),
		size:   after.Size,
	}
	copy(state.digest[:], digest.Sum(nil))
	return state, nil
}

func createLocalCopyRecovery(
	ctx context.Context,
	parent *localCopyRootedParent,
	expected localCopyDestinationState,
) (string, error) {
	name, recovery, err := createLocalCopyNamedTemp(parent.dir, expected.permissions(), ".recover")
	if err != nil {
		return "", fmt.Errorf("creating local destination recovery snapshot: %w", err)
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
		return "", fmt.Errorf("local destination %q changed while staging: %w", parent.display, err)
	}
	source := os.NewFile(uintptr(fd), parent.name)
	if source == nil {
		_ = unix.Close(fd)
		return "", fmt.Errorf("creating local destination recovery snapshot")
	}
	defer source.Close()

	var before unix.Stat_t
	if err := unix.Fstat(fd, &before); err != nil {
		return "", fmt.Errorf("checking local destination %q: %w", parent.display, err)
	}
	if before.Mode&unix.S_IFMT != unix.S_IFREG {
		return "", fmt.Errorf("destination %s is not a regular file", parent.display)
	}

	digest := sha256.New()
	buffer := make([]byte, 64*1024)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, readErr := source.Read(buffer)
		if n > 0 {
			if _, err := recovery.Write(buffer[:n]); err != nil {
				return "", fmt.Errorf("creating local destination recovery snapshot: %w", err)
			}
			_, _ = digest.Write(buffer[:n])
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return "", fmt.Errorf("creating local destination recovery snapshot: %w", readErr)
		}
	}

	var after unix.Stat_t
	if err := unix.Fstat(fd, &after); err != nil {
		return "", fmt.Errorf("checking local destination %q: %w", parent.display, err)
	}
	actual := localCopyDestinationState{
		exists: true,
		device: uint64(after.Dev),
		inode:  uint64(after.Ino),
		mode:   uint32(after.Mode),
		size:   after.Size,
	}
	copy(actual.digest[:], digest.Sum(nil))
	if before.Dev != after.Dev || before.Ino != after.Ino || before.Mode != after.Mode || before.Size != after.Size || actual != expected {
		return "", fmt.Errorf("local destination %q changed while staging", parent.display)
	}
	if err := recovery.Chmod(expected.permissions()); err != nil {
		return "", fmt.Errorf("creating local destination recovery snapshot: %w", err)
	}
	if err := recovery.Close(); err != nil {
		return "", fmt.Errorf("creating local destination recovery snapshot: %w", err)
	}
	cleanup = false
	return name, nil
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
	if err := ctx.Err(); err != nil {
		return err
	}
	parent, _, err := openLocalCopyRootedParent(root, value, true)
	if err != nil {
		return err
	}
	defer parent.dir.Close()
	if hooks.afterParentOpen != nil {
		if err := hooks.afterParentOpen(); err != nil {
			return fmt.Errorf("after opening local destination parent: %w", err)
		}
	}

	destination, err := snapshotLocalCopyDestinationAt(ctx, parent, parent.name)
	if err != nil {
		return err
	}
	if destination.exists && !overwrite {
		return fmt.Errorf("destination %s already exists", parent.display)
	}

	tempName, temp, err := createLocalCopyTemp(parent.dir, destination.permissions())
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		_ = temp.Close()
		if !committed {
			_ = unix.Unlinkat(int(parent.dir.Fd()), tempName, 0)
		}
	}()
	if hooks.afterTempCreated != nil {
		if err := hooks.afterTempCreated(); err != nil {
			return fmt.Errorf("after creating staged local destination: %w", err)
		}
	}
	if err := writeLocalCopyContent(ctx, temp, content); err != nil {
		return err
	}
	if err := temp.Chmod(destination.permissions()); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	recoveryName := ""
	preserveRecovery := false
	defer func() {
		if recoveryName != "" && !preserveRecovery {
			_ = unix.Unlinkat(int(parent.dir.Fd()), recoveryName, 0)
		}
	}()
	if destination.exists {
		recoveryName, err = createLocalCopyRecovery(ctx, parent, destination)
		if err != nil {
			return err
		}
	}

	if err := ctx.Err(); err != nil {
		return err
	}
	if hooks.beforeInstall != nil {
		if err := hooks.beforeInstall(); err != nil {
			return fmt.Errorf("before installing local destination: %w", err)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	if !destination.exists {
		err = renameLocalCopyNoReplace(int(parent.dir.Fd()), tempName, parent.name)
		if err != nil {
			if errors.Is(err, unix.EEXIST) {
				return fmt.Errorf("destination %s already exists", parent.display)
			}
			return fmt.Errorf("committing local destination: %w", err)
		}
		committed = true
		return nil
	}

	current, err := snapshotLocalCopyDestinationAt(ctx, parent, parent.name)
	if err != nil || current != destination {
		preserveRecovery = true
		recoveryPath := filepath.Join(filepath.Dir(parent.display), recoveryName)
		if err != nil {
			return fmt.Errorf("local destination %q changed while staging: %v; recovery preserved at %s", parent.display, err, recoveryPath)
		}
		return fmt.Errorf("local destination %q changed while staging; recovery preserved at %s", parent.display, recoveryPath)
	}
	staged, err := snapshotLocalCopyDestinationAt(ctx, parent, tempName)
	if err != nil {
		return fmt.Errorf("checking staged local destination: %w", err)
	}
	if err := exchangeLocalCopy(int(parent.dir.Fd()), tempName, parent.name); err != nil {
		return fmt.Errorf("committing local destination: %w", err)
	}

	var afterInstallErr error
	if hooks.afterInstall != nil {
		afterInstallErr = hooks.afterInstall()
	}
	displaced, inspectErr := snapshotLocalCopyDestinationAt(ctx, parent, tempName)
	live, liveErr := snapshotLocalCopyDestinationAt(ctx, parent, parent.name)
	if afterInstallErr != nil || inspectErr != nil || liveErr != nil || displaced != destination || live != staged {
		preserveRecovery = true
		recoveryPath := filepath.Join(filepath.Dir(parent.display), recoveryName)
		if liveErr == nil && live == staged {
			if swapErr := exchangeLocalCopy(int(parent.dir.Fd()), tempName, parent.name); swapErr == nil {
				if afterInstallErr != nil && inspectErr == nil && displaced == destination {
					preserveRecovery = false
					return fmt.Errorf("after installing local destination: %w", afterInstallErr)
				}
				if inspectErr != nil {
					return fmt.Errorf("local destination %q changed during atomic replacement: %v; recovery preserved at %s", parent.display, inspectErr, recoveryPath)
				}
				return fmt.Errorf("local destination %q changed during atomic replacement; recovery preserved at %s", parent.display, recoveryPath)
			}
		}
		committed = true
		displacedPath := filepath.Join(filepath.Dir(parent.display), tempName)
		return fmt.Errorf(
			"local destination %q changed during atomic replacement; recovery preserved at %s and displaced path preserved at %s",
			parent.display,
			recoveryPath,
			displacedPath,
		)
	}

	committed = true
	if err := unix.Unlinkat(int(parent.dir.Fd()), tempName, 0); err != nil {
		preserveRecovery = true
		recoveryPath := filepath.Join(filepath.Dir(parent.display), recoveryName)
		displacedPath := filepath.Join(filepath.Dir(parent.display), tempName)
		return fmt.Errorf("installed local destination but displaced destination cleanup failed; recovery preserved at %s and displaced path preserved at %s: %w", recoveryPath, displacedPath, err)
	}
	if err := unix.Unlinkat(int(parent.dir.Fd()), recoveryName, 0); err != nil {
		preserveRecovery = true
		recoveryPath := filepath.Join(filepath.Dir(parent.display), recoveryName)
		return fmt.Errorf("installed local destination but recovery cleanup failed; recovery preserved at %s: %w", recoveryPath, err)
	}
	recoveryName = ""
	return nil
}

func createLocalCopyTemp(parent *os.File, mode os.FileMode) (string, *os.File, error) {
	return createLocalCopyNamedTemp(parent, mode, ".tmp")
}

func createLocalCopyNamedTemp(parent *os.File, mode os.FileMode, suffix string) (string, *os.File, error) {
	for attempt := 0; attempt < 100; attempt++ {
		name, err := randomLocalCopyName(suffix)
		if err != nil {
			return "", nil, err
		}
		fd, err := unix.Openat(
			int(parent.Fd()),
			name,
			unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW,
			uint32(mode.Perm()),
		)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return "", nil, err
		}
		file := os.NewFile(uintptr(fd), name)
		if file == nil {
			_ = unix.Close(fd)
			_ = unix.Unlinkat(int(parent.Fd()), name, 0)
			return "", nil, errors.New("opening staged local destination")
		}
		return name, file, nil
	}
	return "", nil, errors.New("could not reserve staged local destination")
}

func randomLocalCopyName(suffix string) (string, error) {
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return ".copilot-" + hex.EncodeToString(random[:]) + suffix, nil
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
