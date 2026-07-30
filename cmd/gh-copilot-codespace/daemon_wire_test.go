package main

import (
	"context"
	"errors"
	"testing"

	"github.com/ekroon/gh-copilot-codespace/internal/helperinfo"
	"github.com/ekroon/gh-copilot-codespace/internal/registry"
	"github.com/ekroon/gh-copilot-codespace/internal/ssh"
)

func TestDaemonDisabledOptsOut(t *testing.T) {
	t.Setenv(daemonDisabledEnv, "1")

	reg := registry.New()
	sshClient := ssh.NewClient("test-codespace")
	if err := reg.Register(&registry.ManagedCodespace{Alias: "test", Name: "test-codespace", Executor: sshClient}); err != nil {
		t.Fatalf("register: %v", err)
	}

	closers := wrapExecutorsWithDaemon(context.Background(), reg)
	if closers != nil {
		t.Fatalf("expected nil closers, got %d", len(closers))
	}
	cs, err := reg.Resolve("test")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if cs.Executor != sshClient {
		t.Fatalf("expected original ssh executor to remain registered")
	}
}

func TestDaemonDisabledEnvVariants(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{value: "1", want: true},
		{value: "true", want: true},
		{value: "yes", want: true},
		{value: "", want: false},
		{value: "0", want: false},
		{value: "false", want: false},
		{value: "no", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			t.Setenv(daemonDisabledEnv, tt.value)
			if got := daemonDisabled(); got != tt.want {
				t.Fatalf("daemonDisabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestWrapExecutorsSkipsNonSSHExecutors(t *testing.T) {
	t.Setenv(daemonDisabledEnv, "")

	reg := registry.New()
	exec := &daemonWireMockExecutor{}
	if err := reg.Register(&registry.ManagedCodespace{Alias: "test", Name: "test-codespace", Executor: exec}); err != nil {
		t.Fatalf("register: %v", err)
	}

	closers := wrapExecutorsWithDaemon(context.Background(), reg)
	if len(closers) != 0 {
		t.Fatalf("expected no closers, got %d", len(closers))
	}
	cs, err := reg.Resolve("test")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if cs.Executor != exec {
		t.Fatalf("expected non-ssh executor to remain registered")
	}
}

func TestWrapExecutorsHandlesDialFailureGracefully(t *testing.T) {
	t.Setenv(daemonDisabledEnv, "")
	oldDeploy := daemonDeployBinary
	daemonDeployBinary = func(*ssh.Client, string) (string, error) {
		return "", errors.New("deploy failed")
	}
	t.Cleanup(func() { daemonDeployBinary = oldDeploy })

	reg := registry.New()
	sshClient := ssh.NewClient("bogus-codespace")
	sshClient.SetWorkdir("/workspaces/repo")
	if err := reg.Register(&registry.ManagedCodespace{Alias: "test", Name: "bogus-codespace", Executor: sshClient}); err != nil {
		t.Fatalf("register: %v", err)
	}

	closers := wrapExecutorsWithDaemon(context.Background(), reg)
	if len(closers) != 0 {
		t.Fatalf("expected no closers, got %d", len(closers))
	}
	cs, err := reg.Resolve("test")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if cs.Executor != sshClient {
		t.Fatalf("expected original ssh executor after dial failure")
	}
}

func TestDaemonDeployerReusesVerifiedHelperPath(t *testing.T) {
	client := ssh.NewClient("test-codespace")
	if err := client.SelectFilesystemHelper("/home/codespace/helper", helperinfo.Current()); err != nil {
		t.Fatalf("SelectFilesystemHelper() error = %v", err)
	}
	cs := &registry.ManagedCodespace{
		Name:       "test-codespace",
		HelperPath: "/home/codespace/helper",
		Executor:   client,
	}

	path, err := daemonDeployerFor(cs, client)(client, cs.Name)
	if err != nil {
		t.Fatalf("daemon deployer error = %v", err)
	}
	if path != cs.HelperPath {
		t.Fatalf("daemon path = %q, want %q", path, cs.HelperPath)
	}
}

type daemonWireMockExecutor struct {
	workdir string
}

func (m *daemonWireMockExecutor) ViewFile(context.Context, string, []int) (string, error) {
	return "", nil
}

func (m *daemonWireMockExecutor) EditFile(context.Context, string, string, string) error {
	return nil
}

func (m *daemonWireMockExecutor) CreateFile(context.Context, string, string) error {
	return nil
}

func (m *daemonWireMockExecutor) RunBash(context.Context, string, string) (string, string, int, error) {
	return "", "", 0, nil
}

func (m *daemonWireMockExecutor) Grep(context.Context, string, string, string, string) (string, error) {
	return "", nil
}

func (m *daemonWireMockExecutor) Glob(context.Context, string, string, string) (string, error) {
	return "", nil
}

func (m *daemonWireMockExecutor) StartSession(context.Context, string, string, string) error {
	return nil
}

func (m *daemonWireMockExecutor) WriteSession(context.Context, string, string) error {
	return nil
}

func (m *daemonWireMockExecutor) ReadSession(context.Context, string) (string, error) {
	return "", nil
}

func (m *daemonWireMockExecutor) StopSession(context.Context, string) error {
	return nil
}

func (m *daemonWireMockExecutor) ListSessions(context.Context) (string, error) {
	return "", nil
}

func (m *daemonWireMockExecutor) SetWorkdir(dir string) {
	m.workdir = dir
}

func (m *daemonWireMockExecutor) GetWorkdir() string {
	return m.workdir
}

var _ ssh.Executor = (*daemonWireMockExecutor)(nil)
