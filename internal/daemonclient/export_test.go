package daemonclient

import (
	"testing"
	"time"
)

// DaemonBinaryForTests returns the daemon binary built by TestMain so the
// external daemonclient_test package can reuse the same build.
func DaemonBinaryForTests() string { return daemonBinary }

// TempDirForTests creates a working directory below the package directory and
// removes it when the test ends.
func TempDirForTests(t *testing.T) string { return testDir(t) }

// WaitConnectionDeadForTests blocks until the executor's current connection
// generation is terminal, so integration tests can act only after the client
// has observed the daemon's death.
func WaitConnectionDeadForTests(t *testing.T, e *Executor, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if conn := e.current(); conn != nil && conn.dead() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for the daemon connection to be observed as dead", timeout)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
