package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ekroon/gh-copilot-codespace/internal/helperinfo"
	"github.com/ekroon/gh-copilot-codespace/internal/mcp"
	"github.com/ekroon/gh-copilot-codespace/internal/registry"
	"github.com/ekroon/gh-copilot-codespace/internal/ssh"
)

func TestWriteExtensionSessionManifestUsesUserRuntimeDirAndCleansStaleFiles(t *testing.T) {
	runtimeBase := t.TempDir()
	repository := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtimeBase)

	runtimeDir := filepath.Join(runtimeBase, extensionRuntimeDirName)
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(runtimeDir, extensionSessionManifestPrefix+"stale.json")
	fresh := filepath.Join(runtimeDir, extensionSessionManifestPrefix+"fresh.json")
	unrelated := filepath.Join(runtimeDir, "keep.txt")
	for _, path := range []string{stale, fresh, unrelated} {
		if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	staleTime := now.Add(-(extensionSessionManifestMaxAge + time.Hour))
	if err := os.Chtimes(stale, staleTime, staleTime); err != nil {
		t.Fatal(err)
	}

	path, err := writeExtensionSessionManifest(repository, "/usr/local/bin/self", map[string]string{}, "token", "mirror")
	if err != nil {
		t.Fatalf("writeExtensionSessionManifest: %v", err)
	}
	if !strings.HasPrefix(path, runtimeDir+string(filepath.Separator)) {
		t.Fatalf("manifest path = %q, want under %q", path, runtimeDir)
	}
	if strings.HasPrefix(path, repository+string(filepath.Separator)) {
		t.Fatalf("manifest path = %q, must be outside repository %q", path, repository)
	}
	info, err := os.Stat(runtimeDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("runtime dir mode = %v, want 0700", got)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale manifest still exists or unexpected error: %v", err)
	}
	for _, remaining := range []string{fresh, unrelated} {
		if _, err := os.Stat(remaining); err != nil {
			t.Fatalf("%s should remain: %v", remaining, err)
		}
	}
}

func TestGenerateProjectExtensionInstallsOnlyUserScopedExtension(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	t.Setenv("HOME", home)

	if err := generateProjectExtension(project); err != nil {
		t.Fatalf("generateProjectExtension: %v", err)
	}
	if _, err := os.Stat(filepath.Join(project, ".github", "extensions", userExtensionName, "extension.mjs")); !os.IsNotExist(err) {
		t.Fatalf("project extension should not be written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".copilot", "extensions", userExtensionName, "extension.mjs")); err != nil {
		t.Fatalf("user extension not installed: %v", err)
	}
}

func TestPrepareExtensionLaunchUsesSingleCurrentDirectoryMode(t *testing.T) {
	home := t.TempDir()
	runtimeBase := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_RUNTIME_DIR", runtimeBase)

	launch, err := prepareExtensionLaunch("/usr/local/bin/self", registry.New(), mcp.LifecycleConfig{})
	if err != nil {
		t.Fatalf("prepareExtensionLaunch: %v", err)
	}
	if launch.Token == "" || launch.ManifestPath == "" {
		t.Fatalf("launch = %+v, want token and manifest path", launch)
	}
	if _, ok := launch.HostEnv[codespaceExtensionModeEnv]; ok {
		t.Fatalf("host env contains legacy mode: %+v", launch.HostEnv)
	}
	if launch.ProcessEnv[codespaceExtensionTokenEnv] != launch.Token {
		t.Fatalf("process token env = %q, want %q", launch.ProcessEnv[codespaceExtensionTokenEnv], launch.Token)
	}
	if launch.ProcessEnv[codespaceExtensionManifestEnv] != launch.ManifestPath {
		t.Fatalf("process manifest env = %q, want %q", launch.ProcessEnv[codespaceExtensionManifestEnv], launch.ManifestPath)
	}
}

func TestExtensionModeAlwaysUsesCurrentDirectory(t *testing.T) {
	reg := registry.New()
	env := extensionHostEnv(reg, mcp.LifecycleConfig{}, "mirror")
	if _, ok := env[codespaceExtensionModeEnv]; ok {
		t.Fatalf("host env contains legacy mode: %+v", env)
	}
	if got := preambleModeFromEnv("mirror"); got != PreambleModeHere {
		t.Fatalf("preamble mode = %v, want current-directory mode", got)
	}
}

func TestExtensionHostEnvPreservesDeployedFilesystemHelper(t *testing.T) {
	reg := registry.New()
	client := ssh.NewClient("cs-abc")
	if err := client.SelectFilesystemHelper("/remote/agent", helperinfo.Current()); err != nil {
		t.Fatalf("SelectFilesystemHelper() error = %v", err)
	}
	if err := reg.Register(&registry.ManagedCodespace{
		Alias:      "github",
		Name:       "cs-abc",
		Workdir:    "/workspaces/github",
		Executor:   client,
		HelperPath: "/remote/agent",
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	env := extensionHostEnv(reg, mcp.LifecycleConfig{}, "")
	var entries []registryEntry
	if err := json.Unmarshal([]byte(env["CODESPACE_REGISTRY"]), &entries); err != nil {
		t.Fatalf("Unmarshal(CODESPACE_REGISTRY) error = %v", err)
	}
	if len(entries) != 1 || entries[0].HelperPath != "/remote/agent" {
		t.Fatalf("registry entries = %+v", entries)
	}
}

func TestExtensionSourceMapsToolResultsToSDKBinaryResults(t *testing.T) {
	source := extensionSource()

	for _, want := range []string{
		`case "image":`,
		`type: "image"`,
		`mimeType: entry.mimeType`,
		`binaryResultsForLlm`,
		`normalizeBinaryResults(result.binaryResultsForLlm)`,
		`applyStructuredTruncationPolicy`,
		`forceReadLargeFiles`,
		`delete normalized.binaryResultsForLlm`,
		`Array.isArray(bootstrap)`,
	} {
		if !strings.Contains(source, want) {
			t.Fatalf("generated extension source missing %q:\n%s", want, source)
		}
	}
	if strings.Contains(source, `normalized.structuredContent =`) {
		t.Fatalf("generated extension source still emits unsupported structuredContent:\n%s", source)
	}
	if strings.Contains(source, `structuredMetadataText`) || strings.Contains(source, `textParts.push(metadata)`) {
		t.Fatalf("generated extension source concatenates structured metadata into SDK text:\n%s", source)
	}
	if strings.Contains(source, `normalized.contents`) || strings.Contains(source, `contents: [...result.content]`) {
		t.Fatalf("generated extension source still emits unsupported contents:\n%s", source)
	}

	convertStart := strings.Index(source, "function convertMcpCallToolResult(result)")
	normalizeStart := strings.Index(source, "function normalizeToolResult(result)")
	if convertStart < 0 || normalizeStart <= convertStart {
		t.Fatalf("generated extension source is missing result conversion functions:\n%s", source)
	}
	converter := source[convertStart:normalizeStart]
	imageStart := strings.Index(converter, `case "image":`)
	audioStart := strings.Index(converter, `case "audio":`)
	if imageStart < 0 || audioStart <= imageStart {
		t.Fatalf("MCP result converter is missing image handling:\n%s", converter)
	}
	if got := strings.Count(converter[imageStart:audioStart], `data: entry.data`); got != 1 {
		t.Fatalf("MCP image conversion payload occurrences = %d, want 1:\n%s", got, converter[imageStart:audioStart])
	}
	if got := strings.Count(source, `applyStructuredTruncationPolicy(`); got != 3 {
		t.Fatalf("truncation policy occurrences = %d, want definition plus SDK and MCP application:\n%s", got, source)
	}
}

func TestExtensionSourceHasValidJavaScriptSyntax(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}

	cmd := exec.Command(node, "--input-type=module", "--check", "-")
	cmd.Stdin = strings.NewReader(extensionSource())
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node --check failed: %v\n%s", err, output)
	}
}

func TestExtensionSourceAppliesStructuredTruncationPolicy(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}

	source := extensionSource()
	start := strings.Index(source, "function isToolResultContent(entry)")
	end := strings.Index(source, "if (!manifest)")
	if start < 0 || end <= start {
		t.Fatalf("generated extension source is missing result normalization helpers:\n%s", source)
	}
	script := source[start:end] + `
function assert(condition, message) {
  if (!condition) throw new Error(message);
}

const truncatedText = normalizeToolResult({
  textResultForLlm: "one.go\ntwo.go\n",
  resultType: "success",
  structuredContent: { truncated: true, limit: 2 },
});
assert(truncatedText.resultType === "success", "partial text should preserve success result type");
assert(truncatedText.textResultForLlm.includes("WARNING"), "partial text should include a warning");
assert(truncatedText.textResultForLlm.includes("two.go"), "partial text should preserve content");

const mappedText = normalizeToolResult({
  textResultForLlm: truncatedResultWarning + "\n\none.go\n",
  resultType: "success",
  structuredContent: { truncated: true, limit: 1 },
});
assert(mappedText.textResultForLlm.split(truncatedResultWarning).length === 2, "warning should not be duplicated");

const truncatedImage = normalizeToolResult({
  textResultForLlm: "large.png (image/png)",
  resultType: "success",
  structuredContent: { kind: "image", truncated: true },
  binaryResultsForLlm: [{ data: "aW5jb21wbGV0ZQ==", mimeType: "image/png", type: "image" }],
});
assert(truncatedImage.resultType === "failure", "truncated image should fail");
assert(!("binaryResultsForLlm" in truncatedImage), "truncated image binary should be suppressed");
assert(truncatedImage.textResultForLlm.includes("forceReadLargeFiles"), "truncated image should explain retry");

const completeImage = normalizeToolResult({
  textResultForLlm: "large.png (image/png)",
  resultType: "success",
  structuredContent: { kind: "image" },
  binaryResultsForLlm: [{ data: "Y29tcGxldGU=", mimeType: "image/png", type: "image" }],
});
assert(completeImage.textResultForLlm === "large.png (image/png)", "complete image text should stay compatible");
assert(completeImage.binaryResultsForLlm.length === 1, "complete image should be delivered exactly once");

const rawMcpImage = normalizeToolResult({
  content: [
    { type: "text", text: "large.png (image/png)" },
    { type: "image", data: "aW5jb21wbGV0ZQ==", mimeType: "image/png" },
  ],
  structuredContent: { kind: "image", truncated: true },
});
assert(rawMcpImage.resultType === "failure", "raw MCP truncated image should fail");
assert(!("binaryResultsForLlm" in rawMcpImage), "raw MCP truncated image should be suppressed");
`

	cmd := exec.Command(node, "--input-type=module", "-")
	cmd.Stdin = strings.NewReader(script)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated truncation policy failed: %v\n%s", err, output)
	}
}
