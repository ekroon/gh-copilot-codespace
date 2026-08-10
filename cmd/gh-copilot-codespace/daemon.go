package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ekroon/gh-copilot-codespace/internal/codespaceenv"
	"github.com/ekroon/gh-copilot-codespace/internal/daemonproto"
)

const (
	daemonTmuxPrefix        = "copilot-"
	daemonMisePATH          = `PATH="$HOME/.local/bin:$HOME/.local/share/mise/shims:$PATH"`
	daemonSessionExitMarker = "[session exited]"
	daemonCompletionOption  = "@copilot_completion_channel"
)

var errDaemonCanceled = errors.New("request canceled")
var daemonPaneIDRe = regexp.MustCompile(`^%[0-9]+$`)
var daemonTmuxStartMu sync.Mutex

type daemonInflight struct {
	ctxCancel context.CancelFunc

	mu   sync.Mutex
	pgid int
}

type daemonSessionState struct {
	done chan struct{}
	once sync.Once

	mu           sync.Mutex
	waiterCancel context.CancelFunc
}

var daemonSessions sync.Map

func newDaemonSessionState() *daemonSessionState {
	return &daemonSessionState{done: make(chan struct{})}
}

func (s *daemonSessionState) complete() {
	s.once.Do(func() { close(s.done) })
}

func (s *daemonSessionState) setWaiterCancel(cancel context.CancelFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.done:
		cancel()
	default:
		s.waiterCancel = cancel
	}
}

func (s *daemonSessionState) cancelWaiter() {
	s.mu.Lock()
	cancel := s.waiterCancel
	s.waiterCancel = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func daemonRemoveSessionState(sessionID string) {
	if state, ok := daemonSessions.LoadAndDelete(sessionID); ok {
		sessionState := state.(*daemonSessionState)
		sessionState.cancelWaiter()
		sessionState.complete()
	}
	daemonReleaseSessionID(sessionID)
}

func daemonCancelAllSessionWaiters() {
	daemonSessions.Range(func(key, _ any) bool {
		daemonRemoveSessionState(key.(string))
		return true
	})
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
	defer daemonCancelAllSessionWaiters()
	defer daemonStopAllProcessSessions()

	startedAt := time.Now().UTC().Format(time.RFC3339)
	dec := daemonproto.NewDecoder(in)
	enc := daemonproto.NewEncoder(out)

	var writeMu sync.Mutex
	writeFrame := func(frame daemonproto.Frame) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return enc.Write(frame)
	}

	if err := writeFrame(daemonproto.NewHello(daemonproto.ProtocolVersion, daemonAdvertisedVerbs())); err != nil {
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

func daemonAdvertisedVerbs() []daemonproto.Verb {
	verbs := daemonproto.AllVerbs()
	if daemonProcessSessionsSupported() {
		return verbs
	}
	filtered := make([]daemonproto.Verb, 0, len(verbs)-1)
	for _, verb := range verbs {
		if verb != daemonproto.VerbStartProcessSession {
			filtered = append(filtered, verb)
		}
	}
	return filtered
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
		result, err := daemonView(ctx, params)
		if err != nil {
			return nil, daemonMapError(daemonproto.ErrCodeExecFailed, err)
		}
		return result, nil
	case daemonproto.VerbReadFile:
		var params daemonproto.ReadFileParams
		if err := decodeDaemonParams(frame, &params); err != nil {
			return nil, daemonBadRequest(frame.Verb, err)
		}
		data, err := daemonReadFile(ctx, params.Path, params.Root)
		if err != nil {
			return nil, daemonMapError(daemonproto.ErrCodeExecFailed, err)
		}
		return daemonproto.ReadFileResult{Data: data}, nil
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
	case daemonproto.VerbWriteFile:
		var params daemonproto.WriteFileParams
		if err := decodeDaemonParams(frame, &params); err != nil {
			return nil, daemonBadRequest(frame.Verb, err)
		}
		if err := daemonWriteFile(ctx, params.Path, params.Data, params.Overwrite, params.Root); err != nil {
			return nil, daemonMapError(daemonproto.ErrCodeExecFailed, err)
		}
		return daemonproto.WriteFileResult{Bytes: len(params.Data)}, nil
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
		result, err := daemonGrepFiles(ctx, params)
		if err != nil {
			return nil, daemonMapError(daemonproto.ErrCodeExecFailed, err)
		}
		return result, nil
	case daemonproto.VerbGlob:
		var params daemonproto.GlobParams
		if err := decodeDaemonParams(frame, &params); err != nil {
			return nil, daemonBadRequest(frame.Verb, err)
		}
		result, err := daemonGlobFiles(ctx, params)
		if err != nil {
			return nil, daemonMapError(daemonproto.ErrCodeExecFailed, err)
		}
		return result, nil
	case daemonproto.VerbApplyPatch:
		var params daemonproto.ApplyPatchParams
		if err := decodeDaemonParams(frame, &params); err != nil {
			return nil, daemonBadRequest(frame.Verb, err)
		}
		result, err := daemonApplyPatch(ctx, params)
		if err != nil {
			if daemonIsBadPatch(err) {
				return nil, &daemonHandlerError{code: daemonproto.ErrCodeBadRequest, message: err.Error()}
			}
			return nil, daemonMapError(daemonproto.ErrCodeExecFailed, err)
		}
		return result, nil
	case daemonproto.VerbStartSession:
		var params daemonproto.StartSessionParams
		if err := decodeDaemonParams(frame, &params); err != nil {
			return nil, daemonBadRequest(frame.Verb, err)
		}
		if err := daemonStartSession(ctx, params.SessionID, params.Command, params.Cwd); err != nil {
			return nil, daemonMapError(daemonproto.ErrCodeExecFailed, err)
		}
		return daemonproto.StartSessionResult{SessionID: params.SessionID}, nil
	case daemonproto.VerbStartProcessSession:
		var params daemonproto.StartProcessSessionParams
		if err := decodeDaemonParams(frame, &params); err != nil {
			return nil, daemonBadRequest(frame.Verb, err)
		}
		if !daemonProcessSessionsSupported() {
			return nil, daemonMapError(daemonproto.ErrCodeExecFailed, errors.New("daemon-managed process sessions require delegated cgroup v2 support"))
		}
		if err := daemonStartProcessSession(ctx, params.SessionID, params.Command, params.Cwd); err != nil {
			return nil, daemonMapError(daemonproto.ErrCodeExecFailed, err)
		}
		return daemonproto.StartProcessSessionResult{SessionID: params.SessionID}, nil
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
	case daemonproto.VerbWaitSession:
		var params daemonproto.WaitSessionParams
		if err := decodeDaemonParams(frame, &params); err != nil {
			return nil, daemonBadRequest(frame.Verb, err)
		}
		output, completed, err := daemonWaitSession(ctx, params.SessionID, time.Duration(params.TimeoutMS)*time.Millisecond)
		if err != nil {
			return nil, daemonMapError(daemonproto.ErrCodeExecFailed, err)
		}
		return daemonproto.WaitSessionResult{Output: output, Completed: completed}, nil
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
	result, err := daemonView(ctx, daemonproto.ViewFileParams{Path: path, ViewRange: viewRange})
	if err != nil {
		return "", err
	}
	return result.Content, nil
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
	return daemonEditFileWithHook(ctx, path, oldStr, newStr, nil)
}

func daemonEditFileWithHook(ctx context.Context, path, oldStr, newStr string, afterStage func() error) error {
	if err := ctx.Err(); err != nil {
		return errDaemonCanceled
	}
	content, state, err := daemonReadPatchFile(ctx, path, true, nil)
	if err != nil {
		return fmt.Errorf("edit file: %w", err)
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
	actions := []daemonPatchAction{{
		kind:          daemonPatchUpdate,
		sourcePath:    path,
		targetPath:    path,
		sourceDisplay: path,
		targetDisplay: path,
		sourceState:   state,
		content:       newContent,
		mode:          state.info.Mode().Perm(),
	}}
	if err := daemonCommitPatchPlanWithHook(ctx, actions, afterStage); err != nil {
		return fmt.Errorf("edit file: changed during edit: %w", err)
	}
	return nil
}

func daemonCreateFile(ctx context.Context, path, content string, allowOverwrite bool) error {
	mode := os.FileMode(0o644)
	if allowOverwrite {
		if info, err := os.Stat(path); err == nil {
			mode = info.Mode().Perm()
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("create file: %w", err)
		}
	}
	if err := daemonWriteFileAtomic(ctx, path, []byte(content), mode, allowOverwrite); err != nil {
		if !allowOverwrite && strings.Contains(err.Error(), "already exists") {
			return fmt.Errorf("create file: %s already exists", path)
		}
		return fmt.Errorf("create file: %w", err)
	}
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
	result, err := daemonGrepFiles(ctx, daemonproto.GrepParams{
		Pattern: pattern,
		Path:    path,
		Glob:    globPattern,
		Cwd:     cwd,
	})
	if err != nil {
		return "", err
	}
	return result.Output, nil
}

func daemonGlob(ctx context.Context, pattern, path, cwd string) (string, error) {
	result, err := daemonGlobFiles(ctx, daemonproto.GlobParams{
		Pattern: pattern,
		Path:    path,
		Cwd:     cwd,
	})
	if err != nil {
		return "", err
	}
	return result.Output, nil
}

func daemonGlobToFindName(pattern string) string {
	parts := strings.Split(pattern, "/")
	return parts[len(parts)-1]
}

func daemonTmuxSessionName(sessionID string) string {
	return daemonTmuxPrefix + sessionID
}

func daemonNewTmuxChannel(kind string) (string, error) {
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", fmt.Errorf("generate tmux %s channel: %w", kind, err)
	}
	return fmt.Sprintf("copilot-%s-%x", kind, nonce[:]), nil
}

func daemonCompletionHook(paneID, channel string) string {
	return fmt.Sprintf("if-shell -F '#{==:#{hook_pane},%s}' 'wait-for -S %s'", paneID, channel)
}

func daemonTmuxUpdateEnvironmentCommand(keys []string) string {
	return "tmux set-option -g update-environment " + shellQuote(strings.Join(keys, " "))
}

func daemonMergeTmuxEnvironmentKeys(existing string, required []string) []string {
	keys := make(map[string]struct{}, len(required)+len(strings.Fields(existing)))
	for _, key := range strings.Fields(existing) {
		keys[key] = struct{}{}
	}
	for _, key := range required {
		keys[key] = struct{}{}
	}
	result := make([]string, 0, len(keys))
	for key := range keys {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}

func daemonConfigureRunningTmuxEnvironment(ctx context.Context, keys []string) (bool, error) {
	current, _, exitCode, err := daemonExecTmux(ctx, "tmux show-options -gqv update-environment")
	if err != nil {
		return false, fmt.Errorf("inspect tmux environment updates: %w", err)
	}
	if exitCode != 0 {
		return false, nil
	}

	merged := daemonMergeTmuxEnvironmentKeys(current, keys)
	_, stderr, exitCode, err := daemonExecTmux(ctx, daemonTmuxUpdateEnvironmentCommand(merged))
	if err != nil {
		return true, fmt.Errorf("configure tmux environment updates: %w", err)
	}
	if exitCode != 0 {
		return true, daemonFormatCommandFailure("configure tmux environment updates", exitCode, stderr)
	}
	return true, nil
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
	daemonTmuxStartMu.Lock()
	defer daemonTmuxStartMu.Unlock()

	if err := daemonReserveSessionID(sessionID, "tmux"); err != nil {
		return err
	}
	keepReservation := false
	defer func() {
		if !keepReservation {
			daemonReleaseSessionID(sessionID)
		}
	}()
	name := daemonTmuxSessionName(sessionID)
	completionChannel, err := daemonNewTmuxChannel("exit")
	if err != nil {
		return err
	}
	startChannel, err := daemonNewTmuxChannel("start")
	if err != nil {
		return err
	}
	if err := daemonEnsureTmux(ctx); err != nil {
		return err
	}
	bootstrapKeys := codespaceenv.ProcessBootstrapKeys()
	tmuxRunning, err := daemonConfigureRunningTmuxEnvironment(ctx, bootstrapKeys)
	if err != nil {
		return err
	}

	sessionCommand := daemonWrapCommandInWorkdir(command, cwd)
	wrappedCommand := fmt.Sprintf("tmux wait-for %s; %s", shellQuote(startChannel), sessionCommand)
	createCmd := fmt.Sprintf(
		"tmux new-session -d -P -F '#{pane_id}' -s %s -x 200 -y 50 %s",
		shellQuote(name),
		shellQuote(wrappedCommand),
	)
	paneOut, stderr, exitCode, err := daemonExecTmux(ctx, createCmd)
	if err != nil {
		return fmt.Errorf("start session: %w", err)
	}
	if exitCode != 0 {
		return daemonFormatCommandFailure("start session", exitCode, stderr)
	}
	paneID := strings.TrimSpace(paneOut)
	if !daemonPaneIDRe.MatchString(paneID) {
		_, _, _, _ = daemonExecTmux(context.Background(), fmt.Sprintf("tmux kill-session -t %s", shellQuote(name)))
		return fmt.Errorf("start session: invalid pane id %q", paneID)
	}
	if !tmuxRunning {
		running, configureErr := daemonConfigureRunningTmuxEnvironment(ctx, bootstrapKeys)
		err = configureErr
		if err != nil {
			_, _, _, _ = daemonExecTmux(context.Background(), fmt.Sprintf("tmux kill-session -t %s", shellQuote(name)))
			return fmt.Errorf("configure tmux environment updates: %w", err)
		}
		if !running {
			_, _, _, _ = daemonExecTmux(context.Background(), fmt.Sprintf("tmux kill-session -t %s", shellQuote(name)))
			return errors.New("configure tmux environment updates: tmux server did not remain running")
		}
	}
	completionHook := daemonCompletionHook(paneID, completionChannel)

	cleanupCreatedSession := func() {
		_, _, _, _ = daemonExecTmux(context.Background(), fmt.Sprintf("tmux kill-session -t %s", shellQuote(name)))
		daemonRemoveSessionState(sessionID)
	}
	configureCmd := fmt.Sprintf(
		"tmux set-option -t %s remain-on-exit on && tmux set-option -t %s %s %s && tmux set-hook -t %s pane-died %s",
		shellQuote(name),
		shellQuote(name),
		daemonCompletionOption,
		shellQuote(completionChannel),
		shellQuote(name),
		shellQuote(completionHook),
	)
	_, stderr, exitCode, err = daemonExecTmux(ctx, configureCmd)
	if err != nil {
		cleanupCreatedSession()
		return fmt.Errorf("configure session: %w", err)
	}
	if exitCode != 0 {
		cleanupCreatedSession()
		return daemonFormatCommandFailure("configure session", exitCode, stderr)
	}

	state := newDaemonSessionState()
	if _, loaded := daemonSessions.LoadOrStore(sessionID, state); loaded {
		cleanupCreatedSession()
		return fmt.Errorf("session %q is already tracked", sessionID)
	}
	keepReservation = true
	daemonStartSessionCompletionWaiter(completionChannel, state)

	_, stderr, exitCode, err = daemonExecTmux(ctx, fmt.Sprintf("tmux wait-for -S %s", shellQuote(startChannel)))
	if err != nil {
		cleanupCreatedSession()
		return fmt.Errorf("release session start: %w", err)
	}
	if exitCode != 0 {
		cleanupCreatedSession()
		return daemonFormatCommandFailure("release session start", exitCode, stderr)
	}
	return nil
}

func daemonStartSessionCompletionWaiter(channel string, state *daemonSessionState) {
	waiterCtx, cancel := context.WithCancel(context.Background())
	state.setWaiterCancel(cancel)
	go func() {
		defer cancel()
		defer state.complete()
		_, _, _, _ = daemonExecTmux(waiterCtx, fmt.Sprintf("tmux wait-for %s", shellQuote(channel)))
	}()
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
	if _, ok := daemonProcessSessions.Load(sessionID); ok {
		return daemonWriteProcessSession(ctx, sessionID, input)
	}
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
	output, _, err := daemonReadSessionState(ctx, sessionID)
	return output, err
}

func daemonReadSessionState(ctx context.Context, sessionID string) (string, bool, error) {
	if _, ok := daemonProcessSessions.Load(sessionID); ok {
		return daemonReadProcessSession(ctx, sessionID)
	}
	name := daemonTmuxSessionName(sessionID)
	checkCmd := fmt.Sprintf("tmux has-session -t %s 2>/dev/null", shellQuote(name))
	if _, _, ec, _ := daemonExecTmux(ctx, checkCmd); ec != 0 {
		return "", false, fmt.Errorf("session %q does not exist (command may have exited and been cleaned up)", sessionID)
	}
	cmd := fmt.Sprintf("tmux capture-pane -t %s -p -S -100", shellQuote(name))
	stdout, stderr, exitCode, err := daemonExecTmux(ctx, cmd)
	if err != nil {
		return "", false, fmt.Errorf("read session: %w", err)
	}
	if exitCode != 0 {
		return "", false, daemonFormatCommandFailure("read session", exitCode, stderr)
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
	return stdout, paneDead, nil
}

func daemonWaitSession(ctx context.Context, sessionID string, timeout time.Duration) (string, bool, error) {
	if _, ok := daemonProcessSessions.Load(sessionID); ok {
		return daemonWaitProcessSession(ctx, sessionID, timeout)
	}
	if timeout <= 0 {
		return daemonReadSessionState(ctx, sessionID)
	}

	deadline := time.Now().Add(timeout)
	waitCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	name := daemonTmuxSessionName(sessionID)
	channelOut, stderr, exitCode, err := daemonExecTmux(
		waitCtx,
		fmt.Sprintf("tmux show-options -t %s -v %s", shellQuote(name), daemonCompletionOption),
	)
	if err != nil {
		if ctx.Err() != nil {
			return "", false, ctx.Err()
		}
		if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
			return daemonReadSessionState(ctx, sessionID)
		}
		return "", false, fmt.Errorf("read session completion channel: %w", err)
	}
	if exitCode != 0 {
		return "", false, daemonFormatCommandFailure("read session completion channel", exitCode, stderr)
	}
	channel := strings.TrimSpace(channelOut)
	if channel == "" {
		return "", false, errors.New("session completion channel is empty")
	}

	output, completed, err := daemonReadSessionState(waitCtx, sessionID)
	if err != nil {
		if ctx.Err() != nil {
			return "", false, ctx.Err()
		}
		if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
			return daemonReadSessionState(ctx, sessionID)
		}
		return "", false, err
	}
	if completed {
		return output, true, nil
	}

	stateValue, loaded := daemonSessions.Load(sessionID)
	if !loaded {
		state := newDaemonSessionState()
		stateValue, loaded = daemonSessions.LoadOrStore(sessionID, state)
		if !loaded {
			daemonStartSessionCompletionWaiter(channel, state)
			stateValue = state
		}
	}
	state := stateValue.(*daemonSessionState)

	return daemonWaitForSessionCompletion(
		ctx,
		time.Until(deadline),
		state,
		func(readCtx context.Context) (string, bool, error) {
			return daemonReadSessionState(readCtx, sessionID)
		},
	)
}

func daemonWaitForSessionCompletion(
	ctx context.Context,
	timeout time.Duration,
	state *daemonSessionState,
	readState func(context.Context) (string, bool, error),
) (string, bool, error) {
	if timeout <= 0 {
		return readState(ctx)
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-state.done:
	case <-timer.C:
	case <-ctx.Done():
		if ctx.Err() != nil {
			return "", false, ctx.Err()
		}
	}
	return readState(ctx)
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
	if _, ok := daemonProcessSessions.Load(sessionID); ok {
		return daemonStopProcessSession(ctx, sessionID)
	}
	name := daemonTmuxSessionName(sessionID)
	cmd := fmt.Sprintf("tmux kill-session -t %s", shellQuote(name))
	_, stderr, exitCode, err := daemonExecTmux(ctx, cmd)
	if err != nil {
		return fmt.Errorf("stop session: %w", err)
	}
	if exitCode != 0 {
		return daemonFormatCommandFailure("stop session", exitCode, stderr)
	}
	daemonRemoveSessionState(sessionID)
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
	processSessions := daemonListProcessSessions()
	switch {
	case strings.TrimSpace(stdout) == "":
		return processSessions, nil
	case processSessions == "":
		return stdout, nil
	default:
		return strings.TrimRight(stdout, "\n") + "\n" + processSessions, nil
	}
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
