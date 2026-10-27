package main

import (
	"os/exec"
	"strings"
	"testing"
)

// Exercise the generated extension using only mocked public SDK/child-process
// boundaries. No Copilot credentials, approval bypass, or model calls are used.
func TestRemoteNativeExtensionReadinessAndFailure(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	for _, scenario := range []string{"ready", "legacy", "missingManifest", "spawnError", "invalidProtocol", "timeout", "hostExit"} {
		t.Run(scenario, func(t *testing.T) {
			source := extensionSource()
			source = strings.Replace(source, `import { joinSession } from "@github/copilot-sdk/extension";`, `
const configs = [];
const logs = [];
const optionUpdates = [];
const liveSnapshot = {installedPlugins:["host-plugin"],mcpServers:["host-mcp"]};
const contextFields = ["workingDirectory", "cwd", "configDirectory", "configDir", "enableConfigDiscovery", "pluginDirectories"];
let snapshotRejections = 0;
function inheritLiveSnapshot(config) {
  if (contextFields.some(key => key in config)) {
    snapshotRejections++;
    throw new Error("Plugin activation snapshot does not match the installed plugin set.");
  }
  return liveSnapshot;
}
async function joinSession(config) {
  const snapshot = inheritLiveSnapshot(config);
  configs.push(config);
  return {
    snapshot,
    log: async (message, options) => logs.push({message, ...options}),
    rpc: {options:{update:async (options) => {
      optionUpdates.push(options);
      inheritLiveSnapshot(options);
    }}},
  };
}`, 1)
			source = strings.Replace(source, `import { spawn } from "node:child_process";`, `
import { EventEmitter } from "node:events";
let fakeHost;
let patchArgs;
function spawn() {
  const host = fakeHost = new EventEmitter();
  host.stdout = new EventEmitter();
  host.stdout.setEncoding = () => {};
  host.stdin = new EventEmitter();
  host.kill = () => {};
  host.stdin.write = (line, done) => {
    const request = JSON.parse(line);
    if (scenario === "spawnError") {
      queueMicrotask(() => host.emit("error", new Error("spawn unavailable")));
    } else if (scenario !== "timeout") {
      const workspace = {alias:"pinned", repository:"owner/repo", branch:"topic", workdir:"/workspaces/repo", localWorkdir:"/host", artifactDir:"/artifacts"};
      const tools = [
        {name:"view", parameters:{type:"object"}, overridesBuiltInTool:true},
        {name:"apply_patch", parameters:{type:"object", properties:{patch:{type:"string"}}}, overridesBuiltInTool:true},
        {name:"rg", parameters:{type:"object"}, overridesBuiltInTool:true},
        {name:"workspace_skill", parameters:{type:"object"}, overridesBuiltInTool:true},
      ];
      let result;
      if (request.method === "list_tools") {
        result = scenario === "invalidProtocol" ? {tools:[]} : {tools, workspace, systemMessage:{mode:"append",content:"remote only"}};
      } else {
        patchArgs = request.args;
        result = {textResultForLlm:"remote patch applied", resultType:"success"};
      }
      queueMicrotask(() => host.stdout.emit("data", JSON.stringify({id:request.id,result}) + "\n"));
    }
    done?.();
  };
  return host;
}`, 1)
			source = strings.Replace(source, "const manifest = loadManifest();", `
const manifest = scenario === "missingManifest" ? undefined :
  {selfBinary:"/self",env:{CODESPACE_WORKSPACE_CONFIG:'{"alias":"pinned"}',COPILOT_HOME:profile}};
`, 1)
			source = strings.Replace(source, `const remoteNative = process.env.`+codespaceRemoteNativeEnv+` === "1" || Boolean(manifest?.env?.CODESPACE_WORKSPACE_CONFIG);`, `const remoteNative = scenario !== "legacy";`, 1)
			source = strings.Replace(source, "20000);", "25);", 1)
			checks := `
if (configs.length !== 1) throw new Error("unexpected join count: " + configs.length);
if (snapshotRejections !== 0) throw new Error("live plugin snapshot was rejected by a context/discovery override");
for (const options of [...configs, ...optionUpdates]) {
  if (contextFields.some(key => key in options)) throw new Error("join/options update changed inherited context or discovery");
}
if (JSON.stringify(liveSnapshot) !== '{"installedPlugins":["host-plugin"],"mcpServers":["host-mcp"]}') throw new Error("host plugin/MCP snapshot changed");
const config = configs[0];
if (scenario === "legacy") {
  if (JSON.stringify(Object.keys(config).sort()) !== '["systemMessage","tools"]') throw new Error("legacy session options changed");
  if (config.tools.some(tool => tool.overridesBuiltInTool)) throw new Error("legacy built-in overrides changed");
  if (logs.length !== 0) throw new Error("legacy native logging enabled");
} else {
if (config.enableSkills !== false || config.skipCustomInstructions !== true ||
    config.enableHostGitOperations !== false || config.enableFileHooks !== false ||
    config.enableOnDemandInstructionDiscovery !== false) throw new Error("native settings missing");
if (JSON.stringify(config.availableTools) !== '["custom:*","mcp:*","builtin:task","builtin:ask_user","builtin:task_complete"]') throw new Error("native completion tool unavailable");
if ("enableConfigDiscovery" in config || "onPermissionRequest" in config) throw new Error("MCP or permissions overridden");
if (scenario === "ready" || scenario === "hostExit") {
  if (!logs.some(entry => entry.message.includes("LOCAL HOST folder: /host") && entry.message.includes("remote alias: pinned") && entry.message.includes("branch: topic"))) throw new Error("missing source identity status");
  if (!logs.some(entry => entry.level === "warning" && entry.message.includes("@attachment"))) throw new Error("missing local UI warning");
  const patch = config.tools.find(tool => tool.name === "apply_patch");
  const result = await patch.handler("*** Begin Patch\n*** End Patch");
  if (patchArgs.patch !== "*** Begin Patch\n*** End Patch" || result.resultType !== "success") throw new Error("patch bridge broken");
  if (!config.tools.find(tool => tool.name === "rg").overridesBuiltInTool) throw new Error("rg not overridden");
  if (config.tools.find(tool => tool.name === "workspace_skill").overridesBuiltInTool) throw new Error("custom tool override accepted");
  if (scenario === "hostExit") {
    fakeHost.emit("exit", 1, null);
    await new Promise(resolve => setTimeout(resolve, 0));
    const failed = await patch.handler("later patch");
    if (failed.resultType !== "failure") throw new Error("dead host returned success");
    if (!logs.some(entry => entry.level === "error" && entry.message.includes("remote-native extension unavailable"))) throw new Error("dead host not visibly reported");
  }
} else {
  if (config.tools.length !== 0 || !logs.some(entry => entry.level === "error" && entry.message.includes("remote-native extension unavailable"))) throw new Error("startup failure not explicit");
  if (!config.systemMessage.content.includes("STOP repository work")) throw new Error("missing failure instructions");
}
}
`
			cmd := exec.Command(node, "--input-type=module", "-")
			cmd.Stdin = strings.NewReader(`const scenario = "` + scenario + `";
const profile = "/private/native-session/profile with ' quote";
process.env.COPILOT_HOME = profile;
` + source + checks)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("native extension %s: %v\n%s", scenario, err, output)
			}
		})
	}
}
