package delegate

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
	"sort"
	"strings"
	"sync"
)

type CommandFactory func(ctx context.Context, opts StartOptions, envPairs []string) *exec.Cmd
type TokenResolver func(ctx context.Context) (string, error)

type HeadlessRunner struct {
	commandFactory CommandFactory
	tokenResolver  TokenResolver
}

func NewHeadlessRunner() *HeadlessRunner {
	return &HeadlessRunner{
		commandFactory: defaultCommandFactory,
		tokenResolver:  resolveGitHubToken,
	}
}

func (r *HeadlessRunner) RunTask(ctx context.Context, opts StartOptions, progress ProgressFunc) (string, error) {
	token, err := r.tokenResolver(ctx)
	if err != nil {
		return "", err
	}

	cmd := r.commandFactory(ctx, opts, authEnvPairs(token))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", fmt.Errorf("headless delegate stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("headless delegate stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", fmt.Errorf("headless delegate stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("starting remote headless worker: %w", err)
	}

	stderrBuf := &limitedBuffer{max: 8 * 1024}
	var stderrWG sync.WaitGroup
	stderrWG.Add(1)
	go func() {
		defer stderrWG.Done()
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			stderrBuf.WriteString(line + "\n")
		}
	}()

	rpc := newRPCClient(&stdioReadWriteCloser{reader: stdout, writer: stdin, closer: stdin})

	result, runErr := runHeadlessSession(ctx, rpc, opts, progress)
	closeErr := rpc.Close()
	waitErr := cmd.Wait()
	stderrWG.Wait()

	if runErr != nil {
		return "", decorateHeadlessError(runErr, stderrBuf.String())
	}
	if closeErr != nil && ctx.Err() == nil {
		return "", decorateHeadlessError(fmt.Errorf("closing remote headless stdin: %w", closeErr), stderrBuf.String())
	}
	if waitErr != nil && ctx.Err() == nil {
		return "", decorateHeadlessError(fmt.Errorf("remote headless worker exited: %w", waitErr), stderrBuf.String())
	}

	return result, nil
}

func runHeadlessSession(ctx context.Context, rpc *rpcClient, opts StartOptions, progress ProgressFunc) (string, error) {
	progress(fmt.Sprintf("Starting headless delegate on %s.", opts.CodespaceName))

	params := map[string]any{
		"clientName":        "gh-copilot-codespace",
		"requestPermission": true,
		"requestUserInput":  false,
		"hooks":             false,
		"workingDirectory":  opts.Cwd,
		"envValueMode":      "direct",
		"streaming":         false,
	}
	if opts.Model != "" {
		params["model"] = opts.Model
	}

	var createResp struct {
		SessionID string `json:"sessionId"`
	}
	if err := rpc.Call(ctx, "session.create", params, &createResp); err != nil {
		return "", fmt.Errorf("creating remote delegate session: %w", err)
	}
	progress(fmt.Sprintf("Created delegate session %s.", createResp.SessionID))

	idleCh := make(chan struct{}, 1)
	errCh := make(chan error, 1)
	var resultMu sync.Mutex
	var finalMessage string

	rpc.SetEventHandler(func(method string, params json.RawMessage) {
		switch method {
		case "session.event":
			var notification struct {
				SessionID string `json:"sessionId"`
				Event     struct {
					Type string          `json:"type"`
					Data json.RawMessage `json:"data"`
				} `json:"event"`
			}
			if err := json.Unmarshal(params, &notification); err != nil {
				return
			}
			if notification.SessionID != createResp.SessionID {
				return
			}

			switch notification.Event.Type {
			case "assistant.message":
				if text := extractMessageContent(notification.Event.Data); text != "" {
					resultMu.Lock()
					finalMessage = text
					resultMu.Unlock()
					progress("Received final assistant response.")
				}
			case "session.error":
				select {
				case errCh <- extractSessionError(notification.Event.Data):
				default:
				}
			case "session.idle":
				select {
				case idleCh <- struct{}{}:
				default:
				}
			}
		case "session.lifecycle":
			var notification struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(params, &notification); err == nil && notification.Type != "" {
				progress(fmt.Sprintf("Lifecycle event: %s.", notification.Type))
			}
		}
	})

	var sendResp struct {
		MessageID string `json:"messageId"`
	}
	if err := rpc.Call(ctx, "session.send", map[string]any{
		"sessionId": createResp.SessionID,
		"prompt":    opts.Prompt,
	}, &sendResp); err != nil {
		return "", fmt.Errorf("sending delegate prompt: %w", err)
	}

	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case err := <-errCh:
			return "", err
		case <-idleCh:
			resultMu.Lock()
			defer resultMu.Unlock()
			return finalMessage, nil
		}
	}
}

func defaultCommandFactory(ctx context.Context, opts StartOptions, envPairs []string) *exec.Cmd {
	workdir := opts.Cwd
	if workdir == "" {
		workdir = opts.Workdir
	}

	args := []string{"codespace", "ssh", "-c", opts.CodespaceName, "--"}
	if opts.ExecAgent != "" {
		args = append(args, opts.ExecAgent, "exec", "--workdir", workdir)
		for _, pair := range envPairs {
			args = append(args, "--env", pair)
		}
		args = append(args, "--", "copilot", "--headless", "--stdio", "--yolo", "--log-level", "error")
		return exec.CommandContext(ctx, "gh", args...)
	}

	var exports []string
	for _, pair := range envPairs {
		exports = append(exports, "export "+shellQuote(pair))
	}
	scriptParts := []string{"cd " + shellQuote(workdir)}
	scriptParts = append(scriptParts, exports...)
	scriptParts = append(scriptParts, "exec copilot --headless --stdio --yolo --log-level error")
	args = append(args, "bash", "-lc", strings.Join(scriptParts, " && "))
	return exec.CommandContext(ctx, "gh", args...)
}

func resolveGitHubToken(ctx context.Context) (string, error) {
	for _, key := range []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value, nil
		}
	}

	out, err := exec.CommandContext(ctx, "gh", "auth", "token").Output()
	if err != nil {
		return "", fmt.Errorf("resolving GitHub token with gh auth token: %w", err)
	}
	token := strings.TrimSpace(string(out))
	if token == "" {
		return "", fmt.Errorf("gh auth token returned an empty token")
	}
	return token, nil
}

func authEnvPairs(token string) []string {
	pairs := []string{
		"COPILOT_GITHUB_TOKEN=" + token,
		"GH_TOKEN=" + token,
		"GITHUB_TOKEN=" + token,
	}
	sort.Strings(pairs)
	return pairs
}

func extractMessageContent(data json.RawMessage) string {
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		return ""
	}
	if content, ok := payload["content"].(string); ok {
		return content
	}
	return ""
}

func extractSessionError(data json.RawMessage) error {
	var payload struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return fmt.Errorf("remote delegate session failed")
	}
	if payload.Message == "" {
		return fmt.Errorf("remote delegate session failed")
	}
	return errors.New(payload.Message)
}

func decorateHeadlessError(err error, stderr string) error {
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return err
	}
	return fmt.Errorf("%w (stderr: %s)", err, stderr)
}

type stdioReadWriteCloser struct {
	reader io.Reader
	writer io.Writer
	closer io.Closer
}

func (s *stdioReadWriteCloser) Read(p []byte) (int, error)  { return s.reader.Read(p) }
func (s *stdioReadWriteCloser) Write(p []byte) (int, error) { return s.writer.Write(p) }
func (s *stdioReadWriteCloser) Close() error                { return s.closer.Close() }

type limitedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
	max int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	n, err := b.buf.Write(p)
	if b.max > 0 && b.buf.Len() > b.max {
		trimmed := b.buf.Bytes()
		b.buf.Reset()
		b.buf.Write(trimmed[len(trimmed)-b.max:])
	}
	return n, err
}

func (b *limitedBuffer) WriteString(s string) {
	_, _ = b.Write([]byte(s))
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}
