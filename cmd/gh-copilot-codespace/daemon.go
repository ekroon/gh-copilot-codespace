package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ekroon/gh-copilot-codespace/internal/codespaceenv"
	"github.com/ekroon/gh-copilot-codespace/internal/daemonproto"
)

const (
	daemonTmuxPrefix = "copilot-"
	daemonMisePATH   = `PATH="$HOME/.local/bin:$HOME/.local/share/mise/shims:$PATH"`
)

var errDaemonCanceled = errors.New("request canceled")

type daemonInflight struct {
	ctxCancel context.CancelFunc

	mu   sync.Mutex
	pgid int
}

type daemonInflightKey struct{}

func (i *daemonInflight) cancel() {
	i.ctxCancel()
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.pgid > 0 {
		_ = syscall.Kill(-i.pgid, syscall.SIGTERM)
	}
}

func (i *daemonInflight) setProcess(pid int) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.pgid = pid
}

func (i *daemonInflight) clearProcess(pid int) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.pgid == pid {
		i.pgid = 0
	}
}

type daemonHandlerError struct {
	code    string
	message string
}

func runDaemon(args []string) error {
	_ = args // reserved for future daemon flags; unknown flags are ignored for now.
	return runDaemonIO(context.Background(), os.Stdin, os.Stdout)
}

func runDaemonIO(ctx context.Context, in io.Reader, out io.Writer) error {
	codespaceenv.ApplyProcessBootstrap()

	startedAt := time.Now().UTC().Format(time.RFC3339)
	dec := daemonproto.NewDecoder(in)
	enc := daemonproto.NewEncoder(out)

	var writeMu sync.Mutex
	writeFrame := func(frame daemonproto.Frame) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return enc.Write(frame)
	}

	if err := writeFrame(daemonproto.NewHello(daemonproto.ProtocolVersion, daemonproto.AllVerbs())); err != nil {
		return err
	}

	inflight := make(map[uint64]*daemonInflight)
	var inflightMu sync.Mutex
	var wg sync.WaitGroup

	cancelAll := func() {
		inflightMu.Lock()
		items := make([]*daemonInflight, 0, len(inflight))
		for _, item := range inflight {
			items = append(items, item)
		}
		inflightMu.Unlock()
		for _, item := range items {
			item.cancel()
		}
	}

	for {
		frame, err := dec.Read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				cancelAll()
				wg.Wait()
				return nil
			}
			cancelAll()
			wg.Wait()
			return err
		}

		switch frame.Type {
		case daemonproto.TypeCancel:
			inflightMu.Lock()
			item := inflight[frame.ID]
			inflightMu.Unlock()
			if item != nil {
				item.cancel()
			}
		case daemonproto.TypeRequest:
			reqCtx, cancel := context.WithCancel(ctx)
			item := &daemonInflight{ctxCancel: cancel}
			reqCtx = context.WithValue(reqCtx, daemonInflightKey{}, item)

			inflightMu.Lock()
			if _, exists := inflight[frame.ID]; exists {
				inflightMu.Unlock()
				if err := writeFrame(daemonproto.NewErrorResponse(frame.ID, daemonproto.ErrCodeBadRequest, "duplicate request id")); err != nil {
					fmt.Fprintf(os.Stderr, "daemon: write duplicate-id response: %v\n", err)
				}
				cancel()
				continue
			}
			inflight[frame.ID] = item
			inflightMu.Unlock()

			wg.Add(1)
			go func(frame daemonproto.Frame, reqCtx context.Context, item *daemonInflight) {
				defer wg.Done()
				response := handleDaemonRequest(reqCtx, frame, startedAt)
				if err := writeFrame(response); err != nil {
					fmt.Fprintf(os.Stderr, "daemon: write response for request %d: %v\n", frame.ID, err)
				}
				inflightMu.Lock()
				delete(inflight, frame.ID)
				inflightMu.Unlock()
				item.ctxCancel()
			}(frame, reqCtx, item)
		case daemonproto.TypeHello:
			fmt.Fprintln(os.Stderr, "daemon: ignoring unexpected hello frame from client")
		default:
			fmt.Fprintf(os.Stderr, "daemon: ignoring unknown frame type %q\n", frame.Type)
		}
	}
}

func handleDaemonRequest(ctx context.Context, frame daemonproto.Frame, startedAt string) (response daemonproto.Frame) {
	defer func() {
		if r := recover(); r != nil {
			response = daemonproto.NewErrorResponse(frame.ID, daemonproto.ErrCodeInternal, fmt.Sprintf("panic: %v", r))
		}
	}()

	codespaceenv.ApplyProcessBootstrap()
	if err := ctx.Err(); err != nil {
		return daemonproto.NewErrorResponse(frame.ID, daemonproto.ErrCodeCanceled, err.Error())
	}

	result, handlerErr := dispatchDaemonRequest(ctx, frame, startedAt)
	if handlerErr != nil {
		return daemonproto.NewErrorResponse(frame.ID, handlerErr.code, handlerErr.message)
	}
	resp, err := daemonproto.NewResponse(frame.ID, result)
	if err != nil {
		return daemonproto.NewErrorResponse(frame.ID, daemonproto.ErrCodeInternal, err.Error())
	}
	return resp
}

func dispatchDaemonRequest(ctx context.Context, frame daemonproto.Frame, startedAt string) (any, *daemonHandlerError) {
	switch frame.Verb {
	case daemonproto.VerbPing:
		var params daemonproto.PingParams
		if err := decodeDaemonParams(frame, &params); err != nil {
			return nil, daemonBadRequest(frame.Verb, err)
		}
		return daemonproto.PingResult{Pong: true, PID: os.Getpid(), StartedAt: startedAt}, nil
	case daemonproto.VerbViewFile:
		var params daemonproto.ViewFileParams
		if err := decodeDaemonParams(frame, &params); err != nil {
			return nil, daemonBadRequest(frame.Verb, err)
		}
		content, err := daemonViewFile(ctx, params.Path, params.ViewRange)
		if err != nil {
			return nil, daemonMapError(daemonproto.ErrCodeExecFailed, err)
		}
		return daemonproto.ViewFileResult{Content: content}, nil
	case daemonproto.VerbEditFile:
		var params daemonproto.EditFileParams
		if err := decodeDaemonParams(frame, &params); err != nil {
			return nil, daemonBadRequest(frame.Verb, err)
		}
		if err := daemonEditFile(ctx, params.Path, params.OldStr, params.NewStr); err != nil {
			return nil, daemonMapError(daemonproto.ErrCodeExecFailed, err)
		}
		return daemonproto.EditFileResult{Replaced: 1}, nil
	case daemonproto.VerbCreateFile:
		var params daemonproto.CreateFileParams
		if err := decodeDaemonParams(frame, &params); err != nil {
			return nil, daemonBadRequest(frame.Verb, err)
		}
		if err := daemonCreateFile(ctx, params.Path, params.Content, params.AllowOverwrite); err != nil {
			return nil, daemonMapError(daemonproto.ErrCodeExecFailed, err)
		}
		return daemonproto.CreateFileResult{Bytes: len([]byte(params.Content))}, nil
	case daemonproto.VerbRunBash:
		var params daemonproto.RunBashParams
		if err := decodeDaemonParams(frame, &params); err != nil {
			return nil, daemonBadRequest(frame.Verb, err)
		}
		stdout, stderr, exitCode, err := runProcessInDir(ctx, params.Cwd, "bash", "-c", params.Command)
		if err != nil {
			return nil, daemonMapError(daemonproto.ErrCodeInternal, err)
		}
		return daemonproto.RunBashResult{Stdout: stdout, Stderr: stderr, ExitCode: exitCode}, nil
	case daemonproto.VerbGrep:
		var params daemonproto.GrepParams
		if err := decodeDaemonParams(frame, &params); err != nil {
			return nil, daemonBadRequest(frame.Verb, err)
		}
		output, err := daemonGrep(ctx, params.Pattern, params.Path, params.Glob, params.Cwd)
		if err != nil {
			return nil, daemonMapError(daemonproto.ErrCodeExecFailed, err)
		}
		return daemonproto.GrepResult{Output: output}, nil
	case daemonproto.VerbGlob:
		var params daemonproto.GlobParams
		if err := decodeDaemonParams(frame, &params); err != nil {
			return nil, daemonBadRequest(frame.Verb, err)
		}
		output, err := daemonGlob(ctx, params.Pattern, params.Path, params.Cwd)
		if err != nil {
			return nil, daemonMapError(daemonproto.ErrCodeExecFailed, err)
		}
		return daemonproto.GlobResult{Output: output}, nil
	case daemonproto.VerbStartSession:
		var params daemonproto.StartSessionParams
		if err := decodeDaemonParams(frame, &params); err != nil {
			return nil, daemonBadRequest(frame.Verb, err)
		}
		if err := daemonStartSession(ctx, params.SessionID, params.Command, params.Cwd); err != nil {
			return nil, daemonMapError(daemonproto.ErrCodeExecFailed, err)
		}
		return daemonproto.StartSessionResult{SessionID: params.SessionID}, nil
	case daemonproto.VerbWriteSession:
		var params daemonproto.WriteSessionParams
		if err := decodeDaemonParams(frame, &params); err != nil {
			return nil, daemonBadRequest(frame.Verb, err)
		}
		if err := daemonWriteSession(ctx, params.SessionID, params.Input); err != nil {
			return nil, daemonMapError(daemonproto.ErrCodeExecFailed, err)
		}
		return daemonproto.WriteSessionResult{}, nil
	case daemonproto.VerbReadSession:
		var params daemonproto.ReadSessionParams
		if err := decodeDaemonParams(frame, &params); err != nil {
			return nil, daemonBadRequest(frame.Verb, err)
		}
		output, err := daemonReadSession(ctx, params.SessionID)
		if err != nil {
			return nil, daemonMapError(daemonproto.ErrCodeExecFailed, err)
		}
		return daemonproto.ReadSessionResult{Output: output}, nil
	case daemonproto.VerbStopSession:
		var params daemonproto.StopSessionParams
		if err := decodeDaemonParams(frame, &params); err != nil {
			return nil, daemonBadRequest(frame.Verb, err)
		}
		if err := daemonStopSession(ctx, params.SessionID); err != nil {
			return nil, daemonMapError(daemonproto.ErrCodeExecFailed, err)
		}
		return daemonproto.StopSessionResult{}, nil
	case daemonproto.VerbListSessions:
		var params daemonproto.ListSessionsParams
		if err := decodeDaemonParams(frame, &params); err != nil {
			return nil, daemonBadRequest(frame.Verb, err)
		}
		output, err := daemonListSessions(ctx)
		if err != nil {
			return nil, daemonMapError(daemonproto.ErrCodeExecFailed, err)
		}
		return daemonproto.ListSessionsResult{Output: output}, nil
	default:
		return nil, &daemonHandlerError{code: daemonproto.ErrCodeUnknownVerb, message: fmt.Sprintf("unknown verb %q", frame.Verb)}
	}
}

func decodeDaemonParams(frame daemonproto.Frame, target any) error {
	if len(frame.Params) == 0 {
		return json.Unmarshal([]byte("{}"), target)
	}
	return json.Unmarshal(frame.Params, target)
}

func daemonBadRequest(verb daemonproto.Verb, err error) *daemonHandlerError {
	return &daemonHandlerError{code: daemonproto.ErrCodeBadRequest, message: fmt.Sprintf("bad params for %s: %v", verb, err)}
}

func daemonMapError(defaultCode string, err error) *daemonHandlerError {
	if errors.Is(err, errDaemonCanceled) || errors.Is(err, context.Canceled) {
		return &daemonHandlerError{code: daemonproto.ErrCodeCanceled, message: "request canceled"}
	}
	return &daemonHandlerError{code: defaultCode, message: err.Error()}
}

func daemonViewFile(ctx context.Context, path string, viewRange []int) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", errDaemonCanceled
	}
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("view file: %w", err)
	}

	reader := bufio.NewReader(file)
	var out strings.Builder
	lineNo := 1
	for {
		if err := ctx.Err(); err != nil {
			_ = file.Close()
			return "", errDaemonCanceled
		}
		line, readErr := reader.ReadString('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			_ = file.Close()
			return "", fmt.Errorf("view file: %w", readErr)
		}
		if line != "" {
			if daemonLineInRange(lineNo, viewRange) {
				fmt.Fprintf(&out, "%d. %s\n", lineNo, strings.TrimSuffix(line, "\n"))
			}
			lineNo++
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
	}
	if err := ctx.Err(); err != nil {
		_ = file.Close()
		return "", errDaemonCanceled
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("view file: %w", err)
	}
	return out.String(), nil
}

func daemonLineInRange(lineNo int, viewRange []int) bool {
	if len(viewRange) != 2 {
		return true
	}
	start, end := viewRange[0], viewRange[1]
	if lineNo < start {
		return false
	}
	return end == -1 || lineNo <= end
}

func daemonEditFile(ctx context.Context, path, oldStr, newStr string) error {
	if err := ctx.Err(); err != nil {
		return errDaemonCanceled
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("edit file (read): %w", err)
	}
	if err := ctx.Err(); err != nil {
		return errDaemonCanceled
	}
	contentStr := string(content)
	count := strings.Count(contentStr, oldStr)
	if count == 0 {
		return fmt.Errorf("old_str not found in file")
	}
	if count > 1 {
		return fmt.Errorf("old_str found %d times, must be unique", count)
	}
	newContent := strings.Replace(contentStr, oldStr, newStr, 1)
	if err := ctx.Err(); err != nil {
		return errDaemonCanceled
	}
	if err := os.WriteFile(path, []byte(newContent), 0o666); err != nil {
		return fmt.Errorf("edit file (write): %w", err)
	}
	return nil
}

func daemonCreateFile(ctx context.Context, path, content string, allowOverwrite bool) error {
	if err := ctx.Err(); err != nil {
		return errDaemonCanceled
	}
	if !allowOverwrite {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("create file: %s already exists", path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("create file: %w", err)
		}
	}
	dir := filepath.Dir(path)
	if dir == "" {
		dir = "."
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create file: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return errDaemonCanceled
	}

	tmp, err := createDaemonTempFile(dir, filepath.Base(path))
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("create file: %w", err)
	}
	if err := ctx.Err(); err != nil {
		_ = tmp.Close()
		return errDaemonCanceled
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("create file: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return errDaemonCanceled
	}

	if allowOverwrite {
		if err := os.Rename(tmpName, path); err != nil {
			return fmt.Errorf("create file: %w", err)
		}
		cleanup = false
		return nil
	}
	if err := os.Link(tmpName, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("create file: %s already exists", path)
		}
		return fmt.Errorf("create file: %w", err)
	}
	cleanup = true
	return nil
}

func createDaemonTempFile(dir, base string) (*os.File, error) {
	if base == "" || base == "." || base == string(filepath.Separator) {
		base = "daemon-create"
	}
	for attempt := 0; attempt < 100; attempt++ {
		name := filepath.Join(dir, fmt.Sprintf(".%s.%d.%d.tmp", base, os.Getpid(), time.Now().UnixNano()+int64(attempt)))
		file, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			return file, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("could not allocate temporary file in %s", dir)
}

func runProcess(ctx context.Context, name string, args ...string) (stdout, stderr string, exitCode int, err error) {
	return runProcessInDir(ctx, "", name, args...)
}

func runProcessInDir(ctx context.Context, cwd, name string, args ...string) (stdout, stderr string, exitCode int, err error) {
	if err := ctx.Err(); err != nil {
		return "", "", -1, errDaemonCanceled
	}

	cmd := exec.Command(name, args...)
	if cwd != "" {
		cmd.Dir = cwd
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	if err := cmd.Start(); err != nil {
		if ctx.Err() != nil {
			return outBuf.String(), errBuf.String(), -1, errDaemonCanceled
		}
		return outBuf.String(), errBuf.String(), -1, fmt.Errorf("failed to execute command: %w", err)
	}

	pgid := cmd.Process.Pid
	if item, ok := ctx.Value(daemonInflightKey{}).(*daemonInflight); ok {
		item.setProcess(pgid)
		defer item.clearProcess(pgid)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case runErr := <-done:
		stdout, stderr = outBuf.String(), errBuf.String()
		if runErr != nil {
			if exitErr, ok := runErr.(*exec.ExitError); ok {
				return stdout, stderr, exitErr.ExitCode(), nil
			}
			return stdout, stderr, -1, fmt.Errorf("failed to execute command: %w", runErr)
		}
		return stdout, stderr, 0, nil
	case <-ctx.Done():
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			// Escalate to SIGKILL if the process group ignored SIGTERM.
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			<-done
		}
		stdout, stderr = outBuf.String(), errBuf.String()
		return stdout, stderr, -1, errDaemonCanceled
	}
}

func daemonGrep(ctx context.Context, pattern, path, globPattern, cwd string) (string, error) {
	var args []string
	args = append(args, "rg", "--color=never", "-n")
	if globPattern != "" {
		args = append(args, "--glob", shellQuote(globPattern))
	}
	args = append(args, shellQuote(pattern))
	searchPath := path
	if searchPath == "" {
		searchPath = "."
	}
	args = append(args, shellQuote(searchPath))
	cmd := strings.Join(args, " ")
	cmd = fmt.Sprintf("(%s) 2>/dev/null || grep -rn %s %s", cmd, shellQuote(pattern), shellQuote(searchPath))
	stdout, _, exitCode, err := runProcessInDir(ctx, cwd, "bash", "-c", cmd)
	if err != nil {
		return "", fmt.Errorf("grep: %w", err)
	}
	if exitCode > 1 {
		return "", fmt.Errorf("grep failed with exit code %d", exitCode)
	}
	return stdout, nil
}

func daemonGlob(ctx context.Context, pattern, path, cwd string) (string, error) {
	searchPath := path
	if searchPath == "" {
		searchPath = "."
	}
	cmd := fmt.Sprintf(
		"(fd --type f --glob %s --exclude .git %s 2>/dev/null || find %s -name %s -not -path '*/.git/*' 2>/dev/null) | head -200",
		shellQuote(pattern), shellQuote(searchPath), shellQuote(searchPath), shellQuote(daemonGlobToFindName(pattern)))
	stdout, _, exitCode, err := runProcessInDir(ctx, cwd, "bash", "-c", cmd)
	if err != nil {
		return "", fmt.Errorf("glob: %w", err)
	}
	if exitCode > 1 {
		return "", fmt.Errorf("glob failed with exit code %d", exitCode)
	}
	return stdout, nil
}

func daemonGlobToFindName(pattern string) string {
	parts := strings.Split(pattern, "/")
	return parts[len(parts)-1]
}

func daemonTmuxSessionName(sessionID string) string {
	return daemonTmuxPrefix + sessionID
}

func daemonExecTmux(ctx context.Context, tmuxCmd string) (string, string, int, error) {
	return runProcess(ctx, "bash", "-c", daemonMisePATH+" && "+tmuxCmd)
}

func daemonEnsureTmux(ctx context.Context) error {
	if _, _, ec, _ := daemonExecTmux(ctx, "command -v tmux"); ec == 0 {
		return nil
	}
	fmt.Fprintln(os.Stderr, "codespace-mcp: tmux not found, installing via mise...")
	installScript := daemonMisePATH + ` && (command -v mise >/dev/null 2>&1 || curl -fsSL https://mise.jdx.dev/install.sh | sh) && mise use -g tmux`
	_, stderr, exitCode, err := runProcess(ctx, "bash", "-c", installScript)
	if err != nil {
		return fmt.Errorf("installing tmux: %w", err)
	}
	if exitCode != 0 {
		daemonLogDiagnostic("tmux install via mise failed", stderr)
		return fmt.Errorf("failed to install tmux via mise (exit %d); verify that the codespace can run `mise use -g tmux`", exitCode)
	}
	verifyCmd := `command -v tmux >/dev/null 2>&1 || { if [ -x "$HOME/.local/share/mise/shims/tmux" ]; then echo 'tmux shim exists but is not on PATH' >&2; else echo 'tmux shim not found after install' >&2; fi; exit 1; }`
	_, verifyStderr, ec, err := daemonExecTmux(ctx, verifyCmd)
	if err != nil {
		return fmt.Errorf("verifying tmux installation: %w", err)
	}
	if ec != 0 {
		daemonLogDiagnostic("tmux verification after mise install failed", verifyStderr)
		return fmt.Errorf("tmux installation completed but tmux is still unavailable; %s", daemonSummarizeTmuxVerificationFailure(verifyStderr))
	}
	return nil
}

func daemonStartSession(ctx context.Context, sessionID, command, cwd string) error {
	name := daemonTmuxSessionName(sessionID)
	if err := daemonEnsureTmux(ctx); err != nil {
		return err
	}
	wrappedCommand := codespaceenv.BuildShellBootstrap() + " && " + daemonWrapCommandInWorkdir(command, cwd)
	cmd := fmt.Sprintf(
		"tmux new-session -d -s %s -x 200 -y 50 %s && tmux set-option -t %s remain-on-exit on",
		shellQuote(name), shellQuote(wrappedCommand), shellQuote(name))
	_, stderr, exitCode, err := daemonExecTmux(ctx, cmd)
	if err != nil {
		return fmt.Errorf("start session: %w", err)
	}
	if exitCode != 0 {
		return daemonFormatCommandFailure("start session", exitCode, stderr)
	}
	return nil
}

var daemonSpecialKeys = map[string]string{
	"{enter}":     "Enter",
	"{up}":        "Up",
	"{down}":      "Down",
	"{left}":      "Left",
	"{right}":     "Right",
	"{backspace}": "BSpace",
}

func daemonParseInput(input string) []string {
	var segments []string
	for len(input) > 0 {
		bestIdx := -1
		bestKey := ""
		bestTmux := ""
		for pattern, tmuxKey := range daemonSpecialKeys {
			idx := strings.Index(input, pattern)
			if idx >= 0 && (bestIdx < 0 || idx < bestIdx) {
				bestIdx = idx
				bestKey = pattern
				bestTmux = tmuxKey
			}
		}
		if bestIdx < 0 {
			segments = append(segments, input)
			break
		}
		if bestIdx > 0 {
			segments = append(segments, input[:bestIdx])
		}
		segments = append(segments, "\x00"+bestTmux)
		input = input[bestIdx+len(bestKey):]
	}
	return segments
}

func daemonWriteSession(ctx context.Context, sessionID, input string) error {
	name := daemonTmuxSessionName(sessionID)
	for _, seg := range daemonParseInput(input) {
		var cmd string
		if strings.HasPrefix(seg, "\x00") {
			cmd = fmt.Sprintf("tmux send-keys -t %s %s", shellQuote(name), seg[1:])
		} else {
			cmd = fmt.Sprintf("tmux send-keys -t %s %s", shellQuote(name), shellQuote(seg))
		}
		_, stderr, exitCode, err := daemonExecTmux(ctx, cmd)
		if err != nil {
			return fmt.Errorf("write session: %w", err)
		}
		if exitCode != 0 {
			return daemonFormatCommandFailure("write session", exitCode, stderr)
		}
	}
	return nil
}

var daemonPaneDeadRe = regexp.MustCompile(`(?m)^Pane is dead.*$`)

func daemonCleanPaneOutput(s string) string {
	s = daemonPaneDeadRe.ReplaceAllString(s, "")
	return strings.TrimRight(s, "\n ")
}

func daemonReadSession(ctx context.Context, sessionID string) (string, error) {
	name := daemonTmuxSessionName(sessionID)
	checkCmd := fmt.Sprintf("tmux has-session -t %s 2>/dev/null", shellQuote(name))
	if _, _, ec, _ := daemonExecTmux(ctx, checkCmd); ec != 0 {
		return "", fmt.Errorf("session %q does not exist (command may have exited and been cleaned up)", sessionID)
	}
	cmd := fmt.Sprintf("tmux capture-pane -t %s -p -S -100", shellQuote(name))
	stdout, stderr, exitCode, err := daemonExecTmux(ctx, cmd)
	if err != nil {
		return "", fmt.Errorf("read session: %w", err)
	}
	if exitCode != 0 {
		return "", daemonFormatCommandFailure("read session", exitCode, stderr)
	}
	stdout = daemonCleanPaneOutput(stdout)
	statusCmd := fmt.Sprintf("tmux list-panes -t %s -F '#{pane_dead} #{pane_dead_status}' 2>/dev/null", shellQuote(name))
	statusOut, _, _, _ := daemonExecTmux(ctx, statusCmd)
	paneDead, paneExitCode, err := daemonParsePaneStatus(statusOut)
	if err == nil && paneDead {
		if stdout != "" {
			stdout += "\n"
		}
		stdout += "[session exited]"
		if paneExitCode != 0 {
			stdout += fmt.Sprintf("\n[exit code: %d]", paneExitCode)
		}
	}
	return stdout, nil
}

func daemonParsePaneStatus(status string) (bool, int, error) {
	fields := strings.Fields(strings.TrimSpace(status))
	if len(fields) < 2 {
		return false, 0, errors.New("invalid pane status")
	}
	paneDead := fields[0] == "1"
	exitCode, err := strconv.Atoi(fields[1])
	if err != nil {
		return false, 0, fmt.Errorf("parse pane exit code: %w", err)
	}
	return paneDead, exitCode, nil
}

func daemonStopSession(ctx context.Context, sessionID string) error {
	name := daemonTmuxSessionName(sessionID)
	cmd := fmt.Sprintf("tmux kill-session -t %s", shellQuote(name))
	_, stderr, exitCode, err := daemonExecTmux(ctx, cmd)
	if err != nil {
		return fmt.Errorf("stop session: %w", err)
	}
	if exitCode != 0 {
		return daemonFormatCommandFailure("stop session", exitCode, stderr)
	}
	return nil
}

func daemonListSessions(ctx context.Context) (string, error) {
	cmd := "tmux list-sessions -F '#{session_name} #{session_created} #{session_activity}' 2>/dev/null | grep '^" + daemonTmuxPrefix + "'"
	stdout, _, exitCode, err := daemonExecTmux(ctx, cmd)
	if err != nil {
		return "", fmt.Errorf("list sessions: %w", err)
	}
	if exitCode > 1 {
		return "", fmt.Errorf("list sessions failed with exit code %d", exitCode)
	}
	return stdout, nil
}

func daemonWrapCommandInWorkdir(command, cwd string) string {
	if cwd == "" {
		return command
	}
	return fmt.Sprintf("cd %s && %s", shellQuote(cwd), command)
}

func daemonFormatCommandFailure(action string, exitCode int, stderr string) error {
	trimmed := strings.TrimSpace(stderr)
	if trimmed == "" {
		return fmt.Errorf("%s failed (exit %d)", action, exitCode)
	}
	return fmt.Errorf("%s failed (exit %d): %s", action, exitCode, trimmed)
}

func logDiagnostic(label, text string) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return
	}
	fmt.Fprintf(os.Stderr, "codespace-mcp: %s: %s\n", label, trimmed)
}

func summarizeTmuxVerificationFailure(stderr string) string {
	lower := strings.ToLower(stderr)
	switch {
	case strings.Contains(lower, "shim exists but is not on path"):
		return "the tmux shim exists, but `$HOME/.local/share/mise/shims` is not on PATH"
	case strings.Contains(lower, "shim not found after install"):
		return "mise did not create a tmux shim in `$HOME/.local/share/mise/shims`"
	default:
		return "verify that `mise use -g tmux` succeeds and that tmux is on PATH in the codespace"
	}
}

func daemonLogDiagnostic(label, text string) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return
	}
	fmt.Fprintf(os.Stderr, "codespace-mcp: %s: %s\n", label, trimmed)
}

func daemonSummarizeTmuxVerificationFailure(stderr string) string {
	lower := strings.ToLower(stderr)
	switch {
	case strings.Contains(lower, "shim exists but is not on path"):
		return "the tmux shim exists, but `$HOME/.local/share/mise/shims` is not on PATH"
	case strings.Contains(lower, "shim not found after install"):
		return "mise did not create a tmux shim in `$HOME/.local/share/mise/shims`"
	default:
		return "verify that `mise use -g tmux` succeeds and that tmux is on PATH in the codespace"
	}
}
