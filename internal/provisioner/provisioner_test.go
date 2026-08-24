package provisioner

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

type mockCSInfo struct {
	name             string
	repository       string
	workdir          string
	uploadedTerminfo []string
	uploadErr        error
	commands         []string
	runSSH           func(command string) (string, error)
}

func (m *mockCSInfo) CodespaceName() string { return m.name }
func (m *mockCSInfo) Repository() string    { return m.repository }
func (m *mockCSInfo) Workdir() string       { return m.workdir }
func (m *mockCSInfo) RunSSH(_ context.Context, command string) (string, error) {
	m.commands = append(m.commands, command)
	if m.runSSH != nil {
		return m.runSSH(command)
	}
	return "", nil
}
func (m *mockCSInfo) UploadTerminfo(_ context.Context, term string) error {
	m.uploadedTerminfo = append(m.uploadedTerminfo, term)
	return m.uploadErr
}

func TestTerminfoProvisioner_ShouldRun_GhosttySessionWithOverriddenTERM(t *testing.T) {
	p := &TerminfoProvisioner{}
	t.Setenv("TERM", "xterm-color")
	t.Setenv("TERM_PROGRAM", "ghostty")
	t.Setenv("GHOSTTY_RESOURCES_DIR", "/tmp/ghostty")

	if !p.ShouldRun(RunContext{Terminal: DetectedTerminal(os.Getenv("TERM"))}) {
		t.Error("should run when Ghostty is the terminal program")
	}
}

func TestTerminfoProvisioner_ShouldRun_StandardTerminalWithoutGhostty(t *testing.T) {
	p := &TerminfoProvisioner{}
	t.Setenv("TERM", "xterm-color")
	t.Setenv("TERM_PROGRAM", "")
	t.Setenv("GHOSTTY_RESOURCES_DIR", "")

	if p.ShouldRun(RunContext{Terminal: DetectedTerminal(os.Getenv("TERM"))}) {
		t.Error("should not run for standard terminal")
	}
}

func TestTerminfoProvisioner_ShouldRun_Empty(t *testing.T) {
	p := &TerminfoProvisioner{}
	t.Setenv("TERM", "xterm-color")
	t.Setenv("TERM_PROGRAM", "")
	t.Setenv("GHOSTTY_RESOURCES_DIR", "")

	if p.ShouldRun(RunContext{Terminal: DetectedTerminal("")}) {
		t.Error("should not run when terminal is empty")
	}
}

func TestDetectedTerminal_GhosttyNormalizesToXtermGhostty(t *testing.T) {
	t.Setenv("TERM", "xterm-color")
	t.Setenv("TERM_PROGRAM", "ghostty")
	t.Setenv("GHOSTTY_RESOURCES_DIR", "/tmp/ghostty")

	if got := DetectedTerminal(os.Getenv("TERM")); got != "xterm-ghostty" {
		t.Fatalf("DetectedTerminal() = %q, want %q", got, "xterm-ghostty")
	}
}

func TestTerminfoProvisioner_Run_UploadsGhosttyTerminfoWhenTERMOverridden(t *testing.T) {
	p := &TerminfoProvisioner{}
	target := &mockCSInfo{}
	t.Setenv("TERM", "xterm-color")
	t.Setenv("TERM_PROGRAM", "ghostty")
	t.Setenv("GHOSTTY_RESOURCES_DIR", "/tmp/ghostty")

	if err := p.Run(context.Background(), target); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if got, want := target.uploadedTerminfo, []string{"xterm-ghostty"}; !equalStrings(got, want) {
		t.Fatalf("uploadedTerminfo = %v, want %v", got, want)
	}
}

func TestTerminfoProvisioner_Run_UploadsCurrentNonStandardTerm(t *testing.T) {
	p := &TerminfoProvisioner{}
	target := &mockCSInfo{}
	t.Setenv("TERM", "wezterm")
	t.Setenv("TERM_PROGRAM", "")
	t.Setenv("GHOSTTY_RESOURCES_DIR", "")

	if err := p.Run(context.Background(), target); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if got, want := target.uploadedTerminfo, []string{"wezterm"}; !equalStrings(got, want) {
		t.Fatalf("uploadedTerminfo = %v, want %v", got, want)
	}
}

func TestTerminfoProvisioner_Run_DedupesGhosttyTerminfo(t *testing.T) {
	p := &TerminfoProvisioner{}
	target := &mockCSInfo{}
	t.Setenv("TERM", "xterm-ghostty")
	t.Setenv("TERM_PROGRAM", "ghostty")
	t.Setenv("GHOSTTY_RESOURCES_DIR", "/tmp/ghostty")

	if err := p.Run(context.Background(), target); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if got, want := target.uploadedTerminfo, []string{"xterm-ghostty"}; !equalStrings(got, want) {
		t.Fatalf("uploadedTerminfo = %v, want %v", got, want)
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

func TestGitFetchProvisioner_DefaultStartsDetachedFetch(t *testing.T) {
	target := &mockCSInfo{workdir: "/workspaces/repo"}

	if err := (&GitFetchProvisioner{}).Run(context.Background(), target); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(target.commands) != 1 {
		t.Fatalf("commands = %v, want one detached fetch command", target.commands)
	}
	command := target.commands[0]
	for _, want := range []string{"cd '/workspaces/repo'", "nohup", "git fetch origin", "git-fetch.log", "</dev/null", "&"} {
		if !strings.Contains(command, want) {
			t.Fatalf("command = %q, want %q", command, want)
		}
	}
}

func TestGitFetchProvisioner_LocalBranchDoesNotFetch(t *testing.T) {
	target := &mockCSInfo{
		workdir: "/workspaces/repo",
		runSSH: func(command string) (string, error) {
			if strings.Contains(command, "git checkout 'feature'") {
				return "", nil
			}
			t.Fatalf("unexpected command: %s", command)
			return "", nil
		},
	}

	if err := (&GitFetchProvisioner{Branch: "feature"}).Run(context.Background(), target); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(target.commands) != 1 {
		t.Fatalf("commands = %v, want only local checkout", target.commands)
	}
}

func TestGitFetchProvisioner_MissingLocalBranchFetchesRemoteBranchSynchronously(t *testing.T) {
	var call int
	target := &mockCSInfo{
		workdir: "/workspaces/repo",
		runSSH: func(command string) (string, error) {
			call++
			switch call {
			case 1:
				return "", errors.New("local branch missing")
			case 2:
				return "abc refs/heads/feature\n", nil
			case 3, 4:
				return "", nil
			default:
				t.Fatalf("unexpected command: %s", command)
				return "", nil
			}
		},
	}

	if err := (&GitFetchProvisioner{Branch: "feature"}).Run(context.Background(), target); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if got, want := len(target.commands), 4; got != want {
		t.Fatalf("commands = %v, want %d commands", target.commands, want)
	}
	for i, want := range []string{
		"git checkout 'feature'",
		"git ls-remote --heads origin 'feature'",
		"git fetch origin 'refs/heads/feature:refs/remotes/origin/feature'",
		"git checkout --track -b 'feature' 'origin/feature'",
	} {
		if !strings.Contains(target.commands[i], want) {
			t.Fatalf("command[%d] = %q, want %q", i, target.commands[i], want)
		}
	}
}

func TestRunAll_ReportsBackgroundGitFetchStart(t *testing.T) {
	target := &mockCSInfo{workdir: "/workspaces/repo"}
	clock := &sequenceClock{times: []time.Time{
		time.Unix(0, 0),
		time.Unix(2, 0),
	}}
	var out bytes.Buffer

	runAll(context.Background(), []Provisioner{&GitFetchProvisioner{}}, RunContext{}, target, &out, clock.Now)

	if got := out.String(); !strings.Contains(got, "provisioner git-fetch started in background in 2s") {
		t.Fatalf("output = %q, want background start message", got)
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

func TestRunAll_PreservesOrderAndReportsDurations(t *testing.T) {
	var calls []string
	provisioners := []Provisioner{
		&testProvisioner{
			name:      "first",
			shouldRun: true,
			runFunc: func() error {
				calls = append(calls, "first")
				return nil
			},
		},
		&testProvisioner{
			name:      "skip",
			shouldRun: false,
			runFunc: func() error {
				calls = append(calls, "skip")
				return nil
			},
		},
		&testProvisioner{
			name:      "second",
			shouldRun: true,
			runFunc: func() error {
				calls = append(calls, "second")
				return nil
			},
		},
	}

	clock := &sequenceClock{times: []time.Time{
		time.Unix(0, 0),
		time.Unix(2, 0),
		time.Unix(3, 0),
		time.Unix(8, 0),
	}}
	var out bytes.Buffer
	runAll(context.Background(), provisioners, RunContext{}, &mockCSInfo{}, &out, clock.Now)

	if got, want := calls, []string{"first", "second"}; !equalStrings(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	got := out.String()
	if !strings.Contains(got, "provisioner first completed in 2s") || !strings.Contains(got, "provisioner second completed in 5s") {
		t.Fatalf("output = %q, want per-provisioner durations", got)
	}
	if strings.Contains(got, "skip") {
		t.Fatalf("output = %q, want skipped provisioner to remain silent", got)
	}
}

func TestRunAll_ContinuesAfterFailure(t *testing.T) {
	var calls []string
	provisioners := []Provisioner{
		&testProvisioner{
			name:      "failing",
			shouldRun: true,
			runFunc: func() error {
				calls = append(calls, "failing")
				return context.Canceled
			},
		},
		&testProvisioner{
			name:      "later",
			shouldRun: true,
			runFunc: func() error {
				calls = append(calls, "later")
				return nil
			},
		},
	}

	clock := &sequenceClock{times: []time.Time{
		time.Unix(0, 0),
		time.Unix(2, 0),
		time.Unix(2, 0),
		time.Unix(7, 0),
	}}
	var out bytes.Buffer
	runAll(context.Background(), provisioners, RunContext{}, &mockCSInfo{}, &out, clock.Now)

	if got, want := calls, []string{"failing", "later"}; !equalStrings(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	if got := out.String(); !strings.Contains(got, "provisioner failing failed after 2s") || !strings.Contains(got, "provisioner later completed in 5s") {
		t.Fatalf("output = %q, want failure and continuation timings", got)
	}
}

type sequenceClock struct {
	times []time.Time
	i     int
}

func (c *sequenceClock) Now() time.Time {
	if c.i >= len(c.times) {
		return c.times[len(c.times)-1]
	}
	t := c.times[c.i]
	c.i++
	return t
}

type testProvisioner struct {
	name      string
	shouldRun bool
	runFunc   func() error
}

func (p *testProvisioner) Name() string                { return p.name }
func (p *testProvisioner) ShouldRun(_ RunContext) bool { return p.shouldRun }
func (p *testProvisioner) Run(_ context.Context, _ CodespaceTarget) error {
	if p.runFunc != nil {
		return p.runFunc()
	}
	return nil
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
