package delegate

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestDefaultCommandFactory_WithExecAgent(t *testing.T) {
	cmd := defaultCommandFactory(context.Background(), StartOptions{
		CodespaceName: "my-codespace",
		Workdir:       "/workspaces/repo",
		ExecAgent:     "/tmp/gh-copilot-codespace",
	}, []string{"COPILOT_GITHUB_TOKEN=token", "GH_TOKEN=token"})

	got := strings.Join(cmd.Args, " ")
	for _, want := range []string{
		"gh codespace ssh -c my-codespace -- /tmp/gh-copilot-codespace exec --workdir /workspaces/repo",
		"--env COPILOT_GITHUB_TOKEN=token",
		"--env GH_TOKEN=token",
		"copilot --headless --stdio --yolo --log-level error",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected %q in %q", want, got)
		}
	}
}

func TestDefaultCommandFactory_WithoutExecAgent(t *testing.T) {
	cmd := defaultCommandFactory(context.Background(), StartOptions{
		CodespaceName: "my-codespace",
		Workdir:       "/workspaces/repo",
	}, []string{"COPILOT_GITHUB_TOKEN=token"})

	got := strings.Join(cmd.Args, " ")
	for _, want := range []string{
		"gh codespace ssh -c my-codespace -- bash -lc",
		"copilot --headless --stdio --yolo --log-level error",
		"export 'COPILOT_GITHUB_TOKEN=token'",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected %q in %q", want, got)
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

func TestDecorateHeadlessError(t *testing.T) {
	err := decorateHeadlessError(errors.New("boom"), "warning")
	if got := err.Error(); got != "boom (stderr: warning)" {
		t.Fatalf("got %q", got)
	}
}

type fakeRunner struct {
	run func(ctx context.Context, opts StartOptions, progress ProgressFunc) (string, error)
}

func (f fakeRunner) RunTask(ctx context.Context, opts StartOptions, progress ProgressFunc) (string, error) {
	return f.run(ctx, opts, progress)
}
