package mcp

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ekroon/gh-copilot-codespace/internal/daemonclient"
	"github.com/ekroon/gh-copilot-codespace/internal/registry"
	"github.com/ekroon/gh-copilot-codespace/internal/ssh"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

func makeReq(args map[string]any) mcpsdk.CallToolRequest {
	return mcpsdk.CallToolRequest{
		Params: mcpsdk.CallToolParams{
			Arguments: args,
		},
	}
}

func TestRequiredString(t *testing.T) {
	tests := []struct {
		name    string
		args    map[string]any
		key     string
		want    string
		wantErr string
	}{
		{
			name: "key present with string value",
			args: map[string]any{"key": "hello"},
			key:  "key",
			want: "hello",
		},
		{
			name:    "key missing",
			args:    map[string]any{},
			key:     "key",
			wantErr: "missing required parameter",
		},
		{
			name:    "key present with non-string value",
			args:    map[string]any{"key": float64(42)},
			key:     "key",
			wantErr: "must be a string",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := requiredString(makeReq(tt.args), tt.key)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErr)
				}
				if !contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected error containing %q, got %q", tt.wantErr, err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestOptionalString(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
		key  string
		want string
	}{
		{
			name: "key present with string value",
			args: map[string]any{"key": "hello"},
			key:  "key",
			want: "hello",
		},
		{
			name: "key missing",
			args: map[string]any{},
			key:  "key",
			want: "",
		},
		{
			name: "key present with non-string value",
			args: map[string]any{"key": float64(42)},
			key:  "key",
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := optionalString(makeReq(tt.args), tt.key)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestOptionalFloat(t *testing.T) {
	tests := []struct {
		name       string
		args       map[string]any
		key        string
		defaultVal float64
		want       float64
	}{
		{
			name:       "key present with float64 value",
			args:       map[string]any{"key": float64(3.14)},
			key:        "key",
			defaultVal: 1.0,
			want:       3.14,
		},
		{
			name:       "key missing",
			args:       map[string]any{},
			key:        "key",
			defaultVal: 1.0,
			want:       1.0,
		},
		{
			name:       "key present with non-float64 value",
			args:       map[string]any{"key": "notfloat"},
			key:        "key",
			defaultVal: 1.0,
			want:       1.0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := optionalFloat(makeReq(tt.args), tt.key, tt.defaultVal)
			if got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestToInt(t *testing.T) {
	tests := []struct {
		name   string
		input  any
		want   int
		wantOK bool
	}{
		{name: "float64", input: float64(42), want: 42, wantOK: true},
		{name: "int", input: int(42), want: 42, wantOK: true},
		{name: "string", input: "42", want: 0, wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := toInt(tt.input)
			if ok != tt.wantOK {
				t.Errorf("ok = %v, want %v", ok, tt.wantOK)
			}
			if got != tt.want {
				t.Errorf("got %d, want %d", got, tt.want)
			}
		})
	}
}

func TestToolSuccess(t *testing.T) {
	result := toolSuccess("ok")
	if result.IsError {
		t.Error("expected IsError to be false")
	}
	if len(result.Content) != 1 {
		t.Fatalf("expected 1 content item, got %d", len(result.Content))
	}
	tc, ok := result.Content[0].(mcpsdk.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", result.Content[0])
	}
	if tc.Text != "ok" {
		t.Errorf("got text %q, want %q", tc.Text, "ok")
	}
}

func TestToolRuntimeDefinitionsAndCall(t *testing.T) {
	mock := &mockExecutor{}
	mock.workdir = "/workspaces/repo"
	reg := registry.New()
	if err := reg.Register(&registry.ManagedCodespace{
		Alias:    "github",
		Name:     "cs-abc",
		Workdir:  "/workspaces/repo",
		Executor: mock,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	runtime := NewToolRuntime(reg, LifecycleConfig{})
	defs := runtime.Definitions()
	if len(defs) == 0 {
		t.Fatal("expected tool definitions")
	}
	var found bool
	for _, def := range defs {
		if def.Name == "remote_cwd" {
			found = true
			if def.Description == "" {
				t.Fatal("remote_cwd description is empty")
			}
			if def.Parameters.Type != "object" {
				t.Fatalf("remote_cwd parameters type = %q, want object", def.Parameters.Type)
			}
		}
	}
	if !found {
		t.Fatal("remote_cwd definition not found")
	}

	result, err := runtime.Call(context.Background(), "remote_cwd", map[string]any{"codespace": "github"})
	if err != nil {
		t.Fatalf("runtime call: %v", err)
	}
	if result.ResultType != "success" {
		t.Fatalf("result type = %q, want success", result.ResultType)
	}
	if result.TextResultForLlm != "/workspaces/repo" {
		t.Fatalf("result text = %q, want /workspaces/repo", result.TextResultForLlm)
	}

	result, err = runtime.Call(context.Background(), "remote_view", map[string]any{})
	if err != nil {
		t.Fatalf("runtime call missing arg: %v", err)
	}
	if result.ResultType != "failure" || !strings.Contains(result.TextResultForLlm, "missing required parameter") {
		t.Fatalf("missing arg result = %+v, want failure with validation message", result)
	}
}

func TestParseCopyEndpoint(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		want      copyEndpoint
		wantError string
	}{
		{name: "local path", input: "src/app.go", want: copyEndpoint{path: "src/app.go"}},
		{name: "remote path with alias", input: "cs://github/src/app.go", want: copyEndpoint{remote: true, alias: "github", path: "src/app.go"}},
		{name: "remote path without alias", input: "cs:///src/app.go", want: copyEndpoint{remote: true, path: "src/app.go"}},
		{name: "remote missing path", input: "cs://github", wantError: "remote URI must be cs://<alias>/<path>"},
		{name: "blank", input: "", wantError: "path is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseCopyEndpoint(tt.input)
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestRemoteCopy_LocalToCodespace(t *testing.T) {
	localRoot := t.TempDir()
	content := []byte{'h', 'e', 0x00, 0xff, 'o'}
	if err := os.WriteFile(filepath.Join(localRoot, "src.txt"), content, 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	mock := &mockExecutor{runBashExit: 1}
	reg := registry.New()
	if err := reg.Register(&registry.ManagedCodespace{
		Alias:    "github",
		Name:     "cs-abc",
		Workdir:  "/workspaces/repo",
		Executor: mock,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	res, _ := remoteCopyHandler(reg, localRoot)(context.Background(), makeReq(map[string]any{
		"source":      "src.txt",
		"destination": "cs://github/copied.txt",
	}))
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultText(res))
	}
	if mock.lastRootedWrite.Path != "/workspaces/repo/copied.txt" {
		t.Fatalf("write path = %q, want /workspaces/repo/copied.txt", mock.lastRootedWrite.Path)
	}
	if mock.lastRootedWrite.Root != "/workspaces/repo" {
		t.Fatalf("write root = %q, want /workspaces/repo", mock.lastRootedWrite.Root)
	}
	if !bytes.Equal(mock.lastRootedWrite.Data, content) {
		t.Fatalf("write content bytes = %v, want %v", mock.lastRootedWrite.Data, content)
	}
	if mock.lastRootedWrite.Overwrite {
		t.Fatal("write overwrite = true, want false")
	}
	if !strings.Contains(mock.lastRunBashCommand, "test -e") {
		t.Fatalf("expected remote existence check, got %q", mock.lastRunBashCommand)
	}
}

func TestRemoteCopy_LocalToCodespaceOverwrite(t *testing.T) {
	localRoot := t.TempDir()
	content := []byte{0x00, 0xff, 'n', 'e', 'w'}
	if err := os.WriteFile(filepath.Join(localRoot, "src.bin"), content, 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	mock := &mockExecutor{}
	reg := registry.New()
	if err := reg.Register(&registry.ManagedCodespace{
		Alias:    "github",
		Name:     "cs-abc",
		Workdir:  "/workspaces/repo",
		Executor: mock,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	res, _ := remoteCopyHandler(reg, localRoot)(context.Background(), makeReq(map[string]any{
		"source":      "src.bin",
		"destination": "cs://github/existing.bin",
		"overwrite":   true,
	}))
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultText(res))
	}
	if !bytes.Equal(mock.lastRootedWrite.Data, content) || !mock.lastRootedWrite.Overwrite {
		t.Fatalf("write = (%v, overwrite=%v), want (%v, true)", mock.lastRootedWrite.Data, mock.lastRootedWrite.Overwrite, content)
	}
	if mock.runBashCalls != 0 {
		t.Fatalf("RunBash calls = %d, want no racy preflight for overwrite", mock.runBashCalls)
	}
}

func TestRemoteCopy_LocalToCodespaceRequiresRegularSource(t *testing.T) {
	localRoot := t.TempDir()
	target := filepath.Join(localRoot, "target.txt")
	if err := os.WriteFile(target, []byte("target"), 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(localRoot, "link.txt")); err != nil {
		t.Fatalf("create symlink: %v", err)
	}
	fifoAvailable := createLocalCopyFIFO(filepath.Join(localRoot, "pipe")) == nil

	tests := []struct {
		name       string
		source     string
		wantError  string
		maxElapsed time.Duration
	}{
		{name: "directory", source: ".", wantError: "regular file"},
		{name: "symlink", source: "link.txt", wantError: "symbolic link"},
		{name: "fifo", source: "pipe", wantError: "regular file", maxElapsed: 500 * time.Millisecond},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.name == "fifo" && !fifoAvailable {
				t.Skip("FIFO creation is unavailable on this platform")
			}
			mock := &mockExecutor{runBashExit: 1}
			reg := testRegWithExecutor(mock)
			started := time.Now()
			res, _ := remoteCopyHandler(reg, localRoot)(context.Background(), makeReq(map[string]any{
				"source":      tt.source,
				"destination": "cs://test/copied.bin",
			}))
			if !res.IsError || !strings.Contains(resultText(res), tt.wantError) {
				t.Fatalf("result = %q, want error containing %q", resultText(res), tt.wantError)
			}
			if tt.maxElapsed > 0 && time.Since(started) >= tt.maxElapsed {
				t.Fatalf("remote_copy took %v, want non-blocking source rejection", time.Since(started))
			}
			if mock.runBashCalls != 0 || mock.rootedWriteCalls != 0 {
				t.Fatalf("remote calls = existence:%d write:%d, want none", mock.runBashCalls, mock.rootedWriteCalls)
			}
		})
	}
}

func TestRemoteCopy_LocalToCodespaceRejectsOversizedSourceBeforeReading(t *testing.T) {
	localRoot := t.TempDir()
	source := filepath.Join(localRoot, "oversized.bin")
	file, err := os.Create(source)
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	if err := file.Truncate(int64(ssh.MaxFileTransferBytes + 1)); err != nil {
		file.Close()
		t.Fatalf("truncate source: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close source: %v", err)
	}

	mock := &mockExecutor{runBashExit: 1}
	reg := testRegWithExecutor(mock)
	res, _ := remoteCopyHandler(reg, localRoot)(context.Background(), makeReq(map[string]any{
		"source":      "oversized.bin",
		"destination": "cs://test/copied.bin",
	}))
	if !res.IsError || !strings.Contains(resultText(res), "exceeds") {
		t.Fatalf("result = %q, want size-limit error", resultText(res))
	}
	if mock.runBashCalls != 0 || mock.rootedWriteCalls != 0 {
		t.Fatalf("remote calls = existence:%d write:%d, want none", mock.runBashCalls, mock.rootedWriteCalls)
	}
}

func TestReadLocalCopySourceRejectsGrowthBeyondLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "growing.bin")
	if err := os.WriteFile(path, bytes.Repeat([]byte("a"), ssh.MaxFileTransferBytes), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	_, err := readLocalCopySourceWithHooks(context.Background(), path, localCopyReadHooks{
		afterStat: func() error {
			file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				return err
			}
			defer file.Close()
			_, err = file.Write([]byte("b"))
			return err
		},
	})
	if err == nil || !strings.Contains(err.Error(), "grew beyond") {
		t.Fatalf("readLocalCopySourceWithHooks() error = %v, want growth rejection", err)
	}
}

func TestReadLocalCopySourceHonorsCancellationWhileReading(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.bin")
	if err := os.WriteFile(path, bytes.Repeat([]byte("a"), 256*1024), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	_, err := readLocalCopySourceWithHooks(ctx, path, localCopyReadHooks{
		reader: func(source io.Reader) io.Reader {
			return readerFunc(func(p []byte) (int, error) {
				n, err := source.Read(p)
				if n > 0 {
					cancel()
				}
				return n, err
			})
		},
	})
	if err == nil || !strings.Contains(err.Error(), context.Canceled.Error()) {
		t.Fatalf("readLocalCopySourceWithHooks() error = %v, want cancellation", err)
	}
}

func TestRemoteCopy_UsesImmutableCodespaceWorkdirAfterRemoteCD(t *testing.T) {
	localRoot := t.TempDir()
	upload := []byte{0x00, 0xff, 'u', 'p'}
	overwrite := []byte{'n', 0x00, 0xfe, 'w'}
	download := []byte{0xff, 0x00, 'd', 'o', 'w', 'n'}
	if err := os.WriteFile(filepath.Join(localRoot, "upload.bin"), upload, 0o644); err != nil {
		t.Fatalf("write upload source: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localRoot, "overwrite.bin"), overwrite, 0o644); err != nil {
		t.Fatalf("write overwrite source: %v", err)
	}

	const root = "/workspaces/repo"
	const nested = "/workspaces/repo/internal/mcp"
	mock := &mockExecutor{
		workdir:          root,
		runBashStdouts:   []string{nested + "\n", ""},
		runBashExitCodes: []int{0, 1},
		rootedReadData:   download,
	}
	reg := registry.New()
	if err := reg.Register(&registry.ManagedCodespace{
		Alias:    "github",
		Name:     "cs-abc",
		Workdir:  root,
		Executor: mock,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	cdResult, _ := cdHandler(reg)(context.Background(), makeReq(map[string]any{
		"codespace": "github",
		"path":      "internal/mcp",
	}))
	if cdResult.IsError {
		t.Fatalf("remote_cd error: %s", resultText(cdResult))
	}

	for _, call := range []map[string]any{
		{"source": "upload.bin", "destination": "cs://github/root.bin"},
		{"source": "overwrite.bin", "destination": "cs://github/root.bin", "overwrite": true},
	} {
		result, _ := remoteCopyHandler(reg, localRoot)(context.Background(), makeReq(call))
		if result.IsError {
			t.Fatalf("remote_copy upload error: %s", resultText(result))
		}
	}
	result, _ := remoteCopyHandler(reg, localRoot)(context.Background(), makeReq(map[string]any{
		"source":      "cs://github/root.bin",
		"destination": "download.bin",
	}))
	if result.IsError {
		t.Fatalf("remote_copy download error: %s", resultText(result))
	}

	if mock.workdir != nested {
		t.Fatalf("current workdir = %q, want %q", mock.workdir, nested)
	}
	if mock.rootedWriteCalls != 2 {
		t.Fatalf("rooted write calls = %d, want 2", mock.rootedWriteCalls)
	}
	if mock.lastRootedWrite.Path != root+"/root.bin" || mock.lastRootedWrite.Root != root {
		t.Fatalf("rooted write request = %+v, want immutable root %q", mock.lastRootedWrite, root)
	}
	if !mock.lastRootedWrite.Overwrite || !bytes.Equal(mock.lastRootedWrite.Data, overwrite) {
		t.Fatalf("overwrite request = %+v, want binary overwrite %v", mock.lastRootedWrite, overwrite)
	}
	if mock.lastRootedRead.Path != root+"/root.bin" || mock.lastRootedRead.Root != root {
		t.Fatalf("rooted read request = %+v, want immutable root %q", mock.lastRootedRead, root)
	}
	got, err := os.ReadFile(filepath.Join(localRoot, "download.bin"))
	if err != nil {
		t.Fatalf("read download: %v", err)
	}
	if !bytes.Equal(got, download) {
		t.Fatalf("download bytes = %v, want %v", got, download)
	}
}

func TestRemoteCopy_LocalToCodespaceRefusesExistingDestination(t *testing.T) {
	localRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(localRoot, "src.bin"), []byte("new"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	mock := &mockExecutor{runBashExit: 0}
	reg := registry.New()
	if err := reg.Register(&registry.ManagedCodespace{
		Alias:    "github",
		Name:     "cs-abc",
		Workdir:  "/workspaces/repo",
		Executor: mock,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	res, _ := remoteCopyHandler(reg, localRoot)(context.Background(), makeReq(map[string]any{
		"source":      "src.bin",
		"destination": "cs://github/existing.bin",
	}))
	if !res.IsError || !strings.Contains(resultText(res), "already exists") {
		t.Fatalf("result = %+v, want existing destination refusal", res)
	}
	if mock.writeFileCalls != 0 {
		t.Fatalf("WriteFile calls = %d, want 0", mock.writeFileCalls)
	}
}

func TestRemoteCopy_CodespaceToLocal(t *testing.T) {
	localRoot := t.TempDir()
	content := []byte{0x00, 0xff, 'r', 'e', 'm', 'o', 't', 'e'}
	mock := &mockExecutor{
		rootedReadData: content,
	}
	reg := registry.New()
	if err := reg.Register(&registry.ManagedCodespace{
		Alias:    "github",
		Name:     "cs-abc",
		Workdir:  "/workspaces/repo",
		Executor: mock,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	res, _ := remoteCopyHandler(reg, localRoot)(context.Background(), makeReq(map[string]any{
		"source":      "cs://github/remote.txt",
		"destination": "local/remote.txt",
	}))
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultText(res))
	}
	got, err := os.ReadFile(filepath.Join(localRoot, "local", "remote.txt"))
	if err != nil {
		t.Fatalf("read copied file: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("copied content = %v, want %v", got, content)
	}
	if mock.lastRootedRead.Path != "/workspaces/repo/remote.txt" || mock.lastRootedRead.Root != "/workspaces/repo" {
		t.Fatalf("rooted read request = %+v", mock.lastRootedRead)
	}
}

func TestRemoteCopy_CodespaceToLocalOverwriteIsAtomicAndPreservesMode(t *testing.T) {
	localRoot := t.TempDir()
	localPath := filepath.Join(localRoot, "existing.bin")
	if err := os.WriteFile(localPath, []byte("old"), 0o600); err != nil {
		t.Fatalf("write existing: %v", err)
	}
	content := []byte{0x00, 0xff, 'n', 'e', 'w'}
	mock := &mockExecutor{rootedReadData: content}
	reg := registry.New()
	if err := reg.Register(&registry.ManagedCodespace{
		Alias:    "github",
		Name:     "cs-abc",
		Workdir:  "/workspaces/repo",
		Executor: mock,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	res, _ := remoteCopyHandler(reg, localRoot)(context.Background(), makeReq(map[string]any{
		"source":      "cs://github/remote.bin",
		"destination": "existing.bin",
		"overwrite":   true,
	}))
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultText(res))
	}
	got, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatalf("read copied file: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("copied content = %v, want %v", got, content)
	}
	info, err := os.Stat(localPath)
	if err != nil {
		t.Fatalf("stat copied file: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %v, want 0600", got)
	}
}

func TestRemoteCopy_CodespaceToLocalFailureLeavesDestinationUnchanged(t *testing.T) {
	localRoot := t.TempDir()
	localPath := filepath.Join(localRoot, "existing.bin")
	if err := os.WriteFile(localPath, []byte("keep"), 0o600); err != nil {
		t.Fatalf("write existing: %v", err)
	}
	mock := &mockExecutor{rootedReadErr: fmt.Errorf("read failed")}
	reg := registry.New()
	if err := reg.Register(&registry.ManagedCodespace{
		Alias:    "github",
		Name:     "cs-abc",
		Workdir:  "/workspaces/repo",
		Executor: mock,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	res, _ := remoteCopyHandler(reg, localRoot)(context.Background(), makeReq(map[string]any{
		"source":      "cs://github/remote.bin",
		"destination": "existing.bin",
		"overwrite":   true,
	}))
	if !res.IsError {
		t.Fatal("expected remote read failure")
	}
	got, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatalf("read existing file: %v", err)
	}
	if string(got) != "keep" {
		t.Fatalf("destination = %q, want unchanged", got)
	}
}

func TestRemoteCopy_CodespaceToLocalRejectsSymlinkOverwrite(t *testing.T) {
	localRoot := t.TempDir()
	target := filepath.Join(localRoot, "target.bin")
	link := filepath.Join(localRoot, "link.bin")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	mock := &mockExecutor{rootedReadData: []byte("replace")}
	reg := registry.New()
	if err := reg.Register(&registry.ManagedCodespace{
		Alias:    "github",
		Name:     "cs-abc",
		Workdir:  "/workspaces/repo",
		Executor: mock,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	res, _ := remoteCopyHandler(reg, localRoot)(context.Background(), makeReq(map[string]any{
		"source":      "cs://github/remote.bin",
		"destination": "link.bin",
		"overwrite":   true,
	}))
	if !res.IsError || !strings.Contains(resultText(res), "symbolic link") {
		t.Fatalf("result = %+v, want symbolic link rejection", res)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(got) != "keep" {
		t.Fatalf("target = %q, want unchanged", got)
	}
}

func TestRemoteCopy_RefusesOverwrite(t *testing.T) {
	localRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(localRoot, "existing.txt"), []byte("local"), 0o644); err != nil {
		t.Fatalf("write existing: %v", err)
	}
	mock := &mockExecutor{rootedReadData: []byte("remote")}
	reg := registry.New()
	if err := reg.Register(&registry.ManagedCodespace{
		Alias:    "github",
		Name:     "cs-abc",
		Workdir:  "/workspaces/repo",
		Executor: mock,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	res, _ := remoteCopyHandler(reg, localRoot)(context.Background(), makeReq(map[string]any{
		"source":      "cs://github/remote.txt",
		"destination": "existing.txt",
	}))
	if !res.IsError {
		t.Fatal("expected overwrite refusal")
	}
	if !strings.Contains(resultText(res), "already exists locally") {
		t.Fatalf("unexpected error: %s", resultText(res))
	}
}

func TestRemoteCopy_RejectsTraversal(t *testing.T) {
	localRoot := t.TempDir()
	reg := registry.New()
	res, _ := remoteCopyHandler(reg, localRoot)(context.Background(), makeReq(map[string]any{
		"source":      "../secret.txt",
		"destination": "cs://github/secret.txt",
	}))
	if !res.IsError {
		t.Fatal("expected traversal error")
	}
	if !strings.Contains(resultText(res), "escapes local workdir") {
		t.Fatalf("unexpected error: %s", resultText(res))
	}
}

func TestRemoteCopy_RejectsSourceSymlinkEscape(t *testing.T) {
	localRoot := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o600); err != nil {
		t.Fatalf("write outside source: %v", err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(localRoot, "secret-link")); err != nil {
		t.Fatalf("create source symlink: %v", err)
	}

	reg := registry.New()
	if err := reg.Register(&registry.ManagedCodespace{
		Alias:    "github",
		Name:     "cs-abc",
		Workdir:  "/workspaces/repo",
		Executor: &mockExecutor{runBashExit: 1},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	res, _ := remoteCopyHandler(reg, localRoot)(context.Background(), makeReq(map[string]any{
		"source":      "secret-link",
		"destination": "cs://github/secret.txt",
	}))
	if !res.IsError {
		t.Fatal("expected source symlink escape error")
	}
	if !strings.Contains(resultText(res), "symbolic link") {
		t.Fatalf("unexpected error: %s", resultText(res))
	}
}

func TestRemoteCopy_RejectsDestinationParentSymlinkEscape(t *testing.T) {
	localRoot := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(localRoot, "outside-link")); err != nil {
		t.Fatalf("create destination symlink: %v", err)
	}

	mock := &mockExecutor{rootedReadData: []byte("remote")}
	reg := registry.New()
	if err := reg.Register(&registry.ManagedCodespace{
		Alias:    "github",
		Name:     "cs-abc",
		Workdir:  "/workspaces/repo",
		Executor: mock,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	res, _ := remoteCopyHandler(reg, localRoot)(context.Background(), makeReq(map[string]any{
		"source":      "cs://github/remote.txt",
		"destination": "outside-link/copied.txt",
	}))
	if !res.IsError {
		t.Fatal("expected destination symlink escape error")
	}
	if !strings.Contains(resultText(res), "escapes local workdir") {
		t.Fatalf("unexpected error: %s", resultText(res))
	}
	if _, err := os.Stat(filepath.Join(outside, "copied.txt")); !os.IsNotExist(err) {
		t.Fatalf("outside destination was created or stat failed unexpectedly: %v", err)
	}
}

func TestRemoteCopy_RejectsRemoteTraversal(t *testing.T) {
	localRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(localRoot, "src.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	reg := registry.New()
	if err := reg.Register(&registry.ManagedCodespace{
		Alias:    "github",
		Name:     "cs-abc",
		Workdir:  "/workspaces/repo",
		Executor: &mockExecutor{runBashExit: 1},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	res, _ := remoteCopyHandler(reg, localRoot)(context.Background(), makeReq(map[string]any{
		"source":      "src.txt",
		"destination": "cs://github/../secret.txt",
	}))
	if !res.IsError {
		t.Fatal("expected remote traversal error")
	}
	if !strings.Contains(resultText(res), "escapes codespace workdir") {
		t.Fatalf("unexpected error: %s", resultText(res))
	}
}

func TestToolError(t *testing.T) {
	result := toolError("fail")
	if !result.IsError {
		t.Error("expected IsError to be true")
	}
	if len(result.Content) != 1 {
		t.Fatalf("expected 1 content item, got %d", len(result.Content))
	}
	tc, ok := result.Content[0].(mcpsdk.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", result.Content[0])
	}
	if tc.Text != "fail" {
		t.Errorf("got text %q, want %q", tc.Text, "fail")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchSubstring(s, substr)
}

func searchSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// --- Mock Executor ---

type mockExecutor struct {
	viewFileResult      string
	viewFileErr         error
	editFileErr         error
	createFileErr       error
	lastCreatePath      string
	lastCreateContent   string
	writeFileErr        error
	writeFileCalls      int
	lastWritePath       string
	lastWriteContent    []byte
	lastWriteOverwrite  bool
	runBashCalls        int
	lastRunBashCommand  string
	lastRunBashCwd      string
	runBashStdout       string
	runBashStderr       string
	runBashExit         int
	runBashErr          error
	runBashStdouts      []string
	runBashExitCodes    []int
	rootedReadData      []byte
	rootedReadErr       error
	lastRootedRead      ssh.RootedReadRequest
	rootedWriteCalls    int
	rootedWriteErr      error
	lastRootedWrite     ssh.RootedWriteRequest
	lastGrepPattern     string
	lastGrepPath        string
	lastGrepGlob        string
	lastGrepCwd         string
	grepResult          string
	grepErr             error
	lastGlobPattern     string
	lastGlobPath        string
	lastGlobCwd         string
	globResult          string
	globErr             error
	startSessionCalls   int
	lastSessionID       string
	lastCommand         string
	lastStartSessionCwd string
	startSessionErr     error
	writeSessionErr     error
	readSessionCalls    int
	readSessionResults  []string
	readSessionResult   string
	readSessionErr      error
	readSessionFunc     func(context.Context) (string, error)
	stopSessionCalls    int
	stopSessionErr      error
	stopSessionCtxErr   error
	listSessionsResult  string
	listSessionsErr     error
	workdir             string
}

type waitSessionMockExecutor struct {
	*mockExecutor
	output       string
	completed    bool
	err          error
	waitCalls    int
	lastWaitTime time.Duration
}

type processSessionMockExecutor struct {
	*waitSessionMockExecutor
	startProcessCalls int
	startProcessErr   error
}

func (m *processSessionMockExecutor) SupportsProcessSessions() bool {
	return true
}

func (m *processSessionMockExecutor) StartProcessSession(context.Context, string, string, string) error {
	m.startProcessCalls++
	return m.startProcessErr
}

func (m *waitSessionMockExecutor) SupportsWaitSession() bool {
	return true
}

func (m *waitSessionMockExecutor) WaitSession(_ context.Context, _ string, timeout time.Duration) (string, bool, error) {
	m.waitCalls++
	m.lastWaitTime = timeout
	return m.output, m.completed, m.err
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) {
	return f(p)
}

func (m *mockExecutor) ViewFile(_ context.Context, _ string, _ []int) (string, error) {
	return m.viewFileResult, m.viewFileErr
}

func (m *mockExecutor) EditFile(_ context.Context, _, _, _ string) error {
	return m.editFileErr
}

func (m *mockExecutor) CreateFile(_ context.Context, path, content string) error {
	m.lastCreatePath = path
	m.lastCreateContent = content
	return m.createFileErr
}

func (m *mockExecutor) WriteFile(_ context.Context, path string, content []byte, overwrite bool) error {
	m.writeFileCalls++
	m.lastWritePath = path
	m.lastWriteContent = append([]byte(nil), content...)
	m.lastWriteOverwrite = overwrite
	return m.writeFileErr
}

func (m *mockExecutor) ReadFileRooted(_ context.Context, req ssh.RootedReadRequest) ([]byte, error) {
	m.lastRootedRead = req
	return append([]byte(nil), m.rootedReadData...), m.rootedReadErr
}

func (m *mockExecutor) WriteFileRooted(_ context.Context, req ssh.RootedWriteRequest) error {
	m.rootedWriteCalls++
	m.lastRootedWrite = req
	m.lastRootedWrite.Data = append([]byte(nil), req.Data...)
	return m.rootedWriteErr
}

func (m *mockExecutor) RunBash(_ context.Context, command, cwd string) (string, string, int, error) {
	m.runBashCalls++
	m.lastRunBashCommand = command
	m.lastRunBashCwd = cwd
	if len(m.runBashStdouts) > 0 {
		stdout := m.runBashStdouts[0]
		m.runBashStdouts = m.runBashStdouts[1:]
		exitCode := m.runBashExit
		if len(m.runBashExitCodes) > 0 {
			exitCode = m.runBashExitCodes[0]
			m.runBashExitCodes = m.runBashExitCodes[1:]
		}
		return stdout, m.runBashStderr, exitCode, m.runBashErr
	}
	return m.runBashStdout, m.runBashStderr, m.runBashExit, m.runBashErr
}

func (m *mockExecutor) Grep(_ context.Context, pattern, path, glob, cwd string) (string, error) {
	m.lastGrepPattern = pattern
	m.lastGrepPath = path
	m.lastGrepGlob = glob
	m.lastGrepCwd = cwd
	return m.grepResult, m.grepErr
}

func (m *mockExecutor) Glob(_ context.Context, pattern, path, cwd string) (string, error) {
	m.lastGlobPattern = pattern
	m.lastGlobPath = path
	m.lastGlobCwd = cwd
	return m.globResult, m.globErr
}

func (m *mockExecutor) StartSession(_ context.Context, sessionID, command, cwd string) error {
	m.startSessionCalls++
	m.lastSessionID = sessionID
	m.lastCommand = command
	m.lastStartSessionCwd = cwd
	return m.startSessionErr
}

func (m *mockExecutor) WriteSession(_ context.Context, _, _ string) error {
	return m.writeSessionErr
}

func (m *mockExecutor) ReadSession(ctx context.Context, _ string) (string, error) {
	m.readSessionCalls++
	if m.readSessionFunc != nil {
		return m.readSessionFunc(ctx)
	}
	if len(m.readSessionResults) > 0 {
		result := m.readSessionResults[0]
		m.readSessionResults = m.readSessionResults[1:]
		return result, m.readSessionErr
	}
	return m.readSessionResult, m.readSessionErr
}

func (m *mockExecutor) StopSession(ctx context.Context, _ string) error {
	m.stopSessionCalls++
	m.stopSessionCtxErr = ctx.Err()
	return m.stopSessionErr
}

func (m *mockExecutor) ListSessions(_ context.Context) (string, error) {
	return m.listSessionsResult, m.listSessionsErr
}

func (m *mockExecutor) SetWorkdir(dir string) {
	m.workdir = dir
}

func (m *mockExecutor) GetWorkdir() string {
	if m.workdir == "" {
		return "/workspaces"
	}
	return m.workdir
}

// helper to extract text from a CallToolResult
func resultText(r *mcpsdk.CallToolResult) string {
	if len(r.Content) == 0 {
		return ""
	}
	tc, ok := r.Content[0].(mcpsdk.TextContent)
	if !ok {
		return ""
	}
	return tc.Text
}

// testReg wraps a mockExecutor in a single-codespace registry for handler testing.
func testReg(mock *mockExecutor) *registry.Registry {
	reg := registry.New()
	reg.Register(&registry.ManagedCodespace{
		Alias:    "test",
		Name:     "test-cs",
		Executor: mock,
	})
	return reg
}

// --- Handler Tests ---

func TestViewHandler(t *testing.T) {
	tests := []struct {
		name     string
		mock     *mockExecutor
		args     map[string]any
		wantErr  bool
		wantText string
	}{
		{
			name:     "success",
			mock:     &mockExecutor{viewFileResult: "1. hello\n2. world\n"},
			args:     map[string]any{"path": "/tmp/test.txt"},
			wantText: "1. hello\n2. world\n",
		},
		{
			name:     "error from executor",
			mock:     &mockExecutor{viewFileErr: fmt.Errorf("no such file")},
			args:     map[string]any{"path": "/tmp/missing.txt"},
			wantErr:  true,
			wantText: "no such file",
		},
		{
			name:     "missing path arg",
			mock:     &mockExecutor{},
			args:     map[string]any{},
			wantErr:  true,
			wantText: "missing required parameter",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := viewHandler(testReg(tt.mock))
			res, err := handler(context.Background(), makeReq(tt.args))
			if err != nil {
				t.Fatalf("unexpected Go error: %v", err)
			}
			if tt.wantErr && !res.IsError {
				t.Fatal("expected tool error, got success")
			}
			if !tt.wantErr && res.IsError {
				t.Fatalf("expected success, got tool error: %s", resultText(res))
			}
			if !strings.Contains(resultText(res), tt.wantText) {
				t.Errorf("result text %q does not contain %q", resultText(res), tt.wantText)
			}
		})
	}
}

func TestEditHandler(t *testing.T) {
	tests := []struct {
		name     string
		mock     *mockExecutor
		args     map[string]any
		wantErr  bool
		wantText string
	}{
		{
			name:     "success",
			mock:     &mockExecutor{},
			args:     map[string]any{"path": "/tmp/f.txt", "old_str": "a", "new_str": "b"},
			wantText: "Successfully edited",
		},
		{
			name:     "executor error",
			mock:     &mockExecutor{editFileErr: fmt.Errorf("old_str not found")},
			args:     map[string]any{"path": "/tmp/f.txt", "old_str": "x", "new_str": "y"},
			wantErr:  true,
			wantText: "old_str not found",
		},
		{
			name:     "missing old_str arg",
			mock:     &mockExecutor{},
			args:     map[string]any{"path": "/tmp/f.txt", "new_str": "b"},
			wantErr:  true,
			wantText: "missing required parameter",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := editHandler(testReg(tt.mock))
			res, err := handler(context.Background(), makeReq(tt.args))
			if err != nil {
				t.Fatalf("unexpected Go error: %v", err)
			}
			if tt.wantErr && !res.IsError {
				t.Fatal("expected tool error, got success")
			}
			if !tt.wantErr && res.IsError {
				t.Fatalf("expected success, got tool error: %s", resultText(res))
			}
			if !strings.Contains(resultText(res), tt.wantText) {
				t.Errorf("result text %q does not contain %q", resultText(res), tt.wantText)
			}
		})
	}
}

func TestCreateHandler(t *testing.T) {
	tests := []struct {
		name     string
		mock     *mockExecutor
		args     map[string]any
		wantErr  bool
		wantText string
	}{
		{
			name:     "success",
			mock:     &mockExecutor{},
			args:     map[string]any{"path": "/tmp/new.txt", "file_text": "content"},
			wantText: "Created /tmp/new.txt",
		},
		{
			name:     "executor error",
			mock:     &mockExecutor{createFileErr: fmt.Errorf("permission denied")},
			args:     map[string]any{"path": "/root/f.txt", "file_text": "x"},
			wantErr:  true,
			wantText: "permission denied",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := createHandler(testReg(tt.mock))
			res, err := handler(context.Background(), makeReq(tt.args))
			if err != nil {
				t.Fatalf("unexpected Go error: %v", err)
			}
			if tt.wantErr && !res.IsError {
				t.Fatal("expected tool error, got success")
			}
			if !tt.wantErr && res.IsError {
				t.Fatalf("expected success, got tool error: %s", resultText(res))
			}
			if !strings.Contains(resultText(res), tt.wantText) {
				t.Errorf("result text %q does not contain %q", resultText(res), tt.wantText)
			}
		})
	}
}

func TestBashHandler_DefaultReturnsCompletedSessionOutput(t *testing.T) {
	mock := &mockExecutor{
		readSessionResult: "hello world\n[session exited]",
	}

	handler := bashHandler(testReg(mock))
	res, err := handler(context.Background(), makeReq(map[string]any{
		"command":      "echo hello world",
		"shellId":      "s1",
		"initial_wait": 0.001,
	}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got tool error: %s", resultText(res))
	}
	if got := resultText(res); got != "hello world" {
		t.Fatalf("result text = %q, want %q", got, "hello world")
	}
	if mock.startSessionCalls != 1 {
		t.Fatalf("startSessionCalls = %d, want 1", mock.startSessionCalls)
	}
	if mock.stopSessionCalls != 1 {
		t.Fatalf("stopSessionCalls = %d, want 1", mock.stopSessionCalls)
	}
	if mock.lastStartSessionCwd != "" {
		t.Fatalf("lastStartSessionCwd = %q, want empty fallback cwd", mock.lastStartSessionCwd)
	}
	if mock.runBashCalls != 0 {
		t.Fatalf("runBashCalls = %d, want 0", mock.runBashCalls)
	}
}

func TestBashHandler_DefaultReturnsAsSoonAsSessionCompletes(t *testing.T) {
	mock := &mockExecutor{
		readSessionResults: []string{
			"starting",
			"done\n[session exited]",
		},
	}

	handler := bashHandler(testReg(mock))
	start := time.Now()
	res, err := handler(context.Background(), makeReq(map[string]any{
		"command":      "echo done",
		"shellId":      "s-fast",
		"initial_wait": 1.0,
	}))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got tool error: %s", resultText(res))
	}
	if got := resultText(res); got != "done" {
		t.Fatalf("result text = %q, want %q", got, "done")
	}
	if elapsed >= 500*time.Millisecond {
		t.Fatalf("completed session returned after %v, want less than 500ms", elapsed)
	}
	if mock.readSessionCalls < 2 {
		t.Fatalf("readSessionCalls = %d, want at least 2", mock.readSessionCalls)
	}
}

func TestBashHandler_UsesDaemonSessionWaiter(t *testing.T) {
	base := &mockExecutor{}
	mock := &waitSessionMockExecutor{
		mockExecutor: base,
		output:       "done\n[session exited]",
		completed:    true,
	}

	handler := bashHandler(testRegWithExecutor(mock))
	start := time.Now()
	res, err := handler(context.Background(), makeReq(map[string]any{
		"command":      "echo done",
		"shellId":      "s-waiter",
		"initial_wait": 30.0,
	}))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got tool error: %s", resultText(res))
	}
	if got := resultText(res); got != "done" {
		t.Fatalf("result text = %q, want %q", got, "done")
	}
	if elapsed >= 100*time.Millisecond {
		t.Fatalf("daemon waiter returned after %v, want less than 100ms", elapsed)
	}
	if mock.waitCalls != 1 {
		t.Fatalf("waitCalls = %d, want 1", mock.waitCalls)
	}
	if base.readSessionCalls != 0 {
		t.Fatalf("readSessionCalls = %d, want 0", base.readSessionCalls)
	}
}

func TestBashHandler_UsesDirectProcessSessionForSyncCommands(t *testing.T) {
	base := &mockExecutor{}
	mock := &processSessionMockExecutor{
		waitSessionMockExecutor: &waitSessionMockExecutor{
			mockExecutor: base,
			output:       "done\n[session exited]",
			completed:    true,
		},
	}

	handler := bashHandler(testRegWithExecutor(mock))
	res, err := handler(context.Background(), makeReq(map[string]any{
		"command":      "echo done",
		"shellId":      "process-1",
		"initial_wait": 1,
	}))
	if err != nil {
		t.Fatalf("handler error = %v", err)
	}
	if res.IsError {
		t.Fatalf("result error = %s", resultText(res))
	}
	if mock.startProcessCalls != 1 {
		t.Fatalf("startProcessCalls = %d, want 1", mock.startProcessCalls)
	}
	if base.startSessionCalls != 0 {
		t.Fatalf("startSessionCalls = %d, want 0", base.startSessionCalls)
	}
	if base.runBashCalls != 0 {
		t.Fatalf("runBashCalls = %d, want 0", base.runBashCalls)
	}
}

func TestBashHandler_DoesNotFallbackAfterProcessSessionStartError(t *testing.T) {
	base := &mockExecutor{runBashStdout: "must not run"}
	mock := &processSessionMockExecutor{
		waitSessionMockExecutor: &waitSessionMockExecutor{mockExecutor: base},
		startProcessErr:         fmt.Errorf("duplicate session"),
	}

	handler := bashHandler(testRegWithExecutor(mock))
	res, err := handler(context.Background(), makeReq(map[string]any{
		"command": "echo duplicate",
		"shellId": "process-duplicate",
	}))
	if err != nil {
		t.Fatalf("handler error = %v", err)
	}
	if !res.IsError {
		t.Fatalf("result error = false, want true; result = %s", resultText(res))
	}
	if base.startSessionCalls != 0 {
		t.Fatalf("startSessionCalls = %d, want 0", base.startSessionCalls)
	}
	if base.runBashCalls != 0 {
		t.Fatalf("runBashCalls = %d, want 0", base.runBashCalls)
	}
}

func TestBashHandler_DaemonWaiterDoesNotTrustOutputMarker(t *testing.T) {
	base := &mockExecutor{}
	mock := &waitSessionMockExecutor{
		mockExecutor: base,
		output:       "[session exited]\nstill running",
		completed:    false,
	}

	handler := bashHandler(testRegWithExecutor(mock))
	res, err := handler(context.Background(), makeReq(map[string]any{
		"command":      "printf '[session exited]\\n'; sleep 30",
		"shellId":      "s-marker",
		"initial_wait": 0.01,
	}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected running session result, got tool error: %s", resultText(res))
	}
	if !strings.Contains(resultText(res), "[shellId: s-marker") {
		t.Fatalf("unexpected result text: %q", resultText(res))
	}
	if base.stopSessionCalls != 0 {
		t.Fatalf("stopSessionCalls = %d, want 0", base.stopSessionCalls)
	}
}

func TestTrimSessionExitMarkerPreservesUserMarker(t *testing.T) {
	output := "[session exited]\nactual output\n[session exited]\n[exit code: 1]"
	got := trimSessionExitMarker(output)
	want := "[session exited]\nactual output\n[exit code: 1]"
	if got != want {
		t.Fatalf("trimSessionExitMarker() = %q, want %q", got, want)
	}
}

func TestBashHandler_DefaultBoundsSlowSessionReads(t *testing.T) {
	mock := &mockExecutor{
		readSessionFunc: func(ctx context.Context) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
		},
	}

	handler := bashHandler(testReg(mock))
	start := time.Now()
	res, err := handler(context.Background(), makeReq(map[string]any{
		"command":      "sleep 10",
		"shellId":      "s-slow-read",
		"initial_wait": 0.05,
	}))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected running session result, got tool error: %s", resultText(res))
	}
	if elapsed >= 500*time.Millisecond {
		t.Fatalf("slow session read returned after %v, want less than 500ms", elapsed)
	}
	if !strings.Contains(resultText(res), "[shellId: s-slow-read") {
		t.Fatalf("unexpected result text: %q", resultText(res))
	}
}

func TestBashHandler_DefaultUsesFreshContextForCancellationCleanup(t *testing.T) {
	mock := &mockExecutor{}
	handler := bashHandler(testReg(mock))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := handler(ctx, makeReq(map[string]any{
		"command": "sleep 10",
		"shellId": "s-cancelled",
	}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected cancellation tool error")
	}
	if mock.stopSessionCalls != 1 {
		t.Fatalf("stopSessionCalls = %d, want 1", mock.stopSessionCalls)
	}
	if mock.stopSessionCtxErr != nil {
		t.Fatalf("cleanup context error = %v, want nil", mock.stopSessionCtxErr)
	}
}

func TestBashHandler_DefaultReturnsShellIDForRunningCommand(t *testing.T) {
	mock := &mockExecutor{
		readSessionResult: "still running",
	}

	handler := bashHandler(testReg(mock))
	res, err := handler(context.Background(), makeReq(map[string]any{
		"command":      "go test ./...",
		"shellId":      "s2",
		"initial_wait": 0.001,
	}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got tool error: %s", resultText(res))
	}
	text := resultText(res)
	if !strings.Contains(text, "still running") || !strings.Contains(text, "[shellId: s2") {
		t.Fatalf("unexpected result text: %q", text)
	}
	if mock.stopSessionCalls != 0 {
		t.Fatalf("stopSessionCalls = %d, want 0", mock.stopSessionCalls)
	}
}

func TestBashHandler_DefaultPreservesOutputWhenCleanupFails(t *testing.T) {
	mock := &mockExecutor{
		readSessionResult: "done\n[session exited]",
		stopSessionErr:    fmt.Errorf("session not found"),
	}

	handler := bashHandler(testReg(mock))
	res, err := handler(context.Background(), makeReq(map[string]any{
		"command":      "echo done",
		"shellId":      "s2b",
		"initial_wait": 0.001,
	}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got tool error: %s", resultText(res))
	}
	text := resultText(res)
	if !strings.Contains(text, "done") || !strings.Contains(text, "[cleanup warning: failed to stop completed session s2b: session not found]") {
		t.Fatalf("unexpected result text: %q", text)
	}
}

func TestBashHandler_DefaultFallsBackToRunBashWhenSessionStartFails(t *testing.T) {
	mock := &mockExecutor{
		startSessionErr: fmt.Errorf("tmux unavailable"),
		runBashStdout:   "fallback output\n",
	}

	handler := bashHandler(testReg(mock))
	res, err := handler(context.Background(), makeReq(map[string]any{
		"command": "echo hi",
		"shellId": "s3",
	}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got tool error: %s", resultText(res))
	}
	if !strings.Contains(resultText(res), "fallback output") {
		t.Fatalf("unexpected result text: %q", resultText(res))
	}
	if mock.runBashCalls != 1 {
		t.Fatalf("runBashCalls = %d, want 1", mock.runBashCalls)
	}
}

func TestBashHandler_DoesNotFallbackAfterConnectionLossStartingLegacySession(t *testing.T) {
	mock := &mockExecutor{
		startSessionErr: &daemonclient.ConnectionLostError{
			Cause:          fmt.Errorf("daemon exited"),
			Reconnected:    true,
			OutcomeUnknown: true,
			OldGeneration:  1,
			NewGeneration:  2,
		},
		runBashStdout: "must not run",
	}

	handler := bashHandler(testReg(mock))
	res, err := handler(context.Background(), makeReq(map[string]any{
		"command": "touch side-effect",
		"shellId": "connection-lost",
	}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("result error = false, want true; result = %s", resultText(res))
	}
	if mock.runBashCalls != 0 {
		t.Fatalf("runBashCalls = %d, want 0 to avoid replay", mock.runBashCalls)
	}
	text := resultText(res)
	if !strings.Contains(text, "Remote connection lost") ||
		!strings.Contains(text, "outcome of this call is unknown") ||
		!strings.Contains(text, "inspect remote state before repeating") {
		t.Fatalf("result missing connection-loss guidance:\n%s", text)
	}
}

func TestBashHandler_AsyncStartsSession(t *testing.T) {
	mock := &mockExecutor{
		readSessionResult: "server booting",
	}

	handler := bashHandler(testReg(mock))
	res, err := handler(context.Background(), makeReq(map[string]any{
		"command": "npm run dev",
		"mode":    "async",
		"shellId": "s4",
		"cwd":     "/workspaces/repo/web",
	}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got tool error: %s", resultText(res))
	}
	text := resultText(res)
	if !strings.Contains(text, "Started async session: s4") || !strings.Contains(text, "server booting") {
		t.Fatalf("unexpected result text: %q", text)
	}
	if mock.stopSessionCalls != 0 {
		t.Fatalf("stopSessionCalls = %d, want 0", mock.stopSessionCalls)
	}
	if mock.lastStartSessionCwd != "/workspaces/repo/web" {
		t.Fatalf("lastStartSessionCwd = %q, want %q", mock.lastStartSessionCwd, "/workspaces/repo/web")
	}
}

func TestGrepHandler(t *testing.T) {
	tests := []struct {
		name     string
		mock     *mockExecutor
		args     map[string]any
		wantErr  bool
		wantText string
	}{
		{
			name:     "success with results",
			mock:     &mockExecutor{grepResult: "file.go:10:match\n"},
			args:     map[string]any{"pattern": "match"},
			wantText: "file.go:10:match",
		},
		{
			name:     "no matches",
			mock:     &mockExecutor{grepResult: ""},
			args:     map[string]any{"pattern": "nope"},
			wantText: "No matches found.",
		},
		{
			name:     "executor error",
			mock:     &mockExecutor{grepErr: fmt.Errorf("grep failed with exit code 2")},
			args:     map[string]any{"pattern": "bad["},
			wantErr:  true,
			wantText: "grep failed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := grepHandler(testReg(tt.mock))
			res, err := handler(context.Background(), makeReq(tt.args))
			if err != nil {
				t.Fatalf("unexpected Go error: %v", err)
			}
			if tt.wantErr && !res.IsError {
				t.Fatal("expected tool error, got success")
			}
			if !tt.wantErr && res.IsError {
				t.Fatalf("expected success, got tool error: %s", resultText(res))
			}
			if !strings.Contains(resultText(res), tt.wantText) {
				t.Errorf("result text %q does not contain %q", resultText(res), tt.wantText)
			}
		})
	}
}

func TestGrepHandler_PassesExplicitCwd(t *testing.T) {
	mock := &mockExecutor{grepResult: "cmd/main.go:12:match\n"}

	handler := grepHandler(testReg(mock))
	res, err := handler(context.Background(), makeReq(map[string]any{
		"pattern": "match",
		"path":    "cmd",
		"glob":    "*.go",
		"cwd":     "/workspaces/repo",
	}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got tool error: %s", resultText(res))
	}
	if mock.lastGrepPattern != "match" || mock.lastGrepPath != "cmd" || mock.lastGrepGlob != "*.go" || mock.lastGrepCwd != "/workspaces/repo" {
		t.Fatalf("grep args = pattern:%q path:%q glob:%q cwd:%q", mock.lastGrepPattern, mock.lastGrepPath, mock.lastGrepGlob, mock.lastGrepCwd)
	}
}

func TestGlobHandler(t *testing.T) {
	tests := []struct {
		name     string
		mock     *mockExecutor
		args     map[string]any
		wantErr  bool
		wantText string
	}{
		{
			name:     "success with results",
			mock:     &mockExecutor{globResult: "src/main.go\nsrc/util.go\n"},
			args:     map[string]any{"pattern": "**/*.go"},
			wantText: "src/main.go",
		},
		{
			name:     "no matches",
			mock:     &mockExecutor{globResult: ""},
			args:     map[string]any{"pattern": "**/*.xyz"},
			wantText: "No matches found.",
		},
		{
			name:     "executor error",
			mock:     &mockExecutor{globErr: fmt.Errorf("glob failed with exit code 2")},
			args:     map[string]any{"pattern": "**/*"},
			wantErr:  true,
			wantText: "glob failed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := globHandler(testReg(tt.mock))
			res, err := handler(context.Background(), makeReq(tt.args))
			if err != nil {
				t.Fatalf("unexpected Go error: %v", err)
			}
			if tt.wantErr && !res.IsError {
				t.Fatal("expected tool error, got success")
			}
			if !tt.wantErr && res.IsError {
				t.Fatalf("expected success, got tool error: %s", resultText(res))
			}
			if !strings.Contains(resultText(res), tt.wantText) {
				t.Errorf("result text %q does not contain %q", resultText(res), tt.wantText)
			}
		})
	}
}

func TestGlobHandler_PassesExplicitCwd(t *testing.T) {
	mock := &mockExecutor{globResult: "pkg/foo.go\n"}

	handler := globHandler(testReg(mock))
	res, err := handler(context.Background(), makeReq(map[string]any{
		"pattern": "**/*.go",
		"path":    "pkg",
		"cwd":     "/workspaces/repo",
	}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got tool error: %s", resultText(res))
	}
	if mock.lastGlobPattern != "**/*.go" || mock.lastGlobPath != "pkg" || mock.lastGlobCwd != "/workspaces/repo" {
		t.Fatalf("glob args = pattern:%q path:%q cwd:%q", mock.lastGlobPattern, mock.lastGlobPath, mock.lastGlobCwd)
	}
}

func TestStopBashHandler(t *testing.T) {
	tests := []struct {
		name     string
		mock     *mockExecutor
		args     map[string]any
		wantErr  bool
		wantText string
	}{
		{
			name:     "success",
			mock:     &mockExecutor{},
			args:     map[string]any{"shellId": "s1"},
			wantText: "stopped",
		},
		{
			name:     "executor error",
			mock:     &mockExecutor{stopSessionErr: fmt.Errorf("session not found")},
			args:     map[string]any{"shellId": "bad"},
			wantErr:  true,
			wantText: "session not found",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := stopBashHandler(testReg(tt.mock))
			res, err := handler(context.Background(), makeReq(tt.args))
			if err != nil {
				t.Fatalf("unexpected Go error: %v", err)
			}
			if tt.wantErr && !res.IsError {
				t.Fatal("expected tool error, got success")
			}
			if !tt.wantErr && res.IsError {
				t.Fatalf("expected success, got tool error: %s", resultText(res))
			}
			if !strings.Contains(resultText(res), tt.wantText) {
				t.Errorf("result text %q does not contain %q", resultText(res), tt.wantText)
			}
		})
	}
}

func TestListBashHandler(t *testing.T) {
	tests := []struct {
		name     string
		mock     *mockExecutor
		wantErr  bool
		wantText string
	}{
		{
			name:     "success with sessions",
			mock:     &mockExecutor{listSessionsResult: "copilot-s1 123 456\n"},
			wantText: "copilot-s1",
		},
		{
			name:     "empty returns no active",
			mock:     &mockExecutor{listSessionsResult: ""},
			wantText: "No active sessions.",
		},
		{
			name:     "executor error",
			mock:     &mockExecutor{listSessionsErr: fmt.Errorf("list sessions failed with exit code 2")},
			wantErr:  true,
			wantText: "list sessions failed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := listBashHandler(testReg(tt.mock))
			res, err := handler(context.Background(), makeReq(map[string]any{}))
			if err != nil {
				t.Fatalf("unexpected Go error: %v", err)
			}
			if tt.wantErr && !res.IsError {
				t.Fatal("expected tool error, got success")
			}
			if !tt.wantErr && res.IsError {
				t.Fatalf("expected success, got tool error: %s", resultText(res))
			}
			if !strings.Contains(resultText(res), tt.wantText) {
				t.Errorf("result text %q does not contain %q", resultText(res), tt.wantText)
			}
		})
	}
}

func TestCdHandler(t *testing.T) {
	tests := []struct {
		name     string
		args     map[string]any
		mock     *mockExecutor
		wantErr  bool
		wantText string
		wantDir  string
	}{
		{
			name:     "missing path",
			args:     map[string]any{},
			mock:     &mockExecutor{},
			wantErr:  true,
			wantText: "missing required parameter",
		},
		{
			name:     "directory exists",
			args:     map[string]any{"path": "/workspaces/myproject/src"},
			mock:     &mockExecutor{runBashStdout: "/workspaces/myproject/src\n", runBashExit: 0},
			wantText: "Changed working directory",
			wantDir:  "/workspaces/myproject/src",
		},
		{
			name:     "directory does not exist",
			args:     map[string]any{"path": "/nonexistent"},
			mock:     &mockExecutor{runBashExit: 1},
			wantErr:  true,
			wantText: "directory does not exist",
		},
		{
			name:     "executor error",
			args:     map[string]any{"path": "/workspaces"},
			mock:     &mockExecutor{runBashErr: fmt.Errorf("connection failed")},
			wantErr:  true,
			wantText: "failed to change directory",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := cdHandler(testReg(tt.mock))
			res, err := handler(context.Background(), makeReq(tt.args))
			if err != nil {
				t.Fatalf("unexpected Go error: %v", err)
			}
			if tt.wantErr && !res.IsError {
				t.Fatal("expected tool error, got success")
			}
			if !tt.wantErr && res.IsError {
				t.Fatalf("expected success, got tool error: %s", resultText(res))
			}
			if !strings.Contains(resultText(res), tt.wantText) {
				t.Errorf("result text %q does not contain %q", resultText(res), tt.wantText)
			}
			if tt.wantDir != "" && tt.mock.workdir != tt.wantDir {
				t.Errorf("expected workdir %q, got %q", tt.wantDir, tt.mock.workdir)
			}
		})
	}
}

func TestCdHandler_ValidatesRelativePathAgainstCurrentDefaultCwd(t *testing.T) {
	mock := &mockExecutor{
		workdir:       "/workspaces/repo",
		runBashStdout: "/workspaces/repo/src\n",
		runBashExit:   0,
	}

	handler := cdHandler(testReg(mock))
	res, err := handler(context.Background(), makeReq(map[string]any{"path": "src"}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got tool error: %s", resultText(res))
	}
	if mock.lastRunBashCwd != "/workspaces/repo" {
		t.Fatalf("lastRunBashCwd = %q, want %q", mock.lastRunBashCwd, "/workspaces/repo")
	}
	if mock.workdir != "/workspaces/repo/src" {
		t.Fatalf("workdir = %q, want %q", mock.workdir, "/workspaces/repo/src")
	}
}

func TestCwdHandler(t *testing.T) {
	mock := &mockExecutor{workdir: "/workspaces/myproject"}
	handler := cwdHandler(testReg(mock))
	res, err := handler(context.Background(), makeReq(map[string]any{}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got tool error: %s", resultText(res))
	}
	if !strings.Contains(resultText(res), "/workspaces/myproject") {
		t.Errorf("expected workdir in result, got %q", resultText(res))
	}
}

func TestCwdHandlerDefault(t *testing.T) {
	mock := &mockExecutor{}
	handler := cwdHandler(testReg(mock))
	res, err := handler(context.Background(), makeReq(map[string]any{}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if !strings.Contains(resultText(res), "/workspaces") {
		t.Errorf("expected default workdir, got %q", resultText(res))
	}
}

func TestListCodespacesHandler(t *testing.T) {
	reg := registry.New()
	reg.Register(&registry.ManagedCodespace{
		Alias:      "github",
		Name:       "cs-abc",
		Repository: "github/github",
		Branch:     "main",
		Workdir:    "/workspaces/github",
		Executor:   &mockExecutor{},
	})

	handler := listCodespacesHandler(reg)
	res, err := handler(context.Background(), makeReq(map[string]any{}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	text := resultText(res)
	if !strings.Contains(text, "github") {
		t.Errorf("expected 'github' alias in output, got %q", text)
	}
	if !strings.Contains(text, "github/github") {
		t.Errorf("expected repository in output, got %q", text)
	}
}

func TestListCodespacesHandler_Empty(t *testing.T) {
	reg := registry.New()
	handler := listCodespacesHandler(reg)
	res, _ := handler(context.Background(), makeReq(map[string]any{}))
	if !strings.Contains(resultText(res), "No codespaces") {
		t.Errorf("expected 'No codespaces' message, got %q", resultText(res))
	}
}

func TestResolveExecutor_MultiCS_NoAlias(t *testing.T) {
	reg := registry.New()
	reg.Register(&registry.ManagedCodespace{Alias: "a", Name: "cs-a", Executor: &mockExecutor{}})
	reg.Register(&registry.ManagedCodespace{Alias: "b", Name: "cs-b", Executor: &mockExecutor{}})

	handler := viewHandler(reg)
	res, _ := handler(context.Background(), makeReq(map[string]any{"path": "/tmp/f.txt"}))
	if !res.IsError {
		t.Fatal("expected error when multiple codespaces and no alias")
	}
	if !strings.Contains(resultText(res), "multiple codespaces") {
		t.Errorf("expected disambiguation error, got %q", resultText(res))
	}
}

func TestResolveExecutor_MultiCS_WithAlias(t *testing.T) {
	mock := &mockExecutor{viewFileResult: "hello from b"}
	reg := registry.New()
	reg.Register(&registry.ManagedCodespace{Alias: "a", Name: "cs-a", Executor: &mockExecutor{}})
	reg.Register(&registry.ManagedCodespace{Alias: "b", Name: "cs-b", Executor: mock})

	handler := viewHandler(reg)
	res, _ := handler(context.Background(), makeReq(map[string]any{"path": "/tmp/f.txt", "codespace": "b"}))
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultText(res))
	}
	if !strings.Contains(resultText(res), "hello from b") {
		t.Errorf("expected result from codespace b, got %q", resultText(res))
	}
}

func TestWriteBashHandler_CancelDuringDelay(t *testing.T) {
	mock := &mockExecutor{readSessionResult: "delayed-output"}
	handler := writeBashHandler(testReg(mock))

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel immediately — the handler should return promptly instead of sleeping.
	cancel()

	res, err := handler(ctx, makeReq(map[string]any{
		"shellId": "s1",
		"delay":   float64(60),
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected error result on cancelled context")
	}
}

func TestReadBashHandler_CancelDuringDelay(t *testing.T) {
	mock := &mockExecutor{readSessionResult: "session-output"}
	handler := readBashHandler(testReg(mock))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := handler(ctx, makeReq(map[string]any{
		"shellId": "s1",
		"delay":   float64(60),
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected error result on cancelled context")
	}
}

func TestReadBashHandler_ReturnsWhenDaemonSessionCompletes(t *testing.T) {
	base := &mockExecutor{}
	mock := &waitSessionMockExecutor{
		mockExecutor: base,
		output:       "session-output\n[session exited]",
		completed:    true,
	}
	handler := readBashHandler(testRegWithExecutor(mock))

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	res, err := handler(ctx, makeReq(map[string]any{
		"shellId": "s1",
		"delay":   float64(60),
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got tool error: %s", resultText(res))
	}
	if got := resultText(res); got != "session-output\n[session exited]" {
		t.Fatalf("result text = %q, want retained session output", got)
	}
	if mock.waitCalls != 1 {
		t.Fatalf("waitCalls = %d, want 1", mock.waitCalls)
	}
	if mock.lastWaitTime != 60*time.Second {
		t.Fatalf("lastWaitTime = %v, want 60s", mock.lastWaitTime)
	}
	if base.readSessionCalls != 0 {
		t.Fatalf("readSessionCalls = %d, want 0", base.readSessionCalls)
	}
}

func TestReadBashHandler_FallbackReturnsWhenSessionCompletes(t *testing.T) {
	mock := &mockExecutor{
		readSessionResults: []string{
			"running",
			"session-output\n[session exited]",
		},
	}
	handler := readBashHandler(testReg(mock))

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	res, err := handler(ctx, makeReq(map[string]any{
		"shellId": "s1",
		"delay":   float64(60),
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got tool error: %s", resultText(res))
	}
	if got := resultText(res); got != "session-output\n[session exited]" {
		t.Fatalf("result text = %q, want completed session output", got)
	}
	if mock.readSessionCalls != 2 {
		t.Fatalf("readSessionCalls = %d, want 2", mock.readSessionCalls)
	}
}

func TestWriteBashHandler_ReturnsWhenDaemonSessionCompletes(t *testing.T) {
	base := &mockExecutor{}
	mock := &waitSessionMockExecutor{
		mockExecutor: base,
		output:       "command-output\n[session exited]",
		completed:    true,
	}
	handler := writeBashHandler(testRegWithExecutor(mock))

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	res, err := handler(ctx, makeReq(map[string]any{
		"shellId": "s1",
		"input":   "{enter}",
		"delay":   float64(60),
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got tool error: %s", resultText(res))
	}
	if got := resultText(res); got != "command-output\n[session exited]" {
		t.Fatalf("result text = %q, want completed session output", got)
	}
	if mock.waitCalls != 1 {
		t.Fatalf("waitCalls = %d, want 1", mock.waitCalls)
	}
	if mock.lastWaitTime != 60*time.Second {
		t.Fatalf("lastWaitTime = %v, want 60s", mock.lastWaitTime)
	}
	if base.readSessionCalls != 0 {
		t.Fatalf("readSessionCalls = %d, want 0", base.readSessionCalls)
	}
}

func TestBashHandler_AsyncCancelDuringInitialWait(t *testing.T) {
	mock := &mockExecutor{}
	handler := bashHandler(testReg(mock))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := handler(ctx, makeReq(map[string]any{
		"command":      "sleep 100",
		"mode":         "async",
		"initial_wait": float64(60),
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected error result on cancelled context during async initial wait")
	}
}
