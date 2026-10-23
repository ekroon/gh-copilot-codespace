package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIntegrationScriptUsesExtensionHostAndGatesSignoff(t *testing.T) {
	script, err := filepath.Abs("../../scripts/integration-test.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, failure := range []string{"", "bootstrap", "remote-tool"} {
		t.Run("failure="+failure, func(t *testing.T) {
			dir := t.TempDir()
			gh := `#!/bin/sh
case "$1 $2" in
  "codespace list") printf '[{"name":"integration-cs","state":"Available"}]\n' ;;
  "codespace ssh") printf '/workspaces/integration\n' ;;
  *) printf '%s\n' "$*" >> "$INTEGRATION_UNEXPECTED_GH"; exit 1 ;;
esac
`
			binary := `#!/usr/bin/env python3
import json, os, sys
if sys.argv[1:] != ["extension-host"]:
    raise SystemExit("expected extension-host")
native = bool(os.environ.get("CODESPACE_WORKSPACE_CONFIG"))
for line in sys.stdin:
    req = json.loads(line)
    if req["method"] == "list_tools":
        name = "bash" if native else "remote_bash"
        result = {"tools": [{"name": name, "overridesBuiltInTool": native}]}
        if os.environ["INTEGRATION_FAILURE"] == "bootstrap":
            result = {"tools": []}
    elif req["method"] == "call_tool":
        expected = "bash" if native else "remote_bash"
        if req["tool"] != expected:
            raise SystemExit("wrong execution surface")
        result = {"resultType": "success", "textResultForLlm": "/workspaces/integration\nLinux\n"}
        if os.environ["INTEGRATION_FAILURE"] == "remote-tool":
            result = {"resultType": "failure", "textResultForLlm": "injected command failure"}
    else:
        raise SystemExit("unsupported method")
    print(json.dumps({"id": req["id"], "result": result}), flush=True)
`
			for name, content := range map[string]string{
				"gh": gh, "candidate": binary, "go": "#!/bin/sh\nexit 99\n",
			} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			marker := filepath.Join(dir, "unexpected-gh")
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "bash", script, "--no-signoff")
			cmd.Dir = dir
			cmd.Env = append(os.Environ(),
				"PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
				"INTEGRATION_TEST_BINARY="+filepath.Join(dir, "candidate"),
				"CODESPACE_NAME=integration-cs",
				"INTEGRATION_TEST_WORKDIR=/workspaces/integration",
				"INTEGRATION_FAILURE="+failure,
				"INTEGRATION_UNEXPECTED_GH="+marker,
			)
			output, err := cmd.CombinedOutput()
			if failure == "" {
				if err != nil || !strings.Contains(string(output), "All critical tests passed!") {
					t.Fatalf("integration = %v\n%s", err, output)
				}
			} else if err == nil {
				t.Fatalf("integration signed off a failed %s check:\n%s", failure, output)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("--no-signoff invoked a publishing command; stat=%v\n%s", err, output)
			}
		})
	}
}
