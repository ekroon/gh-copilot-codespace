package delegate

import (
"context"
"fmt"
"os"
"os/exec"
"sort"
"strings"
"sync"

copilot "github.com/github/copilot-sdk/go"
)

type TokenResolver func(ctx context.Context) (string, error)

type HeadlessRunner struct {
tokenResolver TokenResolver
}

func NewHeadlessRunner() *HeadlessRunner {
return &HeadlessRunner{
tokenResolver: resolveGitHubToken,
}
}

func (r *HeadlessRunner) RunTask(ctx context.Context, opts StartOptions, progress ProgressFunc) (string, error) {
token, err := r.tokenResolver(ctx)
if err != nil {
return "", err
}

workdir := resolveWorkdir(opts)
cliPath, cliArgs := buildCLICommand(opts, token, workdir)

client := copilot.NewClient(&copilot.ClientOptions{
CLIPath:     cliPath,
CLIArgs:     cliArgs,
UseStdio:    copilot.Bool(true),
AutoRestart: copilot.Bool(false),
LogLevel:    "error",
})

if err := client.Start(ctx); err != nil {
return "", fmt.Errorf("starting remote delegate: %w", err)
}
defer client.ForceStop()

progress(fmt.Sprintf("Starting delegate session on %s.", opts.CodespaceName))

session, err := client.CreateSession(ctx, &copilot.SessionConfig{
OnPermissionRequest: copilot.PermissionHandler.ApproveAll,
Model:               opts.Model,
WorkingDirectory:    workdir,
})
if err != nil {
return "", fmt.Errorf("creating delegate session: %w", err)
}
defer session.Disconnect()

progress(fmt.Sprintf("Created delegate session %s.", session.SessionID))

idleCh := make(chan struct{}, 1)
errCh := make(chan error, 1)
var (
resultMu     sync.Mutex
finalMessage string
)

unsubscribe := session.On(func(event copilot.SessionEvent) {
switch event.Type {
case copilot.AssistantMessage:
if event.Data.Content != nil {
resultMu.Lock()
finalMessage = *event.Data.Content
resultMu.Unlock()
progress("Received assistant response.")
}
case copilot.SessionError:
errMsg := "delegate session error"
if event.Data.Message != nil {
errMsg = *event.Data.Message
}
select {
case errCh <- fmt.Errorf("%s", errMsg):
default:
}
case copilot.SessionIdle:
select {
case idleCh <- struct{}{}:
default:
}
}
})
defer unsubscribe()

if _, err = session.Send(ctx, copilot.MessageOptions{Prompt: opts.Prompt}); err != nil {
return "", fmt.Errorf("sending delegate prompt: %w", err)
}

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

// resolveWorkdir returns the effective working directory from StartOptions,
// preferring Cwd over Workdir.
func resolveWorkdir(opts StartOptions) string {
if opts.Cwd != "" {
return opts.Cwd
}
return opts.Workdir
}

// buildCLICommand returns the executable path and arguments to route a copilot
// headless session through gh codespace ssh. The copilot-sdk appends
// "--headless --no-auto-update --log-level <level> --stdio" to the returned
// args, which ssh forwards as the remote command.
func buildCLICommand(opts StartOptions, token, workdir string) (string, []string) {
envPairs := authEnvPairs(token)

if opts.ExecAgent != "" {
// Use exec agent to set workdir and env without shell escaping.
// SDK appends: --headless --no-auto-update --log-level error --stdio
// Full remote: <execAgent> exec --workdir <dir> --env K=V ... -- copilot --yolo --headless ...
args := []string{
"codespace", "ssh", "-c", opts.CodespaceName, "--",
opts.ExecAgent, "exec", "--workdir", workdir,
}
for _, pair := range envPairs {
args = append(args, "--env", pair)
}
args = append(args, "--", "copilot", "--yolo")
return "gh", args
}

// Fallback: bash -c with "$@" to forward the SDK-appended flags.
// "bash -c script -- arg1 arg2..." sets $0="--" and $@="arg1 arg2...",
// so the SDK's "--headless --no-auto-update --log-level error --stdio"
// are captured in $@ and forwarded to copilot.
var exports []string
for _, pair := range envPairs {
exports = append(exports, "export "+shellQuote(pair))
}
scriptParts := []string{"cd " + shellQuote(workdir)}
scriptParts = append(scriptParts, exports...)
scriptParts = append(scriptParts, `exec copilot --yolo "$@"`)
script := strings.Join(scriptParts, " && ")

return "gh", []string{
"codespace", "ssh", "-c", opts.CodespaceName, "--",
"bash", "-c", script, "--",
}
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

func shellQuote(s string) string {
return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}
