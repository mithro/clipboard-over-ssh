// Copyright 2026 Tim 'mithro' Ansell
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRequestAgainstLiveServer(t *testing.T) {
	sshDir := mkSSHDir(t)
	sock := filepath.Join(sshDir, "clipboard.sock")
	liveSocketAt(t, sock)

	resp, err := request(sock, "TARGETS")
	if err != nil {
		t.Fatal(err)
	}
	if !resp.OK || string(resp.Data) != "TARGETS\n" {
		t.Errorf("resp = %+v", resp)
	}
}

func TestRequestTimesOutOnUndeadServer(t *testing.T) {
	sshDir := mkSSHDir(t)
	sock := filepath.Join(sshDir, "clipboard.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() { // accept and hold: never respond
		for {
			if _, err := l.Accept(); err != nil {
				return
			}
		}
	}()

	if testing.Short() {
		t.Skip("waits out the 5s request deadline")
	}
	start := time.Now()
	_, err = request(sock, "TARGETS")
	if err == nil {
		t.Fatal("want deadline error against undead server")
	}
	if elapsed := time.Since(start); elapsed > 8*time.Second {
		t.Errorf("request took %v, deadline is 5s", elapsed)
	}
}

func TestClientHealsViaSymlinkHop(t *testing.T) {
	// clipboard.sock symlink points at a vanished socket; a live one sits
	// in clipboard.d. The shim's heal (reconcile + retry) must recover
	// without any ssh.
	sshDir := mkSSHDir(t)
	liveSocketAt(t, filepath.Join(sshDir, "clipboard.d", "x1c.aaa111.sock"))
	if err := os.Symlink(filepath.Join("clipboard.d", "x1c.gone00.sock"),
		filepath.Join(sshDir, "clipboard.sock")); err != nil {
		t.Fatal(err)
	}

	resp, err := requestWithHeal(sshDir, filepath.Join(sshDir, "clipboard.sock"), "TARGETS", true)
	if err != nil {
		t.Fatalf("heal did not recover: %v", err)
	}
	if !resp.OK {
		t.Errorf("resp = %+v", resp)
	}
}

func TestClientNoHealForCustomSocketPath(t *testing.T) {
	sshDir := mkSSHDir(t)
	liveSocketAt(t, filepath.Join(sshDir, "clipboard.d", "x1c.aaa111.sock"))
	bogus := filepath.Join(t.TempDir(), "custom.sock")

	if _, err := requestWithHeal(sshDir, bogus, "TARGETS", false); err == nil {
		t.Fatal("custom CLIPBOARD_SOCK must not be healed onto the default dir")
	}
}

// TestClientCustomSocketWorksWithoutHome pins down the actual regression:
// a custom $CLIPBOARD_SOCK must work even when $HOME can't be resolved
// (cron, stripped env). RunClient itself isn't a good unit to drive here —
// it reads os.Getenv/argv internally and, on success, writes the response
// to os.Stdout and returns a process-style exit code, none of which is
// pleasant to assert against from a test. The behavior we actually need to
// prove lives one layer down: (1) request() never touches a home directory
// at all, and (2) requestWithHeal(usingDefault=false) reaches the socket
// and returns successfully without ever calling defaultSSHDir() — passing
// it a garbage sshDir ("/nonexistent") demonstrates that, since if the code
// used sshDir on this path it would fail against a directory that doesn't
// exist. Together these two calls cover every line RunClient would run on
// the custom-socket path, so this is the strongest test that doesn't
// require re-plumbing RunClient's stdout/stderr/env for testability.
func TestClientCustomSocketWorksWithoutHome(t *testing.T) {
	t.Setenv("HOME", "")

	sockDir := t.TempDir()
	sock := filepath.Join(sockDir, "custom.sock")
	liveSocketAt(t, sock)
	t.Setenv("CLIPBOARD_SOCK", sock)

	resp, err := request(sock, "TARGETS")
	if err != nil {
		t.Fatalf("request against live custom socket failed with no $HOME: %v", err)
	}
	if !resp.OK || string(resp.Data) != "TARGETS\n" {
		t.Errorf("resp = %+v", resp)
	}

	resp, err = requestWithHeal("/nonexistent", sock, "TARGETS", false)
	if err != nil {
		t.Fatalf("requestWithHeal(usingDefault=false) must not depend on sshDir: %v", err)
	}
	if !resp.OK {
		t.Errorf("resp = %+v", resp)
	}
}
