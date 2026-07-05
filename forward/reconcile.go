// Copyright 2026 Tim 'mithro' Ansell
// SPDX-License-Identifier: Apache-2.0

package forward

import (
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const (
	probeTimeout = 2 * time.Second
	// minAge: sshd creates the socket file at bind() and listen()s a
	// moment later; during that window connect() is refused. Collecting a
	// just-born socket would orphan the forward on an invisible inode, so
	// GC only touches files older than this.
	minAge = 5 * time.Second
)

// Result reports what Reconcile found and did.
type Result struct {
	LiveName string // basename of the adopted socket; "" if none live
	Legacy   bool   // a live old-scheme regular clipboard.sock was kept
	Cleaned  int    // dead sockets unlinked
}

// Reconcile scans <sshDir>/clipboard.d/*.sock, garbage-collects dead
// sockets, and picks a live one: the adopt name if given and live, else the
// newest by mtime (= bind time; last writer wins across multiple laptops).
func Reconcile(sshDir, adopt string) (Result, error) {
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

	if res.Cleaned > 0 || res.LiveName != "" {
		Logf(sshDir, "reconcile", "live=%q cleaned=%d adopt=%q", res.LiveName, res.Cleaned, adopt)
	}
	return res, nil
}
