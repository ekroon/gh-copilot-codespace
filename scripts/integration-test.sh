#!/usr/bin/env bash
set -euo pipefail

SIGNOFF=1
case "${1:-}" in
  "") ;;
  --no-signoff) SIGNOFF=0 ;;
  *) echo "Usage: $0 [--no-signoff]" >&2; exit 2 ;;
esac

echo "=== gh-copilot-codespace integration tests ==="
BUILD_DIR=""
cleanup() {
  if [[ -n "$BUILD_DIR" ]]; then
    rm -f -- "$BUILD_DIR/gh-copilot-codespace"
    rmdir -- "$BUILD_DIR"
  fi
}
trap cleanup EXIT

# A prebuilt artifact supports remote builds without forwarding host credentials.
BINARY="${INTEGRATION_TEST_BINARY:-}"
if [[ -z "$BINARY" ]]; then
  BUILD_DIR=$(mktemp -d "${TMPDIR:-/tmp}/copilot-integration.XXXXXX")
  BINARY="$BUILD_DIR/gh-copilot-codespace"
  go build -o "$BINARY" ./cmd/gh-copilot-codespace
fi
if [[ ! -x "$BINARY" ]]; then
  echo "Integration binary is not executable: $BINARY" >&2
  exit 1
fi

python3 - "$BINARY" <<'PY'
import contextlib
import json
import os
import pathlib
import posixpath
import selectors
import subprocess
import sys
import tempfile

binary = os.path.abspath(sys.argv[1])


@contextlib.contextmanager
def host(env, cwd):
    with tempfile.TemporaryFile(mode="w+") as errors:
        proc = subprocess.Popen(
            [binary, "extension-host"], stdin=subprocess.PIPE,
            stdout=subprocess.PIPE, stderr=errors, env=env, cwd=cwd, text=True,
        )
        selector = selectors.DefaultSelector()
        selector.register(proc.stdout, selectors.EVENT_READ)
        sequence = 0

        def call(method, **args):
            nonlocal sequence
            sequence += 1
            proc.stdin.write(json.dumps({"id": sequence, "method": method, **args}) + "\n")
            proc.stdin.flush()
            if not selector.select(60):
                raise RuntimeError("extension-host response timed out")
            line = proc.stdout.readline()
            if not line:
                raise RuntimeError("extension-host exited before responding")
            response = json.loads(line)
            if response.get("id") != sequence or "error" in response:
                raise RuntimeError("invalid extension-host response: " + line)
            return response["result"]

        try:
            yield call
        except BaseException:
            proc.terminate()
            proc.wait(timeout=15)
            errors.seek(0)
            print(errors.read(), file=sys.stderr)
            raise
        finally:
            proc.stdin.close()
            try:
                proc.wait(timeout=15)
            except subprocess.TimeoutExpired:
                proc.kill()
                proc.wait(timeout=5)
            selector.close()
            proc.stdout.close()
        if proc.returncode:
            errors.seek(0)
            raise RuntimeError("extension-host failed: " + errors.read())


def check_mode(entries, native=False):
    with tempfile.TemporaryDirectory(prefix="copilot-integration-host-") as root:
        local = pathlib.Path(root) / "host"
        artifacts = pathlib.Path(root) / "artifacts"
        local.mkdir()
        artifacts.mkdir()
        env = os.environ.copy()
        for key in ("CODESPACE_NAME", "CODESPACE_WORKSPACE_CONFIG",
                    "COPILOT_CODESPACE_REMOTE_NATIVE", "COPILOT_CODESPACE_STATUS_FILE"):
            env.pop(key, None)
        env["CODESPACE_REGISTRY"] = json.dumps(entries)
        env["CODESPACE_LOCAL_WORKDIR"] = str(local)
        if native:
            env["COPILOT_CODESPACE_REMOTE_NATIVE"] = "1"
            env["CODESPACE_WORKSPACE_CONFIG"] = json.dumps({
                "alias": "integration", "artifactDir": str(artifacts), "skillRoots": [],
            })
        with host(env, local) as call:
            tools = {tool["name"]: tool for tool in call("list_tools")["tools"]}
            name = "bash" if native else "remote_bash"
            if name not in tools or bool(tools[name].get("overridesBuiltInTool")) != native:
                raise RuntimeError("missing or incorrectly classified " + name)
            if entries:
                result = call("call_tool", tool=name, args={
                    "command": "pwd && uname -s", "cwd": entries[0]["workdir"],
                    "initial_wait": 10,
                })
                if result.get("resultType") != "success":
                    raise RuntimeError("remote execution failed: " + json.dumps(result))
                lines = result.get("textResultForLlm", "").strip().splitlines()
                if lines != [entries[0]["workdir"], "Linux"]:
                    raise RuntimeError("wrong remote execution location: " + repr(lines))
        print("  PASS " + ("native" if native else "ordinary") +
              (" remote execution" if entries else " extension-host bootstrap"), flush=True)


check_mode([])
spaces = json.loads(subprocess.check_output(
    ["gh", "codespace", "list", "--json", "name,state"], text=True, timeout=30,
))
selected = os.environ.get("CODESPACE_NAME")
available = [space["name"] for space in spaces if space["state"] == "Available"]
if selected:
    if selected not in available:
        raise RuntimeError("selected Codespace must already be available: " + selected)
elif available:
    selected = available[0]
else:
    raise RuntimeError("a running Codespace is required; no checks may be skipped")
workdir = os.environ.get("INTEGRATION_TEST_WORKDIR") or subprocess.check_output(
    ["gh", "codespace", "ssh", "-c", selected, "--", "pwd"], text=True, timeout=60,
).strip()
if not posixpath.isabs(workdir) or posixpath.normpath(workdir) == "/":
    raise RuntimeError("integration workdir must be an absolute non-root remote path")
entry = {"alias": "integration", "name": selected, "workdir": workdir}
if os.environ.get("INTEGRATION_TEST_HELPER"):
    entry["helperPath"] = os.environ["INTEGRATION_TEST_HELPER"]
check_mode([entry])
check_mode([entry], native=True)
PY

echo "All critical tests passed!"

if [[ "$SIGNOFF" == 0 ]]; then
  echo "Signoff and promotion were not requested."
  exit 0
fi

# Auto-signoff if gh-signoff is installed
if ! gh extension list 2>/dev/null | grep -q signoff; then
  echo ""
  echo "⚠️  gh-signoff not installed. Run: ./scripts/setup-signoff.sh"
  echo "   Then re-run this script to sign off automatically."
elif gh signoff integration; then
  echo ""
  echo "✅ Signed off on $(git rev-parse --short HEAD)"
  echo ""
  echo "Triggering promote-to-latest workflow..."
  if gh workflow run promote-to-latest.yml; then
    echo "✅ Promote-to-latest workflow triggered"
  else
    echo "⚠️  Failed to trigger promote-to-latest workflow. Run manually:"
    echo "   gh workflow run promote-to-latest.yml"
  fi
else
  echo ""
  echo "⚠️  Signoff failed. Check errors above."
fi
