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
