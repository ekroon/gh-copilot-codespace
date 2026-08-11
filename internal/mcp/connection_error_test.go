package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ekroon/gh-copilot-codespace/internal/daemonclient"
)

func TestConnectionLostGuidanceReconnected(t *testing.T) {
	lost := &daemonclient.ConnectionLostError{
		Cause:          errors.New("daemon exited"),
		Reconnected:    true,
		OutcomeUnknown: true,
		OldGeneration:  1,
		NewGeneration:  2,
	}

	guidance, ok := connectionLostGuidance(lost)
	if !ok {
		t.Fatal("connectionLostGuidance() ok = false, want true")
	}
	for _, want := range []string{
		"daemon exited",
		"a new daemon connection was established (generation 1 -> 2)",
		"not retried automatically",
		"outcome of this call is unknown",
		"inspect remote state before repeating an edit",
		"prior process-session outcomes are unknown",
		"missing shellId is not evidence",
		"Cleanup of stale daemon-owned process cgroups is best-effort",
		"remote_list_bash",
		"Remote tools work again",
	} {
		if !strings.Contains(guidance, want) {
			t.Errorf("guidance missing %q:\n%s", want, guidance)
		}
	}
	if strings.Contains(guidance, "stay unavailable") {
		t.Errorf("reconnected guidance must not claim tools are unavailable:\n%s", guidance)
	}
	for _, forbidden := range []string{
		"process sessions from before the reconnect are gone",
		"rerun the command if it is missing",
	} {
		if strings.Contains(guidance, forbidden) {
			t.Errorf("reconnected guidance recommends blind rerun via %q:\n%s", forbidden, guidance)
		}
	}
}

func TestConnectionLostGuidanceReconnectedRequestNeverSent(t *testing.T) {
	lost := &daemonclient.ConnectionLostError{
		Cause:         errors.New("broken pipe"),
		Reconnected:   true,
		OldGeneration: 3,
		NewGeneration: 4,
	}

	guidance, ok := connectionLostGuidance(lost)
	if !ok {
		t.Fatal("connectionLostGuidance() ok = false, want true")
	}
	if !strings.Contains(guidance, "never reached the codespace, so repeating it as-is is safe") {
		t.Errorf("guidance missing safe-retry statement:\n%s", guidance)
	}
	if strings.Contains(guidance, "outcome of this call is unknown") {
		t.Errorf("guidance must not claim unknown outcome:\n%s", guidance)
	}
}

func TestConnectionLostGuidanceReconnectFailed(t *testing.T) {
	lost := &daemonclient.ConnectionLostError{
		Cause:          errors.New("ssh closed"),
		ReconnectErr:   errors.New("spawn failed"),
		OutcomeUnknown: true,
		OldGeneration:  7,
	}

	guidance, ok := connectionLostGuidance(lost)
	if !ok {
		t.Fatal("connectionLostGuidance() ok = false, want true")
	}
	for _, want := range []string{
		"reconnecting failed (spawn failed)",
		"not retried automatically",
		"outcome of this call is unknown",
		"Remote tools stay unavailable until the connection is restored",
		"list_codespaces",
	} {
		if !strings.Contains(guidance, want) {
			t.Errorf("guidance missing %q:\n%s", want, guidance)
		}
	}
	if strings.Contains(guidance, "generation") {
		t.Errorf("failed reconnect must not report a new generation:\n%s", guidance)
	}
}

func TestConnectionLostGuidanceIgnoresOtherErrors(t *testing.T) {
	if guidance, ok := connectionLostGuidance(errors.New("boom")); ok || guidance != "" {
		t.Fatalf("connectionLostGuidance(plain) = (%q, %v), want (\"\", false)", guidance, ok)
	}
	if guidance, ok := connectionLostGuidance(nil); ok || guidance != "" {
		t.Fatalf("connectionLostGuidance(nil) = (%q, %v), want (\"\", false)", guidance, ok)
	}
}

func TestToolErrorMessageLeavesNormalErrorsUnchanged(t *testing.T) {
	err := fmt.Errorf("file not found: %w", errors.New("no such file"))
	if got := toolErrorMessage(err); got != err.Error() {
		t.Fatalf("toolErrorMessage() = %q, want %q", got, err.Error())
	}
	if got := resultText(toolErrorFor(err)); got != err.Error() {
		t.Fatalf("toolErrorFor() text = %q, want %q", got, err.Error())
	}
	if got := resultText(toolErrorForf(err, "copy to codespace")); got != "copy to codespace: "+err.Error() {
		t.Fatalf("toolErrorForf() text = %q", got)
	}
}

func TestToolErrorForfPrefixesConnectionGuidance(t *testing.T) {
	lost := &daemonclient.ConnectionLostError{Cause: errors.New("eof"), Reconnected: true, OldGeneration: 1, NewGeneration: 2}

	result := toolErrorForf(fmt.Errorf("wrapped: %w", lost), "failed to change directory")
	if !result.IsError {
		t.Fatal("toolErrorForf() IsError = false, want true")
	}
	text := resultText(result)
	if !strings.HasPrefix(text, "failed to change directory\n") {
		t.Fatalf("missing context prefix:\n%s", text)
	}
	if !strings.Contains(text, "Remote connection lost") {
		t.Fatalf("missing connection guidance:\n%s", text)
	}
}

func TestHandlersReportConnectionLossGuidance(t *testing.T) {
	reconnected := &daemonclient.ConnectionLostError{
		Cause:          errors.New("daemon exited"),
		Reconnected:    true,
		OutcomeUnknown: true,
		OldGeneration:  1,
		NewGeneration:  2,
	}
	failed := &daemonclient.ConnectionLostError{
		Cause:          errors.New("daemon exited"),
		ReconnectErr:   errors.New("codespace unavailable"),
		OutcomeUnknown: true,
		OldGeneration:  1,
	}

	tests := []struct {
		name     string
		err      error
		wantText []string
	}{
		{
			name:     "reconnect succeeded",
			err:      reconnected,
			wantText: []string{"Remote connection lost", "a new daemon connection was established", "Remote tools work again"},
		},
		{
			name:     "reconnect failed",
			err:      fmt.Errorf("edit file: %w", failed),
			wantText: []string{"Remote connection lost", "reconnecting failed (codespace unavailable)", "Remote tools stay unavailable"},
		},
		{
			name:     "normal error unchanged",
			err:      errors.New("old_str not found in file"),
			wantText: []string{"old_str not found in file"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockExecutor{editFileErr: tt.err}
			result, err := editHandler(testReg(mock))(context.Background(), makeReq(map[string]any{
				"path":    "main.go",
				"old_str": "a",
				"new_str": "b",
			}))
			if err != nil {
				t.Fatalf("editHandler() error = %v", err)
			}
			if !result.IsError {
				t.Fatal("editHandler() IsError = false, want true")
			}
			text := resultText(result)
			for _, want := range tt.wantText {
				if !strings.Contains(text, want) {
					t.Errorf("result missing %q:\n%s", want, text)
				}
			}
			if tt.name == "normal error unchanged" && strings.Contains(text, "Remote connection lost") {
				t.Errorf("normal error gained connection guidance:\n%s", text)
			}
		})
	}
}
