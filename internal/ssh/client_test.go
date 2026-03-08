package ssh

import (
	"context"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestParseInput(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{"literal text", "hello", []string{"hello"}},
		{"single enter", "{enter}", []string{"\x00Enter"}},
		{"text then enter", "ls{enter}", []string{"ls", "\x00Enter"}},
		{"two special keys", "{up}{down}", []string{"\x00Up", "\x00Down"}},
		{"text-key-text", "foo{enter}bar", []string{"foo", "\x00Enter", "bar"}},
		{"empty string", "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseInput(tt.input)
			if len(got) != len(tt.want) {
				t.Fatalf("parseInput(%q) = %v, want %v", tt.input, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("parseInput(%q)[%d] = %q, want %q", tt.input, i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestGlobToFindName(t *testing.T) {
	tests := []struct {
		pattern string
		want    string
	}{
		{"**/*.go", "*.go"},
		{"src/**/*.test.js", "*.test.js"},
		{"*.ts", "*.ts"},
		{"a/b/c/d.go", "d.go"},
	}
	for _, tt := range tests {
		t.Run(tt.pattern, func(t *testing.T) {
			if got := globToFindName(tt.pattern); got != tt.want {
				t.Errorf("globToFindName(%q) = %q, want %q", tt.pattern, got, tt.want)
			}
		})
	}
}

func TestShellQuote(t *testing.T) {
	tests := []struct {
		name string
		s    string
		want string
	}{
		{"simple", "hello", "'hello'"},
		{"with space", "hello world", "'hello world'"},
		{"with single quote", "it's", "'it'\"'\"'s'"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shellQuote(tt.s); got != tt.want {
				t.Errorf("shellQuote(%q) = %q, want %q", tt.s, got, tt.want)
			}
		})
	}
}

func TestPathDir(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/workspaces/repo", "/workspaces"},
		{"file.txt", "."},
		{"/a", ""},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := pathDir(tt.path); got != tt.want {
				t.Errorf("pathDir(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

func TestTmuxSessionName(t *testing.T) {
	if got := tmuxSessionName("abc"); got != "copilot-abc" {
		t.Errorf("tmuxSessionName(%q) = %q, want %q", "abc", got, "copilot-abc")
	}
}

func TestCleanPaneOutput(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			"strips pane is dead line",
			"hello world\nPane is dead (status 0, Thu Mar  5 08:17:27 2026)\n",
			"hello world",
		},
		{
			"strips pane is dead with non-zero status",
			"some output\nPane is dead (status 1, Thu Mar  5 09:00:00 2026)\n\n",
			"some output",
		},
		{
			"no pane is dead line",
			"just output\nmore output\n",
			"just output\nmore output",
		},
		{
			"trims trailing blank lines",
			"output\n\n\n\n",
			"output",
		},
		{
			"empty output",
			"\n\n",
			"",
		},
		{
			"pane is dead only",
			"Pane is dead (status 0, Thu Mar  5 08:17:27 2026)\n",
			"",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cleanPaneOutput(tt.input); got != tt.want {
				t.Errorf("cleanPaneOutput() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParsePaneStatus(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		wantDead     bool
		wantExitCode int
		wantErr      bool
	}{
		{name: "dead pane", input: "1 127\n", wantDead: true, wantExitCode: 127},
		{name: "running pane", input: "0 0\n", wantDead: false, wantExitCode: 0},
		{name: "invalid format", input: "oops", wantErr: true},
		{name: "invalid exit code", input: "1 nope", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotDead, gotExitCode, err := parsePaneStatus(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatal("parsePaneStatus() error = nil, want non-nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("parsePaneStatus() error = %v", err)
			}
			if gotDead != tt.wantDead || gotExitCode != tt.wantExitCode {
				t.Fatalf("parsePaneStatus() = (%v, %d), want (%v, %d)", gotDead, gotExitCode, tt.wantDead, tt.wantExitCode)
			}
		})
	}
}

func TestSetGetWorkdir(t *testing.T) {
	c := NewClient("test-codespace")

	// Default should fall back to env or /workspaces
	got := c.GetWorkdir()
	if got == "" {
		t.Fatal("GetWorkdir() returned empty string")
	}

	// Set a custom workdir
	c.SetWorkdir("/workspaces/myproject/src")
	if got := c.GetWorkdir(); got != "/workspaces/myproject/src" {
		t.Errorf("GetWorkdir() = %q, want %q", got, "/workspaces/myproject/src")
	}

	// Override again
	c.SetWorkdir("/workspaces/other")
	if got := c.GetWorkdir(); got != "/workspaces/other" {
		t.Errorf("GetWorkdir() = %q, want %q", got, "/workspaces/other")
	}
}

func TestResolveWorkdir(t *testing.T) {
	c := NewClient("demo")
	c.SetWorkdir("/workspaces/default")

	if got := c.resolveWorkdir(""); got != "/workspaces/default" {
		t.Fatalf("resolveWorkdir(\"\") = %q, want %q", got, "/workspaces/default")
	}
	if got := c.resolveWorkdir("/workspaces/override"); got != "/workspaces/override" {
		t.Fatalf("resolveWorkdir(override) = %q, want %q", got, "/workspaces/override")
	}
}

func TestWrapCommandInWorkdir(t *testing.T) {
	got := wrapCommandInWorkdir("pwd", "/workspaces/repo")
	want := "cd '/workspaces/repo' && pwd"
	if got != want {
		t.Fatalf("wrapCommandInWorkdir() = %q, want %q", got, want)
	}
}

func TestEnvSecretsLoaderPreservesExistingVars(t *testing.T) {
	if !strings.Contains(envSecretsLoader, `printenv "$key" >/dev/null 2>&1 || export "$key=$(echo "$value" | base64 -d)"`) {
		t.Fatalf("envSecretsLoader should preserve already-set variables, got %q", envSecretsLoader)
	}
}

type fakeExecCall struct {
	name string
	args []string
}

type fakeExecResponse struct {
	stdout   string
	stderr   string
	exitCode int
}

func fakeCommandContext(t *testing.T, calls *[]fakeExecCall, responses []fakeExecResponse) func(context.Context, string, ...string) *exec.Cmd {
	t.Helper()

	index := 0
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if index >= len(responses) {
			t.Fatalf("unexpected command %q with args %v", name, args)
		}
		resp := responses[index]
		index++

		copiedArgs := append([]string(nil), args...)
		*calls = append(*calls, fakeExecCall{name: name, args: copiedArgs})

		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestCommandContextHelperProcess", "--", name)
		cmd.Args = append(cmd.Args, args...)
		cmd.Env = append(os.Environ(),
			"GO_WANT_HELPER_PROCESS=1",
			"GO_HELPER_STDOUT="+resp.stdout,
			"GO_HELPER_STDERR="+resp.stderr,
			"GO_HELPER_EXIT="+strconv.Itoa(resp.exitCode),
		)
		return cmd
	}
}

func TestCommandContextHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}

	_, _ = os.Stdout.WriteString(os.Getenv("GO_HELPER_STDOUT"))
	_, _ = os.Stderr.WriteString(os.Getenv("GO_HELPER_STDERR"))

	exitCode, err := strconv.Atoi(os.Getenv("GO_HELPER_EXIT"))
	if err != nil {
		exitCode = 1
	}
	os.Exit(exitCode)
}

func TestViewFileRetriesReadOnlyTransportFailure(t *testing.T) {
	client := NewClient("demo")
	socketPath := t.TempDir() + "/control.sock"
	if err := os.WriteFile(socketPath, []byte("socket"), 0o600); err != nil {
		t.Fatalf("write control socket: %v", err)
	}

	client.sshConfigPath = "/tmp/ssh-config"
	client.sshHost = "cs.demo"
	client.controlSocket = socketPath

	var calls []fakeExecCall
	client.commandContext = fakeCommandContext(t, &calls, []fakeExecResponse{
		{exitCode: 255},
		{exitCode: 255},
		{stdout: "1. hello\n"},
	})

	got, err := client.ViewFile(context.Background(), "/tmp/file.txt", nil)
	if err != nil {
		t.Fatalf("ViewFile() error = %v", err)
	}
	if got != "1. hello\n" {
		t.Fatalf("ViewFile() = %q, want %q", got, "1. hello\n")
	}
	if client.sshConfigPath != "" {
		t.Fatalf("sshConfigPath = %q, want empty after fallback", client.sshConfigPath)
	}
	if _, err := os.Stat(socketPath); !os.IsNotExist(err) {
		t.Fatalf("control socket still exists after fallback: %v", err)
	}

	expectedCommand := envSecretsLoader + " && awk '{print NR\". \"$0}' '/tmp/file.txt'"
	wantCalls := []fakeExecCall{
		{name: "ssh", args: []string{"-F", "/tmp/ssh-config", "cs.demo", expectedCommand}},
		{name: "ssh", args: []string{"-F", "/tmp/ssh-config", "-o", "ConnectTimeout=5", "cs.demo", "echo ok"}},
		{name: "gh", args: []string{"codespace", "ssh", "-c", "demo", "--", expectedCommand}},
	}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("calls = %#v, want %#v", calls, wantCalls)
	}
}

func TestRunBashUsesExplicitCwd(t *testing.T) {
	client := NewClient("demo")

	var calls []fakeExecCall
	client.commandContext = fakeCommandContext(t, &calls, []fakeExecResponse{
		{stdout: "ok\n"},
	})

	stdout, stderr, exitCode, err := client.RunBash(context.Background(), "pwd", "/workspaces/repo/app")
	if err != nil {
		t.Fatalf("RunBash() error = %v", err)
	}
	if stdout != "ok\n" || stderr != "" || exitCode != 0 {
		t.Fatalf("RunBash() = stdout:%q stderr:%q exit:%d", stdout, stderr, exitCode)
	}

	wantCalls := []fakeExecCall{
		{name: "gh", args: []string{"codespace", "ssh", "-c", "demo", "--", envSecretsLoader + " && cd '/workspaces/repo/app' && pwd"}},
	}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("calls = %#v, want %#v", calls, wantCalls)
	}
}

func TestGrepUsesExplicitCwd(t *testing.T) {
	client := NewClient("demo")

	var calls []fakeExecCall
	client.commandContext = fakeCommandContext(t, &calls, []fakeExecResponse{
		{stdout: "cmd/main.go:3:match\n"},
	})

	got, err := client.Grep(context.Background(), "match", "cmd", "*.go", "/workspaces/repo")
	if err != nil {
		t.Fatalf("Grep() error = %v", err)
	}
	if got != "cmd/main.go:3:match\n" {
		t.Fatalf("Grep() = %q", got)
	}

	wantCalls := []fakeExecCall{
		{name: "gh", args: []string{"codespace", "ssh", "-c", "demo", "--", envSecretsLoader + " && cd '/workspaces/repo' && (rg --color=never -n --glob '*.go' 'match' 'cmd') 2>/dev/null || grep -rn 'match' 'cmd'"}},
	}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("calls = %#v, want %#v", calls, wantCalls)
	}
}

func TestGlobUsesExplicitCwd(t *testing.T) {
	client := NewClient("demo")

	var calls []fakeExecCall
	client.commandContext = fakeCommandContext(t, &calls, []fakeExecResponse{
		{stdout: "pkg/foo.go\n"},
	})

	got, err := client.Glob(context.Background(), "**/*.go", "pkg", "/workspaces/repo")
	if err != nil {
		t.Fatalf("Glob() error = %v", err)
	}
	if got != "pkg/foo.go\n" {
		t.Fatalf("Glob() = %q", got)
	}

	wantCalls := []fakeExecCall{
		{name: "gh", args: []string{"codespace", "ssh", "-c", "demo", "--", envSecretsLoader + " && cd '/workspaces/repo' && (fd --type f --glob '**/*.go' --exclude .git 'pkg' 2>/dev/null || find 'pkg' -name '*.go' -not -path '*/.git/*' 2>/dev/null) | head -200"}},
	}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("calls = %#v, want %#v", calls, wantCalls)
	}
}

func TestViewFileDoesNotRetryWhenTransportProbeSucceeds(t *testing.T) {
	client := NewClient("demo")
	client.sshConfigPath = "/tmp/ssh-config"
	client.sshHost = "cs.demo"

	var calls []fakeExecCall
	client.commandContext = fakeCommandContext(t, &calls, []fakeExecResponse{
		{exitCode: 255},
		{stdout: "ok\n"},
	})

	_, err := client.ViewFile(context.Background(), "/tmp/file.txt", nil)
	if err == nil {
		t.Fatal("ViewFile() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "view file failed (exit 255)") {
		t.Fatalf("ViewFile() error = %q", err.Error())
	}

	expectedCommand := envSecretsLoader + " && awk '{print NR\". \"$0}' '/tmp/file.txt'"
	wantCalls := []fakeExecCall{
		{name: "ssh", args: []string{"-F", "/tmp/ssh-config", "cs.demo", expectedCommand}},
		{name: "ssh", args: []string{"-F", "/tmp/ssh-config", "-o", "ConnectTimeout=5", "cs.demo", "echo ok"}},
	}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("calls = %#v, want %#v", calls, wantCalls)
	}
	if client.sshConfigPath == "" {
		t.Fatal("sshConfigPath unexpectedly cleared when the transport probe succeeded")
	}
}

func TestViewFileDoesNotRetryNonTransportExit255(t *testing.T) {
	client := NewClient("demo")
	client.sshConfigPath = "/tmp/ssh-config"
	client.sshHost = "cs.demo"

	var calls []fakeExecCall
	client.commandContext = fakeCommandContext(t, &calls, []fakeExecResponse{
		{stderr: "application failure\n", exitCode: 255},
	})

	_, err := client.ViewFile(context.Background(), "/tmp/file.txt", nil)
	if err == nil {
		t.Fatal("ViewFile() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "view file failed (exit 255): application failure") {
		t.Fatalf("ViewFile() error = %q", err.Error())
	}
	if len(calls) != 1 {
		t.Fatalf("len(calls) = %d, want 1", len(calls))
	}
	if client.sshConfigPath == "" {
		t.Fatal("sshConfigPath unexpectedly cleared for non-transport failure")
	}
}

func TestEnsureTmuxInstallFailureReturnsActionableMessage(t *testing.T) {
	client := NewClient("demo")

	var calls []fakeExecCall
	client.commandContext = fakeCommandContext(t, &calls, []fakeExecResponse{
		{exitCode: 1},
		{stderr: "curl: (6) Could not resolve host: mise.jdx.dev\n", exitCode: 2},
	})

	err := client.ensureTmux(context.Background())
	if err == nil {
		t.Fatal("ensureTmux() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "failed to install tmux via mise (exit 2); verify that the codespace can run `mise use -g tmux`") {
		t.Fatalf("ensureTmux() error = %q", err.Error())
	}
	if strings.Contains(err.Error(), "Could not resolve host") {
		t.Fatalf("ensureTmux() leaked raw installer stderr: %q", err.Error())
	}
	if len(calls) != 2 {
		t.Fatalf("len(calls) = %d, want 2", len(calls))
	}
}

func TestEnsureTmuxVerificationExplainsShimPathProblem(t *testing.T) {
	client := NewClient("demo")

	var calls []fakeExecCall
	client.commandContext = fakeCommandContext(t, &calls, []fakeExecResponse{
		{exitCode: 1},
		{exitCode: 0},
		{stderr: "tmux shim exists but is not on PATH\n", exitCode: 1},
	})

	err := client.ensureTmux(context.Background())
	if err == nil {
		t.Fatal("ensureTmux() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "tmux installation completed but tmux is still unavailable") {
		t.Fatalf("ensureTmux() error = %q", err.Error())
	}
	if !strings.Contains(err.Error(), "`$HOME/.local/share/mise/shims` is not on PATH") {
		t.Fatalf("ensureTmux() error = %q", err.Error())
	}
	if len(calls) != 3 {
		t.Fatalf("len(calls) = %d, want 3", len(calls))
	}
}
