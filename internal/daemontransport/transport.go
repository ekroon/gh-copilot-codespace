// Package daemontransport provides byte-stream transports for daemonproto.
package daemontransport

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"sync"
	"time"
)

// ErrNotImplemented is returned by transport stubs that do not yet have an
// executable implementation.
var ErrNotImplemented = errors.New("daemontransport: not implemented")

// Transport opens a full-duplex byte stream to a daemon process running inside
// a sandbox (codespace over SSH, devcontainer over docker exec, WSL distro over
// `wsl`, etc.). Implementations own the lifecycle of getting the binary in
// place (Deploy) and starting the long-lived daemon process (Spawn). The
// DaemonExecutor speaks daemonproto over the io.ReadWriteCloser returned by
// Spawn.
type Transport interface {
	// Name returns a short diagnostic identifier for the transport, such as
	// "ssh", "local", "devcontainer", or "wsl".
	Name() string

	// Deploy makes sure the daemon binary is present at the canonical remote
	// path. Returns the absolute remote path the daemon should be launched from.
	// May be a no-op for transports where the binary is provided by the sandbox
	// image. Called at most once per Transport instance.
	Deploy(ctx context.Context) (remotePath string, err error)

	// Spawn starts the daemon subcommand at remotePath and returns the
	// bidirectional stream over which daemonproto frames are exchanged. The
	// ReadWriteCloser is owned by the caller; closing it terminates the remote
	// daemon process. Multiple Spawn calls on the same Transport are allowed
	// (e.g., to reconnect after a stream death), but each call produces an
	// independent stream and process.
	Spawn(ctx context.Context, remotePath string) (io.ReadWriteCloser, error)

	// Close releases any resources held by the transport itself (control sockets,
	// helper processes, etc.). Does NOT terminate already-spawned streams — those
	// are owned by their callers.
	Close() error
}

type processStream struct {
	stdout io.ReadCloser
	stdin  io.WriteCloser
	cmd    *exec.Cmd

	writeMu   sync.Mutex
	closeOnce sync.Once
	done      chan struct{}
	waitErr   error
	closeErr  error
}

func newProcessStream(cmd *exec.Cmd, stdin io.WriteCloser, stdout io.ReadCloser) *processStream {
	s := &processStream{
		stdout: stdout,
		stdin:  stdin,
		cmd:    cmd,
		done:   make(chan struct{}),
	}
	go func() {
		s.waitErr = cmd.Wait()
		close(s.done)
	}()
	return s
}

func (s *processStream) Read(p []byte) (int, error) {
	return s.stdout.Read(p)
}

func (s *processStream) Write(p []byte) (int, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.stdin.Write(p)
}

func (s *processStream) Close() error {
	s.closeOnce.Do(func() {
		s.closeErr = s.stdin.Close()
		select {
		case <-s.done:
		case <-time.After(2 * time.Second):
			if s.cmd.Process != nil {
				_ = s.cmd.Process.Kill()
			}
			<-s.done
		}
	})
	return s.closeErr
}
