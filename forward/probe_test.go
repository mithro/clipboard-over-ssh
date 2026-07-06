// Copyright 2026 Tim 'mithro' Ansell
// SPDX-License-Identifier: Apache-2.0

package forward

import (
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/mithro/clipboard-over-ssh/protocol"
)

// startLiveServer accepts connections, reads the request line, and answers
// like the real server. Returns the socket path.
func startLiveServer(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "live.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				if _, err := protocol.ReadRequest(c); err != nil {
					return
				}
				protocol.WriteOK(c, []byte("TARGETS\n"))
			}(conn)
		}
	}()
	return path
}

// startUndeadServer accepts connections but never responds (half-open
// forward: sshd accepts, far end is gone).
func startUndeadServer(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "undead.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		var conns []net.Conn // hold connections open, never respond
		for {
			conn, err := l.Accept()
			if err != nil {
				for _, c := range conns {
					c.Close()
				}
				return
			}
			conns = append(conns, conn)
		}
	}()
	return path
}

// makeStaleSocket leaves a socket file on disk with no listener behind it
// (what sshd leaves after a session dies): bind, then close WITHOUT unlink.
func makeStaleSocket(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stale.sock")
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	l.SetUnlinkOnClose(false)
	l.Close()
	return path
}

func TestProbeLive(t *testing.T) {
	if got := Probe(startLiveServer(t), 2*time.Second); got != Live {
		t.Errorf("Probe(live) = %v, want Live", got)
	}
}

func TestProbeUndead(t *testing.T) {
	start := time.Now()
	if got := Probe(startUndeadServer(t), 500*time.Millisecond); got != Undead {
		t.Errorf("Probe(undead) = %v, want Undead", got)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Probe(undead) took %v, must respect the timeout", elapsed)
	}
}

func TestProbeStaleIsDead(t *testing.T) {
	if got := Probe(makeStaleSocket(t), 2*time.Second); got != Dead {
		t.Errorf("Probe(stale) = %v, want Dead", got)
	}
}

func TestProbeMissingIsDead(t *testing.T) {
	if got := Probe(filepath.Join(t.TempDir(), "nope.sock"), 2*time.Second); got != Dead {
		t.Errorf("Probe(missing) = %v, want Dead", got)
	}
}

// TestStateZeroValueIsNotLive locks in the fail-closed property: an
// accidentally-unset State (e.g. a zero-valued struct field never assigned
// due to a bug) must not read as Live, since Live is treated as "safe to
// adopt" by callers whose other outcome is unlinking files.
func TestStateZeroValueIsNotLive(t *testing.T) {
	var zero State
	if zero == Live {
		t.Errorf("zero value of State must not equal Live (fail-open risk); got %v", zero)
	}
}
