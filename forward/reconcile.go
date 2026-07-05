// Copyright 2026 Tim 'mithro' Ansell
// SPDX-License-Identifier: Apache-2.0

package forward

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"
)

const (
	probeTimeout = 2 * time.Second
	// minAge: sshd creates the socket file at bind() and listen()s a
	// moment later; during that window connect() is refused. Collecting a
	// just-born socket would orphan the forward on an invisible inode, so
	// GC only touches files older than this.
	minAge      = 5 * time.Second
	lockTimeout = 3 * time.Second
)

// ErrBusy is returned when another reconcile holds the lock past the
// acquire timeout; callers treat it as "someone else is already healing".
var ErrBusy = errors.New("reconcile: lock busy")

// Result reports what Reconcile found and did.
type Result struct {
	LiveName string // basename of the adopted socket; "" if none live
	Legacy   bool   // a live old-scheme regular clipboard.sock was kept
	Cleaned  int    // dead sockets unlinked
}

// Reconcile serializes on <sshDir>/clipboard.d/.lock, then scans, GCs, and
// maintains the clipboard.sock symlink. See reconcileLocked for the logic.
func Reconcile(sshDir, adopt string) (Result, error) {
	return reconcileWithLockTimeout(sshDir, adopt, lockTimeout)
}

func reconcileWithLockTimeout(sshDir, adopt string, timeout time.Duration) (Result, error) {
	// First-run case: on a fresh host nothing has ever created clipboard.d/
	// yet. Reconcile owns this directory, so create it here rather than
	// erroring — every caller (shim heal, status, remote reconcile --adopt)
	// becomes self-bootstrapping.
	if err := os.MkdirAll(filepath.Join(sshDir, "clipboard.d"), 0700); err != nil {
		return Result{}, err
	}

	lockPath := filepath.Join(sshDir, "clipboard.d", ".lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return Result{}, err
	}
	defer f.Close()

	deadline := time.Now().Add(timeout)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			return Result{}, ErrBusy
		}
		time.Sleep(50 * time.Millisecond)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	return reconcileLocked(sshDir, adopt)
}

// reconcileLocked scans <sshDir>/clipboard.d/*.sock, garbage-collects dead
// sockets, and picks a live one: the adopt name if given and live, else the
// newest by mtime (= bind time; last writer wins across multiple laptops).
// Caller must hold the lock.
func reconcileLocked(sshDir, adopt string) (Result, error) {
	dir := filepath.Join(sshDir, "clipboard.d")
	var res Result

	paths, err := filepath.Glob(filepath.Join(dir, "*.sock"))
	if err != nil {
		return res, err
	}

	type sockInfo struct {
		name  string
		mtime time.Time
		state State
	}
	infos := make([]sockInfo, len(paths))

	var wg sync.WaitGroup
	for i, p := range paths {
		info, err := os.Lstat(p)
		if err != nil || info.Mode()&os.ModeSocket == 0 {
			infos[i].state = Undead // not ours to judge; skip and never unlink
			infos[i].name = filepath.Base(p)
			continue
		}
		infos[i] = sockInfo{name: filepath.Base(p), mtime: info.ModTime()}
		wg.Add(1)
		go func(i int, p string) {
			defer wg.Done()
			infos[i].state = Probe(p, probeTimeout)
		}(i, p)
	}
	wg.Wait()

	var live []sockInfo
	for _, si := range infos {
		switch si.state {
		case Live:
			live = append(live, si)
		case Dead:
			if time.Since(si.mtime) > minAge {
				if os.Remove(filepath.Join(dir, si.name)) == nil {
					res.Cleaned++
				}
			}
		}
	}

	sort.Slice(live, func(a, b int) bool { return live[a].mtime.After(live[b].mtime) })

	for _, si := range live {
		if si.name == adopt {
			res.LiveName = si.name
			break
		}
	}
	if res.LiveName == "" && len(live) > 0 {
		res.LiveName = live[0].name
	}

	wellKnown := filepath.Join(sshDir, "clipboard.sock")

	// Legacy migration (mixed-version rollout safety): a regular socket
	// file is the OLD scheme's forward. If it still answers, keep it — a
	// mid-rollout fleet must not lose its working forward. Replace it with
	// the symlink only once it is provably dead. Removal requires an actual
	// dead socket — anything else (a stray non-socket regular file) is not
	// ours to delete, so it is left alone and logged.
	if info, err := os.Lstat(wellKnown); err == nil && info.Mode()&os.ModeSymlink == 0 {
		if info.Mode()&os.ModeSocket != 0 {
			if Probe(wellKnown, probeTimeout) == Live {
				res.Legacy = true
				Logf(sshDir, "reconcile", "live legacy clipboard.sock kept (cleaned=%d)", res.Cleaned)
				return res, nil
			}
			if err := os.Remove(wellKnown); err != nil {
				return res, err
			}
			Logf(sshDir, "reconcile", "removed dead legacy clipboard.sock")
		} else {
			Logf(sshDir, "reconcile", "clipboard.sock is not a socket or symlink; leaving it alone")
			return res, nil
		}
	}

	if res.LiveName != "" {
		if err := flipSymlink(sshDir, res.LiveName); err != nil {
			return res, err
		}
	}

	if res.Cleaned > 0 || res.LiveName != "" {
		Logf(sshDir, "reconcile", "live=%q cleaned=%d adopt=%q", res.LiveName, res.Cleaned, adopt)
	}
	return res, nil
}

// flipSymlink atomically points <sshDir>/clipboard.sock at
// clipboard.d/<name> (relative target: valid from any mount namespace that
// sees the same ~/.ssh). Temp symlink + rename(2) — same pattern zprofile
// uses for the agent symlink.
func flipSymlink(sshDir, name string) error {
	wellKnown := filepath.Join(sshDir, "clipboard.sock")
	target := filepath.Join("clipboard.d", name)
	if cur, err := os.Readlink(wellKnown); err == nil && cur == target {
		return nil
	}
	tmp := fmt.Sprintf("%s.tmp.%d", wellKnown, os.Getpid())
	os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, wellKnown)
}
