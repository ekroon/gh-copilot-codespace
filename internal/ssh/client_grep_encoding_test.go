package ssh_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/ekroon/gh-copilot-codespace/internal/helperinfo"
	"github.com/ekroon/gh-copilot-codespace/internal/ssh"
)

func TestClientGrepFilesEncodesCanonicalGrepRequest(t *testing.T) {
	binDir := t.TempDir()
	ghPath := filepath.Join(binDir, "gh")
	ghScript := `#!/bin/sh
while [ "$#" -gt 0 ] && [ "$1" != "--" ]; do
	shift
done
if [ "$#" -eq 0 ]; then
	exit 2
fi
shift
exec /bin/sh -c "$1"
`
	if err := os.WriteFile(ghPath, []byte(ghScript), 0o755); err != nil {
		t.Fatalf("WriteFile(fake gh) error = %v", err)
	}

	stdinPath := filepath.Join(t.TempDir(), "grep.json")
	helperPath := filepath.Join(binDir, "filesystem-helper")
	helperScript := fmt.Sprintf(`#!/bin/sh
if [ "$1" != "filesystem" ] || [ "$2" != "grep" ]; then
	exit 2
fi
/bin/cat > %q
printf '{"output":""}\n'
`, stdinPath)
	if err := os.WriteFile(helperPath, []byte(helperScript), 0o755); err != nil {
		t.Fatalf("WriteFile(filesystem helper) error = %v", err)
	}
	t.Setenv("PATH", binDir)

	client := ssh.NewClient("demo")
	if err := client.SelectFilesystemHelper(helperPath, helperinfo.Current()); err != nil {
		t.Fatalf("SelectFilesystemHelper() error = %v", err)
	}
	client.SetWorkdir("/workspaces/repo")

	lineNumbers := false
	req := ssh.GrepRequest{
		Pattern:         "Needle",
		Paths:           []string{"src", "pkg"},
		Glob:            "*.tsx",
		OutputMode:      ssh.GrepOutputModeContent,
		Type:            "tsx",
		CaseInsensitive: true,
		AfterContext:    1,
		BeforeContext:   2,
		Context:         3,
		LineNumbers:     &lineNumbers,
		HeadLimit:       5,
		Multiline:       true,
	}
	if _, err := client.GrepFiles(context.Background(), req); err != nil {
		t.Fatalf("GrepFiles() error = %v", err)
	}

	data, err := os.ReadFile(stdinPath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", stdinPath, err)
	}
	var decoded ssh.GrepRequest
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal(GrepRequest) error = %v", err)
	}
	want := req.Normalize()
	want.Cwd = "/workspaces/repo"
	if !reflect.DeepEqual(decoded, want) {
		t.Fatalf("decoded request = %+v, want %+v", decoded, want)
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("Unmarshal(payload) error = %v", err)
	}
	for _, key := range []string{"case_insensitive", "after_context", "before_context", "context", "line_numbers"} {
		if _, ok := payload[key]; ok {
			t.Fatalf("payload contains duplicate ad-hoc field %q: %s", key, data)
		}
	}
}
