package connecttiming

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"
)

type sequenceClock struct {
	times []time.Time
	i     int
}

func (c *sequenceClock) Now() time.Time {
	if c.i >= len(c.times) {
		return c.times[len(c.times)-1]
	}
	t := c.times[c.i]
	c.i++
	return t
}

func TestSessionDisabledIsSilent(t *testing.T) {
	clock := &sequenceClock{times: []time.Time{time.Unix(0, 0)}}
	var out bytes.Buffer
	sess := New("existing codespace connection", false, &out, clock)

	if err := sess.Step(StageSSHMultiplexing, func() error { return nil }); err != nil {
		t.Fatalf("Step() error = %v", err)
	}
	sess.Skip(StageProvisioning)
	sess.Finish()

	if out.Len() != 0 {
		t.Fatalf("output = %q, want empty", out.String())
	}
}

func TestSessionEnabledFormatsSteps(t *testing.T) {
	clock := &sequenceClock{times: []time.Time{
		time.Unix(0, 0),
		time.Unix(1, 0),
		time.Unix(4, 0),
		time.Unix(10, 0),
	}}
	var out bytes.Buffer
	sess := New("existing codespace connection", true, &out, clock)

	if err := sess.Step(StageSSHMultiplexing, func() error { return nil }); err != nil {
		t.Fatalf("Step() error = %v", err)
	}
	sess.Skip(StageProvisioning)
	sess.Finish()

	got := out.String()
	for _, want := range []string{
		"  ✓ ssh multiplexing completed in 3s\n",
		"  · provisioning skipped\n",
		"  • existing codespace connection total: 10s\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("output %q missing %q", got, want)
		}
	}
}

func TestEnabledFromEnv(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{"", false},
		{"0", false},
		{"false", false},
		{"off", false},
		{"1", true},
		{"true", true},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			t.Setenv("COPILOT_CODESPACE_TIMINGS", tt.value)
			if got := EnabledFromEnv(); got != tt.want {
				t.Fatalf("EnabledFromEnv() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNewFromEnvUsesStderrWhenWriterNil(t *testing.T) {
	t.Setenv("COPILOT_CODESPACE_TIMINGS", "1")
	sess := NewFromEnv("test", nil, &sequenceClock{times: []time.Time{time.Unix(0, 0)}})
	if sess == nil {
		t.Fatal("NewFromEnv returned nil")
	}
	if sess.out != os.Stderr {
		t.Fatalf("writer = %T, want os.Stderr", sess.out)
	}
}
