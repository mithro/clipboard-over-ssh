// Copyright 2026 Tim 'mithro' Ansell
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mithro/clipboard-over-ssh/protocol"
)

func mkSSHDir(t *testing.T) string {
	t.Helper()
	sshDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(sshDir, "clipboard.d"), 0700); err != nil {
		t.Fatal(err)
	}
	return sshDir
}

func liveSocketAt(t *testing.T, path string) {
	t.Helper()
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
}

func TestReconcileCmdLive(t *testing.T) {
	sshDir := mkSSHDir(t)
	liveSocketAt(t, filepath.Join(sshDir, "clipboard.d", "x1c.aaa111.sock"))

	var out bytes.Buffer
	code := runReconcile(sshDir, []string{}, &out)
	if code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
	if out.String() != "live x1c.aaa111.sock\n" {
		t.Errorf("output = %q", out.String())
	}
}

func TestReconcileCmdNone(t *testing.T) {
	var out bytes.Buffer
	code := runReconcile(mkSSHDir(t), []string{}, &out)
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if out.String() != "none\n" {
		t.Errorf("output = %q", out.String())
	}
}

func TestReconcileCmdAdoptFlag(t *testing.T) {
	sshDir := mkSSHDir(t)
	liveSocketAt(t, filepath.Join(sshDir, "clipboard.d", "x1c.aaa111.sock"))
	liveSocketAt(t, filepath.Join(sshDir, "clipboard.d", "x1c.bbb222.sock"))

	var out bytes.Buffer
	code := runReconcile(sshDir, []string{"--adopt", "x1c.bbb222.sock"}, &out)
	if code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
	if out.String() != "live x1c.bbb222.sock\n" {
		t.Errorf("output = %q", out.String())
	}
}

// undeadSocketAt starts a listener that accepts connections but never
// answers them (half-open forward), mirroring forward.Probe's Undead case.
// Probing it blocks for forward's own probeTimeout (2s).
func undeadSocketAt(t *testing.T, path string) {
	t.Helper()
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		var conns []net.Conn
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
}

func TestStatusNoSSHConnectionIsSilent(t *testing.T) {
	t.Setenv("SSH_CONNECTION", "")
	var out bytes.Buffer
	if code := runStatus(mkSSHDir(t), &out); code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
	if out.Len() != 0 {
		t.Errorf("output = %q, want silence on non-ssh logins", out.String())
	}
}

func TestStatusReportsOKWithOrigin(t *testing.T) {
	t.Setenv("SSH_CONNECTION", "2404:e80::51 46070 2404:e80::1 22")
	sshDir := mkSSHDir(t)
	liveSocketAt(t, filepath.Join(sshDir, "clipboard.d", "x1c-work.abc123.sock"))

	var out bytes.Buffer
	if code := runStatus(sshDir, &out); code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
	if out.String() != "clipboard: OK (via x1c-work)\n" {
		t.Errorf("output = %q", out.String())
	}
}

func TestStatusReportsNoForward(t *testing.T) {
	t.Setenv("SSH_CONNECTION", "2404:e80::51 46070 2404:e80::1 22")
	var out bytes.Buffer
	start := time.Now()
	code := runStatus(mkSSHDir(t), &out)
	elapsed := time.Since(start)
	if code != 0 {
		t.Errorf("exit = %d, want 0 (status never fails the login shell)", code)
	}
	if out.String() != "clipboard: no live forward\n" {
		t.Errorf("output = %q", out.String())
	}
	// The retry loop is bounded by wall clock (statusRetryBudget), not an
	// attempt count; give it generous headroom over the budget but catch a
	// regression to unbounded/attempt-count-only retrying.
	if elapsed > 2*time.Second {
		t.Errorf("runStatus took %v, want well under statusRetryBudget-based bound", elapsed)
	}
}

// TestStatusStopsEarlyOnSlowReconcile covers the fleet-wide login-stall fix:
// an undead socket makes a single Reconcile call block for forward's own
// probe timeout (2s), far longer than statusRetryDelay. The retry loop must
// treat that as "the race is already over" and stop after one attempt,
// rather than compounding several such slow probes on top of each other
// (the old attempt-counted loop could take 5 x 2s+ = ~11s in this scenario).
func TestStatusStopsEarlyOnSlowReconcile(t *testing.T) {
	t.Setenv("SSH_CONNECTION", "2404:e80::51 46070 2404:e80::1 22")
	sshDir := mkSSHDir(t)
	undeadSocketAt(t, filepath.Join(sshDir, "clipboard.d", "x1c.dead111.sock"))

	var out bytes.Buffer
	start := time.Now()
	code := runStatus(sshDir, &out)
	elapsed := time.Since(start)

	if code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
	if out.String() != "clipboard: no live forward\n" {
		t.Errorf("output = %q", out.String())
	}
	if elapsed > 3*time.Second {
		t.Errorf("runStatus took %v, want ~one slow reconcile call (<3s), not several stacked", elapsed)
	}
}
