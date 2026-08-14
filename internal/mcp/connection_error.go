package mcp

import (
	"errors"
	"fmt"
	"strings"

	"github.com/ekroon/gh-copilot-codespace/internal/daemonclient"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

// toolErrorMessage renders err for a tool result. Daemon connection losses are
// replaced with concise, model-actionable guidance; every other error keeps its
// own message so existing behaviour is unchanged.
func toolErrorMessage(err error) string {
	if guidance, ok := connectionLostGuidance(err); ok {
		return guidance
	}
	if err == nil {
		return ""
	}
	return err.Error()
}

// toolErrorFor is the error-aware counterpart of toolError.
func toolErrorFor(err error) *mcpsdk.CallToolResult {
	return toolError(toolErrorMessage(err))
}

// toolErrorForf is toolErrorFor with a leading context message describing what
// the handler was doing when err occurred.
func toolErrorForf(err error, format string, args ...any) *mcpsdk.CallToolResult {
	msg := fmt.Sprintf(format, args...)
	if guidance, ok := connectionLostGuidance(err); ok {
		return toolError(msg + "\n" + guidance)
	}
	return toolError(fmt.Sprintf("%s: %v", msg, err))
}

// connectionLostGuidance builds the guidance shown when the daemon connection
// died while an operation was in flight. It reports whether err carries a
// *daemonclient.ConnectionLostError; the executor never replays interrupted
// operations, so the model has to decide what is safe to repeat.
func connectionLostGuidance(err error) (string, bool) {
	var lost *daemonclient.ConnectionLostError
	if !errors.As(err, &lost) {
		return "", false
	}

	var b strings.Builder
	b.WriteString("Remote connection lost: the daemon connection to the codespace dropped")
	if lost.Cause != nil {
		fmt.Fprintf(&b, " (%v)", lost.Cause)
	}
	if lost.Reconnected {
		b.WriteString(" and the codespace was automatically woken when needed before a new daemon connection was established")
		if lost.NewGeneration != 0 {
			fmt.Fprintf(&b, " (generation %d -> %d)", lost.OldGeneration, lost.NewGeneration)
		}
		b.WriteString(".")
	} else if lost.ReconnectErr != nil {
		fmt.Fprintf(&b, " and automatic wake/reconnect failed (%v).", lost.ReconnectErr)
	} else {
		b.WriteString(" and no daemon connection is available.")
	}
	b.WriteString(" This call was not retried automatically.\nNext steps:")

	if lost.OutcomeUnknown {
		b.WriteString("\n- The outcome of this call is unknown: it may have run on the codespace." +
			" Repeat read-only calls (remote_view, remote_grep, remote_glob) freely," +
			" but inspect remote state before repeating an edit, create, patch, or remote_bash command.")
	} else {
		b.WriteString("\n- This call never reached the codespace, so repeating it as-is is safe.")
	}

	b.WriteString("\n- After abrupt connection or daemon loss, prior process-session outcomes are unknown." +
		" Inspect remote state before acting: a missing shellId is not evidence that the process stopped or the command did not run," +
		" and neither is absence from remote_list_bash. Do not rerun merely because the shellId is missing." +
		" Cleanup of stale daemon-owned process cgroups is best-effort where cgroup delegation is supported;" +
		" this guidance does not confirm cleanup succeeded.")

	if lost.Reconnected {
		b.WriteString("\n- Remote tools work again; continue on the new connection.")
	} else {
		b.WriteString("\n- Remote tools stay unavailable until the connection is restored." +
			" Retrying the call shortly triggers another reconnect attempt;" +
			" if it keeps failing, check the codespace with list_codespaces before continuing.")
	}

	return b.String(), true
}
