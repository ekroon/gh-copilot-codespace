package provisioner

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// Provisioner defines a setup step that runs on a codespace after connection.
type Provisioner interface {
	Name() string
	ShouldRun(ctx RunContext) bool
	Run(ctx context.Context, target CodespaceTarget) error
}

type successDescriber interface {
	SuccessDescription() string
}

// RunContext provides information for deciding whether a provisioner should run.
type RunContext struct {
	Terminal       string // detected terminal identifier used for matching (e.g., "xterm-ghostty")
	Repository     string // e.g., "github/github"
	IsNewCodespace bool   // true if the codespace was just created
}

// CodespaceTarget is the interface provisioners use to interact with a codespace.
type CodespaceTarget interface {
	CodespaceName() string
	Repository() string
	Workdir() string
	RunSSH(ctx context.Context, command string) (string, error)
	UploadTerminfo(ctx context.Context, term string) error
}

// RunAll executes all provisioners whose ShouldRun returns true.
// Errors are logged to stderr but don't stop other provisioners.
func RunAll(ctx context.Context, provisioners []Provisioner, rctx RunContext, target CodespaceTarget) {
	runAll(ctx, provisioners, rctx, target, os.Stderr, time.Now)
}

func runAll(ctx context.Context, provisioners []Provisioner, rctx RunContext, target CodespaceTarget, out io.Writer, now func() time.Time) {
	if out == nil {
		out = os.Stderr
	}
	if now == nil {
		now = time.Now
	}
	for _, p := range provisioners {
		if !p.ShouldRun(rctx) {
			continue
		}
		started := now()
		if err := p.Run(ctx, target); err != nil {
			fmt.Fprintf(out, "  ⚠ provisioner %s failed after %s: %v\n", p.Name(), now().Sub(started), err)
		} else {
			description := "completed"
			if describer, ok := p.(successDescriber); ok {
				description = describer.SuccessDescription()
			}
			fmt.Fprintf(out, "  ✓ provisioner %s %s in %s\n", p.Name(), description, now().Sub(started))
		}
	}
}

// --- Built-in provisioners ---

// TerminfoProvisioner uploads local terminfo entries that the codespace may not have.
type TerminfoProvisioner struct{}

func (p *TerminfoProvisioner) Name() string { return "terminfo" }

func (p *TerminfoProvisioner) ShouldRun(ctx RunContext) bool {
	return ctx.Terminal != "" && !isStandardTerminal(ctx.Terminal)
}

func (p *TerminfoProvisioner) Run(ctx context.Context, target CodespaceTarget) error {
	term := DetectedTerminal(os.Getenv("TERM"))
	if term == "" || isStandardTerminal(term) {
		return nil
	}
	if err := target.UploadTerminfo(ctx, term); err != nil {
		return fmt.Errorf("%s: %v", term, err)
	}
	return nil
}

// DetectedTerminal normalizes the current local terminal into the identifier
// used for provisioner matching. Ghostty sessions always normalize to
// xterm-ghostty even when the local TERM is overridden.
func DetectedTerminal(term string) string {
	if isGhosttySession() {
		return "xterm-ghostty"
	}
	return term
}

func isGhosttySession() bool {
	return strings.EqualFold(os.Getenv("TERM_PROGRAM"), "ghostty") ||
		os.Getenv("GHOSTTY_RESOURCES_DIR") != "" ||
		os.Getenv("TERM") == "xterm-ghostty"
}

func isStandardTerminal(term string) bool {
	standard := []string{"xterm", "xterm-color", "xterm-256color", "screen", "screen-256color", "tmux", "tmux-256color", "linux", "vt100", "dumb"}
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

func (p *GitFetchProvisioner) SuccessDescription() string {
	if p.Branch == "" {
		return "started in background"
	}
	return "completed"
}

func (p *GitFetchProvisioner) Run(ctx context.Context, target CodespaceTarget) error {
	workdir := shellQuote(target.Workdir())
	if p.Branch == "" {
		cmd := fmt.Sprintf(
			`cd %s && state_dir="${XDG_STATE_HOME:-$HOME/.local/state}/gh-copilot-codespace" && mkdir -p "$state_dir" && nohup sh -c 'if command -v flock >/dev/null 2>&1; then exec 9>"$1/git-fetch.lock"; flock -n 9 || exit 0; fi; exec git fetch origin' sh "$state_dir" >"$state_dir/git-fetch.log" 2>&1 </dev/null &`,
			workdir,
		)
		if _, err := target.RunSSH(ctx, cmd); err != nil {
			return fmt.Errorf("starting git fetch: %w", err)
		}
		return nil
	}

	checkoutCmd := fmt.Sprintf("cd %s && git checkout %s", workdir, shellQuote(p.Branch))
	if _, err := target.RunSSH(ctx, checkoutCmd); err == nil {
		return nil
	}

	checkRemoteCmd := fmt.Sprintf(
		"cd %s && git ls-remote --heads origin %s",
		workdir,
		shellQuote(p.Branch),
	)
	out, err := target.RunSSH(ctx, checkRemoteCmd)
	if err != nil {
		return fmt.Errorf("checking remote branch %s: %w", p.Branch, err)
	}
	if strings.TrimSpace(out) == "" {
		createCmd := fmt.Sprintf("cd %s && git checkout -b %s", workdir, shellQuote(p.Branch))
		if _, err := target.RunSSH(ctx, createCmd); err != nil {
			return fmt.Errorf("creating branch %s: %w", p.Branch, err)
		}
		return nil
	}

	refspec := fmt.Sprintf("refs/heads/%s:refs/remotes/origin/%s", p.Branch, p.Branch)
	fetchCmd := fmt.Sprintf("cd %s && git fetch origin %s", workdir, shellQuote(refspec))
	if _, err := target.RunSSH(ctx, fetchCmd); err != nil {
		return fmt.Errorf("fetching branch %s: %w", p.Branch, err)
	}
	checkoutRemoteCmd := fmt.Sprintf(
		"cd %s && git checkout --track -b %s %s",
		workdir,
		shellQuote(p.Branch),
		shellQuote("origin/"+p.Branch),
	)
	if _, err := target.RunSSH(ctx, checkoutRemoteCmd); err != nil {
		return fmt.Errorf("checking out branch %s: %w", p.Branch, err)
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
