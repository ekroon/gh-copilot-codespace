package ssh

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	pathpkg "path"
	"strings"
)

const filesystemRunnerScript = `
import base64
import ctypes
import errno
import codecs
import fnmatch
import hashlib
import heapq
import json
import mimetypes
import os
import re
import signal
import stat
import subprocess
import sys
import tempfile

MAX_FILE_TRANSFER_BYTES = 16 * 1024 * 1024
MAX_ENCODED_FILE_TRANSFER_BYTES = ((MAX_FILE_TRANSFER_BYTES + 2) // 3) * 4
MAX_VIEW_BYTES = 20 * 1024
MAX_DIRECTORY_ENTRIES = 1000
MAX_GREP_OUTPUT_BYTES = 1024 * 1024
DIRECTORY_DEPTH = 2
DEFAULT_GLOB_LIMIT = 1000
MAX_GLOB_LIMIT = 10000
DEFAULT_SEARCH_DIRECTORY_BATCH_ENTRIES = 256
rollback_in_progress = False
cancel_signals = []

class PatchCanceled(Exception):
    pass

class RootedWriteError(Exception):
    pass

def handle_cancel_signal(signum, frame):
    if rollback_in_progress:
        return
    raise PatchCanceled("apply patch canceled")

for signal_name in ("SIGHUP", "SIGINT", "SIGTERM"):
    if hasattr(signal, signal_name):
        cancel_signal = getattr(signal, signal_name)
        cancel_signals.append(cancel_signal)
        signal.signal(cancel_signal, handle_cancel_signal)

def block_cancel_signals():
    if hasattr(signal, "pthread_sigmask"):
        return signal.pthread_sigmask(signal.SIG_BLOCK, cancel_signals)
    return None

def restore_cancel_signals(previous):
    if previous is not None:
        signal.pthread_sigmask(signal.SIG_SETMASK, previous)

def fail(message, code=1):
    sys.stderr.write(str(message).rstrip("\n") + "\n")
    raise SystemExit(code)

def load_request():
    raw = sys.stdin.buffer.read()
    if not raw:
        return {}
    try:
        return json.loads(raw.decode("utf-8"))
    except Exception as exc:
        fail("invalid request: %s" % exc)

def emit(result):
    json.dump(result, sys.stdout, separators=(",", ":"))
    sys.stdout.write("\n")

def detect_mime(path, sample):
    if sample.startswith(b"\x89PNG\r\n\x1a\n"):
        return "image/png"
    if sample[:3] == b"\xff\xd8\xff":
        return "image/jpeg"
    if sample.startswith(b"GIF87a") or sample.startswith(b"GIF89a"):
        return "image/gif"
    if sample.startswith(b"BM"):
        return "image/bmp"
    if sample.startswith(b"RIFF") and sample[8:12] == b"WEBP":
        return "image/webp"
    guess, _ = mimetypes.guess_type(path)
    return guess or "application/octet-stream"

def is_binary(sample):
    if b"\x00" in sample:
        return True
    try:
        codecs.getincrementaldecoder("utf-8")().decode(sample, final=False)
    except UnicodeDecodeError:
        return True
    return False

def mime_looks_text(mime_type):
    if mime_type.startswith("text/"):
        return True
    return (
        mime_type == "application/json"
        or mime_type == "application/xml"
        or mime_type == "application/javascript"
        or mime_type == "application/x-sh"
        or mime_type == "application/x-yaml"
        or mime_type == "application/yaml"
        or mime_type.endswith("+json")
        or mime_type.endswith("+xml")
    )

def binary_summary(kind, mime_type, size):
    return "%s (%s), %d bytes\n" % (kind, mime_type, size)

def select_directory_entries(path, limit, skip_entry, after_key=None):
    if limit <= 0:
        return []

    def candidates(scan):
        for entry in scan:
            if skip_entry(entry.name):
                continue
            is_directory = entry.is_dir(follow_symlinks=False)
            sort_key = entry.name + (os.sep if is_directory else "")
            if after_key is not None and sort_key <= after_key:
                continue
            yield sort_key, entry, is_directory

    try:
        with os.scandir(path) as scan:
            selected = heapq.nsmallest(
                limit,
                candidates(scan),
                key=lambda item: item[0],
            )
    except FileNotFoundError:
        return []

    return selected

def list_directory(path):
    entries = []
    total_bytes = 0

    def walk(current, relative="", depth=1):
        nonlocal total_bytes
        remaining = MAX_DIRECTORY_ENTRIES - len(entries)
        selected = select_directory_entries(
            current,
            remaining + 1,
            lambda name: name.startswith("."),
        )
        for _, entry, is_directory in selected:
            name = entry.name
            child_path = entry.path
            rel = name if not relative else os.path.join(relative, name)
            display = rel + (os.sep if is_directory else "")
            entry_bytes = len(display.encode("utf-8")) + 1
            if len(entries) >= MAX_DIRECTORY_ENTRIES or total_bytes + entry_bytes > MAX_VIEW_BYTES:
                return True
            entries.append(display)
            total_bytes += entry_bytes
            if is_directory and depth < DIRECTORY_DEPTH:
                if walk(child_path, rel, depth + 1):
                    return True
        return False

    return entries, walk(path)

def read_text_with_line_numbers(path, view_range, force):
    limited = not force
    start = view_range[0] if len(view_range) == 2 else None
    end = view_range[1] if len(view_range) == 2 else None
    rendered = []
    total = 0
    truncated = False

    with open(path, "rb") as handle:
        line_no = 1
        while True:
            selected = (start is None or line_no >= start) and (end is None or end == -1 or line_no <= end)
            decoder = codecs.getincrementaldecoder("utf-8")()
            line_started = False
            saw_data = False
            reached_eof = False

            while True:
                raw = handle.readline(4096)
                if raw:
                    saw_data = True
                    line_ended = raw.endswith(b"\n")
                    if line_ended:
                        raw = raw[:-1]
                else:
                    if not saw_data:
                        return "".join(rendered), truncated
                    line_ended = True
                    reached_eof = True

                if selected:
                    if not line_started:
                        prefix = "%d. " % line_no
                        prefix_len = len(prefix.encode("utf-8"))
                        if limited and total + prefix_len + 1 > MAX_VIEW_BYTES:
                            return "".join(rendered), True
                        rendered.append(prefix)
                        total += prefix_len
                        line_started = True

                    text = decoder.decode(raw, final=line_ended)
                    encoded = text.encode("utf-8")
                    if limited:
                        available = MAX_VIEW_BYTES - total - 1
                        if len(encoded) > available:
                            prefix = encoded[:max(available, 0)]
                            while prefix:
                                try:
                                    text = prefix.decode("utf-8")
                                    break
                                except UnicodeDecodeError:
                                    prefix = prefix[:-1]
                            else:
                                text = ""
                            rendered.append(text)
                            total += len(prefix)
                            if total < MAX_VIEW_BYTES:
                                rendered.append("\n")
                            return "".join(rendered), True
                    rendered.append(text)
                    total += len(encoded)

                if line_ended:
                    if selected:
                        rendered.append("\n")
                        total += 1
                    break

            if reached_eof:
                break
            line_no += 1
            if end is not None and end != -1 and line_no > end:
                break

    return "".join(rendered), truncated

def handle_view(req):
    path = req.get("path")
    if not isinstance(path, str) or not path:
        fail("view: path is required")

    view_range = req.get("view_range")
    if not isinstance(view_range, list) or len(view_range) != 2:
        view_range = []
    force = bool(req.get("forceReadLargeFiles"))

    if os.path.isdir(path):
        entries, truncated = list_directory(path)
        result = {
            "kind": "directory",
            "entries": entries,
            "truncated": truncated,
            "limit": MAX_DIRECTORY_ENTRIES,
            "byte_limit": MAX_VIEW_BYTES,
        }
        if entries:
            result["content"] = "\n".join(entries) + "\n"
        emit(result)
        return

    if not os.path.isfile(path):
        fail("view: %s not found" % path)

    with open(path, "rb") as handle:
        sample = handle.read(8192)
        handle.seek(0, os.SEEK_END)
        size = handle.tell()

    mime_type = detect_mime(path, sample)
    if mime_type.startswith("image/"):
        result = {
            "kind": "image",
            "mime_type": mime_type,
            "content": binary_summary("Image file", mime_type, size),
        }
        if force or size <= MAX_VIEW_BYTES:
            with open(path, "rb") as handle:
                result["base64_data"] = base64.b64encode(handle.read()).decode("ascii")
        else:
            result["truncated"] = True
        emit(result)
        return

    if is_binary(sample):
        emit({
            "kind": "file",
            "content": binary_summary("Binary file", mime_type, size),
            "mime_type": mime_type,
            "truncated": True,
        })
        return

    try:
        content, truncated = read_text_with_line_numbers(path, view_range, force)
    except UnicodeDecodeError:
        emit({
            "kind": "file",
            "content": binary_summary("Binary file", mime_type, size),
            "mime_type": mime_type,
            "truncated": True,
        })
        return

    result = {"kind": "file", "content": content, "truncated": truncated}
    if mime_type and mime_type != "application/octet-stream":
        result["mime_type"] = mime_type
    emit(result)

def handle_create(req):
    path = req.get("path")
    content = req.get("content")
    if not isinstance(path, str) or not path:
        fail("create file: path is required")
    if not isinstance(content, str):
        fail("create file: content must be a string")
    if os.path.lexists(path):
        fail("create file: %s already exists" % path)

    directory = os.path.dirname(path) or "."
    os.makedirs(directory, exist_ok=True)
    fd, tmp_path = tempfile.mkstemp(prefix="." + (os.path.basename(path) or "create") + ".", suffix=".tmp", dir=directory)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as handle:
            handle.write(content)
        os.chmod(tmp_path, 0o644)
        os.link(tmp_path, path)
    except FileExistsError:
        fail("create file: %s already exists" % path)
    finally:
        try:
            os.remove(tmp_path)
        except FileNotFoundError:
            pass

def rooted_open_flags(directory=False, write=False, create=False):
    flags = os.O_WRONLY if write else os.O_RDONLY
    flags |= getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0)
    if directory:
        flags |= getattr(os, "O_DIRECTORY", 0)
    elif not write:
        flags |= getattr(os, "O_NONBLOCK", 0)
    if create:
        flags |= os.O_CREAT | os.O_EXCL
    return flags

def rooted_path_parts(path, root, operation):
    if root:
        clean_root = os.path.abspath(root)
        clean_path = os.path.abspath(path if os.path.isabs(path) else os.path.join(clean_root, path))
        try:
            relative = os.path.relpath(clean_path, clean_root)
        except ValueError:
            fail("%s file: path %s escapes workdir %s" % (operation, path, root))
        if relative in (".", "..") or relative.startswith(".." + os.sep) or os.path.isabs(relative):
            fail("%s file: path %s escapes workdir %s" % (operation, path, root))
    else:
        clean_path = os.path.abspath(path)
        clean_root = os.path.abspath(os.sep)
        relative = os.path.relpath(clean_path, clean_root)

    parts = relative.split(os.sep)
    if not parts or any(part in ("", ".", "..") for part in parts):
        fail("%s file: invalid path %s" % (operation, path))
    return clean_root, parts

def rooted_parent_opened(operation, parent_fd, path):
    pass

def rooted_write_before_capture(parent_fd, name, path):
    pass

def rooted_write_before_install(parent_fd, name, path):
    pass

def rooted_write_after_install(parent_fd, name, path):
    pass

def open_rooted_parent(path, root, operation, create):
    clean_root, parts = rooted_path_parts(path, root, operation)
    try:
        current_fd = os.open(clean_root, rooted_open_flags(directory=True))
    except OSError as exc:
        fail("%s file: open workdir: %s" % (operation, exc))

    try:
        for part in parts[:-1]:
            try:
                next_fd = os.open(part, rooted_open_flags(directory=True), dir_fd=current_fd)
            except FileNotFoundError:
                if not create:
                    raise
                try:
                    os.mkdir(part, 0o755, dir_fd=current_fd)
                except FileExistsError:
                    pass
                next_fd = os.open(part, rooted_open_flags(directory=True), dir_fd=current_fd)
            except OSError as exc:
                if exc.errno in (errno.ELOOP, errno.ENOTDIR):
                    fail("%s file: path %s escapes workdir %s through symbolic link" % (operation, path, root))
                raise
            os.close(current_fd)
            current_fd = next_fd
        return current_fd, parts[-1]
    except BaseException:
        os.close(current_fd)
        raise

def close_rooted_parent(parent_fd):
    try:
        os.close(parent_fd)
    except OSError:
        pass

def rooted_file_identity(info):
    return (
        info.st_dev,
        info.st_ino,
        stat.S_IFMT(info.st_mode),
        stat.S_IMODE(info.st_mode),
        info.st_size,
        info.st_mtime_ns,
    )

def rooted_destination_snapshot(parent_fd, name, path):
    try:
        fd = os.open(name, rooted_open_flags(), dir_fd=parent_fd)
    except FileNotFoundError:
        return None
    except OSError as exc:
        if exc.errno == errno.ELOOP:
            raise RootedWriteError("write file: refusing to overwrite symbolic link path %s" % path)
        fail_if_rooted_nonregular(parent_fd, name, path, "write", "destination")
        raise RootedWriteError("write file: inspect destination: %s" % exc)

    try:
        before = os.fstat(fd)
        if not stat.S_ISREG(before.st_mode):
            raise RootedWriteError("write file: destination %s is not a regular file" % path)
        digest = hashlib.sha256()
        while True:
            chunk = os.read(fd, 64 * 1024)
            if not chunk:
                break
            digest.update(chunk)
        after = os.fstat(fd)
        if rooted_file_identity(before) != rooted_file_identity(after):
            raise RootedWriteError("write file: destination %s changed during write" % path)
        return (rooted_file_identity(after), digest.digest())
    finally:
        os.close(fd)

def rooted_rename_no_replace(parent_fd, source, target):
    source_bytes = os.fsencode(source)
    target_bytes = os.fsencode(target)

    if sys.platform.startswith("linux"):
        libc = ctypes.CDLL(None, use_errno=True)
        renameat2 = getattr(libc, "renameat2", None)
        if renameat2 is not None:
            renameat2.argtypes = [ctypes.c_int, ctypes.c_char_p, ctypes.c_int, ctypes.c_char_p, ctypes.c_uint]
            renameat2.restype = ctypes.c_int
            if renameat2(parent_fd, source_bytes, parent_fd, target_bytes, 1) == 0:
                return
            error_number = ctypes.get_errno()
            raise OSError(error_number, os.strerror(error_number), target)

    if sys.platform == "darwin":
        libc = ctypes.CDLL(None, use_errno=True)
        renameatx_np = getattr(libc, "renameatx_np", None)
        if renameatx_np is not None:
            renameatx_np.argtypes = [ctypes.c_int, ctypes.c_char_p, ctypes.c_int, ctypes.c_char_p, ctypes.c_uint]
            renameatx_np.restype = ctypes.c_int
            if renameatx_np(parent_fd, source_bytes, parent_fd, target_bytes, 0x00000004) == 0:
                return
            error_number = ctypes.get_errno()
            raise OSError(error_number, os.strerror(error_number), target)

    raise OSError(errno.ENOTSUP, "atomic no-replace rename is not supported", target)

def rooted_rename_exchange(parent_fd, source, target):
    source_bytes = os.fsencode(source)
    target_bytes = os.fsencode(target)

    if sys.platform.startswith("linux"):
        libc = ctypes.CDLL(None, use_errno=True)
        renameat2 = getattr(libc, "renameat2", None)
        if renameat2 is not None:
            renameat2.argtypes = [ctypes.c_int, ctypes.c_char_p, ctypes.c_int, ctypes.c_char_p, ctypes.c_uint]
            renameat2.restype = ctypes.c_int
            if renameat2(parent_fd, source_bytes, parent_fd, target_bytes, 2) == 0:
                return
            error_number = ctypes.get_errno()
            raise OSError(error_number, os.strerror(error_number), target)

    if sys.platform == "darwin":
        libc = ctypes.CDLL(None, use_errno=True)
        renameatx_np = getattr(libc, "renameatx_np", None)
        if renameatx_np is not None:
            renameatx_np.argtypes = [ctypes.c_int, ctypes.c_char_p, ctypes.c_int, ctypes.c_char_p, ctypes.c_uint]
            renameatx_np.restype = ctypes.c_int
            if renameatx_np(parent_fd, source_bytes, parent_fd, target_bytes, 0x00000002) == 0:
                return
            error_number = ctypes.get_errno()
            raise OSError(error_number, os.strerror(error_number), target)

    raise OSError(errno.ENOTSUP, "atomic exchange rename is not supported", target)

def rooted_create_recovery(parent_fd, name, path, expected_snapshot):
    recovery_name, recovery_fd = create_rooted_named_temp(parent_fd, expected_snapshot[0][3] & 0o777, ".recover")
    cleanup = True
    source_fd = None
    try:
        source_fd = os.open(name, rooted_open_flags(), dir_fd=parent_fd)
        before = os.fstat(source_fd)
        if not stat.S_ISREG(before.st_mode):
            raise RootedWriteError("write file: destination %s is not a regular file" % path)
        digest = hashlib.sha256()
        while True:
            chunk = os.read(source_fd, 64 * 1024)
            if not chunk:
                break
            write_all(recovery_fd, chunk)
            digest.update(chunk)
        after = os.fstat(source_fd)
        actual_snapshot = (rooted_file_identity(after), digest.digest())
        if rooted_file_identity(before) != rooted_file_identity(after) or actual_snapshot != expected_snapshot:
            raise RootedWriteError("write file: destination %s changed during write" % path)
        os.fchmod(recovery_fd, expected_snapshot[0][3] & 0o777)
        cleanup = False
        return recovery_name
    finally:
        if source_fd is not None:
            os.close(source_fd)
        os.close(recovery_fd)
        if cleanup:
            try:
                os.unlink(recovery_name, dir_fd=parent_fd)
            except FileNotFoundError:
                pass

def fail_if_rooted_nonregular(parent_fd, name, path, operation, role):
    try:
        info = os.stat(name, dir_fd=parent_fd, follow_symlinks=False)
    except FileNotFoundError:
        return
    if stat.S_ISLNK(info.st_mode):
        fail("%s file: refusing symbolic link path %s" % (operation, path))
    if not stat.S_ISREG(info.st_mode):
        fail("%s file: %s %s is not a regular file" % (operation, role, path))

def read_rooted_file(parent_fd, name, path):
    try:
        fd = os.open(name, rooted_open_flags(), dir_fd=parent_fd)
    except OSError as exc:
        if exc.errno == errno.ELOOP:
            fail("read file: refusing symbolic link path %s" % path)
        fail_if_rooted_nonregular(parent_fd, name, path, "read", "source")
        fail("read file: %s" % exc)
    try:
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode):
            fail("read file: source %s is not a regular file" % path)
        if info.st_size > MAX_FILE_TRANSFER_BYTES:
            fail("read file: %d bytes exceeds %d" % (info.st_size, MAX_FILE_TRANSFER_BYTES))

        chunks = []
        total = 0
        while total <= MAX_FILE_TRANSFER_BYTES:
            chunk = os.read(fd, min(64 * 1024, MAX_FILE_TRANSFER_BYTES + 1 - total))
            if not chunk:
                break
            chunks.append(chunk)
            total += len(chunk)
        if total > MAX_FILE_TRANSFER_BYTES:
            fail("read file: file grew beyond %d bytes" % MAX_FILE_TRANSFER_BYTES)
        return b"".join(chunks)
    finally:
        os.close(fd)

def create_rooted_temp(parent_fd, mode):
    return create_rooted_named_temp(parent_fd, mode, ".tmp")

def create_rooted_named_temp(parent_fd, mode, suffix):
    for _ in range(100):
        name = ".copilot-" + os.urandom(8).hex() + suffix
        try:
            fd = os.open(name, rooted_open_flags(write=True, create=True), mode, dir_fd=parent_fd)
            return name, fd
        except FileExistsError:
            continue
    fail("write file: could not reserve staged file")

def write_all(fd, content):
    offset = 0
    while offset < len(content):
        written = os.write(fd, content[offset:offset + 64 * 1024])
        if written <= 0:
            fail("write file: short write")
        offset += written

def handle_read(req):
    path = req.get("path")
    root = req.get("root") or ""
    if not isinstance(path, str) or not path:
        fail("read file: path is required")
    if not isinstance(root, str):
        fail("read file: root must be a string")

    parent_fd, name = open_rooted_parent(path, root, "read", False)
    try:
        rooted_parent_opened("read", parent_fd, path)
        content = read_rooted_file(parent_fd, name, path)
    finally:
        close_rooted_parent(parent_fd)
    emit({"data": base64.b64encode(content).decode("ascii")})

def handle_write(req):
    global rollback_in_progress
    path = req.get("path")
    encoded = req.get("data")
    overwrite = bool(req.get("overwrite"))
    root = req.get("root") or ""
    if not isinstance(path, str) or not path:
        fail("write file: path is required")
    if not isinstance(encoded, str):
        fail("write file: data must be base64")
    if not isinstance(root, str):
        fail("write file: root must be a string")
    if len(encoded) > MAX_ENCODED_FILE_TRANSFER_BYTES:
        fail("write file: encoded data exceeds %d bytes" % MAX_FILE_TRANSFER_BYTES)
    if len(encoded) % 4 == 0:
        padding = len(encoded) - len(encoded.rstrip("="))
        if padding <= 2:
            decoded_size = (len(encoded) // 4) * 3 - padding
            if decoded_size > MAX_FILE_TRANSFER_BYTES:
                fail("write file: %d bytes exceeds %d" % (decoded_size, MAX_FILE_TRANSFER_BYTES))
    try:
        content = base64.b64decode(encoded, validate=True)
    except Exception as exc:
        fail("write file: data must be valid base64: %s" % exc)
    if len(content) > MAX_FILE_TRANSFER_BYTES:
        fail("write file: %d bytes exceeds %d" % (len(content), MAX_FILE_TRANSFER_BYTES))

    parent_fd, name = open_rooted_parent(path, root, "write", True)
    try:
        rooted_parent_opened("write", parent_fd, path)
        mode = 0o644
        existing_snapshot = rooted_destination_snapshot(parent_fd, name, path)
        if existing_snapshot is not None:
            if not overwrite:
                fail("write file: %s already exists" % path)
            mode = existing_snapshot[0][3] & 0o777

        tmp_name, tmp_fd = create_rooted_temp(parent_fd, mode)
        committed = False
        recovery_name = None
        preserve_recovery = False
        try:
            try:
                write_all(tmp_fd, content)
                os.fchmod(tmp_fd, mode)
            finally:
                os.close(tmp_fd)

            rooted_write_before_capture(parent_fd, name, path)
            try:
                if existing_snapshot is not None:
                    recovery_name = rooted_create_recovery(parent_fd, name, path, existing_snapshot)

                rooted_write_before_install(parent_fd, name, path)
                previous_rollback = rollback_in_progress
                rollback_in_progress = True
                try:
                    if existing_snapshot is None:
                        try:
                            rooted_rename_no_replace(parent_fd, tmp_name, name)
                        except FileExistsError:
                            raise RootedWriteError("write file: %s already exists" % path)
                        except OSError as exc:
                            raise RootedWriteError("write file: commit: %s" % exc)
                        committed = True
                    else:
                        recovery_path = os.path.join(os.path.dirname(path) or ".", recovery_name)
                        current_snapshot = rooted_destination_snapshot(parent_fd, name, path)
                        if current_snapshot != existing_snapshot:
                            preserve_recovery = True
                            raise RootedWriteError(
                                "write file: destination %s changed during write; recovery preserved at %s"
                                % (path, recovery_path)
                            )
                        staged_snapshot = rooted_destination_snapshot(parent_fd, tmp_name, path)
                        try:
                            rooted_rename_exchange(parent_fd, tmp_name, name)
                        except OSError as exc:
                            raise RootedWriteError("write file: commit: %s" % exc)

                        after_install_error = None
                        try:
                            rooted_write_after_install(parent_fd, name, path)
                        except BaseException as exc:
                            after_install_error = exc
                        displaced_snapshot = rooted_destination_snapshot(parent_fd, tmp_name, path)
                        live_snapshot = rooted_destination_snapshot(parent_fd, name, path)
                        if (
                            after_install_error is not None
                            or displaced_snapshot != existing_snapshot
                            or live_snapshot != staged_snapshot
                        ):
                            preserve_recovery = True
                            if live_snapshot == staged_snapshot:
                                try:
                                    rooted_rename_exchange(parent_fd, tmp_name, name)
                                except OSError:
                                    committed = True
                                else:
                                    if after_install_error is not None and displaced_snapshot == existing_snapshot:
                                        preserve_recovery = False
                                        raise RootedWriteError(
                                            "write file: after install: %s" % after_install_error
                                        )
                                    raise RootedWriteError(
                                        "write file: destination %s changed during atomic replacement; "
                                        "recovery preserved at %s" % (path, recovery_path)
                                    )
                            else:
                                committed = True
                            displaced_path = os.path.join(os.path.dirname(path) or ".", tmp_name)
                            raise RootedWriteError(
                                "write file: destination %s changed during atomic replacement; "
                                "recovery preserved at %s and displaced path preserved at %s"
                                % (path, recovery_path, displaced_path)
                            )

                        committed = True
                        try:
                            os.unlink(tmp_name, dir_fd=parent_fd)
                        except OSError as exc:
                            preserve_recovery = True
                            displaced_path = os.path.join(os.path.dirname(path) or ".", tmp_name)
                            raise RootedWriteError(
                                "write file: installed staged content but displaced destination cleanup failed; "
                                "recovery preserved at %s and displaced path preserved at %s: %s"
                                % (recovery_path, displaced_path, exc)
                            )
                        try:
                            os.unlink(recovery_name, dir_fd=parent_fd)
                        except OSError as exc:
                            preserve_recovery = True
                            raise RootedWriteError(
                                "write file: installed staged content but recovery cleanup failed; "
                                "recovery preserved at %s: %s" % (recovery_path, exc)
                            )
                        recovery_name = None
                finally:
                    rollback_in_progress = previous_rollback
            except BaseException:
                raise
        finally:
            if not committed:
                try:
                    os.unlink(tmp_name, dir_fd=parent_fd)
                except FileNotFoundError:
                    pass
            if recovery_name is not None and not preserve_recovery:
                try:
                    os.unlink(recovery_name, dir_fd=parent_fd)
                except FileNotFoundError:
                    pass
    finally:
        close_rooted_parent(parent_fd)

def handle_edit(req):
    path = req.get("path")
    old_str = req.get("old_str")
    new_str = req.get("new_str")
    if not isinstance(path, str) or not path:
        fail("edit file: path is required")
    if not isinstance(old_str, str) or not isinstance(new_str, str):
        fail("edit file: old_str and new_str must be strings")

    try:
        flags = os.O_RDONLY | getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NONBLOCK", 0) | getattr(os, "O_NOFOLLOW", 0)
        fd = os.open(path, flags)
    except FileNotFoundError:
        fail("edit file: %s not found" % path)
    except OSError as exc:
        if exc.errno == errno.ELOOP:
            fail("edit file: symbolic link paths are not supported: %s" % path)
        fail("edit file: %s" % exc)

    try:
        before_info = os.fstat(fd)
        if not stat.S_ISREG(before_info.st_mode):
            fail("edit file: %s is not a regular file" % path)
        chunks = []
        digest = hashlib.sha256()
        while True:
            chunk = os.read(fd, 1024 * 1024)
            if not chunk:
                break
            chunks.append(chunk)
            digest.update(chunk)
        after_info = os.fstat(fd)
    finally:
        os.close(fd)

    before = patch_metadata_identity(before_info)
    if patch_metadata_identity(after_info) != before:
        fail("edit file: file changed during edit: %s" % path)
    try:
        path_info = os.lstat(path)
    except FileNotFoundError:
        fail("edit file: file changed during edit: %s" % path)
    if stat.S_ISLNK(path_info.st_mode):
        fail("edit file: symbolic link paths are not supported: %s" % path)
    if patch_metadata_identity(path_info) != before:
        fail("edit file: file changed during edit: %s" % path)

    try:
        content = b"".join(chunks).decode("utf-8")
    except UnicodeDecodeError:
        fail("edit file: %s is not a text file" % path)

    count = content.count(old_str)
    if count == 0:
        fail("old_str not found in file")
    if count > 1:
        fail("old_str found %d times, must be unique" % count)

    plan = [{
        "kind": "write",
        "path": path,
        "target": path,
        "content": content.replace(old_str, new_str, 1),
        "mode": stat.S_IMODE(before_info.st_mode) or 0o644,
        "identity": before + (digest.hexdigest(),),
        "summary": "Updated %s" % path,
        "_edit": True,
    }]
    try:
        commit_patch_plan(plan)
    except PatchError as exc:
        fail(str(exc))

def normalize_output_mode(value):
    if value in ("content", "files_with_matches", "count"):
        return value
    return "content"

def normalize_search_type(value):
    if not isinstance(value, str):
        return ""
    normalized = value.lower()
    if normalized == "tsx":
        return "ts"
    if normalized == "jsx":
        return "js"
    return normalized

def match_type(file_type, rel):
    if not file_type:
        return True
    normalized = normalize_search_type(file_type)
    ext = os.path.splitext(rel)[1].lower()
    if normalized == "go":
        return ext == ".go"
    if normalized == "js":
        return ext in (".js", ".jsx", ".mjs", ".cjs")
    if normalized == "ts":
        return ext in (".ts", ".tsx", ".mts", ".cts")
    if normalized == "py":
        return ext == ".py"
    if normalized == "rust":
        return ext == ".rs"
    if normalized == "java":
        return ext == ".java"
    return ext == "." + normalized

def display_path(root, rel):
    root = os.path.normpath(root).replace(os.sep, "/")
    rel = rel.replace(os.sep, "/")
    if rel in ("", "."):
        return "." if root in ("", ".") else root
    if root in ("", "."):
        return rel
    return root.rstrip("/") + "/" + rel

def resolve_search_roots(paths, cwd):
    roots = []
    for root in paths:
        if not isinstance(root, str) or not root:
            root = "."
        resolved = root if os.path.isabs(root) else os.path.normpath(os.path.join(cwd, root))
        roots.append((root, resolved))
    return roots

def is_hidden_search_entry(name):
    return name.startswith(".")

def should_skip_search_entry(name, include_hidden):
    return name == ".git" or (not include_hidden and is_hidden_search_entry(name))

def walk_search_root(original, resolved, include_hidden, directory_batch_limit, validate_regular_entries):
    try:
        root_info = os.lstat(resolved)
    except FileNotFoundError:
        fail("grep: %s not found" % original)
    if contains_git(os.path.normpath(resolved)):
        return
    if stat.S_ISREG(root_info.st_mode):
        name = os.path.basename(resolved)
        if should_skip_search_entry(name, include_hidden):
            return
        yield {
            "resolved": resolved,
            "display": display_path(original, "."),
            "rel": os.path.basename(original) or os.path.basename(resolved),
        }
        return
    if not stat.S_ISDIR(root_info.st_mode):
        return

    def walk_directory(current, relative):
        after_key = None
        while True:
            entries = select_directory_entries(
                current,
                directory_batch_limit,
                lambda name: should_skip_search_entry(name, include_hidden),
                after_key,
            )
            if not entries:
                return

            for _, entry, is_directory in entries:
                name = entry.name
                entry_path = entry.path
                rel = name if not relative else relative + "/" + name
                if not validate_regular_entries:
                    if is_directory:
                        for file in walk_directory(entry_path, rel):
                            yield file
                        continue
                    yield {
                        "resolved": entry_path,
                        "display": display_path(original, rel),
                        "rel": rel,
                    }
                    continue
                try:
                    entry_info = os.lstat(entry_path)
                except FileNotFoundError:
                    continue
                if stat.S_ISDIR(entry_info.st_mode):
                    for file in walk_directory(entry_path, rel):
                        yield file
                    continue
                if not stat.S_ISREG(entry_info.st_mode):
                    continue
                yield {
                    "resolved": entry_path,
                    "display": display_path(original, rel),
                    "rel": rel,
                }

            if len(entries) < directory_batch_limit:
                return
            after_key = entries[-1][0]

    for file in walk_directory(resolved, ""):
        yield file

def walk_search_files(paths, cwd, include_hidden=False, directory_batch_limit=DEFAULT_SEARCH_DIRECTORY_BATCH_ENTRIES, validate_regular_entries=True):
    producers = []
    for index, (original, resolved) in enumerate(resolve_search_roots(paths, cwd)):
        producer = iter(walk_search_root(
            original,
            resolved,
            include_hidden,
            directory_batch_limit,
            validate_regular_entries,
        ))
        try:
            first = next(producer)
        except StopIteration:
            continue
        heapq.heappush(producers, (first["display"], index, first, producer))

    while producers:
        _, index, file, producer = heapq.heappop(producers)
        yield file
        try:
            following = next(producer)
        except StopIteration:
            continue
        heapq.heappush(producers, (following["display"], index, following, producer))

def walk_glob_files(paths, cwd, directory_batch_limit):
    return walk_search_files(paths, cwd, True, directory_batch_limit, False)

def match_glob_pattern(pattern, rel):
    if not pattern:
        return True
    rel = rel.replace(os.sep, "/").lstrip("./")
    for expanded in brace_expand(pattern):
        expanded = expanded.replace(os.sep, "/").lstrip("./")
        if "/" not in expanded:
            if fnmatch.fnmatch(os.path.basename(rel), expanded):
                return True
            continue
        if match_glob_segments(expanded.split("/"), rel.split("/")):
            return True
    return False

def match_glob_segments(pattern_parts, path_parts):
    if not pattern_parts:
        return not path_parts
    if pattern_parts[0] == "**":
        return (
            match_glob_segments(pattern_parts[1:], path_parts)
            or bool(path_parts) and match_glob_segments(pattern_parts, path_parts[1:])
        )
    if not path_parts or not fnmatch.fnmatch(path_parts[0], pattern_parts[0]):
        return False
    return match_glob_segments(pattern_parts[1:], path_parts[1:])

def grep_line_matches(regex, text):
    return regex.search(text) is not None

def render_grep_content(display, file_lines, match_set, line_numbers, before_context, after_context, context_lines):
    before = before_context
    after = after_context
    if context_lines > 0:
        before = context_lines
        after = context_lines

    windows = []
    for line_index in sorted(match_set):
        start = max(0, line_index - before)
        end = min(len(file_lines) - 1, line_index + after)
        windows.append([start, end])

    if not windows:
        return ""

    merged = [windows[0]]
    for start, end in windows[1:]:
        if start > merged[-1][1] + 1:
            merged.append([start, end])
        elif end > merged[-1][1]:
            merged[-1][1] = end

    out = []
    show_line_numbers = line_numbers is None or bool(line_numbers)
    for index, (start, end) in enumerate(merged):
        if index > 0:
            out.append("--\n")
        for line_index in range(start, end + 1):
            sep = ":" if line_index in match_set else "-"
            if show_line_numbers:
                out.append("%s%s%d%s%s\n" % (display, sep, line_index + 1, sep, file_lines[line_index]))
            else:
                out.append("%s%s%s\n" % (display, sep, file_lines[line_index]))
    return "".join(out)

class GrepOutputBuffer:
    def __init__(self, head_limit):
        self.head_limit = max(0, int(head_limit or 0))
        self.parts = []
        self.line_count = 0
        self.byte_count = 0
        self.truncated = False

    @property
    def done(self):
        return self.truncated

    def append(self, text):
        if not text or self.done:
            return
        for fragment in text.splitlines(True):
            if self.done:
                return
            if self.head_limit > 0 and self.line_count >= self.head_limit:
                self.truncated = True
                return
            encoded = fragment.encode("utf-8")
            available = MAX_GREP_OUTPUT_BYTES - self.byte_count
            if available <= 0:
                self.truncated = True
                return
            if len(encoded) > available:
                prefix = encoded[:max(available, 0)]
                while prefix:
                    try:
                        rendered = prefix.decode("utf-8")
                        break
                    except UnicodeDecodeError:
                        prefix = prefix[:-1]
                else:
                    rendered = ""
                if rendered:
                    self.parts.append(rendered)
                    self.byte_count += len(prefix)
                self.truncated = True
                return

            self.parts.append(fragment)
            self.byte_count += len(encoded)
            if fragment.endswith(("\n", "\r")):
                self.line_count += 1

    def value(self):
        return "".join(self.parts)

def format_grep_line(display, line_index, line, is_match, line_numbers):
    sep = ":" if is_match else "-"
    if line_numbers is None or bool(line_numbers):
        return "%s%s%d%s%s\n" % (display, sep, line_index + 1, sep, line)
    return "%s%s%s\n" % (display, sep, line)

def open_regular_search_file(path, text=False):
    flags = os.O_RDONLY
    flags |= getattr(os, "O_CLOEXEC", 0)
    flags |= getattr(os, "O_NONBLOCK", 0)
    flags |= getattr(os, "O_NOFOLLOW", 0)
    try:
        fd = os.open(path, flags)
    except FileNotFoundError:
        return None
    except OSError as exc:
        nonregular_errors = {
            errno.ELOOP,
            errno.ENODEV,
            errno.ENXIO,
            getattr(errno, "ENOTSUP", None),
            getattr(errno, "EOPNOTSUPP", None),
        }
        if exc.errno in nonregular_errors:
            return None
        raise

    try:
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode):
            os.close(fd)
            return None
        if text:
            return os.fdopen(fd, "r", encoding="utf-8")
        return os.fdopen(fd, "rb")
    except Exception:
        os.close(fd)
        raise

def fallback_grep_lines(handle, regex, display, output, line_numbers, before, after):
    before_buffer = []
    had_group = False
    after_remaining = 0
    gap_count = 0

    for line_index, raw_line in enumerate(handle):
        line = raw_line.rstrip("\n")
        if line.endswith("\r"):
            line = line[:-1]
        is_match = grep_line_matches(regex, line)
        if is_match:
            if had_group and after_remaining == 0 and gap_count > before:
                output.append("--\n")
                if output.done:
                    return
            for buffered_index, buffered_line in before_buffer:
                output.append(format_grep_line(display, buffered_index, buffered_line, False, line_numbers))
                if output.done:
                    return
            before_buffer = []
            output.append(format_grep_line(display, line_index, line, True, line_numbers))
            if output.done:
                return
            had_group = True
            after_remaining = after
            gap_count = 0
            continue

        if had_group and after_remaining > 0:
            output.append(format_grep_line(display, line_index, line, False, line_numbers))
            if output.done:
                return
            after_remaining -= 1
            if after_remaining == 0:
                gap_count = 0
            continue

        if had_group:
            gap_count += 1
        if before > 0:
            before_buffer.append((line_index, line))
            if len(before_buffer) > before:
                before_buffer.pop(0)

def fallback_grep(req):
    pattern = req.get("pattern")
    if not isinstance(pattern, str) or not pattern:
        fail("grep: pattern is required")

    output_mode = normalize_output_mode(req.get("output_mode"))
    paths = req.get("paths")
    if not isinstance(paths, list) or not paths:
        path = req.get("path")
        paths = [path] if isinstance(path, str) and path else ["."]
    cwd = req.get("cwd") or os.getcwd()
    glob_pattern = req.get("glob") if isinstance(req.get("glob"), str) else ""
    file_type = normalize_search_type(req.get("type"))
    case_insensitive = bool(req.get("case_insensitive"))
    after_context = int(req.get("after_context") or 0)
    before_context = int(req.get("before_context") or 0)
    context_lines = int(req.get("context") or 0)
    line_numbers = req.get("line_numbers")
    head_limit = int(req.get("head_limit") or 0)
    multiline = bool(req.get("multiline"))

    flags = re.MULTILINE
    if case_insensitive:
        flags |= re.IGNORECASE
    try:
        regex = re.compile(pattern, flags)
    except re.error as exc:
        fail("grep: %s" % exc)

    output = GrepOutputBuffer(head_limit)
    before = context_lines if context_lines > 0 else before_context
    after = context_lines if context_lines > 0 else after_context
    directory_batch_limit = DEFAULT_SEARCH_DIRECTORY_BATCH_ENTRIES
    if head_limit > 0:
        directory_batch_limit = min(directory_batch_limit, max(2, head_limit + 1))
    for file in walk_search_files(paths, cwd, False, directory_batch_limit):
        rel = file["rel"]
        if not match_glob_pattern(glob_pattern, rel) or not match_type(file_type, rel):
            continue

        handle = open_regular_search_file(file["resolved"])
        if handle is None:
            continue
        with handle:
            sample = handle.read(8192)
        mime_type = detect_mime(file["resolved"], sample)
        if mime_type.startswith("image/"):
            continue
        if not mime_looks_text(mime_type) and is_binary(sample):
            continue

        if multiline:
            try:
                handle = open_regular_search_file(file["resolved"], True)
                if handle is None:
                    continue
                with handle:
                    text = handle.read().replace("\r\n", "\n")
            except UnicodeDecodeError:
                continue
            matches = regex.finditer(text)
            if output_mode == "files_with_matches":
                if next(matches, None) is not None:
                    output.append(file["display"] + "\n")
            elif output_mode == "count":
                match_count = sum(1 for _ in matches)
                if match_count > 0:
                    output.append("%s:%d\n" % (file["display"], match_count))
            else:
                for match in matches:
                    output.append("%s:%s\n" % (file["display"], match.group(0)))
                    if output.done:
                        break
        else:
            try:
                handle = open_regular_search_file(file["resolved"], True)
                if handle is None:
                    continue
                with handle:
                    if output_mode == "content":
                        fallback_grep_lines(handle, regex, file["display"], output, line_numbers, before, after)
                    elif output_mode == "files_with_matches":
                        for raw_line in handle:
                            if grep_line_matches(regex, raw_line.rstrip("\r\n")):
                                output.append(file["display"] + "\n")
                                break
                    else:
                        match_count = sum(
                            1
                            for raw_line in handle
                            if grep_line_matches(regex, raw_line.rstrip("\r\n"))
                        )
                        if match_count > 0:
                            output.append("%s:%d\n" % (file["display"], match_count))
            except UnicodeDecodeError:
                continue

        if output.done:
            break

    emit({
        "output": output.value(),
        "truncated": output.truncated,
        "limit": MAX_GREP_OUTPUT_BYTES,
        "byte_limit": MAX_GREP_OUTPUT_BYTES,
    })

def terminate_grep_process(proc):
    if proc.poll() is not None:
        return
    proc.terminate()
    try:
        proc.wait(timeout=2)
    except subprocess.TimeoutExpired:
        proc.kill()
        proc.wait()

def handle_grep(req):
    args = req.get("args")
    if isinstance(args, list) and args:
        cwd = req.get("cwd") or None
        head_limit = int(req.get("head_limit") or 0)
        allow_exit_1 = bool(req.get("allow_exit_1"))

        with tempfile.TemporaryFile(mode="w+t", encoding="utf-8", errors="replace") as stderr_handle:
            try:
                proc = subprocess.Popen(
                    args,
                    cwd=cwd,
                    stdout=subprocess.PIPE,
                    stderr=stderr_handle,
                    text=True,
                )
            except FileNotFoundError:
                fallback_grep(req)
                return

            output = GrepOutputBuffer(head_limit)
            stopped_early = False
            try:
                while True:
                    chunk = proc.stdout.read(64 * 1024)
                    if not chunk:
                        break
                    output.append(chunk)
                    if output.done:
                        stopped_early = True
                        terminate_grep_process(proc)
                        break
            finally:
                proc.stdout.close()
                if proc.poll() is None:
                    proc.wait()

            stderr_handle.seek(0)
            stderr = stderr_handle.read(64 * 1024)
            if not stopped_early and proc.returncode == 1 and allow_exit_1:
                pass
            elif not stopped_early and proc.returncode != 0:
                message = stderr.strip() or output.value().strip() or ("grep failed with exit code %d" % proc.returncode)
                fail(message, proc.returncode)

        emit({
            "output": output.value(),
            "truncated": output.truncated,
            "limit": MAX_GREP_OUTPUT_BYTES,
            "byte_limit": MAX_GREP_OUTPUT_BYTES,
        })
        return

    fallback_grep(req)

def contains_git(path):
    return any(part == ".git" for part in path.split(os.sep))

def split_brace_options(body):
    parts = []
    depth = 0
    start = 0
    for idx, ch in enumerate(body):
        if ch == "{":
            depth += 1
        elif ch == "}":
            depth -= 1
        elif ch == "," and depth == 0:
            parts.append(body[start:idx])
            start = idx + 1
    parts.append(body[start:])
    return parts

def brace_expand(pattern):
    start = pattern.find("{")
    if start == -1:
        return [pattern]

    depth = 0
    for idx in range(start, len(pattern)):
        ch = pattern[idx]
        if ch == "{":
            depth += 1
        elif ch == "}":
            depth -= 1
            if depth == 0:
                prefix = pattern[:start]
                suffix = pattern[idx + 1:]
                results = []
                for option in split_brace_options(pattern[start + 1:idx]):
                    for expanded in brace_expand(prefix + option + suffix):
                        results.append(expanded)
                return results
    return [pattern]

def normalize_glob_limit(value):
    try:
        limit = int(value or 0)
    except (TypeError, ValueError, OverflowError):
        limit = 0
    if limit <= 0:
        return DEFAULT_GLOB_LIMIT
    return min(limit, MAX_GLOB_LIMIT)

def handle_glob(req):
    pattern = req.get("pattern")
    if not isinstance(pattern, str) or not pattern:
        fail("glob: pattern is required")

    cwd = req.get("cwd") or os.getcwd()
    paths = req.get("paths")
    if not isinstance(paths, list) or not paths:
        path = req.get("path")
        paths = [path] if isinstance(path, str) and path else ["."]

    limit = normalize_glob_limit(req.get("limit"))
    matches = []
    last_rendered = None
    for file in walk_glob_files(paths, cwd, limit + 1):
        if not match_glob_pattern(pattern, file["rel"]):
            continue
        rendered = file["display"]
        if rendered == last_rendered:
            continue
        last_rendered = rendered
        if len(matches) >= limit:
            emit({
                "output": "".join(item + "\n" for item in matches),
                "truncated": True,
                "limit": limit,
            })
            return
        matches.append(rendered)

    emit({
        "output": "".join(item + "\n" for item in matches),
        "truncated": False,
        "limit": limit,
    })

class PatchError(Exception):
    pass

class PatchLineEndingError(PatchError):
    pass

def split_text(text):
    without_crlf = text.replace("\r\n", "")
    has_crlf = "\r\n" in text
    has_lf = "\n" in without_crlf
    has_bare_cr = "\r" in without_crlf
    if (has_crlf and has_lf) or has_bare_cr:
        raise PatchLineEndingError("mixed line endings are not supported")
    newline = "\r\n" if has_crlf else "\n"
    normalized = text.replace("\r\n", "\n") if has_crlf else text
    return normalized.splitlines(), normalized.endswith("\n"), newline

def join_text(lines, trailing_newline, newline):
    text = newline.join(lines)
    if trailing_newline and lines:
        return text + newline
    return text

def patch_error(message):
    raise PatchError(message)

def parse_update_hunks(lines):
    hunks = []
    current = None
    for line in lines:
        if line == "@@" or line.startswith("@@ "):
            if current is not None:
                if not current["lines"]:
                    patch_error("malformed patch: empty hunk")
                hunks.append(current)
            anchor = line[3:] if line.startswith("@@ ") else None
            current = {"lines": [], "end_of_file": False, "anchor": anchor}
            continue
        if line == "*** End of File":
            if current is None or not current["lines"]:
                patch_error("malformed patch: unexpected *** End of File")
            current["end_of_file"] = True
            hunks.append(current)
            current = None
            continue
        if current is None:
            patch_error("malformed patch: update block missing @@ header")
        if not line or line[0] not in (" ", "+", "-"):
            patch_error("malformed patch: invalid update line %r" % line)
        current["lines"].append(line)

    if current is not None:
        if current["lines"]:
            hunks.append(current)
        else:
            patch_error("malformed patch: empty hunk")
    return hunks

def parse_patch(text):
    lines = text.replace("\r\n", "\n").split("\n")
    if not lines or lines[0] != "*** Begin Patch":
        patch_error("malformed patch: missing *** Begin Patch")

    ops = []
    idx = 1
    while idx < len(lines):
        line = lines[idx]
        if line == "*** End Patch":
            for trailing in lines[idx + 1:]:
                if trailing != "":
                    patch_error("malformed patch: unexpected content after *** End Patch")
            if not ops:
                patch_error("malformed patch: no file operations")
            return ops
        if line == "":
            patch_error("malformed patch: unexpected blank line")
        if line.startswith("*** Add File: "):
            path = line[len("*** Add File: "):].strip()
            if not path:
                patch_error("malformed patch: add file path is required")
            idx += 1
            content_lines = []
            while idx < len(lines) and not lines[idx].startswith("*** "):
                current = lines[idx]
                if not current.startswith("+"):
                    patch_error("malformed patch: add file lines must start with '+'")
                content_lines.append(current[1:])
                idx += 1
            if not content_lines:
                patch_error("malformed patch: add file %s has no content" % path)
            ops.append({
                "kind": "add",
                "path": path,
                "content": "\n".join(content_lines) + ("\n" if content_lines else ""),
            })
            continue
        if line.startswith("*** Delete File: "):
            path = line[len("*** Delete File: "):].strip()
            if not path:
                patch_error("malformed patch: delete file path is required")
            ops.append({"kind": "delete", "path": path})
            idx += 1
            continue
        if line.startswith("*** Update File: "):
            path = line[len("*** Update File: "):].strip()
            if not path:
                patch_error("malformed patch: update file path is required")
            idx += 1
            move_to = None
            if idx < len(lines) and lines[idx].startswith("*** Move to: "):
                move_to = lines[idx][len("*** Move to: "):].strip()
                if not move_to:
                    patch_error("malformed patch: move target is required for %s" % path)
                idx += 1
            update_lines = []
            while idx < len(lines):
                if lines[idx] == "*** End of File":
                    update_lines.append(lines[idx])
                    idx += 1
                    continue
                if lines[idx].startswith("*** "):
                    break
                update_lines.append(lines[idx])
                idx += 1
            hunks = parse_update_hunks(update_lines)
            if not hunks and not move_to:
                patch_error("malformed patch: update file %s has no changes" % path)
            ops.append({
                "kind": "update",
                "path": path,
                "move_to": move_to,
                "hunks": hunks,
            })
            continue
        patch_error("malformed patch: unexpected line %r" % line)

    patch_error("malformed patch: missing *** End Patch")

def find_subsequence(lines, needle, start, end_of_file=False):
    if not needle:
        return len(lines) if end_of_file else min(start, len(lines))
    limit = len(lines) - len(needle)
    for origin in range(start, limit + 1):
        if end_of_file and origin + len(needle) != len(lines):
            continue
        if lines[origin:origin + len(needle)] == needle:
            return origin
    return -1

def apply_hunks(text, hunks):
    lines, trailing_newline, newline = split_text(text)
    updated = list(lines)
    cursor = 0
    for hunk in hunks:
        source = []
        target = []
        saw_change = False
        for entry in hunk["lines"]:
            prefix = entry[0]
            value = entry[1:]
            if prefix in (" ", "-"):
                source.append(value)
            if prefix in (" ", "+"):
                target.append(value)
            if prefix in ("+", "-"):
                saw_change = True
        if not saw_change and source == target:
            continue
        search_start = cursor
        anchor = hunk.get("anchor")
        if anchor is not None:
            anchor_pos = find_subsequence(updated, [anchor], cursor)
            if anchor_pos < 0:
                patch_error("could not apply patch")
            search_start = anchor_pos + 1
        pos = find_subsequence(updated, source, search_start, hunk["end_of_file"])
        if pos < 0:
            patch_error("could not apply patch")
        updated = updated[:pos] + target + updated[pos + len(source):]
        cursor = pos + len(target)
    return join_text(updated, trailing_newline, newline)

def resolve_patch_path(path, cwd):
    if not isinstance(path, str) or not path:
        patch_error("malformed patch: missing path")
    if os.path.isabs(path):
        lexical = os.path.normpath(path)
    else:
        lexical = os.path.normpath(os.path.join(cwd, path))
    try:
        info = os.lstat(lexical)
    except FileNotFoundError:
        info = None
    except OSError as exc:
        if exc.errno != errno.ENOTDIR:
            raise
        info = None
    if info is not None and stat.S_ISLNK(info.st_mode):
        patch_error("could not apply patch: symbolic link paths are not supported: %s" % path)
    try:
        return os.path.realpath(lexical)
    except OSError as exc:
        if exc.errno != errno.ENOTDIR:
            raise
        return lexical

def read_text_file(path):
    try:
        with open(path, "r", encoding="utf-8", newline="") as handle:
            return handle.read()
    except FileNotFoundError:
        patch_error("could not apply patch: missing file %s" % path)
    except UnicodeDecodeError:
        patch_error("could not apply patch: binary file %s" % path)

def patch_metadata_identity(info):
    return (
        info.st_dev,
        info.st_ino,
        stat.S_IFMT(info.st_mode),
        stat.S_IMODE(info.st_mode),
        info.st_size,
        info.st_mtime_ns,
    )

def patch_path_identity(path, info):
    before = patch_metadata_identity(info)
    digest = hashlib.sha256()
    with open(path, "rb") as handle:
        while True:
            chunk = handle.read(1024 * 1024)
            if not chunk:
                break
            digest.update(chunk)
    after = require_patch_file(path, path)
    if patch_metadata_identity(after) != before:
        patch_error("could not apply patch: file changed during patch: %s" % path)
    return before + (digest.hexdigest(),)

def require_patch_file(path, display):
    try:
        info = os.lstat(path)
    except FileNotFoundError:
        patch_error("could not apply patch: missing file %s" % display)
    if stat.S_ISLNK(info.st_mode):
        patch_error("could not apply patch: symbolic link paths are not supported: %s" % display)
    if not stat.S_ISREG(info.st_mode):
        patch_error("could not apply patch: %s is not a regular file" % display)
    return info

def patch_path_is_ancestor(parent, child):
    parent = os.path.normpath(parent)
    child = os.path.normpath(child)
    if parent == child:
        return True
    prefix = parent if parent.endswith(os.sep) else parent + os.sep
    return child.startswith(prefix)

def reserve_patch_path(touched, path, description):
    for previous_path, previous_description in touched:
        if patch_path_is_ancestor(path, previous_path) or patch_path_is_ancestor(previous_path, path):
            patch_error("malformed patch: %s conflicts with %s" % (description, previous_description))
    touched.append((path, description))

def build_patch_plan(ops, cwd):
    plan = []
    touched = []
    for op in ops:
        path = resolve_patch_path(op["path"], cwd)
        if op["kind"] == "add":
            reserve_patch_path(touched, path, "add %s" % op["path"])
            if os.path.lexists(path):
                patch_error("create file: %s already exists" % path)
            plan.append({
                "kind": "add",
                "path": path,
                "content": op["content"],
                "mode": 0o644,
                "summary": "Added %s" % op["path"],
            })
            continue
        if op["kind"] == "delete":
            reserve_patch_path(touched, path, "delete %s" % op["path"])
            info = require_patch_file(path, op["path"])
            plan.append({
                "kind": "delete",
                "path": path,
                "identity": patch_path_identity(path, info),
                "summary": "Deleted %s" % op["path"],
            })
            continue

        reserve_patch_path(touched, path, "update %s" % op["path"])
        info = require_patch_file(path, op["path"])

        target = path
        move_to = op.get("move_to")
        if move_to:
            target = resolve_patch_path(move_to, cwd)
            reserve_patch_path(touched, target, "move target %s" % move_to)
            if os.path.lexists(target):
                patch_error("could not apply patch: %s already exists" % target)

        if target != path and not op["hunks"]:
            plan.append({
                "kind": "move",
                "path": path,
                "target": target,
                "identity": patch_path_identity(path, info),
                "summary": "Updated %s -> %s" % (op["path"], move_to),
            })
            continue

        try:
            updated = apply_hunks(read_text_file(path), op["hunks"])
        except PatchLineEndingError:
            raise
        except PatchError:
            patch_error("could not apply patch")

        plan.append({
            "kind": "write",
            "path": path,
            "target": target,
            "content": updated,
            "mode": stat.S_IMODE(info.st_mode) or 0o644,
            "identity": patch_path_identity(path, info),
            "summary": "Updated %s" % op["path"] if target == path else "Updated %s -> %s" % (op["path"], move_to),
        })
    return plan

def ensure_parent_directory(target, created_dirs=None):
    directory = os.path.dirname(target) or "."
    missing = []
    current = directory
    while not os.path.lexists(current):
        missing.append(current)
        parent = os.path.dirname(current)
        if parent == current:
            break
        current = parent
    if os.path.lexists(current):
        info = os.lstat(current)
        if stat.S_ISLNK(info.st_mode):
            patch_error("could not apply patch: symbolic link directory %s" % current)
        if not stat.S_ISDIR(info.st_mode):
            patch_error("could not apply patch: parent path %s is not a directory" % current)
    for path in reversed(missing):
        try:
            os.mkdir(path, 0o755)
            if created_dirs is not None:
                created_dirs.append(path)
        except FileExistsError:
            info = os.lstat(path)
            if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode):
                patch_error("could not apply patch: parent path %s is not a directory" % path)

def stage_temp_file(target, content, mode, created_dirs=None):
    directory = os.path.dirname(target) or "."
    ensure_parent_directory(target, created_dirs)
    fd, tmp_path = tempfile.mkstemp(prefix="." + (os.path.basename(target) or "patch") + ".", suffix=".tmp", dir=directory)
    try:
        with os.fdopen(fd, "w", encoding="utf-8", newline="") as handle:
            handle.write(content)
        os.chmod(tmp_path, mode)
        return tmp_path
    except BaseException:
        try:
            os.remove(tmp_path)
        except FileNotFoundError:
            pass
        raise

def stage_backup_file(source):
    directory = os.path.dirname(source) or "."
    fd, backup_path = tempfile.mkstemp(prefix="." + (os.path.basename(source) or "patch") + ".", suffix=".bak", dir=directory)
    try:
        os.close(fd)
        os.remove(backup_path)
        return backup_path
    except BaseException:
        try:
            os.close(fd)
        except OSError:
            pass
        try:
            os.remove(backup_path)
        except FileNotFoundError:
            pass
        raise

def rename_patch_path(source, target):
    source_bytes = os.fsencode(source)
    target_bytes = os.fsencode(target)

    if sys.platform.startswith("linux"):
        libc = ctypes.CDLL(None, use_errno=True)
        renameat2 = getattr(libc, "renameat2", None)
        if renameat2 is not None:
            renameat2.argtypes = [ctypes.c_int, ctypes.c_char_p, ctypes.c_int, ctypes.c_char_p, ctypes.c_uint]
            renameat2.restype = ctypes.c_int
            if renameat2(-100, source_bytes, -100, target_bytes, 1) == 0:
                return
            error_number = ctypes.get_errno()
            if error_number not in (errno.ENOSYS, errno.EINVAL, errno.ENOTSUP, errno.EOPNOTSUPP):
                raise OSError(error_number, os.strerror(error_number), target)

    if sys.platform == "darwin":
        libc = ctypes.CDLL(None, use_errno=True)
        renamex_np = getattr(libc, "renamex_np", None)
        if renamex_np is not None:
            renamex_np.argtypes = [ctypes.c_char_p, ctypes.c_char_p, ctypes.c_uint]
            renamex_np.restype = ctypes.c_int
            if renamex_np(source_bytes, target_bytes, 0x00000004) == 0:
                return
            error_number = ctypes.get_errno()
            if error_number not in (errno.EINVAL, errno.ENOTSUP, errno.EOPNOTSUPP):
                raise OSError(error_number, os.strerror(error_number), target)

    if os.name == "nt":
        os.rename(source, target)
        return
    raise OSError(errno.ENOTSUP, "atomic no-replace rename is not supported", target)

def rename_patch_exchange(source, target):
    source_bytes = os.fsencode(source)
    target_bytes = os.fsencode(target)

    if sys.platform.startswith("linux"):
        libc = ctypes.CDLL(None, use_errno=True)
        renameat2 = getattr(libc, "renameat2", None)
        if renameat2 is not None:
            renameat2.argtypes = [ctypes.c_int, ctypes.c_char_p, ctypes.c_int, ctypes.c_char_p, ctypes.c_uint]
            renameat2.restype = ctypes.c_int
            if renameat2(-100, source_bytes, -100, target_bytes, 2) == 0:
                return
            error_number = ctypes.get_errno()
            raise OSError(error_number, os.strerror(error_number), target)

    if sys.platform == "darwin":
        libc = ctypes.CDLL(None, use_errno=True)
        renamex_np = getattr(libc, "renamex_np", None)
        if renamex_np is not None:
            renamex_np.argtypes = [ctypes.c_char_p, ctypes.c_char_p, ctypes.c_uint]
            renamex_np.restype = ctypes.c_int
            if renamex_np(source_bytes, target_bytes, 0x00000002) == 0:
                return
            error_number = ctypes.get_errno()
            raise OSError(error_number, os.strerror(error_number), target)

    raise OSError(errno.ENOTSUP, "atomic exchange rename is not supported", target)

def patch_before_install(entry):
    pass

def patch_after_install(entry):
    pass

def preflight_patch_source(entry):
    info = require_patch_file(entry["path"], entry["path"])
    if patch_path_identity(entry["path"], info) != entry["identity"]:
        patch_error("could not apply patch: file changed during patch: %s" % entry["path"])

def preflight_patch_entry(entry):
    if entry["kind"] == "add":
        if os.path.lexists(entry["path"]):
            patch_error("create file: %s already exists" % entry["path"])
        return

    preflight_patch_source(entry)
    if entry["kind"] in ("move", "write") and entry["target"] != entry["path"] and os.path.lexists(entry["target"]):
        patch_error("could not apply patch: %s already exists" % entry["target"])

def preflight_patch_plan(plan):
    for entry in plan:
        preflight_patch_entry(entry)

def stage_patch_plan(plan, created_dirs):
    for entry in plan:
        entry["tmp_path"] = None
        entry["backup_path"] = None
        entry["installed_identity"] = None
        entry["target_installed"] = False
        entry["source_removed"] = False
        entry["renamed"] = False
        entry["backup_created"] = False

        if entry["kind"] != "add":
            previous_signals = block_cancel_signals()
            try:
                entry["backup_path"] = stage_backup_file(entry["path"])
            finally:
                restore_cancel_signals(previous_signals)
        if entry["kind"] == "delete":
            continue
        if entry["kind"] == "move":
            previous_signals = block_cancel_signals()
            try:
                ensure_parent_directory(entry["target"], created_dirs)
            finally:
                restore_cancel_signals(previous_signals)
            continue

        target = entry["path"] if entry["kind"] == "add" else entry["target"]
        previous_signals = block_cancel_signals()
        try:
            entry["tmp_path"] = stage_temp_file(target, entry["content"], entry["mode"], created_dirs)
        finally:
            restore_cancel_signals(previous_signals)

def capture_patch_source(entry):
    if entry.get("backup_created"):
        preflight_patch_entry(entry)
        return
    preflight_patch_entry(entry)
    previous_signals = block_cancel_signals()
    try:
        backup_path = entry["backup_path"]
        try:
            source_info = require_patch_file(entry["path"], entry["path"])
            with open(entry["path"], "rb") as source, open(backup_path, "xb") as backup:
                while True:
                    chunk = source.read(1024 * 1024)
                    if not chunk:
                        break
                    backup.write(chunk)
            os.chmod(backup_path, stat.S_IMODE(source_info.st_mode))
            os.utime(backup_path, ns=(source_info.st_atime_ns, source_info.st_mtime_ns))
        except FileExistsError:
            recovery_path = entry["backup_path"]
            entry["backup_path"] = None
            patch_error("could not apply patch: recovery path already exists: %s" % recovery_path)
        preflight_patch_source(entry)
        backup_identity = patch_path_identity(backup_path, require_patch_file(backup_path, entry["path"]))
        backup_matches = backup_identity[2:] == entry["identity"][2:]
        if not backup_matches:
            patch_error("could not apply patch: recovery backup does not match source: %s" % entry["path"])
        entry["backup_created"] = True
    finally:
        restore_cancel_signals(previous_signals)

def remove_patch_source(entry):
    directory = os.path.dirname(entry["path"]) or "."
    fd, removed_path = tempfile.mkstemp(
        prefix="." + (os.path.basename(entry["path"]) or "patch") + ".",
        suffix=".removed",
        dir=directory,
    )
    os.close(fd)
    os.remove(removed_path)
    try:
        rename_patch_path(entry["path"], removed_path)
    except FileNotFoundError:
        patch_error("could not apply patch: file changed during patch: %s" % entry["path"])
    moved_identity = patch_path_identity(removed_path, require_patch_file(removed_path, entry["path"]))
    if moved_identity != entry["identity"]:
        try:
            rename_patch_path(removed_path, entry["path"])
        except OSError as exc:
            patch_error(
                "could not apply patch: file changed during patch; changed source preserved at %s: %s"
                % (removed_path, exc)
            )
        patch_error("could not apply patch: file changed during patch: %s" % entry["path"])
    os.remove(removed_path)
    entry["source_removed"] = True

def tracked_remove(path, entry):
    capture_patch_source(entry)
    remove_patch_source(entry)

def tracked_link(source, target, entry):
    if entry["kind"] == "add":
        if os.path.lexists(entry["path"]):
            patch_error("create file: %s already exists" % entry["path"])
    elif target != entry["path"] and os.path.lexists(target):
        patch_error("could not apply patch: %s already exists" % target)
    previous_signals = block_cancel_signals()
    try:
        rename_patch_path(source, target)
        entry["target_installed"] = True
        entry["installed_identity"] = patch_path_identity(target, require_patch_file(target, target))
    finally:
        restore_cancel_signals(previous_signals)

def tracked_replace(source, target, entry):
    capture_patch_source(entry)
    patch_before_install(entry)
    preflight_patch_entry(entry)
    staged_identity = patch_path_identity(source, require_patch_file(source, source))
    previous_signals = block_cancel_signals()
    try:
        rename_patch_exchange(source, target)
        entry["target_installed"] = True
        entry["source_removed"] = True
        entry["installed_identity"] = staged_identity
        patch_after_install(entry)
        live_identity = patch_path_identity(target, require_patch_file(target, target))
        displaced_identity = patch_path_identity(source, require_patch_file(source, entry["path"]))
        if displaced_identity != entry["identity"] or live_identity != staged_identity:
            entry["preserve_backup"] = True
            if live_identity == staged_identity:
                try:
                    rename_patch_exchange(source, target)
                    entry["target_installed"] = False
                    entry["source_removed"] = False
                except OSError:
                    entry["tmp_path"] = None
                    entry["displaced_path"] = source
            else:
                entry["tmp_path"] = None
                entry["displaced_path"] = source
            displaced_message = (
                " and displaced path preserved at %s" % entry["displaced_path"]
                if entry.get("displaced_path")
                else ""
            )
            patch_error(
                "could not apply patch: file changed during atomic replacement: %s; "
                "recovery backup preserved at %s%s"
                % (entry["path"], entry["backup_path"], displaced_message)
            )
    finally:
        restore_cancel_signals(previous_signals)

def tracked_rename(source, target, entry):
    capture_patch_source(entry)
    patch_before_install(entry)
    preflight_patch_entry(entry)
    previous_signals = block_cancel_signals()
    try:
        rename_patch_path(source, target)
        entry["target_installed"] = True
        entry["source_removed"] = True
        entry["installed_identity"] = entry["identity"]
        patch_after_install(entry)
        if patch_path_identity(target, require_patch_file(target, target)) != entry["identity"]:
            entry["preserve_backup"] = True
            patch_error(
                "could not apply patch: file changed during move: %s; recovery backup preserved at %s"
                % (entry["path"], entry["backup_path"])
            )
    finally:
        restore_cancel_signals(previous_signals)
    entry["renamed"] = True

def patch_path_matches_identity(path, identity):
    if identity is None or not os.path.lexists(path):
        return False
    try:
        info = os.lstat(path)
        if not stat.S_ISREG(info.st_mode):
            return False
        return patch_path_identity(path, info) == identity
    except (OSError, PatchError):
        return False

def rollback_remove_installed(entry, path):
    if not os.path.lexists(path):
        return None
    if not patch_path_matches_identity(path, entry.get("installed_identity")):
        return "rollback conflict: installed file was concurrently replaced: %s" % path
    try:
        os.remove(path)
        return None
    except OSError as exc:
        return "rollback could not remove installed file %s: %s" % (path, exc)

def rollback_restore_backup(entry, path):
    backup_path = entry.get("backup_path")
    if not backup_path:
        return "rollback recovery backup is unavailable for %s" % path
    try:
        rename_patch_path(backup_path, path)
        entry["backup_path"] = None
        entry["source_removed"] = False
        return None
    except FileExistsError:
        return "rollback conflict: %s already exists; recovery backup preserved at %s" % (path, backup_path)
    except OSError as exc:
        return "rollback could not restore %s; recovery backup preserved at %s: %s" % (path, backup_path, exc)

def rollback_replace_backup(entry, path):
    backup_path = entry.get("backup_path")
    if not backup_path:
        return "rollback recovery backup is unavailable for %s" % path
    if not os.path.lexists(path):
        return rollback_restore_backup(entry, path)
    if not patch_path_matches_identity(path, entry.get("installed_identity")):
        return "rollback conflict: installed file was concurrently replaced: %s; recovery backup preserved at %s" % (
            path,
            backup_path,
        )
    try:
        original_identity = patch_path_identity(backup_path, require_patch_file(backup_path, backup_path))
        rename_patch_exchange(backup_path, path)
    except OSError as exc:
        return "rollback could not atomically restore %s; recovery backup preserved at %s: %s" % (path, backup_path, exc)

    if patch_path_matches_identity(backup_path, entry.get("installed_identity")):
        try:
            os.remove(backup_path)
            entry["backup_path"] = None
            entry["target_installed"] = False
            entry["source_removed"] = False
            return None
        except OSError as exc:
            return "rollback restored %s but could not remove displaced installed file %s: %s" % (path, backup_path, exc)

    if patch_path_matches_identity(path, original_identity):
        try:
            rename_patch_exchange(backup_path, path)
            return "rollback conflict: installed file was concurrently replaced: %s; recovery backup preserved at %s" % (
                path,
                backup_path,
            )
        except OSError as exc:
            return "rollback conflict at %s; paths preserved at %s and %s: %s" % (path, path, backup_path, exc)
    return "rollback conflict at %s; paths preserved at %s and %s" % (path, path, backup_path)

def rollback_moved_entry(entry):
    source = entry["path"]
    target = entry["target"]
    backup_path = entry.get("backup_path")
    failures = []
    source_conflict = False

    if (
        entry["kind"] == "move"
        and not os.path.lexists(source)
        and patch_path_matches_identity(target, entry.get("installed_identity"))
    ):
        try:
            rename_patch_path(target, source)
            entry["target_installed"] = False
            entry["source_removed"] = False
            entry["renamed"] = False
            return failures
        except OSError as exc:
            failures.append("rollback could not restore move %s -> %s: %s" % (target, source, exc))

    if not os.path.lexists(source):
        if not backup_path:
            failures.append("rollback recovery backup is unavailable for %s" % source)
            source_conflict = True
        else:
            try:
                os.link(backup_path, source)
                entry["source_removed"] = False
            except FileExistsError:
                source_conflict = True
                failures.append("rollback conflict: %s already exists; recovery backup preserved at %s" % (source, backup_path))
            except OSError as exc:
                source_conflict = True
                failures.append("rollback could not restore %s; recovery backup preserved at %s: %s" % (source, backup_path, exc))
    elif not patch_path_matches_identity(source, entry.get("identity")):
        source_conflict = True
        failures.append("rollback conflict: concurrent source replacement preserved at %s" % source)

    if not source_conflict and os.path.lexists(target):
        if patch_path_matches_identity(target, entry.get("installed_identity")):
            try:
                os.remove(target)
                entry["target_installed"] = False
                entry["renamed"] = False
            except OSError as exc:
                failures.append("rollback could not remove installed target %s: %s" % (target, exc))
        else:
            failures.append("rollback conflict: moved file was concurrently replaced: %s" % target)

    return failures

def rollback_patch_plan(plan):
    global rollback_in_progress
    rollback_in_progress = True
    failures = []
    try:
        for entry in reversed(plan):
            try:
                if entry["kind"] == "move":
                    if entry.get("renamed") or entry.get("target_installed") or entry.get("source_removed"):
                        failures.extend(rollback_moved_entry(entry))
                    continue

                if entry["kind"] == "add":
                    if entry.get("target_installed"):
                        remove_failure = rollback_remove_installed(entry, entry["path"])
                        if remove_failure:
                            failures.append(remove_failure)
                    continue

                if entry["kind"] == "delete":
                    if entry.get("source_removed") and entry.get("backup_path"):
                        restore_failure = rollback_restore_backup(entry, entry["path"])
                        if restore_failure:
                            failures.append(restore_failure)
                    continue

                target = entry["target"]
                if target == entry["path"]:
                    if entry.get("target_installed") and entry.get("backup_path"):
                        restore_failure = rollback_replace_backup(entry, entry["path"])
                        if restore_failure:
                            failures.append(restore_failure)
                    continue
                if entry.get("target_installed") or entry.get("source_removed"):
                    failures.extend(rollback_moved_entry(entry))
            except Exception as exc:
                failures.append(str(exc))
    finally:
        rollback_in_progress = False
    return failures

def cleanup_patch_plan(plan, created_dirs, preserve_backups=False):
    global rollback_in_progress
    rollback_in_progress = True
    try:
        for entry in plan:
            keys = ("tmp_path",) if preserve_backups else ("tmp_path", "backup_path")
            for key in keys:
                path = entry.get(key)
                if path:
                    try:
                        os.remove(path)
                    except FileNotFoundError:
                        pass
                    entry[key] = None
        for directory in reversed(created_dirs):
            try:
                os.rmdir(directory)
            except OSError:
                pass
    finally:
        rollback_in_progress = False

def commit_patch_plan(plan):
    changed = 0
    created_dirs = []
    preserve_backups = False
    try:
        preflight_patch_plan(plan)
        stage_patch_plan(plan, created_dirs)
        preflight_patch_plan(plan)

        for entry in plan:
            if entry["kind"] == "delete":
                tracked_remove(entry["path"], entry)
                changed += 1
                continue

            if entry["kind"] == "move":
                try:
                    tracked_rename(entry["path"], entry["target"], entry)
                except FileExistsError:
                    patch_error("could not apply patch: %s already exists" % entry["target"])
                except OSError as exc:
                    patch_error("could not apply patch: move %s -> %s: %s" % (entry["path"], entry["target"], exc))
                changed += 1
                continue

            tmp_path = entry["tmp_path"]
            if entry["kind"] == "add":
                try:
                    tracked_link(tmp_path, entry["path"], entry)
                except FileExistsError:
                    patch_error("create file: %s already exists" % entry["path"])
                entry["tmp_path"] = None
                changed += 1
                continue

            target = entry["target"]
            if target == entry["path"]:
                tracked_replace(tmp_path, target, entry)
                os.remove(tmp_path)
                entry["tmp_path"] = None
                changed += 1
                continue

            capture_patch_source(entry)
            patch_before_install(entry)
            preflight_patch_entry(entry)
            try:
                tracked_link(tmp_path, target, entry)
            except FileExistsError:
                patch_error("could not apply patch: %s already exists" % target)
            entry["tmp_path"] = None
            patch_after_install(entry)
            if not patch_path_matches_identity(target, entry.get("installed_identity")):
                entry["preserve_backup"] = True
                patch_error(
                    "could not apply patch: installed target changed during patch: %s; "
                    "recovery backup preserved at %s" % (target, entry["backup_path"])
                )
            remove_patch_source(entry)
            changed += 1
        return changed
    except BaseException as exc:
        rollback_failures = rollback_patch_plan(plan)
        if rollback_failures or any(entry.get("preserve_backup") for entry in plan):
            preserve_backups = True
        if rollback_failures:
            recovery_paths = sorted(
                entry["backup_path"]
                for entry in plan
                if entry.get("backup_path") and os.path.lexists(entry["backup_path"])
            )
            recovery_message = ", ".join(recovery_paths) if recovery_paths else "none available"
            patch_error(
                "apply patch rollback failed after %s: %s; recovery backups preserved at: %s"
                % (exc, "; ".join(rollback_failures), recovery_message)
            )
        raise
    finally:
        cleanup_patch_plan(plan, created_dirs, preserve_backups)

def handle_apply_patch(req):
    patch = req.get("patch")
    if not isinstance(patch, str):
        fail("apply_patch: patch is required")
    cwd = req.get("cwd") or os.getcwd()

    try:
        plan = build_patch_plan(parse_patch(patch), cwd)
        changed = commit_patch_plan(plan)
    except PatchError as exc:
        fail(str(exc))

    summaries = [entry["summary"] for entry in plan]
    emit({"output": "\n".join(summaries), "files_changed": changed})

def main():
    op = sys.argv[1] if len(sys.argv) > 1 else ""
    req = load_request()

    if op == "view":
        handle_view(req)
        return
    if op == "read":
        handle_read(req)
        return
    if op == "create":
        handle_create(req)
        return
    if op == "write":
        handle_write(req)
        return
    if op == "edit":
        handle_edit(req)
        return
    if op == "grep":
        handle_grep(req)
        return
    if op == "glob":
        handle_glob(req)
        return
    if op == "apply_patch":
        handle_apply_patch(req)
        return

    fail("unknown filesystem runner op: %s" % op)

try:
    main()
except BrokenPipeError:
    pass
except PatchCanceled as exc:
    fail(str(exc))
except RootedWriteError as exc:
    fail(str(exc))
except SystemExit:
    raise
except Exception as exc:
    fail("filesystem runner internal error: %s" % exc)
`

type filesystemGrepCommand struct {
	Args            []string       `json:"args,omitempty"`
	Pattern         string         `json:"pattern,omitempty"`
	Paths           []string       `json:"paths,omitempty"`
	Glob            string         `json:"glob,omitempty"`
	OutputMode      GrepOutputMode `json:"output_mode,omitempty"`
	Type            string         `json:"type,omitempty"`
	CaseInsensitive bool           `json:"case_insensitive,omitempty"`
	AfterContext    int            `json:"after_context,omitempty"`
	BeforeContext   int            `json:"before_context,omitempty"`
	Context         int            `json:"context,omitempty"`
	LineNumbers     *bool          `json:"line_numbers,omitempty"`
	HeadLimit       int            `json:"head_limit,omitempty"`
	Multiline       bool           `json:"multiline,omitempty"`
	Cwd             string         `json:"cwd,omitempty"`
	AllowExit1      bool           `json:"allow_exit_1,omitempty"`
}

type filesystemCreateRequest struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type filesystemWriteRequest struct {
	Path      string `json:"path"`
	Data      []byte `json:"data"`
	Overwrite bool   `json:"overwrite"`
	Root      string `json:"root,omitempty"`
}

type filesystemEditRequest struct {
	Path   string `json:"path"`
	OldStr string `json:"old_str"`
	NewStr string `json:"new_str"`
}

func (c *Client) View(ctx context.Context, req ViewRequest) (ViewResult, error) {
	req = req.Normalize()
	req.Path = c.resolveRemotePath(req.Path)

	var result ViewResult
	if err := c.execFilesystemJSON(ctx, "view", req, &result, "view", true); err != nil {
		return ViewResult{}, err
	}
	return result, nil
}

func (c *Client) GrepFiles(ctx context.Context, req GrepRequest) (GrepResult, error) {
	req = req.Normalize()
	switch req.OutputMode {
	case GrepOutputModeContent, GrepOutputModeFilesWithMatches, GrepOutputModeCount:
	default:
		return GrepResult{}, fmt.Errorf("grep: unsupported output mode %q", req.OutputMode)
	}
	req.Cwd = c.resolveWorkdir(req.Cwd)

	var runnerResp struct {
		Output         string `json:"output"`
		Truncated      bool   `json:"truncated,omitempty"`
		Limit          int    `json:"limit,omitempty"`
		ByteLimit      int    `json:"byte_limit,omitempty"`
		SkippedFiles   int    `json:"skipped_files,omitempty"`
		InputByteLimit int    `json:"input_byte_limit,omitempty"`
	}
	if err := c.execFilesystemJSON(ctx, "grep", req, &runnerResp, "grep", true); err != nil {
		return GrepResult{}, err
	}

	return GrepResult{
		Output:         runnerResp.Output,
		OutputMode:     req.OutputMode,
		Paths:          req.EffectivePaths(),
		Truncated:      runnerResp.Truncated,
		Limit:          runnerResp.Limit,
		ByteLimit:      runnerResp.ByteLimit,
		SkippedFiles:   runnerResp.SkippedFiles,
		InputByteLimit: runnerResp.InputByteLimit,
	}, nil
}

func (c *Client) GlobFiles(ctx context.Context, req GlobRequest) (GlobResult, error) {
	req = req.Normalize()
	req.Cwd = c.resolveWorkdir(req.Cwd)

	var result GlobResult
	if err := c.execFilesystemJSON(ctx, "glob", req, &result, "glob", true); err != nil {
		return GlobResult{}, err
	}

	result.Paths = req.EffectivePaths()
	return result, nil
}

func (c *Client) ApplyPatch(ctx context.Context, req ApplyPatchRequest) (ApplyPatchResult, error) {
	if req.Cwd == "" {
		req.Cwd = c.GetWorkdir()
	}

	var result ApplyPatchResult
	if err := c.execFilesystemJSON(ctx, "apply_patch", req, &result, "apply patch", false); err != nil {
		return ApplyPatchResult{}, err
	}
	return result, nil
}

func (c *Client) createFileWithRunner(ctx context.Context, path, content string) error {
	return c.execFilesystemJSON(ctx, "create", filesystemCreateRequest{
		Path:    c.resolveRemotePath(path),
		Content: content,
	}, nil, "create file", false)
}

func (c *Client) writeFileWithRunner(ctx context.Context, path string, content []byte, overwrite bool) error {
	return c.WriteFileRooted(ctx, RootedWriteRequest{
		Path:      c.resolveRemotePath(path),
		Data:      content,
		Overwrite: overwrite,
		Root:      c.GetWorkdir(),
	})
}

func (c *Client) ReadFileRooted(ctx context.Context, req RootedReadRequest) ([]byte, error) {
	req.Path = resolveRootedRemotePath(req.Path, req.Root)
	var result struct {
		Data []byte `json:"data"`
	}
	if err := c.execFilesystemJSON(ctx, "read", req, &result, "read file", true); err != nil {
		return nil, err
	}
	return result.Data, nil
}

func (c *Client) WriteFileRooted(ctx context.Context, req RootedWriteRequest) error {
	if len(req.Data) > MaxFileTransferBytes {
		return fmt.Errorf("%w: %d bytes exceeds %d", ErrFileTransferTooLarge, len(req.Data), MaxFileTransferBytes)
	}
	req.Path = resolveRootedRemotePath(req.Path, req.Root)
	return c.execFilesystemJSON(ctx, "write", filesystemWriteRequest{
		Path:      req.Path,
		Data:      req.Data,
		Overwrite: req.Overwrite,
		Root:      req.Root,
	}, nil, "write file", false)
}

func resolveRootedRemotePath(path, root string) string {
	if path == "" || pathpkg.IsAbs(path) || root == "" {
		return path
	}
	return pathpkg.Join(root, path)
}

func (c *Client) editFileWithRunner(ctx context.Context, path, oldStr, newStr string) error {
	return c.execFilesystemJSON(ctx, "edit", filesystemEditRequest{
		Path:   c.resolveRemotePath(path),
		OldStr: oldStr,
		NewStr: newStr,
	}, nil, "edit file", false)
}

func (c *Client) buildFilesystemGrepRequest(req GrepRequest) (filesystemGrepCommand, error) {
	args := []string{"rg", "--color=never"}
	normalizedType := normalizeFilesystemSearchType(req.Type)

	switch req.OutputMode {
	case GrepOutputModeContent:
		args = append(args, "--with-filename", "--no-heading")
		if req.LineNumbers == nil || *req.LineNumbers {
			args = append(args, "-n")
		} else {
			args = append(args, "--no-line-number")
		}
	case GrepOutputModeFilesWithMatches:
		args = append(args, "--files-with-matches")
	case GrepOutputModeCount:
		args = append(args, "--with-filename", "--no-heading", "--count")
	default:
		return filesystemGrepCommand{}, fmt.Errorf("grep: unsupported output mode %q", req.OutputMode)
	}

	if req.Glob != "" {
		args = append(args, "-g", req.Glob)
	}
	if normalizedType != "" {
		args = append(args, "-t", normalizedType)
	}
	if req.CaseInsensitive {
		args = append(args, "-i")
	}
	if req.Context > 0 {
		args = append(args, "-C", fmt.Sprintf("%d", req.Context))
	} else {
		if req.BeforeContext > 0 {
			args = append(args, "-B", fmt.Sprintf("%d", req.BeforeContext))
		}
		if req.AfterContext > 0 {
			args = append(args, "-A", fmt.Sprintf("%d", req.AfterContext))
		}
	}
	if req.Multiline {
		args = append(args, "-U")
	}

	args = append(args, "--", req.Pattern)
	args = append(args, req.EffectivePaths()...)

	return filesystemGrepCommand{
		Args:            args,
		Pattern:         req.Pattern,
		Paths:           req.EffectivePaths(),
		Glob:            req.Glob,
		OutputMode:      req.OutputMode,
		Type:            normalizedType,
		CaseInsensitive: req.CaseInsensitive,
		AfterContext:    req.AfterContext,
		BeforeContext:   req.BeforeContext,
		Context:         req.Context,
		LineNumbers:     req.LineNumbers,
		HeadLimit:       req.HeadLimit,
		Multiline:       req.Multiline,
		Cwd:             c.resolveWorkdir(req.Cwd),
		AllowExit1:      true,
	}, nil
}

func normalizeFilesystemSearchType(fileType string) string {
	switch strings.ToLower(fileType) {
	case "tsx":
		return "ts"
	case "jsx":
		return "js"
	default:
		return strings.ToLower(fileType)
	}
}

func (c *Client) execFilesystemJSON(ctx context.Context, op string, payload any, out any, action string, readOnly bool) error {
	input, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("%s: marshal request: %w", action, err)
	}

	helperPath := c.FilesystemHelperPath()
	if helperPath == "" {
		return fmt.Errorf("%s: compatible deployed filesystem helper is unavailable", action)
	}
	command := shellQuote(helperPath) + " filesystem " + op

	var stdout, stderr string
	var exitCode int
	if readOnly {
		stdout, stderr, exitCode, err = c.execReadOnlyWithInput(ctx, command, input)
	} else {
		stdout, stderr, exitCode, err = c.execWithInput(ctx, command, input)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", action, err)
	}
	if exitCode != 0 {
		return formatCommandFailure(action, exitCode, stderr)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal([]byte(stdout), out); err != nil {
		return fmt.Errorf("%s: decode response: %w", action, err)
	}
	return nil
}

func (c *Client) execWithInput(ctx context.Context, command string, input []byte) (stdout string, stderr string, exitCode int, err error) {
	wrapped := envSecretsLoader + " && " + command
	sshConfigPath, _, _ := c.sshState()
	return c.runRemoteCommandWithInput(ctx, wrapped, input, sshConfigPath != "")
}

func (c *Client) execReadOnlyWithInput(ctx context.Context, command string, input []byte) (stdout string, stderr string, exitCode int, err error) {
	wrapped := envSecretsLoader + " && " + command
	sshConfigPath, _, _ := c.sshState()
	useMultiplex := sshConfigPath != ""
	stdout, stderr, exitCode, err = c.runRemoteCommandWithInput(ctx, wrapped, input, useMultiplex)
	if err != nil {
		return stdout, stderr, exitCode, err
	}
	trimmedStderr := strings.TrimSpace(stderr)
	if !isRetryableTransportFailure(useMultiplex, exitCode, stderr) && !(useMultiplex && exitCode == 255 && trimmedStderr == "" && !c.probeMultiplexing(ctx)) {
		return stdout, stderr, exitCode, nil
	}

	fmt.Fprintln(os.Stderr, "codespace-mcp: SSH transport failed for read-only command, retrying without multiplexing")
	c.disableMultiplexing()
	return c.runRemoteCommandWithInput(ctx, wrapped, input, false)
}

var _ RootedFileExecutor = (*Client)(nil)
