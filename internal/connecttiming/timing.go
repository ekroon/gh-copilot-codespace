package connecttiming

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

const (
	StageRepositoryLookup = "repository lookup"
	StageSSHMultiplexing  = "ssh multiplexing"
	StageHelperDeployment = "helper deployment"
	StageWorkdirDetection = "workdir detection"
	StageBranchDetection  = "branch detection"
	StageExecutorSetup    = "executor setup"
	StageProvisioning     = "provisioning"
)

// Clock provides deterministic time for timing output.
type Clock interface {
	Now() time.Time
}

// ClockFunc adapts a function into a Clock.
type ClockFunc func() time.Time

// Now returns the current time.
func (f ClockFunc) Now() time.Time { return f() }

// Session emits step timings when enabled.
type Session struct {
	label   string
	enabled bool
	out     io.Writer
	clock   Clock
	started time.Time
}

// New constructs a timing session.
func New(label string, enabled bool, out io.Writer, clock Clock) *Session {
	if out == nil {
		out = os.Stderr
	}
	if clock == nil {
		clock = ClockFunc(time.Now)
	}
	return &Session{
		label:   label,
		enabled: enabled,
		out:     out,
		clock:   clock,
		started: clock.Now(),
	}
}

// NewFromEnv enables tracing when COPILOT_CODESPACE_TIMINGS is set.
func NewFromEnv(label string, out io.Writer, clock Clock) *Session {
	return New(label, EnabledFromEnv(), out, clock)
}

// EnabledFromEnv reports whether timing output is enabled.
func EnabledFromEnv() bool {
	value := strings.TrimSpace(os.Getenv("COPILOT_CODESPACE_TIMINGS"))
	if value == "" {
		return false
	}
	switch strings.ToLower(value) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

// Step runs fn and emits a duration when enabled.
func (s *Session) Step(label string, fn func() error) error {
	start := s.clock.Now()
	err := fn()
	if !s.enabled {
		return err
	}
	duration := s.clock.Now().Sub(start)
	if err != nil {
		fmt.Fprintf(s.out, "  ⚠ %s failed after %s: %v\n", label, duration, err)
		return err
	}
	fmt.Fprintf(s.out, "  ✓ %s completed in %s\n", label, duration)
	return nil
}

// Skip emits a skipped stage when enabled.
func (s *Session) Skip(label string) {
	if !s.enabled {
		return
	}
	fmt.Fprintf(s.out, "  · %s skipped\n", label)
}

// Finish emits the overall duration when enabled.
func (s *Session) Finish() {
	if !s.enabled {
		return
	}
	fmt.Fprintf(s.out, "  • %s total: %s\n", s.label, s.clock.Now().Sub(s.started))
}
