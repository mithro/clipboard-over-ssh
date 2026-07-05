// Copyright 2026 Tim 'mithro' Ansell
// SPDX-License-Identifier: Apache-2.0

package forward

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/mithro/clipboard-over-ssh/protocol"
)

// mkSSHDir returns a fake ~/.ssh containing an empty clipboard.d/.
func mkSSHDir(t *testing.T) string {
	t.Helper()
	sshDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(sshDir, "clipboard.d"), 0700); err != nil {
		t.Fatal(err)
	}
	return sshDir
}

// liveSocketAt starts a live responder bound at exactly path.
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

// staleSocketAt leaves a listener-less socket file at path, backdated so
// it is old enough for GC.
func staleSocketAt(t *testing.T, path string) {
	t.Helper()
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	l.SetUnlinkOnClose(false)
	l.Close()
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}

func TestReconcileEmptyDir(t *testing.T) {
	res, err := Reconcile(mkSSHDir(t), "")
	if err != nil {
		t.Fatal(err)
	}
	if res.LiveName != "" || res.Cleaned != 0 {
		t.Errorf("got %+v, want empty result", res)
	}
}

func TestReconcileCollectsDeadKeepsLive(t *testing.T) {
	sshDir := mkSSHDir(t)
	d := filepath.Join(sshDir, "clipboard.d")
	liveSocketAt(t, filepath.Join(d, "x1c.aaa111.sock"))
	staleSocketAt(t, filepath.Join(d, "x1c.bbb222.sock"))
	staleSocketAt(t, filepath.Join(d, "x1c.ccc333.sock"))

	res, err := Reconcile(sshDir, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.LiveName != "x1c.aaa111.sock" {
		t.Errorf("LiveName = %q, want x1c.aaa111.sock", res.LiveName)
	}
	if res.Cleaned != 2 {
		t.Errorf("Cleaned = %d, want 2", res.Cleaned)
	}
	if _, err := os.Lstat(filepath.Join(d, "x1c.bbb222.sock")); !os.IsNotExist(err) {
		t.Error("dead socket bbb222 not unlinked")
	}
	if _, err := os.Lstat(filepath.Join(d, "x1c.aaa111.sock")); err != nil {
		t.Error("live socket aaa111 must survive GC")
	}
}

func TestReconcileSparesYoungDeadSockets(t *testing.T) {
	// sshd creates the file at bind() and listen()s a moment later; a
	// concurrent reconcile must not collect a just-born socket.
	sshDir := mkSSHDir(t)
	path := filepath.Join(sshDir, "clipboard.d", "x1c.young1.sock")
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	l.SetUnlinkOnClose(false)
	l.Close() // refuses connections now, but mtime is fresh

	res, err := Reconcile(sshDir, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Cleaned != 0 {
		t.Errorf("Cleaned = %d, want 0 (younger than minAge)", res.Cleaned)
	}
	if _, err := os.Lstat(path); err != nil {
		t.Error("young dead socket must not be unlinked")
	}
}

func TestReconcileAdoptPreferred(t *testing.T) {
	sshDir := mkSSHDir(t)
	d := filepath.Join(sshDir, "clipboard.d")
	liveSocketAt(t, filepath.Join(d, "x1c.old111.sock"))
	liveSocketAt(t, filepath.Join(d, "x1c.new222.sock"))
	// Make old111 the newest by mtime — adopt must still win.
	future := time.Now().Add(time.Hour)
	os.Chtimes(filepath.Join(d, "x1c.old111.sock"), future, future)

	res, err := Reconcile(sshDir, "x1c.new222.sock")
	if err != nil {
		t.Fatal(err)
	}
	if res.LiveName != "x1c.new222.sock" {
		t.Errorf("LiveName = %q, want adopted x1c.new222.sock", res.LiveName)
	}
}

func TestReconcileAdoptDeadFallsBackToNewestLive(t *testing.T) {
	sshDir := mkSSHDir(t)
	d := filepath.Join(sshDir, "clipboard.d")
	liveSocketAt(t, filepath.Join(d, "x1c.live11.sock"))
	staleSocketAt(t, filepath.Join(d, "x1c.dead22.sock"))

	res, err := Reconcile(sshDir, "x1c.dead22.sock")
	if err != nil {
		t.Fatal(err)
	}
	if res.LiveName != "x1c.live11.sock" {
		t.Errorf("LiveName = %q, want fallback x1c.live11.sock", res.LiveName)
	}
}

func TestReconcileNeverTouchesNonSockFiles(t *testing.T) {
	sshDir := mkSSHDir(t)
	d := filepath.Join(sshDir, "clipboard.d")
	keep := filepath.Join(d, "README.txt")
	if err := os.WriteFile(keep, []byte("hi"), 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Minute)
	os.Chtimes(keep, old, old)

	if _, err := Reconcile(sshDir, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Error("non-.sock file must never be unlinked")
	}
}

func TestReconcileFlipsSymlink(t *testing.T) {
	sshDir := mkSSHDir(t)
	d := filepath.Join(sshDir, "clipboard.d")
	liveSocketAt(t, filepath.Join(d, "x1c.aaa111.sock"))

	if _, err := Reconcile(sshDir, ""); err != nil {
		t.Fatal(err)
	}
	target, err := os.Readlink(filepath.Join(sshDir, "clipboard.sock"))
	if err != nil {
		t.Fatalf("clipboard.sock is not a symlink: %v", err)
	}
	if target != filepath.Join("clipboard.d", "x1c.aaa111.sock") {
		t.Errorf("symlink -> %q, want clipboard.d/x1c.aaa111.sock", target)
	}
}

func TestReconcileRepointsStaleSymlink(t *testing.T) {
	sshDir := mkSSHDir(t)
	d := filepath.Join(sshDir, "clipboard.d")
	liveSocketAt(t, filepath.Join(d, "x1c.new222.sock"))
	// Symlink points at a socket that no longer exists.
	if err := os.Symlink(filepath.Join("clipboard.d", "x1c.gone00.sock"),
		filepath.Join(sshDir, "clipboard.sock")); err != nil {
		t.Fatal(err)
	}

	if _, err := Reconcile(sshDir, ""); err != nil {
		t.Fatal(err)
	}
	target, _ := os.Readlink(filepath.Join(sshDir, "clipboard.sock"))
	if target != filepath.Join("clipboard.d", "x1c.new222.sock") {
		t.Errorf("symlink -> %q, want clipboard.d/x1c.new222.sock", target)
	}
}

func TestReconcileKeepsLiveLegacySocket(t *testing.T) {
	// Mixed-version rollout (spec review C1): a LIVE regular-file
	// clipboard.sock from the old scheme must be left untouched even when
	// clipboard.d has live sockets.
	sshDir := mkSSHDir(t)
	liveSocketAt(t, filepath.Join(sshDir, "clipboard.sock"))
	liveSocketAt(t, filepath.Join(sshDir, "clipboard.d", "x1c.aaa111.sock"))

	res, err := Reconcile(sshDir, "")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Legacy {
		t.Error("Legacy = false, want true")
	}
	info, err := os.Lstat(filepath.Join(sshDir, "clipboard.sock"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Error("live legacy socket was replaced by a symlink")
	}
}

func TestReconcileMigratesDeadLegacySocket(t *testing.T) {
	sshDir := mkSSHDir(t)
	staleSocketAt(t, filepath.Join(sshDir, "clipboard.sock"))
	liveSocketAt(t, filepath.Join(sshDir, "clipboard.d", "x1c.aaa111.sock"))

	res, err := Reconcile(sshDir, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Legacy {
		t.Error("Legacy = true, want false (dead legacy replaced)")
	}
	target, err := os.Readlink(filepath.Join(sshDir, "clipboard.sock"))
	if err != nil {
		t.Fatalf("dead legacy not replaced by symlink: %v", err)
	}
	if target != filepath.Join("clipboard.d", "x1c.aaa111.sock") {
		t.Errorf("symlink -> %q", target)
	}
}

func TestReconcileLockBusy(t *testing.T) {
	sshDir := mkSSHDir(t)
	lockPath := filepath.Join(sshDir, "clipboard.d", ".lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	start := time.Now()
	_, err = reconcileWithLockTimeout(sshDir, "", 300*time.Millisecond)
	if !errors.Is(err, ErrBusy) {
		t.Errorf("err = %v, want ErrBusy", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Error("lock wait did not respect timeout")
	}
}
