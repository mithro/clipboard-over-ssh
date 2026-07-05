// Copyright 2026 Tim 'mithro' Ansell
// SPDX-License-Identifier: Apache-2.0

package forward

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestLogfAppendsOneLine(t *testing.T) {
	dir := t.TempDir()
	Logf(dir, "shim", "no live forward (%d stale cleaned)", 2)
	Logf(dir, "reconcile", "adopted %s", "x1c-work.abc123.sock")

	data, err := os.ReadFile(filepath.Join(dir, "clipboard-over-ssh.log"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %q", len(lines), lines)
	}
	if !strings.Contains(lines[0], "shim: no live forward (2 stale cleaned)") {
		t.Errorf("line 0 = %q", lines[0])
	}
	if !strings.HasPrefix(lines[0], "20") { // ISO timestamp prefix
		t.Errorf("line 0 missing timestamp: %q", lines[0])
	}
}

func TestLogfRotatesAt1MB(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "clipboard-over-ssh.log")
	if err := os.WriteFile(logPath, make([]byte, 1<<20+1), 0600); err != nil {
		t.Fatal(err)
	}
	Logf(dir, "shim", "after rotation")

	old, err := os.Stat(filepath.Join(dir, "clipboard-over-ssh.log.old"))
	if err != nil {
		t.Fatalf("expected .log.old after rotation: %v", err)
	}
	if old.Size() <= 1<<20 {
		t.Errorf(".log.old size = %d, want the full old file", old.Size())
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "after rotation") {
		t.Errorf("new log missing fresh line: %q", data)
	}
}

func TestLogfNeverPanicsOnBadDir(t *testing.T) {
	Logf("/nonexistent/dir", "shim", "best effort only") // must not panic
}

// TestLogfSkipsRotationWhenLockHeld simulates a concurrent rotator holding
// the <log>.lock flock. Logf must not rotate in that case (no .log.old, the
// oversized content stays put) but must still append its line — the
// best-effort contract only ever sacrifices rotation, never the write.
//
// A deterministic interleaved double-rotate (two Logf calls racing through
// the Stat->flock->re-Stat->Rename sequence) isn't practical to force in a
// unit test without injecting hooks into Logf itself. This test instead
// pins down the losing side of that race directly: hold the lock the way a
// winning rotator would, then confirm the loser backs off cleanly. Combined
// with code review of the re-check-under-lock logic in Logf, this covers
// the fix.
func TestLogfSkipsRotationWhenLockHeld(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "clipboard-over-ssh.log")
	oversized := make([]byte, 1<<20+1)
	if err := os.WriteFile(logPath, oversized, 0600); err != nil {
		t.Fatal(err)
	}

	lf, err := os.OpenFile(logPath+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lf.Close()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("failed to acquire test lock: %v", err)
	}
	defer syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)

	Logf(dir, "shim", "should still append")

	if _, err := os.Stat(logPath + ".old"); !os.IsNotExist(err) {
		t.Fatalf(".log.old should not exist while lock is held (err=%v)", err)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), string(oversized)) {
		t.Errorf("log lost its original oversized content when rotation was skipped")
	}
	if !strings.Contains(string(data), "should still append") {
		t.Errorf("log missing appended line when rotation was skipped: len=%d", len(data))
	}
}
