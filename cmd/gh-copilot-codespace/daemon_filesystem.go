package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/ekroon/gh-copilot-codespace/internal/daemonproto"
	"github.com/ekroon/gh-copilot-codespace/internal/ssh"
)

const (
	daemonViewMaxBytes          = ssh.MaxViewBytes
	daemonViewClassificationMax = 8 * 1024
	daemonDirectoryListMaxDepth = 2
	daemonDirectoryMaxEntries   = ssh.MaxDirectoryEntries
	daemonDirectoryMaxBytes     = ssh.MaxViewBytes
	daemonDirectoryReadBatch    = 128
	daemonGrepMaxOutputBytes    = ssh.MaxGrepOutputBytes
	// Fallback grep skips files and lines above these bounds and marks the result truncated.
	daemonGrepMaxInputBytes = ssh.MaxGrepInputBytes
	daemonGrepMaxLineBytes  = 4 * daemonGrepMaxOutputBytes
)

type daemonSearchRoot struct {
	original string
	resolved string
}

type daemonSearchFile struct {
	resolved string
	display  string
	rel      string
}

type daemonBadPatchError struct {
	message string
}

func (e *daemonBadPatchError) Error() string {
	return e.message
}

type daemonPatchOpKind string

const (
	daemonPatchAdd    daemonPatchOpKind = "add"
	daemonPatchDelete daemonPatchOpKind = "delete"
	daemonPatchUpdate daemonPatchOpKind = "update"
)

type daemonPatchLine struct {
	op   byte
	text string
}

type daemonPatchHunk struct {
	context   string
	lines     []daemonPatchLine
	endOfFile bool
}

type daemonPatchOperation struct {
	kind     daemonPatchOpKind
	path     string
	moveTo   string
	addLines []string
	hunks    []daemonPatchHunk
}

type daemonPatchAction struct {
	kind           daemonPatchOpKind
	sourcePath     string
	targetPath     string
	sourceDisplay  string
	targetDisplay  string
	sourceState    daemonPatchFileState
	content        string
	mode           os.FileMode
	renameOnly     bool
	stagedTempPath string
	stagedState    daemonPatchFileState
	createdDirs    []string
}

type daemonPatchFileState struct {
	info   os.FileInfo
	digest [sha256.Size]byte
}

type daemonPatchRollback struct {
	backupPath string
	undo       func() error
}

type daemonPatchHooks struct {
	afterStage           func() error
	afterActionValidate  func(index int) error
	beforeActionInstall  func(index int) error
	afterActionInstall   func(index int) error
	beforeMoveOnlyCommit func(source, target string) error
	readFileContent      func(context.Context, io.Reader) ([]byte, error)
	afterActionCommit    func(index int) error
}

type daemonViewHooks struct {
	afterRead func(int)
}

type daemonGrepHooks struct {
	afterRead  func()
	beforeFile func(daemonSearchFile) error
}

type daemonRootedFileHooks struct {
	afterParentOpen func() error
	afterRead       func(int)
	beforeCommit    func() error
	beforeInstall   func() error
	afterInstall    func() error
}

type daemonViewReader struct {
	ctx   context.Context
	src   io.Reader
	hooks daemonViewHooks
}

type daemonGrepReader struct {
	ctx   context.Context
	src   io.Reader
	hooks daemonGrepHooks
}

type daemonGrepInputReader struct {
	src       io.Reader
	remaining int64
}

func (r *daemonGrepInputReader) Read(p []byte) (int, error) {
	if r.remaining > 0 {
		if int64(len(p)) > r.remaining {
			p = p[:r.remaining]
		}
		n, err := r.src.Read(p)
		r.remaining -= int64(n)
		return n, err
	}

	var probe [1]byte
	n, err := r.src.Read(probe[:])
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, err
	}
	if n > 0 {
		return 0, errDaemonGrepInputLimitReached
	}
	return 0, err
}

func (r daemonGrepReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, errDaemonCanceled
	}
	n, err := r.src.Read(p)
	if n > 0 && r.hooks.afterRead != nil {
		r.hooks.afterRead()
	}
	if r.ctx.Err() != nil {
		return n, errDaemonCanceled
	}
	return n, err
}

func (r daemonViewReader) Read(p []byte) (int, error) {
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

type daemonPatchReservation struct {
	info        os.FileInfo
	description string
}

type daemonPatchReservations struct {
	paths map[string]string
	files []daemonPatchReservation
}

func daemonView(ctx context.Context, req ssh.ViewRequest) (ssh.ViewResult, error) {
	return daemonViewWithHooks(ctx, req, daemonViewHooks{})
}

func daemonViewWithHooks(ctx context.Context, req ssh.ViewRequest, hooks daemonViewHooks) (ssh.ViewResult, error) {
	req = req.Normalize()
	if err := ctx.Err(); err != nil {
		return ssh.ViewResult{}, errDaemonCanceled
	}
	if req.Path == "" {
		return ssh.ViewResult{}, fmt.Errorf("view path: path is required")
	}
	info, err := os.Lstat(req.Path)
	if err != nil {
		return ssh.ViewResult{}, fmt.Errorf("view path: %w", err)
	}
	if info.IsDir() {
		return daemonViewDirectory(ctx, req.Path)
	}
	if !info.Mode().IsRegular() {
		return ssh.ViewResult{}, fmt.Errorf("view path: %s is not a regular file", req.Path)
	}
	return daemonViewRegularFile(ctx, req.Path, req.ViewRange, req.ForceReadLargeFiles, hooks)
}

func daemonViewDirectory(ctx context.Context, root string) (ssh.ViewResult, error) {
	collector := daemonDirectoryCollector{
		entries: make([]string, 0, min(daemonDirectoryMaxEntries, 128)),
	}
	if _, err := daemonCollectDirectoryEntries(ctx, root, "", 1, &collector); err != nil {
		return ssh.ViewResult{}, err
	}
	return ssh.ViewResult{
		Kind:      ssh.ViewKindDirectory,
		Content:   collector.content.String(),
		Entries:   collector.entries,
		Truncated: collector.truncated,
		Limit:     daemonDirectoryMaxEntries,
		ByteLimit: daemonDirectoryMaxBytes,
	}, nil
}

type daemonDirectoryCollector struct {
	entries   []string
	content   strings.Builder
	truncated bool
}

func (c *daemonDirectoryCollector) add(entry string) bool {
	entryBytes := len(entry) + 1
	if len(c.entries) >= daemonDirectoryMaxEntries ||
		c.content.Len()+entryBytes > daemonDirectoryMaxBytes {
		c.truncated = true
		return false
	}
	c.entries = append(c.entries, entry)
	c.content.WriteString(entry)
	c.content.WriteByte('\n')
	return true
}

func daemonCollectDirectoryEntries(ctx context.Context, root, rel string, depth int, collector *daemonDirectoryCollector) (bool, error) {
	if depth > daemonDirectoryListMaxDepth {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return false, errDaemonCanceled
	}

	dir := root
	if rel != "" {
		dir = filepath.Join(root, rel)
	}
	handle, err := os.Open(dir)
	if err != nil {
		return false, fmt.Errorf("view directory: %w", err)
	}
	defer handle.Close()

	for {
		items, readErr := handle.ReadDir(daemonDirectoryReadBatch)
		sort.Slice(items, func(i, j int) bool {
			return items[i].Name() < items[j].Name()
		})
		for _, item := range items {
			if err := ctx.Err(); err != nil {
				return false, errDaemonCanceled
			}
			if daemonIsHiddenSearchEntry(item.Name()) {
				continue
			}
			childRel := item.Name()
			if rel != "" {
				childRel = filepath.Join(rel, item.Name())
			}
			display := filepath.ToSlash(childRel)
			if item.IsDir() {
				if !collector.add(display + "/") {
					return true, nil
				}
				if depth < daemonDirectoryListMaxDepth {
					stopped, err := daemonCollectDirectoryEntries(ctx, root, childRel, depth+1, collector)
					if err != nil {
						return false, err
					}
					if stopped {
						return true, nil
					}
				}
				continue
			}
			if !collector.add(display) {
				return true, nil
			}
		}
		switch {
		case readErr == nil:
			continue
		case errors.Is(readErr, io.EOF):
			return false, nil
		default:
			return false, fmt.Errorf("view directory: %w", readErr)
		}
	}
}

func daemonViewRegularFile(ctx context.Context, path string, viewRange []int, forceReadLargeFiles bool, hooks daemonViewHooks) (ssh.ViewResult, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return ssh.ViewResult{}, fmt.Errorf("view file: %w", err)
	}
	if !before.Mode().IsRegular() {
		return ssh.ViewResult{}, fmt.Errorf("view file: %s is not a regular file", path)
	}

	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return ssh.ViewResult{}, fmt.Errorf("view file: %w", err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return ssh.ViewResult{}, fmt.Errorf("view file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return ssh.ViewResult{}, fmt.Errorf("view file: %s is not a regular file", path)
	}
	if !os.SameFile(before, info) {
		return ssh.ViewResult{}, fmt.Errorf("view file: %s changed while opening", path)
	}

	sample := make([]byte, daemonViewClassificationMax)
	n, readErr := daemonViewReader{ctx: ctx, src: file, hooks: hooks}.Read(sample)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		if errors.Is(readErr, errDaemonCanceled) {
			return ssh.ViewResult{}, errDaemonCanceled
		}
		return ssh.ViewResult{}, fmt.Errorf("view file: %w", readErr)
	}
	if err := ctx.Err(); err != nil {
		return ssh.ViewResult{}, errDaemonCanceled
	}

	mimeType := daemonDetectMimeType(path, sample[:n])
	switch {
	case strings.HasPrefix(mimeType, "image/"):
		result := ssh.ViewResult{
			Kind:     ssh.ViewKindImage,
			Content:  daemonBinarySummary("Image file", mimeType, info.Size()),
			MimeType: mimeType,
		}
		if !forceReadLargeFiles && info.Size() > daemonViewMaxBytes {
			result.Truncated = true
			return result, nil
		}
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return ssh.ViewResult{}, fmt.Errorf("view file: %w", err)
		}
		reader := io.Reader(daemonViewReader{ctx: ctx, src: file, hooks: hooks})
		if !forceReadLargeFiles {
			reader = io.LimitReader(reader, daemonViewMaxBytes+1)
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			if errors.Is(err, errDaemonCanceled) {
				return ssh.ViewResult{}, errDaemonCanceled
			}
			return ssh.ViewResult{}, fmt.Errorf("view file: %w", err)
		}
		if !forceReadLargeFiles && len(data) > daemonViewMaxBytes {
			result.Truncated = true
			return result, nil
		}
		result.Base64Data = base64.StdEncoding.EncodeToString(data)
		return result, nil
	case daemonLooksBinary(sample[:n], mimeType):
		return ssh.ViewResult{
			Kind:      ssh.ViewKindFile,
			Content:   daemonBinarySummary("Binary file", mimeType, info.Size()),
			MimeType:  mimeType,
			Truncated: true,
		}, nil
	default:
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return ssh.ViewResult{}, fmt.Errorf("view file: %w", err)
		}
		return daemonViewTextFile(ctx, file, viewRange, forceReadLargeFiles, mimeType, info.Size(), hooks)
	}
}

func daemonViewTextFile(ctx context.Context, file *os.File, viewRange []int, forceReadLargeFiles bool, mimeType string, size int64, hooks daemonViewHooks) (ssh.ViewResult, error) {
	if err := ctx.Err(); err != nil {
		return ssh.ViewResult{}, errDaemonCanceled
	}

	reader := bufio.NewReader(daemonViewReader{ctx: ctx, src: file, hooks: hooks})
	var out strings.Builder
	truncated := false
	lineNo := 1
	lineStarted := false
	var pendingUTF8 []byte
	collectOutput := true

	for {
		if err := ctx.Err(); err != nil {
			return ssh.ViewResult{}, errDaemonCanceled
		}
		fragment, readErr := reader.ReadSlice('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, bufio.ErrBufferFull) {
			if errors.Is(readErr, errDaemonCanceled) {
				return ssh.ViewResult{}, errDaemonCanceled
			}
			return ssh.ViewResult{}, fmt.Errorf("view file: %w", readErr)
		}
		if len(fragment) == 0 && errors.Is(readErr, io.EOF) {
			break
		}

		lineEnded := len(fragment) > 0 && fragment[len(fragment)-1] == '\n'
		if lineEnded {
			fragment = fragment[:len(fragment)-1]
		}
		finalFragment := lineEnded || errors.Is(readErr, io.EOF)

		if collectOutput && daemonLineInRange(lineNo, viewRange) {
			if bytes.IndexByte(fragment, 0) >= 0 {
				return daemonBinaryViewResult(mimeType, size), nil
			}
			if !lineStarted {
				prefix := fmt.Sprintf("%d. ", lineNo)
				if !forceReadLargeFiles && out.Len()+len(prefix)+1 > daemonViewMaxBytes {
					truncated = true
					collectOutput = false
				} else {
					out.WriteString(prefix)
					lineStarted = true
				}
			}

			if collectOutput {
				pendingUTF8 = append(pendingUTF8, fragment...)
				completeBytes, invalid := daemonCompleteUTF8Prefix(pendingUTF8, finalFragment)
				if invalid {
					return daemonBinaryViewResult(mimeType, size), nil
				}
				if daemonAppendViewContent(&out, pendingUTF8[:completeBytes], forceReadLargeFiles) {
					truncated = true
					collectOutput = false
					pendingUTF8 = pendingUTF8[:0]
				} else {
					pendingUTF8 = append(pendingUTF8[:0], pendingUTF8[completeBytes:]...)
				}

				if finalFragment && collectOutput {
					out.WriteByte('\n')
					lineStarted = false
					pendingUTF8 = pendingUTF8[:0]
				}
			}
		}

		if finalFragment {
			lineNo++
			if len(viewRange) == 2 && viewRange[1] != -1 && lineNo > viewRange[1] {
				break
			}
		}
		if !collectOutput {
			break
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
	}

	return ssh.ViewResult{
		Kind:      ssh.ViewKindFile,
		Content:   out.String(),
		Truncated: truncated,
		MimeType:  mimeType,
	}, nil
}

func daemonBinaryViewResult(mimeType string, size int64) ssh.ViewResult {
	return ssh.ViewResult{
		Kind:      ssh.ViewKindFile,
		Content:   daemonBinarySummary("Binary file", mimeType, size),
		MimeType:  mimeType,
		Truncated: true,
	}
}

func daemonCompleteUTF8Prefix(data []byte, final bool) (int, bool) {
	offset := 0
	for offset < len(data) {
		if !utf8.FullRune(data[offset:]) {
			return offset, final
		}
		r, size := utf8.DecodeRune(data[offset:])
		if r == utf8.RuneError && size == 1 {
			return offset, true
		}
		offset += size
	}
	return offset, false
}

func daemonAppendViewContent(out *strings.Builder, content []byte, forceReadLargeFiles bool) bool {
	if forceReadLargeFiles {
		out.Write(content)
		return false
	}

	available := daemonViewMaxBytes - out.Len() - 1
	if available < 0 {
		available = 0
	}
	if len(content) <= available {
		out.Write(content)
		return false
	}

	cut := available
	for cut > 0 && !utf8.Valid(content[:cut]) {
		cut--
	}
	out.Write(content[:cut])
	if out.Len() < daemonViewMaxBytes {
		out.WriteByte('\n')
	}
	return true
}

func daemonDetectMimeType(path string, sample []byte) string {
	detected := strings.TrimSpace(strings.SplitN(http.DetectContentType(sample), ";", 2)[0])
	if strings.HasPrefix(detected, "image/") {
		return detected
	}
	if byExt := mime.TypeByExtension(strings.ToLower(filepath.Ext(path))); byExt != "" {
		return strings.TrimSpace(strings.SplitN(byExt, ";", 2)[0])
	}
	return detected
}

func daemonLooksBinary(sample []byte, mimeType string) bool {
	if len(sample) == 0 {
		return false
	}
	if strings.HasPrefix(mimeType, "image/") {
		return true
	}
	if bytes.IndexByte(sample, 0) >= 0 {
		return true
	}
	if _, invalid := daemonCompleteUTF8Prefix(sample, false); invalid {
		return true
	}
	if daemonMimeLooksText(mimeType) {
		return false
	}
	controlBytes := 0
	for _, b := range sample {
		if b < 0x20 && b != '\n' && b != '\r' && b != '\t' && b != '\f' {
			controlBytes++
		}
	}
	return controlBytes > len(sample)/10
}

func daemonMimeLooksText(mimeType string) bool {
	if strings.HasPrefix(mimeType, "text/") {
		return true
	}
	switch {
	case mimeType == "application/json",
		mimeType == "application/xml",
		mimeType == "application/javascript",
		mimeType == "application/x-sh",
		mimeType == "application/x-yaml",
		mimeType == "application/yaml",
		strings.HasSuffix(mimeType, "+json"),
		strings.HasSuffix(mimeType, "+xml"):
		return true
	default:
		return false
	}
}

func daemonBinarySummary(kind, mimeType string, size int64) string {
	return fmt.Sprintf("%s (%s), %d bytes\n", kind, mimeType, size)
}

func daemonGrepFiles(ctx context.Context, req ssh.GrepRequest) (ssh.GrepResult, error) {
	req = req.Normalize()
	if req.Pattern == "" {
		return ssh.GrepResult{}, fmt.Errorf("grep: pattern is required")
	}
	if err := ctx.Err(); err != nil {
		return ssh.GrepResult{}, errDaemonCanceled
	}
	if _, err := exec.LookPath("rg"); err == nil {
		return daemonGrepWithRG(ctx, req)
	} else if err != nil && !errors.Is(err, exec.ErrNotFound) {
		return ssh.GrepResult{}, fmt.Errorf("grep: locate rg: %w", err)
	}
	return daemonGrepFilesFallback(ctx, req)
}

func daemonGrepWithRG(ctx context.Context, req ssh.GrepRequest) (ssh.GrepResult, error) {
	args := []string{"--color=never", "--no-heading", "--with-filename"}
	switch req.OutputMode {
	case ssh.GrepOutputModeFilesWithMatches:
		args = append(args, "-l")
	case ssh.GrepOutputModeCount:
		args = append(args, "-c")
	default:
		if req.LineNumbers != nil && !*req.LineNumbers {
			args = append(args, "--no-line-number")
		} else {
			args = append(args, "-n")
		}
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
		args = append(args, "--multiline")
	}
	if req.Glob != "" {
		args = append(args, "--glob", req.Glob)
	}
	if req.Type != "" {
		args = append(args, "--type", daemonNormalizeSearchType(req.Type))
	}
	args = append(args, "--", req.Pattern)
	args = append(args, req.EffectivePaths()...)

	stdout, stderr, exitCode, truncated, err := daemonRunProcessWithLimits(
		ctx, req.Cwd, req.HeadLimit, daemonGrepMaxOutputBytes, "rg", args...,
	)
	if err != nil {
		return ssh.GrepResult{}, fmt.Errorf("grep: %w", err)
	}
	if exitCode > 1 {
		return ssh.GrepResult{}, daemonFormatCommandFailure("grep", exitCode, stderr)
	}
	return ssh.GrepResult{
		Output:     stdout,
		OutputMode: req.OutputMode,
		Paths:      req.EffectivePaths(),
		Truncated:  truncated,
		Limit:      daemonGrepMaxOutputBytes,
		ByteLimit:  daemonGrepMaxOutputBytes,
	}, nil
}

type daemonBoundedBuffer struct {
	max int
	buf bytes.Buffer
}

func (b *daemonBoundedBuffer) Write(p []byte) (int, error) {
	if remaining := b.max - b.buf.Len(); remaining > 0 {
		if remaining > len(p) {
			remaining = len(p)
		}
		_, _ = b.buf.Write(p[:remaining])
	}
	return len(p), nil
}

func (b *daemonBoundedBuffer) String() string {
	return b.buf.String()
}

func daemonRunProcessWithOutputLimit(ctx context.Context, cwd string, limit int, name string, args ...string) (stdout, stderr string, exitCode int, err error) {
	stdout, stderr, exitCode, _, err = daemonRunProcessWithLimits(ctx, cwd, limit, 0, name, args...)
	return stdout, stderr, exitCode, err
}

func daemonRunProcessWithLimits(ctx context.Context, cwd string, unitLimit, byteLimit int, name string, args ...string) (stdout, stderr string, exitCode int, truncated bool, err error) {
	if err := ctx.Err(); err != nil {
		return "", "", -1, false, errDaemonCanceled
	}

	cmd := exec.Command(name, args...)
	if cwd != "" {
		cmd.Dir = cwd
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return "", "", -1, false, fmt.Errorf("open stdout: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return "", "", -1, false, fmt.Errorf("open stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		if ctx.Err() != nil {
			return "", "", -1, false, errDaemonCanceled
		}
		return "", "", -1, false, fmt.Errorf("failed to execute command: %w", err)
	}

	pgid := cmd.Process.Pid
	if item, ok := ctx.Value(daemonInflightKey{}).(*daemonInflight); ok {
		item.setProcess(pgid)
		defer item.clearProcess(pgid)
	}
	processFinished := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = syscall.Kill(-pgid, syscall.SIGTERM)
		case <-processFinished:
		}
	}()

	var errBuf daemonBoundedBuffer
	errBuf.max = 64 * 1024
	stderrDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(&errBuf, stderrPipe)
		close(stderrDone)
	}()

	var out strings.Builder
	limitReached, readErr := daemonReadOutputUnits(stdoutPipe, unitLimit, byteLimit, &out)
	var stdoutDrainDone chan struct{}
	if limitReached {
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
		stdoutDrainDone = make(chan struct{})
		go func() {
			_, _ = io.Copy(io.Discard, stdoutPipe)
			close(stdoutDrainDone)
		}()
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var runErr error
	switch {
	case limitReached:
		select {
		case runErr = <-done:
		case <-time.After(2 * time.Second):
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			runErr = <-done
		}
	case readErr != nil:
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
		select {
		case runErr = <-done:
		case <-time.After(2 * time.Second):
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			runErr = <-done
		}
	case ctx.Err() != nil:
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
		select {
		case runErr = <-done:
		case <-time.After(2 * time.Second):
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			runErr = <-done
		}
	default:
		select {
		case runErr = <-done:
		case <-ctx.Done():
			_ = syscall.Kill(-pgid, syscall.SIGTERM)
			select {
			case runErr = <-done:
			case <-time.After(2 * time.Second):
				_ = syscall.Kill(-pgid, syscall.SIGKILL)
				runErr = <-done
			}
		}
	}
	close(processFinished)
	<-stderrDone
	if stdoutDrainDone != nil {
		<-stdoutDrainDone
	}

	stdout, stderr = out.String(), errBuf.String()
	if ctx.Err() != nil && !limitReached {
		return stdout, stderr, -1, false, errDaemonCanceled
	}
	if readErr != nil && !limitReached {
		return stdout, stderr, -1, false, fmt.Errorf("read stdout: %w", readErr)
	}
	if limitReached {
		return stdout, stderr, 0, true, nil
	}
	if runErr != nil {
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			return stdout, stderr, exitErr.ExitCode(), false, nil
		}
		return stdout, stderr, -1, false, fmt.Errorf("failed to execute command: %w", runErr)
	}
	return stdout, stderr, 0, false, nil
}

func daemonReadOutputUnits(reader io.Reader, unitLimit, byteLimit int, out *strings.Builder) (bool, error) {
	if unitLimit <= 0 && byteLimit <= 0 {
		_, err := io.Copy(out, reader)
		return false, err
	}

	buffered := bufio.NewReaderSize(reader, 32*1024)
	units := 0
	unitStarted := false
	for {
		fragment, err := buffered.ReadSlice('\n')
		if len(fragment) > 0 {
			unitStarted = true
			if byteLimit > 0 {
				remaining := byteLimit - out.Len()
				if remaining <= 0 {
					return true, nil
				}
				if len(fragment) > remaining {
					out.Write(fragment[:remaining])
					daemonTrimBuilderToValidUTF8(out)
					return true, nil
				}
			}
			out.Write(fragment)
			if fragment[len(fragment)-1] == '\n' {
				units++
				unitStarted = false
				if (unitLimit > 0 && units >= unitLimit) ||
					(byteLimit > 0 && out.Len() >= byteLimit) {
					return daemonProbeMoreOutput(buffered)
				}
			} else if byteLimit > 0 && out.Len() >= byteLimit {
				return daemonProbeMoreOutput(buffered)
			}
		}
		switch {
		case err == nil:
			continue
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			if unitStarted {
				units++
			}
			return false, nil
		default:
			return false, err
		}
	}
}

func daemonProbeMoreOutput(reader *bufio.Reader) (bool, error) {
	_, err := reader.ReadByte()
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, io.EOF):
		return false, nil
	default:
		return false, err
	}
}

func daemonTrimBuilderToValidUTF8(out *strings.Builder) {
	value := out.String()
	for value != "" && !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	out.Reset()
	out.WriteString(value)
}

var (
	errDaemonGrepLimitReached      = errors.New("grep head limit reached")
	errDaemonGrepInputLimitReached = errors.New("grep input limit reached")
)

type daemonGrepOutputCollector struct {
	limit        int
	byteLimit    int
	units        int
	skippedFiles int
	truncated    bool
	out          strings.Builder
}

func (c *daemonGrepOutputCollector) addLine(line string) bool {
	if (c.limit > 0 && c.units >= c.limit) ||
		(c.byteLimit > 0 && c.out.Len() >= c.byteLimit) {
		c.truncated = true
		return true
	}
	if c.byteLimit > 0 && c.out.Len()+len(line)+1 > c.byteLimit {
		remaining := c.byteLimit - c.out.Len()
		if remaining > 0 {
			if remaining <= len(line) {
				c.out.WriteString(line[:remaining])
			} else {
				c.out.WriteString(line)
				c.out.WriteByte('\n')
			}
		}
		daemonTrimBuilderToValidUTF8(&c.out)
		c.truncated = true
		return true
	}
	c.out.WriteString(line)
	c.out.WriteByte('\n')
	c.units++
	return false
}

func (c *daemonGrepOutputCollector) addText(text string) bool {
	for text != "" {
		index := strings.IndexByte(text, '\n')
		if index < 0 {
			return c.addLine(text)
		}
		if c.addLine(text[:index]) {
			return true
		}
		text = text[index+1:]
	}
	return false
}

func daemonGrepFilesFallback(ctx context.Context, req ssh.GrepRequest) (ssh.GrepResult, error) {
	return daemonGrepFilesFallbackWithHooks(ctx, req, daemonGrepHooks{})
}

func daemonGrepFilesFallbackWithHooks(ctx context.Context, req ssh.GrepRequest, hooks daemonGrepHooks) (ssh.GrepResult, error) {
	re, err := daemonCompileSearchRegexp(req.Pattern, req.CaseInsensitive)
	if err != nil {
		return ssh.GrepResult{}, fmt.Errorf("grep: %w", err)
	}

	roots := daemonResolveSearchRoots(req.EffectivePaths(), req.Cwd)
	collector := daemonGrepOutputCollector{
		limit:     req.HeadLimit,
		byteLimit: daemonGrepMaxOutputBytes,
	}

	if err := daemonWalkSearchFiles(ctx, roots, false, func(file daemonSearchFile) error {
		if hooks.beforeFile != nil {
			if err := hooks.beforeFile(file); err != nil {
				return err
			}
		}
		if !daemonSearchFileMatches(req.Glob, req.Type, file.rel) {
			return nil
		}
		input, err := os.OpenFile(file.resolved, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return err
		}
		defer input.Close()
		info, err := input.Stat()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if info.Size() > int64(daemonGrepMaxInputBytes) {
			collector.skippedFiles++
			collector.truncated = true
			return nil
		}
		reader := daemonGrepReader{ctx: ctx, src: input, hooks: hooks}

		sample := make([]byte, 512)
		sampleLength, sampleErr := reader.Read(sample)
		if sampleErr != nil && !errors.Is(sampleErr, io.EOF) {
			return sampleErr
		}
		sample = sample[:sampleLength]
		if daemonLooksBinary(sample, daemonDetectMimeType(file.resolved, sample)) {
			return nil
		}
		if _, err := input.Seek(0, io.SeekStart); err != nil {
			return err
		}
		boundedReader := &daemonGrepInputReader{
			src:       daemonGrepReader{ctx: ctx, src: input, hooks: hooks},
			remaining: int64(daemonGrepMaxInputBytes),
		}

		if req.Multiline {
			data, err := io.ReadAll(boundedReader)
			if err != nil {
				if errors.Is(err, errDaemonGrepInputLimitReached) {
					collector.skippedFiles++
					collector.truncated = true
					return nil
				}
				return err
			}
			text := strings.ReplaceAll(string(data), "\r\n", "\n")
			first := re.FindStringIndex(text)
			if first == nil {
				return nil
			}
			switch req.OutputMode {
			case ssh.GrepOutputModeFilesWithMatches:
				if collector.addLine(file.display) {
					return errDaemonGrepLimitReached
				}
			case ssh.GrepOutputModeCount:
				if collector.addLine(fmt.Sprintf("%s:%d", file.display, daemonCountRegexpMatches(re, text))) {
					return errDaemonGrepLimitReached
				}
			default:
				for offset := 0; offset <= len(text); {
					match := re.FindStringIndex(text[offset:])
					if match == nil {
						break
					}
					start, end := offset+match[0], offset+match[1]
					if collector.addText(file.display + ":" + text[start:end] + "\n") {
						return errDaemonGrepLimitReached
					}
					if end > offset {
						offset = end
					} else {
						offset++
					}
				}
			}
			return nil
		}

		switch req.OutputMode {
		case ssh.GrepOutputModeFilesWithMatches:
			matched, incomplete, err := daemonSearchReaderHasMatch(boundedReader, re)
			if err != nil {
				return err
			}
			if incomplete {
				collector.skippedFiles++
			}
			collector.truncated = collector.truncated || incomplete
			if matched && collector.addLine(file.display) {
				return errDaemonGrepLimitReached
			}
		case ssh.GrepOutputModeCount:
			count, incomplete, err := daemonCountReaderMatches(boundedReader, re)
			if err != nil {
				return err
			}
			if incomplete {
				collector.skippedFiles++
			}
			collector.truncated = collector.truncated || incomplete
			if count > 0 && collector.addLine(fmt.Sprintf("%s:%d", file.display, count)) {
				return errDaemonGrepLimitReached
			}
		default:
			if err := daemonCollectGrepContent(boundedReader, file.display, re, req, &collector); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		if !errors.Is(err, errDaemonGrepLimitReached) {
			return ssh.GrepResult{}, fmt.Errorf("grep: %w", err)
		}
	}

	return ssh.GrepResult{
		Output:         collector.out.String(),
		OutputMode:     req.OutputMode,
		Paths:          req.EffectivePaths(),
		Truncated:      collector.truncated,
		Limit:          daemonGrepMaxOutputBytes,
		ByteLimit:      daemonGrepMaxOutputBytes,
		SkippedFiles:   collector.skippedFiles,
		InputByteLimit: daemonGrepMaxInputBytes,
	}, nil
}

func daemonCountRegexpMatches(re *regexp.Regexp, text string) int {
	count := 0
	for offset := 0; offset <= len(text); {
		match := re.FindStringIndex(text[offset:])
		if match == nil {
			return count
		}
		count++
		end := offset + match[1]
		if end > offset {
			offset = end
		} else {
			offset++
		}
	}
	return count
}

func daemonSearchReaderHasMatch(reader io.Reader, re *regexp.Regexp) (bool, bool, error) {
	buffered := bufio.NewReader(reader)
	incomplete := false
	for {
		line, eof, hasLine, oversized, err := daemonReadSearchLine(buffered)
		if err != nil {
			if errors.Is(err, errDaemonGrepInputLimitReached) {
				return false, true, nil
			}
			return false, incomplete, err
		}
		if !hasLine {
			return false, incomplete, nil
		}
		if oversized {
			incomplete = true
		} else if re.MatchString(line) {
			return true, incomplete, nil
		}
		if eof {
			return false, incomplete, nil
		}
	}
}

func daemonCountReaderMatches(reader io.Reader, re *regexp.Regexp) (int, bool, error) {
	buffered := bufio.NewReader(reader)
	count := 0
	incomplete := false
	for {
		line, eof, hasLine, oversized, err := daemonReadSearchLine(buffered)
		if err != nil {
			if errors.Is(err, errDaemonGrepInputLimitReached) {
				return count, true, nil
			}
			return 0, incomplete, err
		}
		if !hasLine {
			return count, incomplete, nil
		}
		if oversized {
			incomplete = true
		} else if re.MatchString(line) {
			count++
		}
		if eof {
			return count, incomplete, nil
		}
	}
}

func daemonCollectGrepContent(reader io.Reader, display string, re *regexp.Regexp, req ssh.GrepRequest, collector *daemonGrepOutputCollector) error {
	before, after := req.BeforeContext, req.AfterContext
	if req.Context > 0 {
		before, after = req.Context, req.Context
	}
	if before < 0 {
		before = 0
	}
	if after < 0 {
		after = 0
	}
	if collector.byteLimit > 0 {
		if before > collector.byteLimit {
			before = collector.byteLimit
		}
		if after > collector.byteLimit {
			after = collector.byteLimit
		}
	}

	type bufferedLine struct {
		index int
		text  string
	}
	buffered := bufio.NewReader(reader)
	initialCapacity := before
	if initialCapacity > 1024 {
		initialCapacity = 1024
	}
	previous := make([]bufferedLine, 0, initialCapacity)
	hadGroup := false
	windowEnd := -1
	lastEmitted := -1
	lineIndex := 0
	incomplete := false

	markIncomplete := func() {
		if !incomplete {
			collector.skippedFiles++
			incomplete = true
		}
		collector.truncated = true
	}

	emit := func(line bufferedLine, match bool) error {
		separator := "-"
		if match {
			separator = ":"
		}
		showLineNumbers := req.LineNumbers == nil || *req.LineNumbers
		var rendered string
		if showLineNumbers {
			rendered = fmt.Sprintf("%s%s%d%s%s", display, separator, line.index+1, separator, line.text)
		} else {
			rendered = fmt.Sprintf("%s%s%s", display, separator, line.text)
		}
		if collector.addLine(rendered) {
			return errDaemonGrepLimitReached
		}
		lastEmitted = line.index
		return nil
	}

	for {
		line, eof, hasLine, oversized, err := daemonReadSearchLine(buffered)
		if err != nil {
			if errors.Is(err, errDaemonGrepInputLimitReached) {
				markIncomplete()
				return nil
			}
			return err
		}
		if !hasLine {
			return nil
		}
		if oversized {
			markIncomplete()
			previous = previous[:0]
			lineIndex++
			if eof {
				return nil
			}
			continue
		}
		current := bufferedLine{index: lineIndex, text: line}
		matched := re.MatchString(line)
		if matched {
			windowStart := lineIndex - before
			if windowStart < 0 {
				windowStart = 0
			}
			newGroup := !hadGroup || windowStart > windowEnd+1
			if newGroup && hadGroup {
				if collector.addLine("--") {
					return errDaemonGrepLimitReached
				}
			}
			for _, prior := range previous {
				if prior.index >= windowStart && prior.index > lastEmitted {
					if err := emit(prior, false); err != nil {
						return err
					}
				}
			}
			if err := emit(current, true); err != nil {
				return err
			}
			if lineIndex+after > windowEnd || newGroup {
				windowEnd = lineIndex + after
			}
			hadGroup = true
		} else if hadGroup && lineIndex <= windowEnd && lineIndex > lastEmitted {
			if err := emit(current, false); err != nil {
				return err
			}
		}

		if before > 0 {
			previous = append(previous, current)
			if len(previous) > before {
				previous = previous[len(previous)-before:]
			}
		}
		lineIndex++
		if eof {
			return nil
		}
	}
}

func daemonReadSearchLine(reader *bufio.Reader) (string, bool, bool, bool, error) {
	var line strings.Builder
	oversized := false
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(fragment) > 0 && !oversized {
			if line.Len()+len(fragment) > daemonGrepMaxLineBytes {
				line.Reset()
				oversized = true
			} else {
				line.Write(fragment)
			}
		}
		switch {
		case err == nil:
			if oversized {
				return "", false, true, true, nil
			}
			value := strings.TrimSuffix(line.String(), "\n")
			value = strings.TrimSuffix(value, "\r")
			return value, false, true, false, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			if oversized {
				return "", true, true, true, nil
			}
			value := line.String()
			if value == "" {
				return "", true, false, false, nil
			}
			value = strings.TrimSuffix(value, "\r")
			return value, true, true, false, nil
		default:
			return "", false, false, false, err
		}
	}
}

func daemonRenderGrepContent(display string, fileLines []string, matchSet map[int]bool, req ssh.GrepRequest) string {
	before, after := req.BeforeContext, req.AfterContext
	if req.Context > 0 {
		before, after = req.Context, req.Context
	}

	type window struct {
		start int
		end   int
	}
	windows := make([]window, 0, len(matchSet))
	for i := range matchSet {
		start := i - before
		if start < 0 {
			start = 0
		}
		end := i + after
		if end >= len(fileLines) {
			end = len(fileLines) - 1
		}
		windows = append(windows, window{start: start, end: end})
	}
	if len(windows) == 0 {
		return ""
	}

	for i := 1; i < len(windows); i++ {
		for j := i; j > 0 && windows[j].start < windows[j-1].start; j-- {
			windows[j], windows[j-1] = windows[j-1], windows[j]
		}
	}

	merged := make([]window, 0, len(windows))
	for _, w := range windows {
		if len(merged) == 0 || w.start > merged[len(merged)-1].end+1 {
			merged = append(merged, w)
			continue
		}
		if w.end > merged[len(merged)-1].end {
			merged[len(merged)-1].end = w.end
		}
	}

	var out strings.Builder
	showLineNumbers := req.LineNumbers == nil || *req.LineNumbers
	for index, w := range merged {
		if index > 0 {
			out.WriteString("--\n")
		}
		for lineIndex := w.start; lineIndex <= w.end; lineIndex++ {
			sep := "-"
			if matchSet[lineIndex] {
				sep = ":"
			}
			if showLineNumbers {
				fmt.Fprintf(&out, "%s%s%d%s%s\n", display, sep, lineIndex+1, sep, fileLines[lineIndex])
			} else {
				fmt.Fprintf(&out, "%s%s%s\n", display, sep, fileLines[lineIndex])
			}
		}
	}
	return out.String()
}

func daemonGlobFiles(ctx context.Context, req ssh.GlobRequest) (ssh.GlobResult, error) {
	req = req.Normalize()
	if req.Pattern == "" {
		return ssh.GlobResult{}, fmt.Errorf("glob: pattern is required")
	}
	if err := ctx.Err(); err != nil {
		return ssh.GlobResult{}, errDaemonCanceled
	}
	if _, err := exec.LookPath("fd"); err == nil {
		return daemonGlobWithFD(ctx, req)
	} else if err != nil && !errors.Is(err, exec.ErrNotFound) {
		return ssh.GlobResult{}, fmt.Errorf("glob: locate fd: %w", err)
	}
	return daemonGlobFilesFallback(ctx, req)
}

func daemonGlobWithFD(ctx context.Context, req ssh.GlobRequest) (ssh.GlobResult, error) {
	args := []string{"--type", "f", "--color", "never", "--hidden", "--no-ignore", "--glob", "--exclude", ".git", "--", req.Pattern}
	args = append(args, req.EffectivePaths()...)
	stdout, stderr, exitCode, err := daemonRunProcessWithOutputLimit(ctx, req.Cwd, req.Limit+1, "fd", args...)
	if err != nil {
		return ssh.GlobResult{}, fmt.Errorf("glob: %w", err)
	}
	if exitCode > 1 {
		return ssh.GlobResult{}, daemonFormatCommandFailure("glob", exitCode, stderr)
	}
	lines := daemonSplitTextLines(stdout)
	truncated := len(lines) > req.Limit
	if truncated {
		lines = lines[:req.Limit]
	}
	return ssh.GlobResult{
		Output:    daemonJoinLines(lines),
		Paths:     req.EffectivePaths(),
		Truncated: truncated,
		Limit:     req.Limit,
	}, nil
}

var errDaemonGlobLimitReached = errors.New("glob result limit reached")

func daemonGlobFilesFallback(ctx context.Context, req ssh.GlobRequest) (ssh.GlobResult, error) {
	roots := daemonResolveSearchRoots(req.EffectivePaths(), req.Cwd)
	lines := make([]string, 0)
	truncated := false
	if err := daemonWalkSearchFiles(ctx, roots, true, func(file daemonSearchFile) error {
		if daemonMatchGlobPattern(req.Pattern, file.rel) {
			if len(lines) >= req.Limit {
				truncated = true
				return errDaemonGlobLimitReached
			}
			lines = append(lines, file.display)
		}
		return nil
	}); err != nil && !errors.Is(err, errDaemonGlobLimitReached) {
		return ssh.GlobResult{}, fmt.Errorf("glob: %w", err)
	}
	return ssh.GlobResult{
		Output:    daemonJoinLines(lines),
		Paths:     req.EffectivePaths(),
		Truncated: truncated,
		Limit:     req.Limit,
	}, nil
}

func daemonResolveSearchRoots(paths []string, cwd string) []daemonSearchRoot {
	roots := make([]daemonSearchRoot, 0, len(paths))
	for _, p := range paths {
		if p == "" {
			p = "."
		}
		roots = append(roots, daemonSearchRoot{
			original: p,
			resolved: daemonResolvePath(cwd, p),
		})
	}
	return roots
}

func daemonResolvePath(cwd, p string) string {
	if p == "" {
		p = "."
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	if cwd == "" {
		return filepath.Clean(p)
	}
	return filepath.Clean(filepath.Join(cwd, p))
}

func daemonWalkSearchFiles(ctx context.Context, roots []daemonSearchRoot, includeHidden bool, visit func(daemonSearchFile) error) error {
	for _, root := range roots {
		if err := ctx.Err(); err != nil {
			return errDaemonCanceled
		}
		info, err := os.Lstat(root.resolved)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			if !info.Mode().IsRegular() {
				continue
			}
			rel := filepath.Base(root.original)
			if filepath.IsAbs(root.original) {
				rel = filepath.Base(root.resolved)
			}
			if rel == "." || rel == "" {
				rel = filepath.Base(root.resolved)
			}
			if daemonShouldSkipSearchEntry(filepath.Base(root.resolved), includeHidden) {
				continue
			}
			if err := visit(daemonSearchFile{
				resolved: root.resolved,
				display:  daemonDisplayPath(root.original, "."),
				rel:      filepath.ToSlash(rel),
			}); err != nil {
				return err
			}
			continue
		}

		if err := filepath.WalkDir(root.resolved, func(current string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if err := ctx.Err(); err != nil {
				return errDaemonCanceled
			}
			if current != root.resolved && daemonShouldSkipSearchEntry(entry.Name(), includeHidden) {
				if entry.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if entry.IsDir() {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return nil
			}
			rel, err := filepath.Rel(root.resolved, current)
			if err != nil {
				return err
			}
			return visit(daemonSearchFile{
				resolved: current,
				display:  daemonDisplayPath(root.original, rel),
				rel:      filepath.ToSlash(rel),
			})
		}); err != nil {
			return err
		}
	}
	return nil
}

func daemonDisplayPath(root, rel string) string {
	root = filepath.ToSlash(filepath.Clean(root))
	rel = filepath.ToSlash(rel)
	switch {
	case rel == "." || rel == "":
		if root == "." {
			return "."
		}
		return root
	case root == "." || root == "":
		return rel
	default:
		return strings.TrimSuffix(root, "/") + "/" + rel
	}
}

func daemonIsHiddenSearchEntry(name string) bool {
	return strings.HasPrefix(name, ".")
}

func daemonShouldSkipSearchEntry(name string, includeHidden bool) bool {
	return name == ".git" || (!includeHidden && daemonIsHiddenSearchEntry(name))
}

func daemonCompileSearchRegexp(pattern string, caseInsensitive bool) (*regexp.Regexp, error) {
	if caseInsensitive {
		pattern = "(?i)" + pattern
	}
	return regexp.Compile(pattern)
}

func daemonSearchFileMatches(globPattern, fileType, rel string) bool {
	if globPattern != "" && !daemonMatchGlobPattern(globPattern, rel) {
		return false
	}
	return daemonMatchType(fileType, rel)
}

func daemonNormalizeSearchType(fileType string) string {
	switch strings.ToLower(fileType) {
	case "tsx":
		return "ts"
	case "jsx":
		return "js"
	default:
		return strings.ToLower(fileType)
	}
}

func daemonMatchType(fileType, rel string) bool {
	if fileType == "" {
		return true
	}
	normalized := daemonNormalizeSearchType(fileType)
	ext := strings.ToLower(filepath.Ext(rel))
	switch normalized {
	case "go":
		return ext == ".go"
	case "js":
		return ext == ".js" || ext == ".jsx" || ext == ".mjs" || ext == ".cjs"
	case "ts":
		return ext == ".ts" || ext == ".tsx" || ext == ".mts" || ext == ".cts"
	case "py":
		return ext == ".py"
	case "rust":
		return ext == ".rs"
	case "java":
		return ext == ".java"
	default:
		return ext == "."+normalized
	}
}

func daemonMatchGlobPattern(pattern, rel string) bool {
	if pattern == "" {
		return true
	}
	rel = filepath.ToSlash(rel)
	rel = strings.TrimPrefix(rel, "./")
	for _, expanded := range daemonExpandBraces(filepath.ToSlash(pattern)) {
		expanded = strings.TrimPrefix(expanded, "./")
		if !strings.Contains(expanded, "/") {
			ok, err := path.Match(expanded, path.Base(rel))
			if err == nil && ok {
				return true
			}
			continue
		}
		if daemonMatchGlobSegments(strings.Split(expanded, "/"), strings.Split(rel, "/")) {
			return true
		}
	}
	return false
}

func daemonMatchGlobSegments(patternSegments, pathSegments []string) bool {
	var match func(int, int) bool
	match = func(patternIndex, pathIndex int) bool {
		for patternIndex < len(patternSegments) {
			segment := patternSegments[patternIndex]
			if segment == "**" {
				if patternIndex == len(patternSegments)-1 {
					return true
				}
				for next := pathIndex; next <= len(pathSegments); next++ {
					if match(patternIndex+1, next) {
						return true
					}
				}
				return false
			}
			if pathIndex >= len(pathSegments) {
				return false
			}
			ok, err := path.Match(segment, pathSegments[pathIndex])
			if err != nil || !ok {
				return false
			}
			patternIndex++
			pathIndex++
		}
		return pathIndex == len(pathSegments)
	}
	return match(0, 0)
}

func daemonExpandBraces(pattern string) []string {
	start := strings.IndexByte(pattern, '{')
	if start < 0 {
		return []string{pattern}
	}
	depth := 0
	end := -1
	for i := start; i < len(pattern); i++ {
		switch pattern[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				end = i
				i = len(pattern)
			}
		}
	}
	if end < 0 {
		return []string{pattern}
	}
	prefix := pattern[:start]
	suffix := pattern[end+1:]
	parts := daemonSplitBraceAlternatives(pattern[start+1 : end])
	results := make([]string, 0, len(parts))
	for _, part := range parts {
		for _, expandedSuffix := range daemonExpandBraces(suffix) {
			results = append(results, prefix+part+expandedSuffix)
		}
	}
	return results
}

func daemonSplitBraceAlternatives(value string) []string {
	parts := make([]string, 0, 2)
	depth := 0
	start := 0
	for i, r := range value {
		switch r {
		case '{':
			depth++
		case '}':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, value[start:i])
				start = i + 1
			}
		}
	}
	parts = append(parts, value[start:])
	return parts
}

func daemonLimitLines(output string, limit int) string {
	if limit <= 0 || output == "" {
		return output
	}
	hasTrailingNewline := strings.HasSuffix(output, "\n")
	trimmed := strings.TrimRight(output, "\n")
	if trimmed == "" {
		return output
	}
	lines := strings.Split(trimmed, "\n")
	if len(lines) > limit {
		lines = lines[:limit]
		hasTrailingNewline = true
	}
	result := strings.Join(lines, "\n")
	if hasTrailingNewline {
		result += "\n"
	}
	return result
}

func daemonJoinLines(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

func daemonSplitTextLines(text string) []string {
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func daemonSample(data []byte) []byte {
	if len(data) > 512 {
		return data[:512]
	}
	return data
}

func daemonApplyPatch(ctx context.Context, req ssh.ApplyPatchRequest) (ssh.ApplyPatchResult, error) {
	return daemonApplyPatchWithHooks(ctx, req, daemonPatchHooks{})
}

func daemonApplyPatchWithHook(ctx context.Context, req ssh.ApplyPatchRequest, afterStage func() error) (ssh.ApplyPatchResult, error) {
	return daemonApplyPatchWithHooks(ctx, req, daemonPatchHooks{afterStage: afterStage})
}

func daemonApplyPatchWithHooks(ctx context.Context, req ssh.ApplyPatchRequest, hooks daemonPatchHooks) (ssh.ApplyPatchResult, error) {
	if err := ctx.Err(); err != nil {
		return ssh.ApplyPatchResult{}, errDaemonCanceled
	}
	operations, err := daemonParsePatch(req.Patch)
	if err != nil {
		return ssh.ApplyPatchResult{}, err
	}
	actions, summaries, err := daemonBuildPatchPlan(ctx, operations, req.Cwd, hooks)
	if err != nil {
		return ssh.ApplyPatchResult{}, err
	}
	if err := daemonCommitPatchPlanWithHooks(ctx, actions, hooks); err != nil {
		return ssh.ApplyPatchResult{}, err
	}
	return ssh.ApplyPatchResult{
		Output:       strings.Join(summaries, "\n"),
		FilesChanged: len(summaries),
	}, nil
}

func daemonIsBadPatch(err error) bool {
	var target *daemonBadPatchError
	return errors.As(err, &target)
}

func daemonBadPatchf(format string, args ...any) error {
	return &daemonBadPatchError{message: fmt.Sprintf(format, args...)}
}

func daemonParsePatch(patch string) ([]daemonPatchOperation, error) {
	lines := strings.Split(strings.ReplaceAll(patch, "\r\n", "\n"), "\n")
	if len(lines) == 0 || lines[0] != "*** Begin Patch" {
		return nil, daemonBadPatchf("invalid patch: missing *** Begin Patch")
	}

	ops := make([]daemonPatchOperation, 0)
	for index := 1; index < len(lines); {
		line := lines[index]
		switch {
		case line == "*** End Patch":
			for _, trailing := range lines[index+1:] {
				if trailing != "" {
					return nil, daemonBadPatchf("invalid patch: unexpected content after *** End Patch")
				}
			}
			if len(ops) == 0 {
				return nil, daemonBadPatchf("invalid patch: no file operations")
			}
			return ops, nil
		case strings.HasPrefix(line, "*** Add File: "):
			op, next, err := daemonParseAddPatch(lines, index)
			if err != nil {
				return nil, err
			}
			ops = append(ops, op)
			index = next
		case strings.HasPrefix(line, "*** Delete File: "):
			op, next, err := daemonParseDeletePatch(lines, index)
			if err != nil {
				return nil, err
			}
			ops = append(ops, op)
			index = next
		case strings.HasPrefix(line, "*** Update File: "):
			op, next, err := daemonParseUpdatePatch(lines, index)
			if err != nil {
				return nil, err
			}
			ops = append(ops, op)
			index = next
		case line == "":
			return nil, daemonBadPatchf("invalid patch: unexpected blank line")
		default:
			return nil, daemonBadPatchf("invalid patch: unexpected line %q", line)
		}
	}

	return nil, daemonBadPatchf("invalid patch: missing *** End Patch")
}

func daemonParseAddPatch(lines []string, start int) (daemonPatchOperation, int, error) {
	path := strings.TrimSpace(strings.TrimPrefix(lines[start], "*** Add File: "))
	if path == "" {
		return daemonPatchOperation{}, 0, daemonBadPatchf("invalid patch: add file path is required")
	}
	addLines := make([]string, 0)
	index := start + 1
	for index < len(lines) {
		line := lines[index]
		if strings.HasPrefix(line, "*** ") {
			break
		}
		if !strings.HasPrefix(line, "+") {
			return daemonPatchOperation{}, 0, daemonBadPatchf("invalid patch: add file %s contains non-add line %q", path, line)
		}
		addLines = append(addLines, line[1:])
		index++
	}
	if len(addLines) == 0 {
		return daemonPatchOperation{}, 0, daemonBadPatchf("invalid patch: add file %s has no content", path)
	}
	return daemonPatchOperation{kind: daemonPatchAdd, path: path, addLines: addLines}, index, nil
}

func daemonParseDeletePatch(lines []string, start int) (daemonPatchOperation, int, error) {
	path := strings.TrimSpace(strings.TrimPrefix(lines[start], "*** Delete File: "))
	if path == "" {
		return daemonPatchOperation{}, 0, daemonBadPatchf("invalid patch: delete file path is required")
	}
	return daemonPatchOperation{kind: daemonPatchDelete, path: path}, start + 1, nil
}

func daemonParseUpdatePatch(lines []string, start int) (daemonPatchOperation, int, error) {
	path := strings.TrimSpace(strings.TrimPrefix(lines[start], "*** Update File: "))
	if path == "" {
		return daemonPatchOperation{}, 0, daemonBadPatchf("invalid patch: update file path is required")
	}

	index := start + 1
	moveTo := ""
	if index < len(lines) && strings.HasPrefix(lines[index], "*** Move to: ") {
		moveTo = strings.TrimSpace(strings.TrimPrefix(lines[index], "*** Move to: "))
		if moveTo == "" {
			return daemonPatchOperation{}, 0, daemonBadPatchf("invalid patch: move target is required for %s", path)
		}
		index++
	}

	hunks := make([]daemonPatchHunk, 0)
	for index < len(lines) {
		line := lines[index]
		if strings.HasPrefix(line, "*** ") {
			break
		}
		if line != "@@" && !strings.HasPrefix(line, "@@ ") {
			return daemonPatchOperation{}, 0, daemonBadPatchf("invalid patch: expected @@ for %s, got %q", path, line)
		}
		hunkContext := ""
		if strings.HasPrefix(line, "@@ ") {
			hunkContext = strings.TrimPrefix(line, "@@ ")
		}
		index++

		hunk := daemonPatchHunk{context: hunkContext}
		for index < len(lines) {
			line = lines[index]
			switch {
			case line == "*** End of File":
				hunk.endOfFile = true
				index++
				goto nextHunk
			case line == "@@" || strings.HasPrefix(line, "@@ ") || strings.HasPrefix(line, "*** "):
				goto nextHunk
			case line == "":
				return daemonPatchOperation{}, 0, daemonBadPatchf("invalid patch: unexpected blank line in hunk for %s", path)
			}

			prefix := line[0]
			if prefix != ' ' && prefix != '+' && prefix != '-' {
				return daemonPatchOperation{}, 0, daemonBadPatchf("invalid patch: bad hunk line %q", line)
			}
			hunk.lines = append(hunk.lines, daemonPatchLine{op: prefix, text: line[1:]})
			index++
		}

	nextHunk:
		if len(hunk.lines) == 0 {
			return daemonPatchOperation{}, 0, daemonBadPatchf("invalid patch: empty hunk for %s", path)
		}
		hunks = append(hunks, hunk)
	}

	if len(hunks) == 0 && moveTo == "" {
		return daemonPatchOperation{}, 0, daemonBadPatchf("invalid patch: update file %s has no changes", path)
	}
	return daemonPatchOperation{kind: daemonPatchUpdate, path: path, moveTo: moveTo, hunks: hunks}, index, nil
}

func daemonBuildPatchPlan(ctx context.Context, ops []daemonPatchOperation, cwd string, hooks daemonPatchHooks) ([]daemonPatchAction, []string, error) {
	if cwd == "" {
		cwd = "."
	}
	actions := make([]daemonPatchAction, 0, len(ops))
	summaries := make([]string, 0, len(ops))
	reservations := daemonPatchReservations{paths: make(map[string]string)}

	for _, op := range ops {
		if err := ctx.Err(); err != nil {
			return nil, nil, errDaemonCanceled
		}
		sourcePath, err := daemonResolvePatchPath(cwd, op.path)
		if err != nil {
			return nil, nil, fmt.Errorf("apply patch: resolve %s: %w", op.path, err)
		}
		switch op.kind {
		case daemonPatchAdd:
			if err := reservations.reservePath(sourcePath, fmt.Sprintf("add %s", op.path)); err != nil {
				return nil, nil, err
			}
			if _, err := os.Lstat(sourcePath); err == nil {
				return nil, nil, fmt.Errorf("apply patch: add file %s already exists", op.path)
			} else if !errors.Is(err, os.ErrNotExist) {
				return nil, nil, fmt.Errorf("apply patch: add file %s: %w", op.path, err)
			}
			actions = append(actions, daemonPatchAction{
				kind:          daemonPatchAdd,
				targetPath:    sourcePath,
				targetDisplay: op.path,
				content:       daemonJoinPatchLines(op.addLines),
				mode:          0o644,
			})
			summaries = append(summaries, fmt.Sprintf("Added %s", op.path))

		case daemonPatchDelete:
			description := fmt.Sprintf("delete %s", op.path)
			if err := reservations.reservePath(sourcePath, description); err != nil {
				return nil, nil, err
			}
			_, state, err := daemonReadPatchFile(ctx, sourcePath, false, hooks.readFileContent)
			if err != nil {
				return nil, nil, fmt.Errorf("apply patch: delete file %s: %w", op.path, err)
			}
			if err := reservations.reserveFile(state.info, description); err != nil {
				return nil, nil, err
			}
			actions = append(actions, daemonPatchAction{
				kind:          daemonPatchDelete,
				sourcePath:    sourcePath,
				sourceDisplay: op.path,
				sourceState:   state,
			})
			summaries = append(summaries, fmt.Sprintf("Deleted %s", op.path))

		case daemonPatchUpdate:
			targetDisplay := op.path
			targetPath := sourcePath
			if op.moveTo != "" {
				targetDisplay = op.moveTo
				targetPath, err = daemonResolvePatchPath(cwd, op.moveTo)
				if err != nil {
					return nil, nil, fmt.Errorf("apply patch: resolve %s: %w", op.moveTo, err)
				}
			}

			description := fmt.Sprintf("update %s", op.path)
			if err := reservations.reservePath(sourcePath, description); err != nil {
				return nil, nil, err
			}
			if op.moveTo != "" {
				if err := reservations.reservePath(targetPath, fmt.Sprintf("move target %s", op.moveTo)); err != nil {
					return nil, nil, err
				}
			}

			readContent := len(op.hunks) > 0
			data, state, err := daemonReadPatchFile(ctx, sourcePath, readContent, hooks.readFileContent)
			if err != nil {
				return nil, nil, fmt.Errorf("apply patch: update file %s: %w", op.path, err)
			}
			if err := reservations.reserveFile(state.info, description); err != nil {
				return nil, nil, err
			}
			if targetPath != sourcePath {
				if _, err := os.Lstat(targetPath); err == nil {
					return nil, nil, fmt.Errorf("apply patch: move target %s already exists", op.moveTo)
				} else if !errors.Is(err, os.ErrNotExist) {
					return nil, nil, fmt.Errorf("apply patch: move target %s: %w", op.moveTo, err)
				}
			}

			updated := ""
			if readContent {
				updated, err = daemonApplyPatchHunks(ctx, string(data), op.hunks)
				if err != nil {
					return nil, nil, fmt.Errorf("apply patch: update %s: %w", op.path, err)
				}
			}

			actions = append(actions, daemonPatchAction{
				kind:          daemonPatchUpdate,
				sourcePath:    sourcePath,
				targetPath:    targetPath,
				sourceDisplay: op.path,
				targetDisplay: targetDisplay,
				sourceState:   state,
				content:       updated,
				mode:          state.info.Mode().Perm(),
				renameOnly:    targetPath != sourcePath && len(op.hunks) == 0,
			})
			if targetPath == sourcePath {
				summaries = append(summaries, fmt.Sprintf("Updated %s", op.path))
			} else {
				summaries = append(summaries, fmt.Sprintf("Updated %s -> %s", op.path, op.moveTo))
			}
		}
	}

	return actions, summaries, nil
}

func daemonReadPatchFile(
	ctx context.Context,
	path string,
	readContent bool,
	contentReader func(context.Context, io.Reader) ([]byte, error),
) ([]byte, daemonPatchFileState, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, daemonPatchFileState{}, err
	}
	if before.Mode()&os.ModeSymlink != 0 {
		return nil, daemonPatchFileState{}, fmt.Errorf("symbolic link paths are not supported")
	}
	if !before.Mode().IsRegular() {
		return nil, daemonPatchFileState{}, fmt.Errorf("is not a regular file")
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, daemonPatchFileState{}, err
	}
	opened, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, daemonPatchFileState{}, err
	}
	if !daemonSamePatchFileMetadata(before, opened) {
		_ = file.Close()
		return nil, daemonPatchFileState{}, fmt.Errorf("file changed while opening")
	}

	hasher := sha256.New()
	var data []byte
	if readContent {
		if contentReader == nil {
			contentReader = daemonReadAllPatchContent
		}
		data, err = contentReader(ctx, io.TeeReader(file, hasher))
	} else {
		_, err = daemonCopyPatchContent(ctx, hasher, file)
	}
	closeErr := file.Close()
	if err != nil {
		return nil, daemonPatchFileState{}, err
	}
	if closeErr != nil {
		return nil, daemonPatchFileState{}, closeErr
	}

	after, err := os.Lstat(path)
	if err != nil {
		return nil, daemonPatchFileState{}, err
	}
	if after.Mode()&os.ModeSymlink != 0 || !daemonSamePatchFileMetadata(before, after) {
		return nil, daemonPatchFileState{}, fmt.Errorf("file changed while reading")
	}
	var digest [sha256.Size]byte
	copy(digest[:], hasher.Sum(nil))
	return data, daemonPatchFileState{
		info:   after,
		digest: digest,
	}, nil
}

func daemonReadAllPatchContent(ctx context.Context, reader io.Reader) ([]byte, error) {
	var content bytes.Buffer
	if _, err := daemonCopyPatchContent(ctx, &content, reader); err != nil {
		return nil, err
	}
	return content.Bytes(), nil
}

func daemonCopyPatchContent(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buffer := make([]byte, 64*1024)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, errDaemonCanceled
		}
		read, readErr := src.Read(buffer)
		if read > 0 {
			written, writeErr := dst.Write(buffer[:read])
			total += int64(written)
			if writeErr != nil {
				return total, writeErr
			}
			if written != read {
				return total, io.ErrShortWrite
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return total, nil
			}
			return total, readErr
		}
	}
}

func daemonSamePatchFileMetadata(left, right os.FileInfo) bool {
	return os.SameFile(left, right) &&
		left.Mode() == right.Mode() &&
		left.Size() == right.Size() &&
		left.ModTime().Equal(right.ModTime())
}

func daemonSamePatchFileState(left, right daemonPatchFileState) bool {
	return daemonSamePatchFileMetadata(left.info, right.info) && left.digest == right.digest
}

func (r *daemonPatchReservations) reservePath(resolvedPath, description string) error {
	if previous, exists := r.paths[resolvedPath]; exists {
		return daemonBadPatchf("invalid patch: %s conflicts with %s", description, previous)
	}
	r.paths[resolvedPath] = description
	return nil
}

func (r *daemonPatchReservations) reserveFile(info os.FileInfo, description string) error {
	for _, previous := range r.files {
		if os.SameFile(info, previous.info) {
			return daemonBadPatchf("invalid patch: %s conflicts with %s", description, previous.description)
		}
	}
	r.files = append(r.files, daemonPatchReservation{
		info:        info,
		description: description,
	})
	return nil
}

func daemonResolvePatchPath(cwd, patchPath string) (string, error) {
	resolved := daemonResolvePath(cwd, patchPath)
	absolute, err := filepath.Abs(resolved)
	if err != nil {
		return "", err
	}
	parent, err := daemonResolvePathSymlinks(filepath.Dir(absolute))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(absolute)), nil
}

func daemonJoinPatchLines(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

func daemonApplyPatchHunks(ctx context.Context, original string, hunks []daemonPatchHunk) (string, error) {
	if len(hunks) == 0 {
		return original, nil
	}
	normalized, lineEnding, err := daemonNormalizePatchLineEndings(original)
	if err != nil {
		return "", err
	}
	hasFinalNewline := strings.HasSuffix(normalized, "\n")
	lines := daemonSplitTextLines(normalized)
	output := make([]string, 0, len(lines))
	cursor := 0

	for _, hunk := range hunks {
		if err := ctx.Err(); err != nil {
			return "", errDaemonCanceled
		}
		oldLines, newLines := daemonPatchHunkLines(hunk)
		searchStart := cursor
		if hunk.context != "" {
			contextIndex, err := daemonFindPatchContext(lines, hunk.context, cursor)
			if err != nil {
				return "", err
			}
			searchStart = contextIndex + 1
		}
		matchIndex, err := daemonFindPatchHunk(lines, oldLines, searchStart, hunk.endOfFile)
		if err != nil {
			return "", err
		}
		output = append(output, lines[cursor:matchIndex]...)
		output = append(output, newLines...)
		cursor = matchIndex + len(oldLines)
	}

	output = append(output, lines[cursor:]...)
	result := strings.Join(output, lineEnding)
	if hasFinalNewline && len(output) > 0 {
		result += lineEnding
	}
	return result, nil
}

func daemonNormalizePatchLineEndings(original string) (normalized, lineEnding string, err error) {
	withoutCRLF := strings.ReplaceAll(original, "\r\n", "")
	hasCRLF := strings.Contains(original, "\r\n")
	hasLF := strings.Contains(withoutCRLF, "\n")
	hasBareCR := strings.Contains(withoutCRLF, "\r")
	if (hasCRLF && hasLF) || hasBareCR {
		return "", "", fmt.Errorf("mixed line endings are not supported")
	}
	if hasCRLF {
		return strings.ReplaceAll(original, "\r\n", "\n"), "\r\n", nil
	}
	return original, "\n", nil
}

func daemonPatchHunkLines(hunk daemonPatchHunk) ([]string, []string) {
	oldLines := make([]string, 0, len(hunk.lines))
	newLines := make([]string, 0, len(hunk.lines))
	for _, line := range hunk.lines {
		switch line.op {
		case ' ':
			oldLines = append(oldLines, line.text)
			newLines = append(newLines, line.text)
		case '-':
			oldLines = append(oldLines, line.text)
		case '+':
			newLines = append(newLines, line.text)
		}
	}
	return oldLines, newLines
}

func daemonFindPatchHunk(lines, oldLines []string, start int, endOfFile bool) (int, error) {
	if len(oldLines) == 0 {
		if endOfFile {
			return len(lines), nil
		}
		return start, nil
	}
	for index := start; index+len(oldLines) <= len(lines); index++ {
		if endOfFile && index+len(oldLines) != len(lines) {
			continue
		}
		if daemonEqualStrings(lines[index:index+len(oldLines)], oldLines) {
			return index, nil
		}
	}
	return 0, fmt.Errorf("hunk did not match file contents")
}

func daemonFindPatchContext(lines []string, contextLine string, start int) (int, error) {
	for index := start; index < len(lines); index++ {
		if lines[index] == contextLine {
			return index, nil
		}
	}
	return 0, fmt.Errorf("context %q did not match file contents", contextLine)
}

func daemonEqualStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func daemonCommitPatchPlan(ctx context.Context, actions []daemonPatchAction) error {
	return daemonCommitPatchPlanWithHooks(ctx, actions, daemonPatchHooks{})
}

func daemonCommitPatchPlanWithHook(ctx context.Context, actions []daemonPatchAction, afterStage func() error) error {
	return daemonCommitPatchPlanWithHooks(ctx, actions, daemonPatchHooks{afterStage: afterStage})
}

func daemonCommitPatchPlanWithHooks(ctx context.Context, actions []daemonPatchAction, hooks daemonPatchHooks) error {
	if err := daemonStagePatchPlan(ctx, actions); err != nil {
		return err
	}
	defer daemonCleanupPatchStages(actions)

	rollbacks := make([]daemonPatchRollback, 0, len(actions))
	fail := func(err error) error {
		rollbackErr := daemonRollbackPatchPlan(rollbacks)
		daemonCleanupPatchStages(actions)
		directoryErr := daemonCleanupPatchDirectories(actions)
		if cleanupErr := errors.Join(rollbackErr, directoryErr); cleanupErr != nil {
			return fmt.Errorf("%w (rollback failed: %v)", err, cleanupErr)
		}
		return err
	}

	if hooks.afterStage != nil {
		if err := hooks.afterStage(); err != nil {
			return fail(fmt.Errorf("apply patch: after staging: %w", err))
		}
	}
	if err := daemonRevalidatePatchPlan(ctx, actions); err != nil {
		return fail(err)
	}
	for index := range actions {
		if err := ctx.Err(); err != nil {
			return fail(errDaemonCanceled)
		}
		action := &actions[index]
		if err := daemonRevalidatePatchAction(ctx, action); err != nil {
			return fail(err)
		}
		if hooks.afterActionValidate != nil {
			if err := hooks.afterActionValidate(index); err != nil {
				return fail(fmt.Errorf("apply patch: after validating action %d: %w", index, err))
			}
		}
		rollback, err := daemonCommitPatchAction(ctx, action, index, hooks)
		if rollback.undo != nil {
			rollbacks = append(rollbacks, rollback)
		}
		if err != nil {
			return fail(err)
		}
		if hooks.afterActionCommit != nil {
			if err := hooks.afterActionCommit(index); err != nil {
				return fail(fmt.Errorf("apply patch: after action %d: %w", index, err))
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return fail(errDaemonCanceled)
	}
	for _, rollback := range rollbacks {
		if rollback.backupPath != "" {
			_ = os.Remove(rollback.backupPath)
		}
	}
	return nil
}

func daemonRevalidatePatchPlan(ctx context.Context, actions []daemonPatchAction) error {
	for index := range actions {
		if err := daemonRevalidatePatchAction(ctx, &actions[index]); err != nil {
			return err
		}
	}
	return nil
}

func daemonRevalidatePatchAction(ctx context.Context, action *daemonPatchAction) error {
	if err := ctx.Err(); err != nil {
		return errDaemonCanceled
	}

	if action.kind == daemonPatchAdd {
		if _, err := os.Lstat(action.targetPath); err == nil {
			return fmt.Errorf("apply patch: add %s: target appeared during patch", action.targetDisplay)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("apply patch: add %s: revalidate target: %w", action.targetDisplay, err)
		}
		return nil
	}

	_, currentState, err := daemonReadPatchFile(ctx, action.sourcePath, false, nil)
	if err != nil {
		return fmt.Errorf("apply patch: file changed during patch: %s: %w", action.sourceDisplay, err)
	}
	if !daemonSamePatchFileState(action.sourceState, currentState) {
		return fmt.Errorf("apply patch: file changed during patch: %s", action.sourceDisplay)
	}

	if action.kind == daemonPatchUpdate && action.targetPath != action.sourcePath {
		if _, err := os.Lstat(action.targetPath); err == nil {
			return fmt.Errorf("apply patch: move target %s already exists", action.targetDisplay)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("apply patch: revalidate move target %s: %w", action.targetDisplay, err)
		}
	}
	return nil
}

func daemonCommitPatchAction(ctx context.Context, action *daemonPatchAction, index int, hooks daemonPatchHooks) (daemonPatchRollback, error) {
	switch action.kind {
	case daemonPatchAdd:
		if err := daemonCommitStagedFile(action.targetPath, action.stagedTempPath, false); err != nil {
			return daemonPatchRollback{}, fmt.Errorf("apply patch: add %s: %w", action.targetDisplay, err)
		}
		action.stagedTempPath = ""
		targetPath := action.targetPath
		installedState := action.stagedState
		return daemonPatchRollback{
			undo: func() error {
				return daemonRollbackAddedPath(targetPath, installedState)
			},
		}, nil

	case daemonPatchDelete:
		backupPath, err := daemonCreatePatchBackup(ctx, action)
		if err != nil {
			return daemonPatchRollback{}, fmt.Errorf("apply patch: delete %s: %w", action.sourceDisplay, err)
		}
		sourcePath := action.sourcePath
		restoreSource := daemonPatchRollback{
			backupPath: backupPath,
			undo: func() error {
				if err := daemonRestorePatchBackupNoReplace(backupPath, sourcePath); err != nil {
					return fmt.Errorf("rollback delete %s: %w; original backup preserved at %s", sourcePath, err, backupPath)
				}
				return nil
			},
		}
		if err := daemonRemoveValidatedPatchSource(ctx, action); err != nil {
			_ = os.Remove(backupPath)
			return daemonPatchRollback{}, fmt.Errorf("apply patch: delete %s: %w", action.sourceDisplay, err)
		}
		return restoreSource, nil

	case daemonPatchUpdate:
		if action.renameOnly {
			if hooks.beforeMoveOnlyCommit != nil {
				if err := hooks.beforeMoveOnlyCommit(action.sourcePath, action.targetPath); err != nil {
					return daemonPatchRollback{}, fmt.Errorf("apply patch: before move %s -> %s: %w", action.sourceDisplay, action.targetDisplay, err)
				}
			}
		}

		backupPath, err := daemonCreatePatchBackup(ctx, action)
		if err != nil {
			return daemonPatchRollback{}, fmt.Errorf("apply patch: update %s: %w", action.sourceDisplay, err)
		}
		sourcePath := action.sourcePath
		cleanupBackup := func() {
			_ = os.Remove(backupPath)
		}

		if hooks.beforeActionInstall != nil {
			if err := hooks.beforeActionInstall(index); err != nil {
				cleanupBackup()
				return daemonPatchRollback{}, fmt.Errorf("apply patch: before installing action %d: %w", index, err)
			}
		}
		if err := daemonRevalidatePatchAction(ctx, action); err != nil {
			if removeErr := os.Remove(backupPath); removeErr != nil {
				return daemonPatchRollback{}, fmt.Errorf("%w; recovery backup preserved at %s: cleanup failed: %v", err, backupPath, removeErr)
			}
			return daemonPatchRollback{}, err
		}

		if action.renameOnly {
			if err := daemonMovePatchPathNoReplace(sourcePath, action.targetPath); err != nil {
				cleanupBackup()
				return daemonPatchRollback{}, fmt.Errorf("apply patch: move %s -> %s: %w", action.sourceDisplay, action.targetDisplay, err)
			}
			var afterInstallErr error
			if hooks.afterActionInstall != nil {
				afterInstallErr = hooks.afterActionInstall(index)
			}
			matches, matchErr := daemonPatchPathMatchesState(action.targetPath, action.sourceState)
			if afterInstallErr != nil || !matches {
				restoreErr := daemonMovePatchPathNoReplace(action.targetPath, sourcePath)
				if restoreErr == nil {
					if afterInstallErr != nil && matches {
						cleanupBackup()
						return daemonPatchRollback{}, fmt.Errorf("apply patch: after installing action %d: %w", index, afterInstallErr)
					}
					return daemonPatchRollback{}, fmt.Errorf(
						"apply patch: move %s -> %s raced with a concurrent source change (%v); recovery backup preserved at %s",
						action.sourceDisplay,
						action.targetDisplay,
						matchErr,
						backupPath,
					)
				}
				return daemonPatchRollback{}, fmt.Errorf(
					"apply patch: move %s -> %s raced with a concurrent source change (%v); moved path and recovery backup preserved at %s and %s: %w",
					action.sourceDisplay,
					action.targetDisplay,
					matchErr,
					action.targetPath,
					backupPath,
					restoreErr,
				)
			}
			targetPath := action.targetPath
			installedState := action.sourceState
			return daemonPatchRollback{
				backupPath: backupPath,
				undo: func() error {
					return daemonRollbackMovedPath(sourcePath, targetPath, backupPath, action.sourceState, installedState, true)
				},
			}, nil
		}

		if action.targetPath == action.sourcePath {
			if err := daemonRenameExchange(action.stagedTempPath, action.sourcePath); err != nil {
				cleanupBackup()
				return daemonPatchRollback{}, fmt.Errorf("apply patch: write %s: %w", action.targetDisplay, err)
			}
			displacedPath := action.stagedTempPath
			var afterInstallErr error
			if hooks.afterActionInstall != nil {
				afterInstallErr = hooks.afterActionInstall(index)
			}
			matches, matchErr := daemonPatchPathMatchesState(displacedPath, action.sourceState)
			liveMatches, liveErr := daemonPatchPathMatchesState(action.sourcePath, action.stagedState)
			if afterInstallErr != nil || !matches || !liveMatches {
				if liveMatches {
					if restoreErr := daemonRenameExchange(displacedPath, action.sourcePath); restoreErr == nil {
						if afterInstallErr != nil && matches {
							cleanupBackup()
							return daemonPatchRollback{}, fmt.Errorf("apply patch: after installing action %d: %w", index, afterInstallErr)
						}
						return daemonPatchRollback{}, fmt.Errorf(
							"apply patch: file changed during atomic replacement: %s (displaced: %v, live: %v); recovery backup preserved at %s",
							action.sourceDisplay,
							matchErr,
							liveErr,
							backupPath,
						)
					}
				}
				action.stagedTempPath = ""
				return daemonPatchRollback{}, fmt.Errorf(
					"apply patch: file changed during atomic replacement: %s (displaced: %v, live: %v); displaced path and recovery backup preserved at %s and %s",
					action.sourceDisplay,
					matchErr,
					liveErr,
					displacedPath,
					backupPath,
				)
			}
			action.stagedTempPath = ""
			targetPath := action.targetPath
			installedState := action.stagedState
			rollback := daemonPatchRollback{
				backupPath: backupPath,
				undo: func() error {
					return daemonRollbackUpdatedPath(targetPath, backupPath, installedState)
				},
			}
			if err := os.Remove(displacedPath); err != nil {
				return rollback, fmt.Errorf("apply patch: write %s: remove displaced source %s: %w", action.targetDisplay, displacedPath, err)
			}
			return rollback, nil
		}

		if err := daemonCommitStagedFile(action.targetPath, action.stagedTempPath, false); err != nil {
			cleanupBackup()
			return daemonPatchRollback{}, fmt.Errorf("apply patch: write %s: %w", action.targetDisplay, err)
		}
		action.stagedTempPath = ""

		targetPath := action.targetPath
		installedState := action.stagedState
		rollback := daemonPatchRollback{
			backupPath: backupPath,
			undo: func() error {
				return daemonRollbackMovedPath(sourcePath, targetPath, backupPath, action.sourceState, installedState, false)
			},
		}
		if hooks.afterActionInstall != nil {
			if err := hooks.afterActionInstall(index); err != nil {
				return rollback, fmt.Errorf("apply patch: after installing action %d: %w", index, err)
			}
		}
		if err := daemonRemoveValidatedPatchSource(ctx, action); err != nil {
			return rollback, fmt.Errorf("apply patch: remove updated source %s: %w", action.sourceDisplay, err)
		}
		return rollback, nil
	default:
		return daemonPatchRollback{}, fmt.Errorf("apply patch: unsupported action %q", action.kind)
	}
}

func daemonCreatePatchBackup(ctx context.Context, action *daemonPatchAction) (string, error) {
	backupPath, err := daemonCopyPatchBackup(ctx, action.sourcePath, action.sourceState)
	if err != nil {
		return "", err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(backupPath)
		}
	}()

	_, current, err := daemonReadPatchFile(ctx, action.sourcePath, false, nil)
	if err != nil || !daemonSamePatchFileState(action.sourceState, current) {
		if err != nil {
			return "", fmt.Errorf("file changed during patch: %w", err)
		}
		return "", fmt.Errorf("file changed during patch")
	}
	_, backup, err := daemonReadPatchFile(ctx, backupPath, false, nil)
	if err != nil {
		return "", fmt.Errorf("validate recovery backup: %w", err)
	}
	if !daemonSamePatchFileSnapshot(action.sourceState, backup) {
		return "", fmt.Errorf("recovery backup does not match source")
	}
	cleanup = false
	return backupPath, nil
}

func daemonCopyPatchBackup(ctx context.Context, path string, expected daemonPatchFileState) (string, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return "", fmt.Errorf("source is not a regular file")
	}
	if !daemonSamePatchFileMetadata(expected.info, before) {
		return "", fmt.Errorf("file changed during patch")
	}

	source, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer source.Close()
	opened, err := source.Stat()
	if err != nil {
		return "", err
	}
	if !daemonSamePatchFileMetadata(before, opened) {
		return "", fmt.Errorf("file changed during patch")
	}

	tmp, err := createDaemonTempFile(filepath.Dir(path), filepath.Base(path)+".backup")
	if err != nil {
		return "", err
	}
	backupPath := tmp.Name()
	cleanup := true
	defer func() {
		_ = tmp.Close()
		if cleanup {
			_ = os.Remove(backupPath)
		}
	}()
	hasher := sha256.New()
	if _, err := daemonCopyPatchContent(ctx, io.MultiWriter(tmp, hasher), source); err != nil {
		return "", err
	}
	afterOpen, err := source.Stat()
	if err != nil {
		return "", err
	}
	afterPath, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	var digest [sha256.Size]byte
	copy(digest[:], hasher.Sum(nil))
	if !daemonSamePatchFileMetadata(before, afterOpen) ||
		!daemonSamePatchFileMetadata(before, afterPath) ||
		digest != expected.digest {
		return "", fmt.Errorf("file changed during patch")
	}
	if err := tmp.Chmod(expected.info.Mode().Perm()); err != nil {
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Chtimes(backupPath, expected.info.ModTime(), expected.info.ModTime()); err != nil {
		return "", err
	}
	cleanup = false
	return backupPath, nil
}

func daemonSamePatchFileSnapshot(left, right daemonPatchFileState) bool {
	return left.info.Mode() == right.info.Mode() &&
		left.info.Size() == right.info.Size() &&
		left.info.ModTime().Equal(right.info.ModTime()) &&
		left.digest == right.digest
}

func daemonRemoveValidatedPatchSource(ctx context.Context, action *daemonPatchAction) error {
	removedPath, err := daemonReservePatchSiblingPath(action.sourcePath, "removed")
	if err != nil {
		return err
	}
	if err := daemonMovePatchPathNoReplace(action.sourcePath, removedPath); err != nil {
		return err
	}
	matches, matchErr := daemonPatchPathMatchesState(removedPath, action.sourceState)
	if !matches {
		restoreErr := daemonMovePatchPathNoReplace(removedPath, action.sourcePath)
		if restoreErr != nil {
			return fmt.Errorf("file changed during patch (%v); changed source preserved at %s: %w", matchErr, removedPath, restoreErr)
		}
		return fmt.Errorf("file changed during patch")
	}
	if err := ctx.Err(); err != nil {
		if restoreErr := daemonMovePatchPathNoReplace(removedPath, action.sourcePath); restoreErr != nil {
			return fmt.Errorf("%w; restore source failed, source preserved at %s: %v", errDaemonCanceled, removedPath, restoreErr)
		}
		return errDaemonCanceled
	}
	return os.Remove(removedPath)
}

func daemonMovePatchPathNoReplace(source, target string) error {
	if err := daemonRenameNoReplace(source, target); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%s already exists", target)
		}
		return err
	}
	return nil
}

func daemonStagePatchPlan(ctx context.Context, actions []daemonPatchAction) error {
	for index := range actions {
		if err := ctx.Err(); err != nil {
			daemonCleanupPatchStages(actions)
			_ = daemonCleanupPatchDirectories(actions)
			return errDaemonCanceled
		}

		action := &actions[index]
		if action.kind == daemonPatchDelete {
			continue
		}

		createdDirs, err := daemonEnsurePatchTargetDirectory(action.targetPath)
		if err != nil {
			daemonCleanupPatchStages(actions)
			_ = daemonCleanupPatchDirectories(actions)
			return fmt.Errorf("apply patch: stage %s: %w", action.targetDisplay, err)
		}
		action.createdDirs = append(action.createdDirs, createdDirs...)
		if action.renameOnly {
			continue
		}

		content := action.content
		stagedTempPath, err := daemonStageFile(ctx, action.targetPath, []byte(content), action.mode)
		if err != nil {
			daemonCleanupPatchStages(actions)
			_ = daemonCleanupPatchDirectories(actions)
			return fmt.Errorf("apply patch: stage %s: %w", action.targetDisplay, err)
		}
		action.stagedTempPath = stagedTempPath
		_, stagedState, err := daemonReadPatchFile(ctx, stagedTempPath, false, nil)
		if err != nil {
			daemonCleanupPatchStages(actions)
			_ = daemonCleanupPatchDirectories(actions)
			return fmt.Errorf("apply patch: stage %s: inspect staged file: %w", action.targetDisplay, err)
		}
		action.stagedState = stagedState
	}
	return nil
}

func daemonCleanupPatchStages(actions []daemonPatchAction) {
	for _, action := range actions {
		if action.stagedTempPath == "" {
			continue
		}
		_ = os.Remove(action.stagedTempPath)
	}
}

func daemonRollbackPatchPlan(rollbacks []daemonPatchRollback) error {
	var rollbackErrors []error
	for index := len(rollbacks) - 1; index >= 0; index-- {
		if rollbacks[index].undo == nil {
			continue
		}
		if err := rollbacks[index].undo(); err != nil {
			rollbackErrors = append(rollbackErrors, err)
		}
	}
	return errors.Join(rollbackErrors...)
}

func daemonRollbackAddedPath(path string, installedState daemonPatchFileState) error {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("rollback add %s: inspect path: %w", path, err)
	}

	matches, matchErr := daemonPatchPathMatchesState(path, installedState)
	if matches {
		if removeErr := os.Remove(path); removeErr != nil {
			return fmt.Errorf("rollback add %s: remove patch-installed content: %w", path, removeErr)
		}
		return nil
	}

	return fmt.Errorf("rollback conflict at %s: path no longer matches patch-installed content (%v); concurrent replacement preserved at %s", path, matchErr, path)
}

func daemonRollbackUpdatedPath(path, backupPath string, installedState daemonPatchFileState) error {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		if err := daemonRestorePatchBackupNoReplace(backupPath, path); err != nil {
			return fmt.Errorf("rollback update %s: %w; original backup preserved at %s", path, err, backupPath)
		}
		return nil
	} else if err != nil {
		return fmt.Errorf("rollback update %s: inspect installed path: %w; original backup preserved at %s", path, err, backupPath)
	}

	_, originalState, err := daemonReadPatchFile(context.Background(), backupPath, false, nil)
	if err != nil {
		return fmt.Errorf("rollback update %s: inspect original backup: %w; original backup preserved at %s", path, err, backupPath)
	}
	matches, matchErr := daemonPatchPathMatchesState(path, installedState)
	if !matches {
		return fmt.Errorf("rollback conflict at %s: installed content changed (%v); concurrent replacement preserved at %s and original backup preserved at %s", path, matchErr, path, backupPath)
	}
	if err := daemonRenameExchange(backupPath, path); err != nil {
		return fmt.Errorf("rollback update %s: atomically restore original: %w; original backup preserved at %s", path, err, backupPath)
	}

	matches, matchErr = daemonPatchPathMatchesState(backupPath, installedState)
	if matches {
		if err := os.Remove(backupPath); err != nil {
			return fmt.Errorf("rollback update %s: remove patch-installed recovery path %s: %w", path, backupPath, err)
		}
		return nil
	}

	originalMatches, originalErr := daemonPatchPathMatchesState(path, originalState)
	if originalMatches {
		if restoreErr := daemonRenameExchange(backupPath, path); restoreErr == nil {
			return fmt.Errorf("rollback conflict at %s: installed content changed (%v); concurrent replacement preserved at %s and original backup preserved at %s", path, matchErr, path, backupPath)
		}
	}
	return fmt.Errorf(
		"rollback conflict at %s: installed content changed (%v); original restore state changed (%v); paths preserved at %s and %s",
		path,
		matchErr,
		originalErr,
		path,
		backupPath,
	)
}

func daemonRollbackMovedPath(
	sourcePath, targetPath, backupPath string,
	originalState, installedState daemonPatchFileState,
	renameOnly bool,
) error {
	var failures []string
	_, sourceErr := os.Lstat(sourcePath)
	targetMatches, targetMatchErr := daemonPatchPathMatchesState(targetPath, installedState)
	if renameOnly && errors.Is(sourceErr, os.ErrNotExist) && targetMatches {
		if err := daemonMovePatchPathNoReplace(targetPath, sourcePath); err == nil {
			if removeErr := os.Remove(backupPath); removeErr != nil {
				return fmt.Errorf("rollback move %s -> %s: restored source but could not remove recovery backup %s: %w", sourcePath, targetPath, backupPath, removeErr)
			}
			return nil
		}
	}

	if errors.Is(sourceErr, os.ErrNotExist) {
		if err := daemonLinkPatchPathNoReplace(backupPath, sourcePath); err != nil {
			failures = append(failures, fmt.Sprintf("restore source without replacement: %v", err))
		}
	} else if sourceErr != nil {
		failures = append(failures, fmt.Sprintf("inspect source: %v", sourceErr))
	} else {
		matches, matchErr := daemonPatchPathMatchesState(sourcePath, originalState)
		if !matches {
			failures = append(failures, fmt.Sprintf("concurrent source replacement preserved at %s (%v)", sourcePath, matchErr))
		}
	}

	if _, err := os.Lstat(targetPath); errors.Is(err, os.ErrNotExist) {
		// The installed target was already removed.
	} else if err != nil {
		failures = append(failures, fmt.Sprintf("inspect target: %v", err))
	} else {
		if targetMatches {
			if err := os.Remove(targetPath); err != nil {
				failures = append(failures, fmt.Sprintf("remove patch-installed target: %v", err))
			}
		} else {
			failures = append(failures, fmt.Sprintf("concurrent target replacement preserved at %s (%v)", targetPath, targetMatchErr))
		}
	}

	if len(failures) == 0 {
		if err := os.Remove(backupPath); err != nil {
			return fmt.Errorf("rollback move %s -> %s: remove recovery backup %s: %w", sourcePath, targetPath, backupPath, err)
		}
		return nil
	}
	return fmt.Errorf("rollback move %s -> %s conflicted: %s; original backup preserved at %s", sourcePath, targetPath, strings.Join(failures, "; "), backupPath)
}

func daemonPatchPathMatchesState(path string, expected daemonPatchFileState) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	if !daemonSamePatchFileMetadata(info, expected.info) {
		return false, nil
	}
	_, actual, err := daemonReadPatchFile(context.Background(), path, false, nil)
	if err != nil {
		return false, err
	}
	return daemonSamePatchFileState(actual, expected), nil
}

func daemonRestorePatchBackupNoReplace(backupPath, targetPath string) error {
	if err := daemonMovePatchPathNoReplace(backupPath, targetPath); err != nil {
		return fmt.Errorf("restore backup: %w", err)
	}
	return nil
}

func daemonLinkPatchPathNoReplace(source, target string) error {
	if err := os.Link(source, target); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%s already exists", target)
		}
		return err
	}
	return nil
}

func daemonEnsurePatchTargetDirectory(targetPath string) ([]string, error) {
	dir := filepath.Dir(targetPath)
	if dir == "" {
		dir = "."
	}

	var missing []string
	for current := dir; ; current = filepath.Dir(current) {
		info, err := os.Stat(current)
		if err == nil {
			if !info.IsDir() {
				return nil, fmt.Errorf("%s is not a directory", current)
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		missing = append(missing, current)
		parent := filepath.Dir(current)
		if parent == current {
			return nil, err
		}
	}

	created := make([]string, 0, len(missing))
	for index := len(missing) - 1; index >= 0; index-- {
		if err := os.Mkdir(missing[index], 0o755); err != nil {
			if errors.Is(err, os.ErrExist) {
				info, statErr := os.Stat(missing[index])
				if statErr == nil && info.IsDir() {
					continue
				}
			}
			for cleanupIndex := len(created) - 1; cleanupIndex >= 0; cleanupIndex-- {
				_ = os.Remove(created[cleanupIndex])
			}
			return nil, err
		}
		created = append(created, missing[index])
	}
	return created, nil
}

func daemonCleanupPatchDirectories(actions []daemonPatchAction) error {
	var cleanupErrors []error
	for actionIndex := len(actions) - 1; actionIndex >= 0; actionIndex-- {
		for dirIndex := len(actions[actionIndex].createdDirs) - 1; dirIndex >= 0; dirIndex-- {
			if err := os.Remove(actions[actionIndex].createdDirs[dirIndex]); err != nil && !errors.Is(err, os.ErrNotExist) {
				cleanupErrors = append(cleanupErrors, err)
			}
		}
	}
	return errors.Join(cleanupErrors...)
}

func daemonReservePatchSiblingPath(path, purpose string) (string, error) {
	tmp, err := createDaemonTempFile(filepath.Dir(path), filepath.Base(path)+"."+purpose)
	if err != nil {
		return "", err
	}
	backupPath := tmp.Name()
	if err := tmp.Close(); err != nil {
		_ = os.Remove(backupPath)
		return "", err
	}
	if err := os.Remove(backupPath); err != nil {
		return "", err
	}
	return backupPath, nil
}

func daemonCommitStagedFile(path, stagedTempPath string, allowOverwrite bool) error {
	if stagedTempPath == "" {
		return fmt.Errorf("missing staged file for %s", path)
	}
	if allowOverwrite {
		return os.Rename(stagedTempPath, path)
	}
	return daemonMovePatchPathNoReplace(stagedTempPath, path)
}

func daemonStageFile(ctx context.Context, path string, content []byte, mode os.FileMode) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", errDaemonCanceled
	}
	dir := filepath.Dir(path)
	if dir == "" {
		dir = "."
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}

	tmp, err := createDaemonTempFile(dir, filepath.Base(path))
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := ctx.Err(); err != nil {
		_ = tmp.Close()
		return "", errDaemonCanceled
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", errDaemonCanceled
	}

	cleanup = false
	return tmpName, nil
}

func daemonWriteFileAtomic(ctx context.Context, path string, content []byte, mode os.FileMode, allowOverwrite bool) error {
	dir := filepath.Dir(path)
	if dir == "" {
		dir = "."
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return daemonWriteRootedFile(ctx, filepath.Base(path), content, allowOverwrite, dir, daemonRootedFileHooks{})
}

func daemonWriteFile(ctx context.Context, path string, content []byte, overwrite bool, root string) error {
	return daemonWriteFileWithHooks(ctx, path, content, overwrite, root, daemonRootedFileHooks{})
}

func daemonWriteFileWithHooks(ctx context.Context, path string, content []byte, overwrite bool, root string, hooks daemonRootedFileHooks) error {
	if path == "" {
		return fmt.Errorf("write file: path is required")
	}
	if len(content) > daemonproto.MaxFileTransferBytes {
		return fmt.Errorf("%w: %d bytes exceeds %d", daemonproto.ErrFileTransferTooLarge, len(content), daemonproto.MaxFileTransferBytes)
	}
	if root != "" {
		return daemonWriteRootedFile(ctx, path, content, overwrite, root, hooks)
	}
	resolvedPath, err := daemonResolveWritePath(path, root)
	if err != nil {
		return err
	}

	mode := os.FileMode(0o644)
	info, err := os.Lstat(resolvedPath)
	switch {
	case err == nil:
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("write file: refusing to overwrite symbolic link path %s", resolvedPath)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("write file: destination %s is not a regular file", resolvedPath)
		}
		if !overwrite {
			return fmt.Errorf("write file: %s already exists", resolvedPath)
		}
		mode = info.Mode().Perm()
	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("write file: %w", err)
	}

	if err := daemonWriteFileAtomic(ctx, resolvedPath, content, mode, overwrite); err != nil {
		if !overwrite && strings.Contains(err.Error(), "already exists") {
			return fmt.Errorf("write file: %s already exists", resolvedPath)
		}
		return fmt.Errorf("write file: %w", err)
	}
	return nil
}

func daemonReadFile(ctx context.Context, path, root string) ([]byte, error) {
	return daemonReadFileWithHooks(ctx, path, root, daemonRootedFileHooks{})
}

func daemonReadFileWithHooks(ctx context.Context, path, root string, hooks daemonRootedFileHooks) ([]byte, error) {
	if path == "" {
		return nil, fmt.Errorf("read file: path is required")
	}
	if root != "" {
		return daemonReadRootedFile(ctx, path, root, hooks)
	}
	resolvedPath, err := daemonResolveReadPath(path, root)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(resolvedPath)
	if err != nil {
		return nil, fmt.Errorf("read file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("read file: source %s is not a regular file", resolvedPath)
	}
	file, err := os.OpenFile(resolvedPath, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("read file: %w", err)
	}
	defer file.Close()
	return daemonReadBoundedFile(ctx, file, hooks)
}

func daemonResolveReadPath(path, root string) (string, error) {
	if root != "" && !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	cleanPath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("read file: resolve path: %w", err)
	}
	resolvedPath, err := daemonResolvePathSymlinks(cleanPath)
	if err != nil {
		return "", fmt.Errorf("read file: resolve path: %w", err)
	}
	if root == "" {
		return resolvedPath, nil
	}

	cleanRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("read file: resolve workdir: %w", err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(cleanRoot)
	if err != nil {
		return "", fmt.Errorf("read file: resolve workdir: %w", err)
	}
	rel, err := filepath.Rel(resolvedRoot, resolvedPath)
	if err != nil {
		return "", fmt.Errorf("read file: resolve source: %w", err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("read file: path %s escapes workdir %s", path, root)
	}
	return resolvedPath, nil
}

func daemonResolveWritePath(path, root string) (string, error) {
	if root != "" && !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	cleanPath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("write file: resolve path: %w", err)
	}
	resolvedParent, err := daemonResolvePathSymlinks(filepath.Dir(cleanPath))
	if err != nil {
		return "", fmt.Errorf("write file: resolve parent: %w", err)
	}
	resolvedPath := filepath.Join(resolvedParent, filepath.Base(cleanPath))
	if root == "" {
		return resolvedPath, nil
	}

	cleanRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("write file: resolve workdir: %w", err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(cleanRoot)
	if err != nil {
		return "", fmt.Errorf("write file: resolve workdir: %w", err)
	}
	rel, err := filepath.Rel(resolvedRoot, resolvedPath)
	if err != nil {
		return "", fmt.Errorf("write file: resolve destination: %w", err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("write file: path %s escapes workdir %s", path, root)
	}
	return resolvedPath, nil
}

func daemonResolvePathSymlinks(path string) (string, error) {
	current := path
	var missing []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return resolved, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		if _, lstatErr := os.Lstat(current); lstatErr == nil {
			return "", err
		} else if !errors.Is(lstatErr, os.ErrNotExist) {
			return "", lstatErr
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}
