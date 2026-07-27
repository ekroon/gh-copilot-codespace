package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ekroon/gh-copilot-codespace/internal/mcp"
	"github.com/ekroon/gh-copilot-codespace/internal/registry"
	"github.com/ekroon/gh-copilot-codespace/internal/ssh"
)

type codespace struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Repository  string `json:"repository"`
	State       string `json:"state"`
}

const codespaceLifecycleConfigEnv = "CODESPACE_LIFECYCLE_CONFIG"
const codespaceLocalWorkdirEnv = "CODESPACE_LOCAL_WORKDIR"

func printUsage() {
	fmt.Fprintf(os.Stderr, `Usage: gh copilot-codespace [flags] [-- copilot-args...]

Run Copilot CLI against remote GitHub Codespace(s) via SSH.

Flags:
  -c, --codespace NAME   Use a specific codespace (repeatable, or comma-separated)
      --no-codespace     Start without connecting to any codespace (skip picker)
      --selected-only[=BOOL]
                         Restrict existing-codespace connections to codespaces selected at startup

Subcommands:
  exec                   Execute a command on the codespace (used internally)
  daemon                 Run daemon protocol server (used internally)
`)
}

func main() {
	// Handle --help / -h before anything else
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--help", "-h":
			printUsage()
			return
		}
	}

	// If first arg is "exec", run a command with workdir/env setup (used on codespace)
	if len(os.Args) > 1 && os.Args[1] == "exec" {
		if err := runExec(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "exec: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// If first arg is "extension-host", run the JSON bridge used by generated extensions
	if len(os.Args) > 1 && os.Args[1] == "extension-host" {
		if err := runExtensionHost(); err != nil {
			fmt.Fprintf(os.Stderr, "extension-host: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// If first arg is "daemon", run the sandbox-side daemon protocol server
	if len(os.Args) > 1 && os.Args[1] == "daemon" {
		if err := runDaemon(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "daemon: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// Otherwise, run as interactive launcher
	if err := runLauncher(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func toolRuntimeInputsFromEnv() (*registry.Registry, mcp.LifecycleConfig, error) {
	// Support multi-codespace via CODESPACE_REGISTRY env var (JSON)
	// Falls back to single CODESPACE_NAME for backward compatibility
	registryJSON := os.Getenv("CODESPACE_REGISTRY")
	lifecycleCfg, err := lifecycleConfigFromEnv(os.Getenv(codespaceLifecycleConfigEnv))
	if err != nil {
		return nil, mcp.LifecycleConfig{}, fmt.Errorf("invalid %s: %w", codespaceLifecycleConfigEnv, err)
	}
	lifecycleCfg.LocalWorkdir = os.Getenv(codespaceLocalWorkdirEnv)
	lifecycleCfg.Provisioners = loadProvisioners()

	var reg *registry.Registry
	if registryJSON != "" {
		reg, err = registryFromJSON(registryJSON)
		if err != nil {
			return nil, mcp.LifecycleConfig{}, fmt.Errorf("invalid CODESPACE_REGISTRY: %w", err)
		}
	} else {
		codespaceName := os.Getenv("CODESPACE_NAME")
		if codespaceName == "" {
			return nil, mcp.LifecycleConfig{}, fmt.Errorf("CODESPACE_NAME or CODESPACE_REGISTRY environment variable is required")
		}
		sshClient := ssh.NewClient(codespaceName)
		ctx := context.Background()
		if err := sshClient.SetupMultiplexing(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "extension-host: multiplexing setup warning: %v\n", err)
		}
		workdir := os.Getenv("CODESPACE_WORKDIR")
		reg = registry.New()
		if err := reg.Register(&registry.ManagedCodespace{
			Alias:    registry.DefaultAlias(codespaceName, nil),
			Name:     codespaceName,
			Workdir:  workdir,
			Executor: sshClient,
		}); err != nil {
			return nil, mcp.LifecycleConfig{}, fmt.Errorf("invalid single-codespace registry: %w", err)
		}
	}
	return reg, lifecycleCfg, nil
}

// registryEntry is the JSON-serializable form of a codespace for extension-host.
type registryEntry struct {
	Alias      string `json:"alias"`
	Name       string `json:"name"`
	Repository string `json:"repository"`
	Branch     string `json:"branch"`
	Workdir    string `json:"workdir"`
}

type lifecycleConfigEnvData struct {
	AccessPolicy *mcp.CodespaceAccessPolicy `json:"accessPolicy,omitempty"`
}

func lifecycleConfigFromEnv(data string) (mcp.LifecycleConfig, error) {
	if strings.TrimSpace(data) == "" {
		return mcp.LifecycleConfig{}, nil
	}

	var env lifecycleConfigEnvData
	if err := json.Unmarshal([]byte(data), &env); err != nil {
		return mcp.LifecycleConfig{}, fmt.Errorf("parsing lifecycle config: %w", err)
	}

	var cfg mcp.LifecycleConfig
	if env.AccessPolicy != nil {
		cfg.AccessPolicy = mcp.CodespaceAccessPolicy{
			SelectedOnly:          env.AccessPolicy.SelectedOnly,
			AllowedCodespaceNames: uniqueStrings(env.AccessPolicy.AllowedCodespaceNames),
		}
	}
	return cfg, nil
}

func lifecycleConfigEnvJSON(cfg mcp.LifecycleConfig) string {
	var env lifecycleConfigEnvData
	if cfg.AccessPolicy.SelectedOnly || len(cfg.AccessPolicy.AllowedCodespaceNames) > 0 {
		env.AccessPolicy = &mcp.CodespaceAccessPolicy{
			SelectedOnly:          cfg.AccessPolicy.SelectedOnly,
			AllowedCodespaceNames: uniqueStrings(cfg.AccessPolicy.AllowedCodespaceNames),
		}
	}
	if env.AccessPolicy == nil {
		return ""
	}
	out, err := json.Marshal(env)
	if err != nil {
		return ""
	}
	return string(out)
}

// registryFromJSON deserializes CODESPACE_REGISTRY env var and creates SSH clients.
func registryFromJSON(data string) (*registry.Registry, error) {
	var entries []registryEntry
	if err := json.Unmarshal([]byte(data), &entries); err != nil {
		return nil, fmt.Errorf("parsing registry: %w", err)
	}
	return registryFromEntries(context.Background(), entries, func(ctx context.Context, e registryEntry) (*registry.ManagedCodespace, error) {
		sshClient := ssh.NewClient(e.Name)
		if err := sshClient.SetupMultiplexing(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "extension-host: multiplexing warning for %s: %v\n", e.Alias, err)
		}
		if e.Workdir != "" {
			sshClient.SetWorkdir(e.Workdir)
		}
		return &registry.ManagedCodespace{
			Alias:      e.Alias,
			Name:       e.Name,
			Repository: e.Repository,
			Branch:     e.Branch,
			Workdir:    e.Workdir,
			Executor:   sshClient,
		}, nil
	})
}

func registryFromEntries(ctx context.Context, entries []registryEntry, build func(context.Context, registryEntry) (*registry.ManagedCodespace, error)) (*registry.Registry, error) {
	reg := registry.New()
	for _, e := range entries {
		cs, err := build(ctx, e)
		if err != nil {
			return nil, fmt.Errorf("building registry entry %q: %w", e.Alias, err)
		}
		if err := reg.Register(cs); err != nil {
			return nil, fmt.Errorf("registering registry entry %q: %w", e.Alias, err)
		}
	}
	return reg, nil
}

type launcherOptions struct {
	codespaceNames []string
	noCodespace    bool
	selectedOnly   optionalBool
	copilotArgs    []string
}

type optionalBool struct {
	set   bool
	value bool
}

func (b optionalBool) resolve(defaultValue bool) bool {
	if !b.set {
		return defaultValue
	}
	return b.value
}

type launchContext struct {
	copilotCWD string
}

func parseOptionalBoolFlag(arg string, flagName string) (optionalBool, bool, error) {
	if arg == flagName {
		return optionalBool{set: true, value: true}, true, nil
	}
	prefix := flagName + "="
	if !strings.HasPrefix(arg, prefix) {
		return optionalBool{}, false, nil
	}
	value, err := strconv.ParseBool(strings.TrimSpace(arg[len(prefix):]))
	if err != nil {
		return optionalBool{}, true, fmt.Errorf("parsing %s: invalid boolean value %q", flagName, arg[len(prefix):])
	}
	return optionalBool{set: true, value: value}, true, nil
}

func parseLauncherArgs(args []string) (launcherOptions, error) {
	var opts launcherOptions
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			opts.copilotArgs = append(opts.copilotArgs, args[i+1:]...)
			break
		}
		if parsed, ok, err := parseOptionalBoolFlag(args[i], "--selected-only"); err != nil {
			return launcherOptions{}, err
		} else if ok {
			opts.selectedOnly = parsed
			continue
		}

		switch {
		case args[i] == "--here" || strings.HasPrefix(args[i], "--here="):
			return launcherOptions{}, fmt.Errorf("--here is no longer supported")
		case args[i] == "--workdir" || strings.HasPrefix(args[i], "--workdir="):
			return launcherOptions{}, fmt.Errorf("--workdir is no longer supported")
		case args[i] == "-w":
			return launcherOptions{}, fmt.Errorf("-w is no longer supported")
		case args[i] == "--local-tools" || strings.HasPrefix(args[i], "--local-tools="):
			return launcherOptions{}, fmt.Errorf("--local-tools is no longer supported")
		case args[i] == "--extension-tools" || strings.HasPrefix(args[i], "--extension-tools="):
			return launcherOptions{}, fmt.Errorf("--extension-tools is no longer supported")
		case args[i] == "--name" || strings.HasPrefix(args[i], "--name="):
			return launcherOptions{}, fmt.Errorf("--name is no longer supported")
		case args[i] == "--no-codespace":
			opts.noCodespace = true
		case (args[i] == "--codespace" || args[i] == "-c") && i+1 < len(args):
			// Support comma-separated: -c cs1,cs2
			for _, name := range strings.Split(args[i+1], ",") {
				name = strings.TrimSpace(name)
				if name != "" {
					opts.codespaceNames = append(opts.codespaceNames, name)
				}
			}
			i++
		default:
			opts.copilotArgs = append(opts.copilotArgs, args[i])
		}
	}

	if opts.noCodespace && len(opts.codespaceNames) > 0 {
		return launcherOptions{}, fmt.Errorf("--no-codespace and --codespace are mutually exclusive")
	}

	return opts, nil
}

func resolveLaunchContext(originalCWD string) launchContext {
	return launchContext{
		copilotCWD: originalCWD,
	}
}

func selectedCodespaceNames(selected []codespace) []string {
	names := make([]string, 0, len(selected))
	for _, cs := range selected {
		names = append(names, cs.Name)
	}
	return uniqueStrings(names)
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func runLauncher(args []string) error {
	originalCWD, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting current directory: %w", err)
	}

	opts, err := parseLauncherArgs(args)
	if err != nil {
		return err
	}

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("finding executable: %w", err)
	}

	// Select codespace(s): use --codespace flag(s) or interactive picker
	var selectedList []codespace
	if len(opts.codespaceNames) > 0 {
		for _, name := range opts.codespaceNames {
			cs, err := lookupCodespace(name)
			if err != nil {
				return fmt.Errorf("codespace %q: %w", name, err)
			}
			selectedList = append(selectedList, cs)
		}
	} else if !opts.noCodespace {
		selectedList, err = selectCodespaces()
		if err != nil {
			return err
		}
	}

	lifecycleCfg := mcp.LifecycleConfig{}
	if opts.selectedOnly.resolve(false) {
		lifecycleCfg.AccessPolicy = mcp.CodespaceAccessPolicy{
			SelectedOnly:          true,
			AllowedCodespaceNames: selectedCodespaceNames(selectedList),
		}
	}
	lifecycleCfg.LocalWorkdir = originalCWD

	ctx := context.Background()
	reg := registry.New()
	provisioners := loadProvisioners()

	for _, selected := range selectedList {
		fmt.Printf("Selected: %s (%s)\n", selected.DisplayName, selected.Repository)

		// Start codespace if needed
		if selected.State != "Available" {
			if err := startCodespace(selected.Name); err != nil {
				return err
			}
		}

		// Detect workspace directory
		workdir, err := detectWorkdir(selected.Name, selected.Repository)
		if err != nil {
			return err
		}
		fmt.Printf("  Workspace: %s\n", workdir)

		// Set up SSH multiplexing early for remote tools and IDE forwarding.
		sshClient := ssh.NewClient(selected.Name)
		if err := sshClient.SetupMultiplexing(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: SSH multiplexing failed for %s: %v\n", selected.Name, err)
		}

		// Deploy exec agent binary
		remoteBinary, err := deployBinary(sshClient, selected.Name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not deploy exec agent for %s: %v\n", selected.Name, err)
		}

		// Detect branch
		branch := detectRemoteBranch(sshClient, selected.Name, workdir)

		alias := registry.DefaultAlias(selected.Repository, reg.Aliases())
		sshClient.SetWorkdir(workdir)
		if err := reg.Register(&registry.ManagedCodespace{
			Alias:      alias,
			Name:       selected.Name,
			Repository: selected.Repository,
			Branch:     branch,
			Workdir:    workdir,
			Executor:   sshClient,
			ExecAgent:  remoteBinary,
		}); err != nil {
			return fmt.Errorf("registering selected codespace %q: %w", selected.Name, err)
		}
		runProvisioners(ctx, provisioners, selected.Name, selected.Repository, workdir, sshClient, false)
	}

	launchCtx := resolveLaunchContext(originalCWD)
	fmt.Printf("  Local cwd:  %s\n", originalCWD)

	// Preserve the caller's working directory for Copilot and all local tools.
	if err := prepareLaunchDirectory(launchCtx.copilotCWD); err != nil {
		return fmt.Errorf("changing to copilot directory: %w", err)
	}

	extensionLaunch, err := prepareExtensionLaunch(self, reg, lifecycleCfg)
	if err != nil {
		return fmt.Errorf("preparing extension launch: %w", err)
	}
	for name, value := range extensionLaunch.ProcessEnv {
		os.Setenv(name, value)
	}

	// Forward IDE connections from all connected codespaces
	for _, cs := range reg.All() {
		if sshClient, ok := cs.Executor.(*ssh.Client); ok && sshClient.SSHConfigPath() != "" {
			_, err = forwardIDEConnections(sshClient, cs.Name, launchCtx.copilotCWD, cs.Workdir)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: IDE forwarding failed for %s: %v\n", cs.Alias, err)
			}
		}
	}

	fmt.Printf("\nLaunching Copilot CLI with remote codespace tools...\n")
	if reg.Len() == 0 {
		fmt.Printf("  Codespace: none connected yet\n")
	}
	for _, cs := range reg.All() {
		fmt.Printf("  Codespace: %s (alias: %s, repo: %s)\n", cs.Name, cs.Alias, cs.Repository)
	}
	fmt.Printf("  Tools:     generated Copilot extension\n")
	fmt.Printf("\n")

	// Exec copilot
	return execCopilot(opts.copilotArgs)
}

func prepareLaunchDirectory(dir string) error {
	return os.Chdir(dir)
}

// lookupCodespace finds a codespace by name (exact or prefix match).
func lookupCodespace(name string) (codespace, error) {
	out, err := exec.Command("gh", "codespace", "list",
		"--json", "name,displayName,repository,state",
		"--limit", "50").Output()
	if err != nil {
		return codespace{}, fmt.Errorf("listing codespaces: %w", err)
	}

	var codespaces []codespace
	if err := json.Unmarshal(out, &codespaces); err != nil {
		return codespace{}, fmt.Errorf("parsing codespace list: %w", err)
	}

	// Try exact match first, then prefix match
	for _, cs := range codespaces {
		if cs.Name == name {
			return cs, nil
		}
	}
	for _, cs := range codespaces {
		if strings.HasPrefix(cs.Name, name) || strings.HasPrefix(cs.DisplayName, name) {
			return cs, nil
		}
	}
	return codespace{}, fmt.Errorf("codespace %q not found", name)
}

// selectCodespaces lets the user pick zero, one, or many codespaces interactively.
// Uses gum choose for multi-select if available, otherwise falls back to a numbered list.
func selectCodespaces() ([]codespace, error) {
	out, err := exec.Command("gh", "codespace", "list",
		"--json", "name,displayName,repository,state",
		"--limit", "50").Output()
	if err != nil {
		return nil, fmt.Errorf("listing codespaces: %w", err)
	}

	var codespaces []codespace
	if err := json.Unmarshal(out, &codespaces); err != nil {
		return nil, fmt.Errorf("parsing codespace list: %w", err)
	}
	if len(codespaces) == 0 {
		return nil, nil
	}

	// Sort: available first, then by display name
	sort.Slice(codespaces, func(i, j int) bool {
		if (codespaces[i].State == "Available") != (codespaces[j].State == "Available") {
			return codespaces[i].State == "Available"
		}
		return codespaces[i].DisplayName < codespaces[j].DisplayName
	})

	// Build display lines: "name\ticon repo: display [state]"
	lines := make([]string, len(codespaces))
	for i, cs := range codespaces {
		icon := "🟢"
		if cs.State != "Available" {
			icon = "⏸️"
		}
		lines[i] = fmt.Sprintf("%s\t%s %s: %s [%s]", cs.Name, icon, cs.Repository, cs.DisplayName, cs.State)
	}

	// Try gum choose for interactive multi-select.
	if gumPath, err := exec.LookPath("gum"); err == nil {
		byChoice := make(map[string]codespace, len(lines))
		for i, l := range lines {
			byChoice[l] = codespaces[i]
		}

		cmd := exec.Command(gumPath, "choose", "--no-limit", "--header", "Choose codespace(s) (Space toggles, Enter submits none)")
		cmd.Stdin = strings.NewReader(strings.Join(lines, "\n"))
		cmd.Stderr = os.Stderr
		selected, err := cmd.Output()
		if err == nil {
			return resolveSelectedCodespaces(strings.Split(strings.TrimSpace(string(selected)), "\n"), byChoice), nil
		}
		// gum failed (e.g., no TTY), fall through to numbered list.
	}

	// Fallback: numbered list
	for i, l := range lines {
		parts := strings.SplitN(l, "\t", 2)
		fmt.Printf("  %2d) %s\n", i+1, parts[1])
	}

	fmt.Printf("\nSelect [1-%d] (comma-separated, blank for none): ", len(codespaces))
	reader := bufio.NewReader(os.Stdin)
	input, err := reader.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("reading input: %w", err)
	}
	indices, err := parseSelectionIndices(input, len(codespaces))
	if err != nil {
		return nil, err
	}

	selected := make([]codespace, 0, len(indices))
	for _, idx := range indices {
		selected = append(selected, codespaces[idx])
	}
	return selected, nil
}

func resolveSelectedCodespaces(selected []string, byDisplay map[string]codespace) []codespace {
	result := make([]codespace, 0, len(selected))
	seen := make(map[string]bool, len(selected))
	for _, choice := range selected {
		choice = strings.TrimSpace(choice)
		if choice == "" {
			continue
		}
		cs, ok := byDisplay[choice]
		if !ok || seen[cs.Name] {
			continue
		}
		seen[cs.Name] = true
		result = append(result, cs)
	}
	return result
}

func parseSelectionIndices(input string, max int) ([]int, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return nil, nil
	}

	parts := strings.FieldsFunc(input, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t'
	})
	selected := make([]int, 0, len(parts))
	seen := make(map[int]bool, len(parts))
	for _, part := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || n < 1 || n > max {
			return nil, fmt.Errorf("invalid selection")
		}
		idx := n - 1
		if seen[idx] {
			continue
		}
		seen[idx] = true
		selected = append(selected, idx)
	}
	return selected, nil
}

func startCodespace(name string) error {
	fmt.Println("Starting codespace (this may take a moment)...")
	time.Sleep(3 * time.Second)

	for i := 0; i < 30; i++ {
		if exec.Command("gh", "codespace", "ssh", "-c", name, "--", "echo ready").Run() == nil {
			fmt.Println("Codespace is ready!")
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("timed out waiting for codespace SSH")
}

func detectWorkdir(codespaceName, repository string) (string, error) {
	out, err := exec.Command("gh", "codespace", "ssh", "-c", codespaceName,
		"--", "ls -d /workspaces/*/ 2>/dev/null",
	).Output()
	if err != nil {
		return "/workspaces", nil
	}

	raw := strings.TrimSpace(string(out))
	if raw == "" {
		return "/workspaces", nil
	}

	// Parse directory list and strip trailing slashes
	var dirs []string
	for _, line := range strings.Split(raw, "\n") {
		d := strings.TrimRight(strings.TrimSpace(line), "/")
		if d != "" {
			dirs = append(dirs, d)
		}
	}
	if len(dirs) == 0 {
		return "/workspaces", nil
	}

	// Try automatic selection based on repository name
	repoName := repoBaseName(repository)
	chosen := chooseWorkdir(dirs, repoName)
	if chosen != "" {
		return chosen, nil
	}

	// Multiple dirs, no repo match — interactive selection
	return selectWorkdir(dirs)
}

func sshCommand(codespaceName, command string) (string, error) {
	out, err := exec.Command("gh", "codespace", "ssh", "-c", codespaceName,
		"--", command,
	).Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// execSSH runs a command on the codespace using the multiplexed SSH client.
// Falls back to gh codespace ssh if the client has no multiplexing.
func execSSH(sshClient *ssh.Client, codespaceName, command string) (string, error) {
	if sshClient != nil {
		ctx := context.Background()
		stdout, stderr, exitCode, err := sshClient.Exec(ctx, command)
		if err != nil {
			return "", err
		}
		if exitCode != 0 {
			return "", fmt.Errorf("exit %d: %s", exitCode, strings.TrimSpace(stderr))
		}
		return stdout, nil
	}
	return sshCommand(codespaceName, command)
}

func preambleCodespacesFromRegistry(reg *registry.Registry) []PreambleCodespace {
	if reg == nil {
		return nil
	}
	all := reg.All()
	out := make([]PreambleCodespace, 0, len(all))
	for _, cs := range all {
		out = append(out, PreambleCodespace{
			Alias:      cs.Alias,
			Repository: cs.Repository,
			Branch:     cs.Branch,
			Workdir:    cs.Workdir,
		})
	}
	return out
}

// repoBaseName extracts the repository name from an "owner/repo" string.
func repoBaseName(repository string) string {
	if i := strings.LastIndex(repository, "/"); i >= 0 {
		return repository[i+1:]
	}
	return repository
}

// chooseWorkdir picks the best workspace directory from a list given the repo name.
// Returns the best match or "" if interactive selection is needed.
func chooseWorkdir(dirs []string, repoName string) string {
	if len(dirs) == 1 {
		return dirs[0]
	}
	if repoName == "" {
		return ""
	}
	for _, d := range dirs {
		base := filepath.Base(d)
		if base == repoName {
			return d
		}
	}
	return ""
}

// selectWorkdir lets the user pick a workspace directory interactively.
func selectWorkdir(dirs []string) (string, error) {
	if len(dirs) == 0 {
		return "/workspaces", nil
	}

	// Try gum filter for fuzzy interactive picker
	if gumPath, err := exec.LookPath("gum"); err == nil {
		cmd := exec.Command(gumPath, "filter", "--placeholder", "Choose workspace directory...")
		cmd.Stdin = strings.NewReader(strings.Join(dirs, "\n"))
		cmd.Stderr = os.Stderr
		selected, err := cmd.Output()
		if err == nil {
			choice := strings.TrimSpace(string(selected))
			if choice != "" {
				return choice, nil
			}
		}
	}

	// Fallback: numbered list
	fmt.Println("Multiple workspace directories found:")
	for i, d := range dirs {
		fmt.Printf("  %2d) %s\n", i+1, d)
	}
	fmt.Printf("\nSelect [1-%d]: ", len(dirs))
	reader := bufio.NewReader(os.Stdin)
	input, err := reader.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("reading input: %w", err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(input))
	if err != nil || n < 1 || n > len(dirs) {
		return "", fmt.Errorf("invalid selection")
	}
	return dirs[n-1], nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

func execCopilot(extraArgs []string) error {
	copilotArgs := buildCopilotArgs(extraArgs)

	// Try standalone copilot binary first
	if copilotPath, err := exec.LookPath("copilot"); err == nil {
		args := append([]string{"copilot"}, copilotArgs...)
		return syscall.Exec(copilotPath, args, os.Environ())
	}

	// Fall back to gh copilot (gh manages the copilot binary installation)
	ghPath, err := exec.LookPath("gh")
	if err != nil {
		return fmt.Errorf("neither 'copilot' nor 'gh' found in PATH; install copilot CLI or gh CLI")
	}

	// Use "--" so gh doesn't interpret copilot's flags
	args := append([]string{"gh", "copilot", "--"}, copilotArgs...)
	return syscall.Exec(ghPath, args, os.Environ())
}

func buildCopilotArgs(extraArgs []string) []string {
	return append([]string(nil), extraArgs...)
}

// detectRemoteBranch reads the current git branch from the codespace via SSH.
func detectRemoteBranch(sshClient *ssh.Client, codespaceName, workdir string) string {
	cmd := fmt.Sprintf("git -C %s rev-parse --abbrev-ref HEAD 2>/dev/null", shellQuote(workdir))
	out, err := execSSH(sshClient, codespaceName, cmd)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}
