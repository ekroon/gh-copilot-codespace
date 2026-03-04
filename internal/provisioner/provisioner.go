package provisioner

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// Provisioner defines a setup step that runs on a codespace after connection.
type Provisioner interface {
	Name() string
	ShouldRun(ctx RunContext) bool
	Run(ctx context.Context, target CodespaceTarget) error
}

// RunContext provides information for deciding whether a provisioner should run.
type RunContext struct {
	Terminal       string // e.g., "xterm-ghostty"
	Repository     string // e.g., "github/github"
	IsNewCodespace bool   // true if the codespace was just created
}

// CodespaceTarget is the interface provisioners use to interact with a codespace.
type CodespaceTarget interface {
	CodespaceName() string
	Repository() string
	Workdir() string
	RunSSH(ctx context.Context, command string) (string, error)
}

// RunAll executes all provisioners whose ShouldRun returns true.
// Errors are logged to stderr but don't stop other provisioners.
func RunAll(ctx context.Context, provisioners []Provisioner, rctx RunContext, target CodespaceTarget) {
	for _, p := range provisioners {
		if !p.ShouldRun(rctx) {
			continue
		}
		if err := p.Run(ctx, target); err != nil {
			fmt.Fprintf(os.Stderr, "  ⚠ provisioner %s failed: %v\n", p.Name(), err)
		} else {
			fmt.Fprintf(os.Stderr, "  ✓ provisioner %s completed\n", p.Name())
		}
	}
}

// --- Built-in provisioners ---

// TerminfoProvisioner uploads terminal info to the codespace for non-standard terminals.
type TerminfoProvisioner struct{}

func (p *TerminfoProvisioner) Name() string { return "terminfo" }

func (p *TerminfoProvisioner) ShouldRun(ctx RunContext) bool {
	term := ctx.Terminal
	// Only run for non-standard terminals that codespaces likely don't have
	return term != "" && !isStandardTerminal(term)
}

func (p *TerminfoProvisioner) Run(ctx context.Context, target CodespaceTarget) error {
	term := os.Getenv("TERM")
	if term == "" {
		return nil
	}
	// infocmp outputs the terminfo, tic compiles it on the remote
	// This is piped: local infocmp → ssh → remote tic
	_, err := target.RunSSH(ctx, fmt.Sprintf("infocmp -x %s 2>/dev/null | tic -x - 2>/dev/null", term))
	return err
}

func isStandardTerminal(term string) bool {
	standard := []string{"xterm", "xterm-256color", "screen", "screen-256color", "tmux", "tmux-256color", "linux", "vt100", "dumb"}
	for _, s := range standard {
		if term == s {
			return true
		}
	}
	return false
}

// GitFetchProvisioner runs git fetch on the codespace.
type GitFetchProvisioner struct {
	Branch string // optional branch to checkout
}

func (p *GitFetchProvisioner) Name() string { return "git-fetch" }

func (p *GitFetchProvisioner) ShouldRun(_ RunContext) bool { return true }

func (p *GitFetchProvisioner) Run(ctx context.Context, target CodespaceTarget) error {
	cmd := fmt.Sprintf("cd %s && git fetch origin", shellQuote(target.Workdir()))
	if _, err := target.RunSSH(ctx, cmd); err != nil {
		return fmt.Errorf("git fetch: %w", err)
	}
	if p.Branch != "" {
		// Check if branch exists remotely
		checkCmd := fmt.Sprintf("cd %s && git ls-remote --heads origin %s",
			shellQuote(target.Workdir()), shellQuote(p.Branch))
		out, _ := target.RunSSH(ctx, checkCmd)
		if strings.TrimSpace(out) != "" {
			checkoutCmd := fmt.Sprintf("cd %s && git checkout %s",
				shellQuote(target.Workdir()), shellQuote(p.Branch))
			target.RunSSH(ctx, checkoutCmd)
		} else {
			createCmd := fmt.Sprintf("cd %s && git checkout -b %s",
				shellQuote(target.Workdir()), shellQuote(p.Branch))
			target.RunSSH(ctx, createCmd)
		}
	}
	return nil
}

// WaitForConfigProvisioner waits for devcontainer configuration to complete.
type WaitForConfigProvisioner struct {
	MaxAttempts int
	IntervalSec int
}

func (p *WaitForConfigProvisioner) Name() string { return "wait-for-config" }

func (p *WaitForConfigProvisioner) ShouldRun(ctx RunContext) bool {
	return ctx.IsNewCodespace
}

func (p *WaitForConfigProvisioner) Run(ctx context.Context, target CodespaceTarget) error {
	// This provisioner checks gh cs logs for "Finished configuring codespace."
	// Implementation deferred to Phase 6.3 wiring (needs gh CLI, not just SSH)
	return nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}
