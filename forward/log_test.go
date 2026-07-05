// Copyright 2026 Tim 'mithro' Ansell
// SPDX-License-Identifier: Apache-2.0

package forward

import (
	"os"
	"path/filepath"
	"strings"
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
