package delegate

import (
"context"
"strings"
"testing"
"time"
)

func TestBuildCLICommand_WithExecAgent(t *testing.T) {
cliPath, args := buildCLICommand(StartOptions{
CodespaceName: "my-codespace",
Workdir:       "/workspaces/repo",
ExecAgent:     "/tmp/gh-copilot-codespace",
}, "mytoken", "/workspaces/repo")

if cliPath != "gh" {
t.Fatalf("cliPath = %q, want gh", cliPath)
}
got := cliPath + " " + strings.Join(args, " ")
for _, want := range []string{
"gh codespace ssh -c my-codespace -- /tmp/gh-copilot-codespace exec --workdir /workspaces/repo",
"--env COPILOT_GITHUB_TOKEN=mytoken",
"--env GH_TOKEN=mytoken",
"-- copilot --yolo",
} {
if !strings.Contains(got, want) {
t.Fatalf("expected %q in %q", want, got)
}
}
}

func TestBuildCLICommand_WithoutExecAgent(t *testing.T) {
cliPath, args := buildCLICommand(StartOptions{
CodespaceName: "my-codespace",
Workdir:       "/workspaces/repo",
}, "mytoken", "/workspaces/repo")

if cliPath != "gh" {
t.Fatalf("cliPath = %q, want gh", cliPath)
}

// Verify the last arg is "--" (the bash $0 sentinel that makes $@ capture the SDK flags).
if last := args[len(args)-1]; last != "--" {
t.Fatalf("last arg = %q, want \"--\"", last)
}

got := cliPath + " " + strings.Join(args, " ")
for _, want := range []string{
"gh codespace ssh -c my-codespace -- bash -c",
"export 'COPILOT_GITHUB_TOKEN=mytoken'",
`exec copilot --yolo "$@"`,
} {
if !strings.Contains(got, want) {
t.Fatalf("expected %q in %q", want, got)
}
}
}

func TestResolveWorkdir(t *testing.T) {
tests := []struct {
opts StartOptions
want string
}{
{StartOptions{Cwd: "/cwd", Workdir: "/workdir"}, "/cwd"},
{StartOptions{Cwd: "", Workdir: "/workdir"}, "/workdir"},
{StartOptions{Cwd: "/cwd", Workdir: ""}, "/cwd"},
}
for _, tc := range tests {
got := resolveWorkdir(tc.opts)
if got != tc.want {
t.Errorf("resolveWorkdir(%+v) = %q, want %q", tc.opts, got, tc.want)
}
}
}

func TestManagerLifecycle(t *testing.T) {
manager := NewManager(fakeRunner{
run: func(ctx context.Context, opts StartOptions, progress ProgressFunc) (string, error) {
progress("running")
return "done", nil
},
})
manager.now = func() time.Time { return time.Unix(1, 0) }

taskID, err := manager.StartTask(StartOptions{
CodespaceName: "cs",
Workdir:       "/workspaces/repo",
Prompt:        "do the thing",
})
if err != nil {
t.Fatalf("StartTask: %v", err)
}

deadline := time.Now().Add(2 * time.Second)
for time.Now().Before(deadline) {
snapshot, err := manager.GetTask(taskID)
if err != nil {
t.Fatalf("GetTask: %v", err)
}
if snapshot.Status == StatusCompleted {
if snapshot.Result != "done" {
t.Fatalf("result = %q, want done", snapshot.Result)
}
if !strings.Contains(snapshot.Log, "running") {
t.Fatalf("log = %q, want running", snapshot.Log)
}
return
}
time.Sleep(10 * time.Millisecond)
}
t.Fatal("task did not complete")
}

func TestManagerCancelTask(t *testing.T) {
wait := make(chan struct{})
manager := NewManager(fakeRunner{
run: func(ctx context.Context, opts StartOptions, progress ProgressFunc) (string, error) {
<-ctx.Done()
close(wait)
return "", ctx.Err()
},
})

taskID, err := manager.StartTask(StartOptions{
CodespaceName: "cs",
Workdir:       "/workspaces/repo",
Prompt:        "do the thing",
})
if err != nil {
t.Fatalf("StartTask: %v", err)
}

if err := manager.CancelTask(taskID); err != nil {
t.Fatalf("CancelTask: %v", err)
}

select {
case <-wait:
case <-time.After(2 * time.Second):
t.Fatal("task was not canceled")
}

snapshot, err := manager.GetTask(taskID)
if err != nil {
t.Fatalf("GetTask: %v", err)
}
if snapshot.Status != StatusCanceled {
t.Fatalf("status = %s, want %s", snapshot.Status, StatusCanceled)
}
}

type fakeRunner struct {
run func(ctx context.Context, opts StartOptions, progress ProgressFunc) (string, error)
}

func (f fakeRunner) RunTask(ctx context.Context, opts StartOptions, progress ProgressFunc) (string, error) {
return f.run(ctx, opts, progress)
}
