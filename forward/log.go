// Copyright 2026 Tim 'mithro' Ansell
// SPDX-License-Identifier: Apache-2.0

package forward

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const logMaxBytes = 1 << 20 // 1 MB: rotate, never truncate — an incident's
// evidence is generated exactly when the log is busiest.

// Logf appends one timestamped line to <sshDir>/clipboard-over-ssh.log.
// Best-effort: logging must never break a paste or a login shell, so all
// errors are swallowed. The single Write on an O_APPEND fd keeps lines from
// concurrent writers (shims, reconcile, ensure-forward) intact.
func Logf(sshDir, role, format string, args ...any) {
	path := filepath.Join(sshDir, "clipboard-over-ssh.log")

	if info, err := os.Stat(path); err == nil && info.Size() > logMaxBytes {
		// The rename itself is atomic, but the preceding Stat is not: two
		// concurrent writers can both see an oversized file and both decide
		// to rotate. rename(2) atomically REPLACES its destination, so the
		// loser's rename would move the winner's fresh log onto .log.old,
		// destroying the just-rotated incident evidence. Guard rotation
		// (never the write) with a non-blocking flock and re-check size
		// under the lock; losing the lock just skips rotation this time.
		if lf, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600); err == nil {
			if syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
				// Re-check under the lock: a concurrent writer may have
				// rotated while we raced to acquire; without this re-stat the
				// loser's rename would move the winner's fresh log onto
				// .log.old, destroying the evidence rotation exists to keep.
				if info2, err := os.Stat(path); err == nil && info2.Size() > logMaxBytes {
					os.Rename(path, path+".old")
				}
				syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
			}
			lf.Close()
		}
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	defer f.Close()

	line := fmt.Sprintf("%s pid=%d %s: %s\n",
		time.Now().Format("2006-01-02T15:04:05"), os.Getpid(), role,
		fmt.Sprintf(format, args...))
	f.WriteString(line)
}
