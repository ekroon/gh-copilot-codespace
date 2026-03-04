package provisioner

import (
	"context"
	"testing"
)

type mockCSInfo struct {
	name       string
	repository string
	workdir    string
}

func (m *mockCSInfo) CodespaceName() string { return m.name }
func (m *mockCSInfo) Repository() string    { return m.repository }
func (m *mockCSInfo) Workdir() string       { return m.workdir }
func (m *mockCSInfo) RunSSH(_ context.Context, _ string) (string, error) {
	return "", nil
}

func TestTerminfoProvisioner_ShouldRun_GhosttyDetected(t *testing.T) {
	p := &TerminfoProvisioner{}
	t.Setenv("TERM", "xterm-ghostty")

	if !p.ShouldRun(RunContext{Terminal: "xterm-ghostty"}) {
		t.Error("should run when TERM is xterm-ghostty")
	}
}

func TestTerminfoProvisioner_ShouldRun_NonGhostty(t *testing.T) {
	p := &TerminfoProvisioner{}

	if p.ShouldRun(RunContext{Terminal: "xterm-256color"}) {
		t.Error("should not run for standard terminal")
	}
}

func TestTerminfoProvisioner_ShouldRun_Empty(t *testing.T) {
	p := &TerminfoProvisioner{}

	if p.ShouldRun(RunContext{Terminal: ""}) {
		t.Error("should not run when terminal is empty")
	}
}

func TestTerminfoProvisioner_Name(t *testing.T) {
	p := &TerminfoProvisioner{}
	if p.Name() != "terminfo" {
		t.Errorf("got name %q, want %q", p.Name(), "terminfo")
	}
}

func TestGitFetchProvisioner_Name(t *testing.T) {
	p := &GitFetchProvisioner{}
	if p.Name() != "git-fetch" {
		t.Errorf("got name %q, want %q", p.Name(), "git-fetch")
	}
}

func TestGitFetchProvisioner_ShouldRun(t *testing.T) {
	p := &GitFetchProvisioner{}
	if !p.ShouldRun(RunContext{}) {
		t.Error("git-fetch should always run")
	}
}

func TestWaitForConfigProvisioner_Name(t *testing.T) {
	p := &WaitForConfigProvisioner{}
	if p.Name() != "wait-for-config" {
		t.Errorf("got name %q, want %q", p.Name(), "wait-for-config")
	}
}

func TestWaitForConfigProvisioner_ShouldRun_NewCodespace(t *testing.T) {
	p := &WaitForConfigProvisioner{}
	if !p.ShouldRun(RunContext{IsNewCodespace: true}) {
		t.Error("should run for newly created codespaces")
	}
}

func TestWaitForConfigProvisioner_ShouldRun_ExistingCodespace(t *testing.T) {
	p := &WaitForConfigProvisioner{}
	if p.ShouldRun(RunContext{IsNewCodespace: false}) {
		t.Error("should not run for existing codespaces")
	}
}

func TestRunAll_SkipsNonMatching(t *testing.T) {
	ran := false
	provisioners := []Provisioner{
		&testProvisioner{
			name:      "test",
			shouldRun: false,
			runFunc:   func() error { ran = true; return nil },
		},
	}

	RunAll(context.Background(), provisioners, RunContext{}, nil)

	if ran {
		t.Error("provisioner should not have run")
	}
}

func TestRunAll_RunsMatching(t *testing.T) {
	ran := false
	provisioners := []Provisioner{
		&testProvisioner{
			name:      "test",
			shouldRun: true,
			runFunc:   func() error { ran = true; return nil },
		},
	}

	RunAll(context.Background(), provisioners, RunContext{}, nil)

	if !ran {
		t.Error("provisioner should have run")
	}
}

type testProvisioner struct {
	name      string
	shouldRun bool
	runFunc   func() error
}

func (p *testProvisioner) Name() string              { return p.name }
func (p *testProvisioner) ShouldRun(_ RunContext) bool { return p.shouldRun }
func (p *testProvisioner) Run(_ context.Context, _ CodespaceTarget) error {
	if p.runFunc != nil {
		return p.runFunc()
	}
	return nil
}
