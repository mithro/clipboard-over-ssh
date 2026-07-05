# Self-Healing Clipboard Forwards Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the single shared `clipboard.sock` RemoteForward + bash guard with per-connection unique sockets under `~/.ssh/clipboard.d/`, reconciled onto a `clipboard.sock` symlink, forwarded at runtime via `LocalCommand` → `ssh -O forward`.

**Architecture:** New `forward/` package holds all reconciliation logic (liveness probe, GC, symlink flip, legacy migration, flock, logging). Thin CLI subcommands (`reconcile`, `status`, `ensure-forward`) in `cmd/` wire it up; the existing shim in `cmd/client.go` gains deadlines and a heal-retry path. ssh interactions in `ensure-forward` sit behind a `runner` interface so unit tests never touch the network.

**Tech Stack:** Go 1.22+, stdlib only (no external deps), `CGO_ENABLED=0`, linux-only syscalls OK (`syscall.Flock`) — all deploy targets are Linux.

**Spec:** `docs/superpowers/specs/2026-07-06-self-healing-forward-design.md` (v2). Read it before starting.

## Global Constraints

- Go 1.22+, stdlib ONLY — adding any module dependency is a plan violation.
- Every new `.go` file starts with:
  `// Copyright 2026 Tim 'mithro' Ansell` / `// SPDX-License-Identifier: Apache-2.0`
- All timeouts from the spec verbatim: liveness probe **2 s**, GC minimum age **5 s**, flock acquire **3 s**, shim paste deadline **5 s**, ensure-forward budget **10 s**.
- Only paths inside `clipboard.d/` matching `*.sock` may ever be unlinked by GC.
- `ensure-forward` must NEVER write to stdout (LocalCommand stdout interleaves into `-W`/ProxyJump streams).
- Log lines: single `write(2)` on an `O_APPEND` fd; rotate to `.log.old` at >1 MB (never truncate).
- Run from repo root (the worktree): `go test ./...`, `go vet ./...`, `gofmt -l .` must all be clean before every commit.
- Commit after every task (small, discrete commits; imperative subject; body explains why).

---

### Task 1: `forward` package — liveness probe

**Files:**
- Create: `forward/probe.go`
- Test: `forward/probe_test.go`

**Interfaces:**
- Consumes: `protocol.ReadResponse(io.Reader) (*protocol.Response, error)` (existing).
- Produces: `type State int` with `Live`, `Undead`, `Dead` constants; `func Probe(path string, timeout time.Duration) State`. Task 3's reconcile and Task 5's status build on exactly these.

Liveness model (spec): **Live** = full application round trip (`TARGETS\n` → parseable `OK`/`ERR` response) within timeout. **Dead** = `connect()` refused or path missing. **Undead** = connects but no valid response in time (half-open forward after laptop suspend — sshd accepts even when the tunnel target is gone).

- [ ] **Step 1: Write the failing tests**

Test helpers create three kinds of socket in a `t.TempDir()`:

```go
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./forward/ -v`
Expected: compile error — `Probe`, `Live`, `Undead`, `Dead` undefined.

- [ ] **Step 3: Write the implementation**

```go
// Copyright 2026 Tim 'mithro' Ansell
// SPDX-License-Identifier: Apache-2.0

// Package forward maintains the pool of per-connection forwarded clipboard
// sockets in ~/.ssh/clipboard.d/ and the ~/.ssh/clipboard.sock symlink that
// shims dial through.
package forward

import (
	"io"
	"net"
	"time"

	"github.com/mithro/clipboard-over-ssh/protocol"
)

// State classifies a forwarded socket.
type State int

const (
	// Live: completed a TARGETS round trip — safe to adopt.
	Live State = iota
	// Undead: accepts connections but never answers (half-open forward,
	// e.g. after laptop suspend, until sshd reaps the session). Not
	// adoptable, not collectable.
	Undead
	// Dead: connection refused or path missing — collectable.
	Dead
)

// Probe classifies the socket at path. A bare connect() is NOT liveness:
// sshd accepts on a forwarded socket even when the tunnel's far end is
// gone, so only a full application-level round trip counts as Live.
func Probe(path string, timeout time.Duration) State {
	conn, err := net.DialTimeout("unix", path, timeout)
	if err != nil {
		return Dead
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(timeout))
	if _, err := io.WriteString(conn, "TARGETS\n"); err != nil {
		return Undead
	}
	if uc, ok := conn.(*net.UnixConn); ok {
		uc.CloseWrite()
	}
	if _, err := protocol.ReadResponse(conn); err != nil {
		return Undead
	}
	return Live
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./forward/ -v`
Expected: 4 tests PASS. Also run `go vet ./...` and `gofmt -l .` (empty output).

- [ ] **Step 5: Commit**

```bash
git add forward/probe.go forward/probe_test.go
git commit -m "forward: application-level liveness probe (Live/Undead/Dead)

A bare connect() lies: sshd accepts on a forwarded unix socket even when
the tunnel's far end died (laptop suspend), for hours under default
ClientAliveInterval. Only a TARGETS round trip counts as Live; refused
or missing is Dead (collectable); accepting-but-silent is Undead."
```

---

### Task 2: `forward` package — append-only log with rotation

**Files:**
- Create: `forward/log.go`
- Test: `forward/log_test.go`

**Interfaces:**
- Produces: `func Logf(sshDir, role, format string, args ...any)` — best-effort (never returns an error, never panics); writes one timestamped line to `<sshDir>/clipboard-over-ssh.log`. Tasks 5–7 call it with roles `"reconcile"`, `"status"`, `"ensure-forward"`, `"shim"`.

- [ ] **Step 1: Write the failing tests**

```go
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./forward/ -run TestLogf -v`
Expected: compile error — `Logf` undefined.

- [ ] **Step 3: Write the implementation**

```go
// Copyright 2026 Tim 'mithro' Ansell
// SPDX-License-Identifier: Apache-2.0

package forward

import (
	"fmt"
	"os"
	"path/filepath"
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
		os.Rename(path, path+".old") // atomic; concurrent double-rotate is harmless
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
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./forward/ -v`
Expected: all PASS (including Task 1's). `go vet ./...`, `gofmt -l .` clean.

- [ ] **Step 5: Commit**

```bash
git add forward/log.go forward/log_test.go
git commit -m "forward: best-effort append log with rotation at 1MB

Breakage must never be silent again (2026-07-05 incident), but logging
must never break a paste either: all errors swallowed, single O_APPEND
write per line for concurrent writers, rotate to .log.old instead of
truncating so incident evidence survives."
```

---

### Task 3: `forward` package — reconcile: scan, GC, selection

**Files:**
- Create: `forward/reconcile.go`
- Test: `forward/reconcile_test.go`

**Interfaces:**
- Consumes: `Probe`, `Logf` (Tasks 1–2).
- Produces (used by Tasks 4–7):

```go
type Result struct {
	LiveName string // basename of the adopted socket; "" if none live
	Legacy   bool   // true when a live old-scheme regular clipboard.sock was kept
	Cleaned  int    // dead sockets unlinked
}
func Reconcile(sshDir, adopt string) (Result, error)
```

This task implements scan/GC/selection and returns the choice; symlink flip, legacy migration and flock land in Task 4 (same function, extended). `sshDir` is a parameter (not hardcoded `~/.ssh`) so tests run in temp dirs.

- [ ] **Step 1: Write the failing tests**

```go
// Copyright 2026 Tim 'mithro' Ansell
// SPDX-License-Identifier: Apache-2.0

package forward

import (
	"net"
	"os"
	"path/filepath"
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./forward/ -run TestReconcile -v`
Expected: compile error — `Reconcile`, `Result` undefined.

- [ ] **Step 3: Write the implementation**

```go
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
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./forward/ -v`
Expected: all PASS. `go vet ./...`, `gofmt -l .` clean.

- [ ] **Step 5: Commit**

```bash
git add forward/reconcile.go forward/reconcile_test.go
git commit -m "forward: reconcile scan/GC/selection over clipboard.d

Parallel application-level probes; GC unlinks only *.sock files inside
clipboard.d that are provably dead AND older than 5s (a just-bound sshd
socket briefly refuses before listen(); collecting it would orphan the
forward on an invisible inode). Selection: adopt name if live, else
newest mtime — last writer wins across laptops, matching the old
scheme's rebind semantics."
```

---

### Task 4: `forward` package — symlink flip, legacy migration, flock

**Files:**
- Modify: `forward/reconcile.go`
- Test: `forward/reconcile_test.go` (append tests)

**Interfaces:**
- `Reconcile(sshDir, adopt string) (Result, error)` keeps its signature; it now additionally (a) serializes under `<sshDir>/clipboard.d/.lock` with a 3 s acquire timeout (returns `ErrBusy` on timeout), (b) handles a legacy regular-file `clipboard.sock`, and (c) atomically points the `<sshDir>/clipboard.sock` symlink at the chosen socket.
- Produces: `var ErrBusy = errors.New("reconcile: lock busy")` (Task 5's status treats it as non-fatal).

- [ ] **Step 1: Write the failing tests** (append to `forward/reconcile_test.go`)

```go
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
```

Add imports `"errors"` and `"syscall"` to the test file.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./forward/ -run TestReconcile -v`
Expected: compile error — `ErrBusy`, `reconcileWithLockTimeout` undefined.

- [ ] **Step 3: Extend the implementation**

In `forward/reconcile.go`: rename the existing body to an unexported `reconcileLocked(sshDir, adopt string) (Result, error)` and wrap it:

```go
const lockTimeout = 3 * time.Second

// ErrBusy is returned when another reconcile holds the lock past the
// acquire timeout; callers treat it as "someone else is already healing".
var ErrBusy = errors.New("reconcile: lock busy")

// Reconcile serializes on <sshDir>/clipboard.d/.lock, then scans, GCs, and
// maintains the clipboard.sock symlink. See reconcileLocked for the logic.
func Reconcile(sshDir, adopt string) (Result, error) {
	return reconcileWithLockTimeout(sshDir, adopt, lockTimeout)
}

func reconcileWithLockTimeout(sshDir, adopt string, timeout time.Duration) (Result, error) {
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
```

At the end of `reconcileLocked`, after selection and before the final `Logf`, add legacy handling + symlink flip:

```go
	wellKnown := filepath.Join(sshDir, "clipboard.sock")

	// Legacy migration (mixed-version rollout safety): a regular socket
	// file is the OLD scheme's forward. If it still answers, keep it — a
	// mid-rollout fleet must not lose its working forward. Replace it with
	// the symlink only once it is provably dead.
	if info, err := os.Lstat(wellKnown); err == nil && info.Mode()&os.ModeSymlink == 0 {
		if info.Mode()&os.ModeSocket != 0 && Probe(wellKnown, probeTimeout) == Live {
			res.Legacy = true
			Logf(sshDir, "reconcile", "live legacy clipboard.sock kept (cleaned=%d)", res.Cleaned)
			return res, nil
		}
		if err := os.Remove(wellKnown); err != nil {
			return res, err
		}
	}

	if res.LiveName != "" {
		if err := flipSymlink(sshDir, res.LiveName); err != nil {
			return res, err
		}
	}
```

And the flip helper:

```go
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
```

Add imports `"errors"`, `"fmt"`, `"syscall"`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./forward/ -v`
Expected: all PASS. `go vet ./...`, `gofmt -l .` clean.

- [ ] **Step 5: Commit**

```bash
git add forward/reconcile.go forward/reconcile_test.go
git commit -m "forward: symlink flip, legacy migration, flock serialization

clipboard.sock becomes an atomically-flipped relative symlink into
clipboard.d/. A LIVE regular-file clipboard.sock (old scheme) is kept
untouched so a mixed-version fleet keeps working mid-rollout; it is
replaced only once provably dead. Reconciles serialize on
clipboard.d/.lock with a 3s acquire timeout (ErrBusy)."
```

---

### Task 5: CLI — `reconcile` and `status` subcommands

**Files:**
- Create: `cmd/reconcile.go`
- Test: `cmd/reconcile_test.go`
- Modify: `main.go` (dispatch + usage text)

**Interfaces:**
- Consumes: `forward.Reconcile`, `forward.ErrBusy`, `forward.Logf`.
- Produces:
  - `cmd.RunReconcile(args []string) int` — parses `--adopt <name>`; prints `live <name>\n` (exit 0) or `none\n` (exit 1); `busy\n` (exit 0) on `ErrBusy`.
  - `cmd.RunStatus() int` — always exit 0; silent no-op unless `$SSH_CONNECTION` is set; retries reconcile 4×250 ms on "none" (fresh-login race with the connection's own backgrounded reconcile) before reporting.
  - Both use `defaultSSHDir() (string, error)` = `$HOME/.ssh` (shared helper in `cmd/reconcile.go`, reused by Task 7).

- [ ] **Step 1: Write the failing tests**

Testable cores take `sshDir` and an `io.Writer`; the exported `Run*` wrappers stay thin:

```go
// Copyright 2026 Tim 'mithro' Ansell
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"testing"

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
	if code := runStatus(mkSSHDir(t), &out); code != 0 {
		t.Errorf("exit = %d, want 0 (status never fails the login shell)", code)
	}
	if out.String() != "clipboard: no live forward\n" {
		t.Errorf("output = %q", out.String())
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./cmd/ -run 'TestReconcileCmd|TestStatus' -v`
Expected: compile error — `runReconcile`, `runStatus` undefined.

- [ ] **Step 3: Write the implementation**

```go
// Copyright 2026 Tim 'mithro' Ansell
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mithro/clipboard-over-ssh/forward"
)

// statusRetries × statusRetryDelay ≈ 1s: a fresh login races the
// connection's own backgrounded ensure-forward reconcile; retry briefly
// before crying wolf.
const (
	statusRetries    = 4
	statusRetryDelay = 250 * time.Millisecond
)

func defaultSSHDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".ssh"), nil
}

// RunReconcile implements "clipboard-over-ssh reconcile [--adopt <name>]".
func RunReconcile(args []string) int {
	sshDir, err := defaultSSHDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "clipboard-over-ssh reconcile: %v\n", err)
		return 1
	}
	return runReconcile(sshDir, args, os.Stdout)
}

func runReconcile(sshDir string, args []string, out io.Writer) int {
	var adopt string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--adopt" && i+1 < len(args):
			i++
			adopt = args[i]
		case strings.HasPrefix(args[i], "--adopt="):
			adopt = args[i][len("--adopt="):]
		}
	}

	res, err := forward.Reconcile(sshDir, adopt)
	if errors.Is(err, forward.ErrBusy) {
		fmt.Fprintln(out, "busy") // someone else is healing; that's fine
		return 0
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "clipboard-over-ssh reconcile: %v\n", err)
		return 1
	}
	if res.Legacy {
		fmt.Fprintln(out, "live clipboard.sock (legacy)")
		return 0
	}
	if res.LiveName == "" {
		fmt.Fprintln(out, "none")
		return 1
	}
	fmt.Fprintf(out, "live %s\n", res.LiveName)
	return 0
}

// RunStatus implements "clipboard-over-ssh status" for login shells.
func RunStatus() int {
	sshDir, err := defaultSSHDir()
	if err != nil {
		return 0 // status must never fail a login shell
	}
	return runStatus(sshDir, os.Stdout)
}

func runStatus(sshDir string, out io.Writer) int {
	if os.Getenv("SSH_CONNECTION") == "" {
		return 0 // local desktop login: real clipboard, alarms are noise
	}

	for attempt := 0; ; attempt++ {
		res, err := forward.Reconcile(sshDir, "")
		switch {
		case errors.Is(err, forward.ErrBusy):
			// another reconcile is running; treat like "not yet"
		case err != nil:
			fmt.Fprintf(out, "clipboard: error: %v\n", err)
			return 0
		case res.Legacy:
			fmt.Fprintln(out, "clipboard: OK (legacy forward)")
			return 0
		case res.LiveName != "":
			origin := strings.SplitN(res.LiveName, ".", 2)[0]
			fmt.Fprintf(out, "clipboard: OK (via %s)\n", origin)
			return 0
		}
		if attempt >= statusRetries {
			fmt.Fprintln(out, "clipboard: no live forward")
			forward.Logf(sshDir, "status", "no live forward after %d attempts", attempt+1)
			return 0
		}
		time.Sleep(statusRetryDelay)
	}
}
```

In `main.go`, add to the subcommand switch (after `"install-remote"`):

```go
	case "reconcile":
		return cmd.RunReconcile(os.Args[2:])
	case "status":
		return cmd.RunStatus()
```

and extend the usage text's command list:

```
  reconcile       Adopt a live forwarded socket onto ~/.ssh/clipboard.sock (remote side)
  status          One-line forward health for login shells (remote side)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./... -v`
Expected: all PASS. `go vet ./...`, `gofmt -l .` clean. `go -C . build ./...` succeeds.

- [ ] **Step 5: Commit**

```bash
git add cmd/reconcile.go cmd/reconcile_test.go main.go
git commit -m "cmd: reconcile and status subcommands

reconcile: CLI over forward.Reconcile — 'live <name>'/'none'/'busy'.
status: login-shell wrapper — silent on non-SSH logins, retries ~1s to
absorb the fresh-login race with ensure-forward's backgrounded
reconcile, always exits 0 so it can never break a login."
```

---

### Task 6: CLI — `ensure-forward` (laptop side)

**Files:**
- Create: `cmd/ensure_forward.go`
- Test: `cmd/ensure_forward_test.go`
- Modify: `main.go` (dispatch + usage text)

**Interfaces:**
- Consumes: `forward.Logf`.
- Produces: `cmd.RunEnsureForward(args []string) int` (args: `<user> <host> <port>`). Internally:

```go
// runner abstracts the two kinds of ssh child so tests never exec anything.
type runner interface {
	// run executes synchronously, capturing combined output.
	run(name string, args ...string) ([]byte, error)
	// startDetached launches a child in its own session with fds 0/1 on
	// /dev/null and 2 on the log; it is never waited on.
	startDetached(name string, args ...string) error
}
```

fd discipline (spec, review C2): `RunEnsureForward` — before anything else — replaces `os.Stdout` and `os.Stdin` with `/dev/null` (LocalCommand stdout interleaves into `-W`/ProxyJump data streams; one stray byte corrupts git/rsync/ProxyJump sessions). All diagnostics go to the log only. The synchronous part is a local control-socket round trip (milliseconds); only the remote reconcile is detached.

- [ ] **Step 1: Write the failing tests**

```go
// Copyright 2026 Tim 'mithro' Ansell
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
)

type fakeCall struct {
	name string
	args []string
}

type fakeRunner struct {
	calls    []fakeCall
	failures map[int]error // call index (0-based) -> error to return from run
	detached []fakeCall
}

func (f *fakeRunner) run(name string, args ...string) ([]byte, error) {
	idx := len(f.calls)
	f.calls = append(f.calls, fakeCall{name, args})
	if err, ok := f.failures[idx]; ok {
		return []byte("mux_client_forward: forwarding request failed"), err
	}
	return nil, nil
}

func (f *fakeRunner) startDetached(name string, args ...string) error {
	f.detached = append(f.detached, fakeCall{name, args})
	return nil
}

func TestEnsureForwardHappyPath(t *testing.T) {
	r := &fakeRunner{}
	name, err := ensureForward(r, "/home/tim/.ssh", "x1c-work", "tim", "ten64.welland.mithis.com", "22")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^x1c-work\.[0-9a-f]{6}\.sock$`).MatchString(name) {
		t.Errorf("socket name = %q, want x1c-work.<hex6>.sock", name)
	}

	if len(r.calls) != 1 {
		t.Fatalf("got %d sync calls, want 1 (-O forward): %v", len(r.calls), r.calls)
	}
	fwd := strings.Join(r.calls[0].args, " ")
	wantFwd := fmt.Sprintf("-O forward -R /home/tim/.ssh/clipboard.d/%s:/home/tim/.ssh/clipboard-over-ssh.sock -l tim -p 22 ten64.welland.mithis.com", name)
	if fwd != wantFwd {
		t.Errorf("-O forward args:\n got %q\nwant %q", fwd, wantFwd)
	}

	if len(r.detached) != 1 {
		t.Fatalf("got %d detached calls, want 1 (reconcile): %v", len(r.detached), r.detached)
	}
	adopt := strings.Join(r.detached[0].args, " ")
	wantAdopt := fmt.Sprintf("-o BatchMode=yes -l tim -p 22 ten64.welland.mithis.com ~/.local/bin/clipboard-over-ssh reconcile --adopt %s", name)
	if adopt != wantAdopt {
		t.Errorf("reconcile args:\n got %q\nwant %q", adopt, wantAdopt)
	}
}

func TestEnsureForwardBootstrapsClipboardDir(t *testing.T) {
	// First -O forward fails (clipboard.d missing on a never-installed
	// remote) -> mkdir over the mux -> retry succeeds.
	r := &fakeRunner{failures: map[int]error{0: errors.New("exit status 255")}}
	_, err := ensureForward(r, "/home/tim/.ssh", "x1c-work", "tim", "ten64.welland.mithis.com", "22")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 3 {
		t.Fatalf("got %d sync calls, want 3 (forward, mkdir, forward): %v", len(r.calls), r.calls)
	}
	mkdir := strings.Join(r.calls[1].args, " ")
	want := "-o BatchMode=yes -l tim -p 22 ten64.welland.mithis.com mkdir -p -m 700 .ssh/clipboard.d"
	if mkdir != want {
		t.Errorf("mkdir call:\n got %q\nwant %q", mkdir, want)
	}
}

func TestEnsureForwardFailsAfterRetry(t *testing.T) {
	r := &fakeRunner{failures: map[int]error{
		0: errors.New("exit status 255"),
		2: errors.New("exit status 255"),
	}}
	_, err := ensureForward(r, "/home/tim/.ssh", "x1c-work", "tim", "ten64.welland.mithis.com", "22")
	if err == nil {
		t.Fatal("want error when -O forward fails twice")
	}
	if len(r.detached) != 0 {
		t.Error("must not run reconcile after forward failure")
	}
}

func TestEnsureForwardUniqueNames(t *testing.T) {
	r := &fakeRunner{}
	a, _ := ensureForward(r, "/home/tim/.ssh", "x1c-work", "tim", "h", "22")
	b, _ := ensureForward(r, "/home/tim/.ssh", "x1c-work", "tim", "h", "22")
	if a == b {
		t.Errorf("two calls produced the same socket name %q", a)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./cmd/ -run TestEnsureForward -v`
Expected: compile error — `ensureForward` undefined.

- [ ] **Step 3: Write the implementation**

```go
// Copyright 2026 Tim 'mithro' Ansell
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/mithro/clipboard-over-ssh/forward"
)

// internalEnv marks nested ssh children spawned by this tool. Recursion is
// already structurally impossible (mux slaves and -O control ops never run
// LocalCommand — verified 2026-07-05/06); this is belt-and-braces.
const internalEnv = "CLIPBOARD_OVER_SSH_INTERNAL"

type runner interface {
	run(name string, args ...string) ([]byte, error)
	startDetached(name string, args ...string) error
}

// execRunner runs real commands with stdout/stderr kept away from the
// inherited fds (LocalCommand stdout is the session's data stream).
type execRunner struct {
	logDir string
}

func (e *execRunner) env() []string {
	return append(os.Environ(), internalEnv+"=1")
}

func (e *execRunner) run(name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	cmd.Env = e.env()
	cmd.Stdin = nil
	return cmd.CombinedOutput()
}

func (e *execRunner) startDetached(name string, args ...string) error {
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer devnull.Close()
	logf, err := os.OpenFile(filepath.Join(e.logDir, "clipboard-over-ssh.log"),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		logf = devnull
	} else {
		defer logf.Close()
	}

	cmd := exec.Command(name, args...)
	cmd.Env = e.env()
	cmd.Stdin = devnull
	cmd.Stdout = devnull
	cmd.Stderr = logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Start() // never waited on; reparents to init when we exit
}

// RunEnsureForward implements "clipboard-over-ssh ensure-forward <user>
// <host> <port>", the LocalCommand hook that gives each new mux master its
// own uniquely-named clipboard forward.
func RunEnsureForward(args []string) int {
	// fd discipline FIRST (spec review C2): LocalCommand's stdout is
	// interleaved into the connection's data stream for -W/ProxyJump
	// masters, and git/rsync use stdout as the protocol pipe. Nothing in
	// this process may ever write there.
	if devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0); err == nil {
		os.Stdout = devnull
		os.Stdin = devnull
	}

	if os.Getenv(internalEnv) != "" {
		return 0
	}
	if len(args) != 3 {
		return 1 // no usage spam: stderr of every ssh would show it
	}
	user, host, port := args[0], args[1], args[2]

	sshDir, err := defaultSSHDir()
	if err != nil {
		return 1
	}
	shortHost, err := os.Hostname()
	if err != nil {
		return 1
	}
	shortHost = strings.SplitN(shortHost, ".", 2)[0]

	name, err := ensureForward(&execRunner{logDir: sshDir}, sshDir, shortHost, user, host, port)
	if err != nil {
		forward.Logf(sshDir, "ensure-forward", "%s@%s:%s failed: %v", user, host, port, err)
		return 1
	}
	forward.Logf(sshDir, "ensure-forward", "%s@%s:%s forwarded %s", user, host, port, name)
	return 0
}

// ensureForward requests a uniquely-named remote forward through the mux
// master, then detaches a remote reconcile to adopt it. The -O forward is
// synchronous (a local control-socket round trip, milliseconds) so the
// forward exists before the user's shell starts; only the reconcile is
// backgrounded. Assumes matching home paths on both ends (documented
// limitation of the existing scheme too).
func ensureForward(r runner, sshDir, shortHost, user, host, port string) (string, error) {
	nonce := make([]byte, 3)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s.%s.sock", shortHost, hex.EncodeToString(nonce))

	remoteSock := filepath.Join(sshDir, "clipboard.d", name)
	localSock := filepath.Join(sshDir, "clipboard-over-ssh.sock")
	fwdArgs := []string{"-O", "forward",
		"-R", remoteSock + ":" + localSock,
		"-l", user, "-p", port, host}

	if out, err := r.run("ssh", fwdArgs...); err != nil {
		// Likely a never-installed remote missing ~/.ssh/clipboard.d
		// (sshd bind fails with ENOENT). Self-bootstrap and retry once.
		mkdirArgs := []string{"-o", "BatchMode=yes", "-l", user, "-p", port, host,
			"mkdir -p -m 700 .ssh/clipboard.d"}
		if _, merr := r.run("ssh", mkdirArgs...); merr != nil {
			return "", fmt.Errorf("-O forward failed (%v: %s) and mkdir failed: %v", err, out, merr)
		}
		if out2, err2 := r.run("ssh", fwdArgs...); err2 != nil {
			return "", fmt.Errorf("-O forward failed after mkdir: %v: %s", err2, out2)
		}
	}

	adoptArgs := []string{"-o", "BatchMode=yes", "-l", user, "-p", port, host,
		"~/.local/bin/clipboard-over-ssh reconcile --adopt " + name}
	if err := r.startDetached("ssh", adoptArgs...); err != nil {
		return "", fmt.Errorf("detaching reconcile: %v", err)
	}
	return name, nil
}
```

In `main.go`, add to the switch:

```go
	case "ensure-forward":
		return cmd.RunEnsureForward(os.Args[2:])
```

and to the usage text:

```
  ensure-forward  LocalCommand hook: forward a unique clipboard socket (local side)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./... -v`
Expected: all PASS. `go vet ./...`, `gofmt -l .` clean.

- [ ] **Step 5: Commit**

```bash
git add cmd/ensure_forward.go cmd/ensure_forward_test.go main.go
git commit -m "cmd: ensure-forward LocalCommand hook

Each new mux master synchronously requests a uniquely-named forward
(shorthost.nonce.sock — collision-free by construction, so no guard and
no StreamLocalBindUnlink), self-bootstraps clipboard.d on never-
installed remotes, then detaches a remote reconcile to adopt it.
Strict fd discipline: stdout/stdin replaced with /dev/null before any
work because LocalCommand stdout interleaves into -W/ProxyJump streams;
diagnostics go to the log only. ssh calls sit behind a runner interface
for tests."
```

---

### Task 7: Shim — deadlines, heal-retry, loud fallthrough

**Files:**
- Modify: `cmd/client.go`
- Test: `cmd/client_test.go` (new)

**Interfaces:**
- Consumes: `forward.Reconcile`, `forward.Logf`, `defaultSSHDir()` (Task 5).
- Produces: `request(sockPath string) (*protocol.Response, error)` — extracted single-attempt request with a hard 5 s deadline; `RunClient` gains the heal path. `fallThrough` and arg parsing are unchanged.

Behavior (spec): the paste path gets a 5 s deadline (today `ReadResponse` can block forever on an undead socket). On dial failure / deadline / protocol error: if using the default socket path, run `forward.Reconcile` and retry **once**; then fall through loudly (one stderr line + log line). `$CLIPBOARD_SOCK` override skips healing (reconcile only manages the default dir).

- [ ] **Step 1: Write the failing tests**

```go
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

	resp, err := request(sock)
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
	_, err = request(sock)
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

	resp, err := requestWithHeal(sshDir, filepath.Join(sshDir, "clipboard.sock"), true)
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

	if _, err := requestWithHeal(sshDir, bogus, false); err == nil {
		t.Fatal("custom CLIPBOARD_SOCK must not be healed onto the default dir")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./cmd/ -run 'TestRequest|TestClient' -v`
Expected: compile error — `request`, `requestWithHeal` undefined.

- [ ] **Step 3: Refactor `cmd/client.go`**

Replace the middle of `RunClient` (everything from the `os.Stat` check through reading the response — current lines ~44–76) with:

```go
	usingDefault := os.Getenv("CLIPBOARD_SOCK") == ""

	sshDir, err := defaultSSHDir()
	if err != nil {
		return fallThrough(invocationName, args)
	}

	resp, err := requestWithHeal(sshDir, sockPath, usingDefault)
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"clipboard-over-ssh: no live clipboard forward (%v); falling back to real %s\n",
			err, invocationName)
		forward.Logf(sshDir, "shim", "fallthrough for %s: %v", invocationName, err)
		return fallThrough(invocationName, args)
	}
```

(keep the existing `if !resp.OK { ... }` error branch and everything after it unchanged), and add:

```go
// requestDeadline bounds the whole paste attempt. An undead forward (half-
// open after laptop suspend) accepts and then never answers; without a
// deadline the old shim blocked forever.
const requestDeadline = 5 * time.Second

// request performs one clipboard request against sockPath.
func request(sockPath string) (*protocol.Response, error) {
	conn, err := net.DialTimeout("unix", sockPath, requestDeadline)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(requestDeadline))

	if _, err := fmt.Fprintf(conn, "%s\n", reqTarget); err != nil {
		return nil, fmt.Errorf("writing request: %w", err)
	}
	if uc, ok := conn.(*net.UnixConn); ok {
		uc.CloseWrite()
	}
	return protocol.ReadResponse(conn)
}

// requestWithHeal tries once; on failure with the default socket path it
// reconciles (symlink hop to any other live forward — no ssh involved) and
// retries once.
func requestWithHeal(sshDir, sockPath string, usingDefault bool) (*protocol.Response, error) {
	resp, err := request(sockPath)
	if err == nil || !usingDefault {
		return resp, err
	}
	res, rerr := forward.Reconcile(sshDir, "")
	if rerr != nil || (res.LiveName == "" && !res.Legacy) {
		return nil, err
	}
	return request(sockPath)
}
```

Note: `request` needs the target string — thread it through. Rename the extracted function signature to `request(sockPath, target string)` and `requestWithHeal(sshDir, sockPath, target string, usingDefault bool)`, passing `req.target`; update the tests to pass `"TARGETS"`. Delete the now-unused `os.Stat` existence check (dial failure covers it) and the old inline send/read code. Add imports `"time"` and `"github.com/mithro/clipboard-over-ssh/forward"`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./... -v` (the undead-deadline test takes ~5 s; `go test ./... -short` skips it)
Expected: all PASS. `go vet ./...`, `gofmt -l .` clean.

- [ ] **Step 5: Manual smoke test on this machine (ten64 has a live forward)**

Run: `go -C . build -o /tmp/cos-test . && /tmp/cos-test client --target TARGETS; rm /tmp/cos-test`
(If the build sandbox forbids `/tmp`, use `./tmp-cos-test` in the worktree and delete it.)
Expected: prints the current TARGETS list (e.g. `image/png`) via the live forward, exit 0.

- [ ] **Step 6: Commit**

```bash
git add cmd/client.go cmd/client_test.go
git commit -m "shim: request deadlines + sshless heal-retry + loud fallthrough

The paste path gets a hard 5s deadline (an undead forward accepts and
never answers; the old shim blocked forever). On any failure with the
default socket path: reconcile (symlink hop to another live forward, no
ssh), retry once, then fall through LOUDLY — one stderr line + log line
— instead of the old silent 'Can't open display' mystery."
```

---

### Task 8: Installers + repo docs teach the new scheme

**Files:**
- Modify: `cmd/install_local.go`, `cmd/install_remote.go`, `README.md`, `CLAUDE.md`, `ssh-config.snippet`

No unit tests (print/setup code); verified by build + reading output.

- [ ] **Step 1: `install_local.go` — create `clipboard.d`, print the new ssh-config advice**

After the unit-enable loop, before `return 0`, replace the advice block (current lines ~102–111) with:

```go
	// The receiving side of forwards needs ~/.ssh/clipboard.d/ (most
	// machines are both sides; cheap to always create).
	if err := os.MkdirAll(filepath.Join(home, ".ssh", "clipboard.d"), 0700); err != nil {
		fmt.Fprintf(os.Stderr, "clipboard-over-ssh install-local: creating clipboard.d: %v\n", err)
		return 1
	}

	fmt.Println("\nInstalled successfully. Socket is active.")
	fmt.Println("\nAdd this to your ~/.ssh/config for remote hosts:")
	fmt.Println()
	fmt.Println("    Host <hostname-pattern>")
	fmt.Println("        PermitLocalCommand yes")
	fmt.Println("        LocalCommand sh -c 'test -x ~/.local/bin/clipboard-over-ssh && exec ~/.local/bin/clipboard-over-ssh ensure-forward %r %h %p; true'")
	fmt.Println()
	fmt.Println("Each new connection forwards its own uniquely-named socket into")
	fmt.Println("~/.ssh/clipboard.d/ on the remote; 'reconcile' points the")
	fmt.Println("~/.ssh/clipboard.sock symlink at a live one. No RemoteForward or")
	fmt.Println("StreamLocalBindUnlink config is needed (or wanted) any more.")
	fmt.Println()
	fmt.Println("Home directory paths must match on both ends (e.g. both /home/tim).")
```

- [ ] **Step 2: `install_remote.go` — create `clipboard.d`**

After the `binDir` MkdirAll block, add:

```go
	if err := os.MkdirAll(filepath.Join(home, ".ssh", "clipboard.d"), 0700); err != nil {
		fmt.Fprintf(os.Stderr, "clipboard-over-ssh install-remote: creating clipboard.d: %v\n", err)
		return 1
	}
```

- [ ] **Step 3: Rewrite `ssh-config.snippet`**

```
# clipboard-over-ssh: each new mux master forwards its own uniquely-named
# socket into ~/.ssh/clipboard.d/ on the remote (LocalCommand below), and
# `clipboard-over-ssh reconcile` maintains the ~/.ssh/clipboard.sock symlink
# that the xclip/wl-paste shims dial.
#
# Do NOT add a RemoteForward for clipboard.sock or StreamLocalBindUnlink —
# that is the old single-shared-socket scheme; mixing both re-creates the
# clobber bug this design removed.
#
# NOTE: ClearAllForwardings does NOT suppress LocalCommand-driven forwards.
# Hosts that must never receive one need: PermitLocalCommand no
Host *.example.com
    PermitLocalCommand yes
    LocalCommand sh -c 'test -x ~/.local/bin/clipboard-over-ssh && exec ~/.local/bin/clipboard-over-ssh ensure-forward %r %h %p; true'
```

- [ ] **Step 4: Update `README.md` and `CLAUDE.md`**

`README.md`: in "How It Works" replace the `~/.ssh/clipboard.sock` ← RemoteForward line of the diagram with the unique-socket flow (`~/.ssh/clipboard.d/<host>.<nonce>.sock` ← `ssh -O forward` ← LocalCommand; `clipboard.sock` → symlink); replace section "2. Configure SSH" with the snippet above; add a short "Self-healing" paragraph: unique names make clobbering impossible; the shim reconciles the symlink and retries before falling back, loudly; `status` gives a login-time health line.

`CLAUDE.md`: update Architecture/Modes to list the new subcommands (`reconcile`, `status`, `ensure-forward`) and the `forward/` package; note the liveness model (Live/Undead/Dead, TARGETS round trip, GC = refused + >5 s) and the fd-discipline rule for anything LocalCommand-spawned; replace "There are no tests yet" with `go test ./...`.

- [ ] **Step 5: Build, vet, review output**

Run: `go test ./... -short && go vet ./... && gofmt -l .`
Expected: clean.

- [ ] **Step 6: Commit**

```bash
git add cmd/install_local.go cmd/install_remote.go README.md CLAUDE.md ssh-config.snippet
git commit -m "installers+docs: teach the unique-socket scheme, create clipboard.d

Following the old printed advice (RemoteForward + StreamLocalBindUnlink)
would re-create the slave-clobber bug the new design removes, so every
place that documents setup now shows the LocalCommand/ensure-forward
flow. Both installers create ~/.ssh/clipboard.d (most machines are both
initiator and receiver)."
```

---

### Task 9: rcfiles — config flip, zprofile status, delete the bash guard

**Files (in `~/rcfiles`, a SEPARATE repo — commit there, not in this worktree):**
- Modify: `~/rcfiles/ssh/config` (lines ~128–137)
- Modify: `~/rcfiles/tmux/zprofile`
- Delete: `~/rcfiles/ssh/bin/clipboard-forward-on-master`

**GATE: do NOT execute this task until the new binaries are deployed to ALL machines (Task 10 steps 1–3). The config activates fleet-wide on the next rcfiles pull. Get explicit go-ahead from Tim.**

- [ ] **Step 1: Replace the clipboard block in `~/rcfiles/ssh/config`**

Delete lines 128–137 (the comment block + `Match host *.mithis.com exec ...` + `RemoteForward` + `StreamLocalBindUnlink`) and insert:

```
# clipboard-over-ssh (unique-socket scheme): each new mux master forwards its
# own uniquely-named socket into ~/.ssh/clipboard.d/ on the remote via
# LocalCommand -> ensure-forward -> ssh -O forward. `reconcile` on the remote
# maintains the ~/.ssh/clipboard.sock symlink the shims dial. No shared
# RemoteForward, no StreamLocalBindUnlink, no master/slave guard — collisions
# are impossible by construction. See mithro/clipboard-over-ssh.
#
# NOTE: ClearAllForwardings does NOT suppress this (LocalCommand is not a
# forward). Hosts that must never receive a clipboard forward need
# `PermitLocalCommand no` (the HAOS block above has it).
# NOTE: if the commented-out `ControlPath ~/.ssh/tmp/master-%j-%r@%h:%p`
# variant is ever restored, ensure-forward's ssh -O calls cannot reproduce
# %j for jumped connections — revisit before switching.
Host *.mithis.com
	PermitLocalCommand yes
	LocalCommand sh -c 'test -x ~/.local/bin/clipboard-over-ssh && exec ~/.local/bin/clipboard-over-ssh ensure-forward %r %h %p; true'
```

And extend the HAOS block (line ~120) to:

```
Host ha.welland.mithis.com ha.monarto.mithis.com
	ClearAllForwardings yes
	PermitLocalCommand no
```

- [ ] **Step 2: Add the status line to `~/rcfiles/tmux/zprofile`**

Insert after the env-file block (after current line ~132, before the log-cleanup loop):

```zsh
# Clipboard forward health (clipboard-over-ssh). One line, bounded runtime,
# silent when the binary is missing or this is not an SSH login.
if [ -x "$HOME/.local/bin/clipboard-over-ssh" ]; then
	_zlog "clipboard: running status check..."
	"$HOME/.local/bin/clipboard-over-ssh" status
fi
```

- [ ] **Step 3: Delete the bash guard**

```bash
git -C ~/rcfiles rm ssh/bin/clipboard-forward-on-master
```

- [ ] **Step 4: Verify ssh still works before committing**

Run: `ssh -G ten64.welland.mithis.com | grep -E 'permitlocalcommand|localcommand|remoteforward'`
Expected: `permitlocalcommand yes`, the `localcommand sh -c ...` line, NO `remoteforward` lines.
Run: `ssh ten64.welland.mithis.com true` — must succeed with no stdout/stderr noise.

- [ ] **Step 5: Commit (in rcfiles)**

```bash
git -C ~/rcfiles add ssh/config tmux/zprofile
git -C ~/rcfiles commit -m "ssh: clipboard-over-ssh unique-socket scheme

Replace the shared clipboard.sock RemoteForward + Match-exec bash guard
with LocalCommand -> ensure-forward (unique socket per master, remote
reconcile maintains the clipboard.sock symlink). Guard deleted: with
unique names there is no master/slave decision to make. zprofile prints
a one-line forward health status on SSH logins."
```

---

### Task 10: Rollout + manual end-to-end validation

No code. Sequenced checklist; steps 5+ require Tim's explicit go-ahead (merge to main, fleet deploy).

- [ ] 1. In the worktree: `go test ./...`, `go vet ./...`, `gofmt -l .`, `make` (cross-compile check) — all clean.
- [ ] 2. **ASK TIM** before merging: merge `self-healing-forward` → `main` with `git merge --no-ff`, push (per repo rules: never push to main without asking).
- [ ] 3. CI builds artifacts → on x1c (rcfiles checkout): run `~/rcfiles/ssh/bin/update-clipboard-over-ssh`, commit refreshed binaries to rcfiles, then run `setup.sh` (installs binary + units) on: x1c-work, ten64 (welland), monarto ten64, and every `*.mithis.com` host that receives pastes. Verify each: `~/.local/bin/clipboard-over-ssh status` exists and `~/.ssh/clipboard.d/` was created.
- [ ] 4. Recommend on receiving sshds (ten64s; via etckeeper commit): `ClientAliveInterval 30` + `ClientAliveCountMax 4` so undead forwards become collectable in ~2 min.
- [ ] 5. Execute Task 9 (config flip) — **after** step 3 confirms binaries everywhere.
- [ ] 6. Manual E2E on x1c ↔ ten64:
  - Fresh connect → within ~2 s `ls -la ~/.ssh/clipboard.d/` on ten64 shows `x1c-work.<nonce>.sock` and `clipboard.sock` symlink points at it (or at the still-live legacy socket — also OK); `xclip -selection clipboard -o` returns laptop clipboard.
  - Legacy handover: once the pre-rollout hand-repaired forward dies (or `ssh -O cancel` it), next paste/reconcile replaces the regular file with the symlink.
  - Unclean master death: `kill -9` the mux master on x1c → reconnect → paste works (new unique socket, no guard misfire possible).
  - Mid-session hop: two x1c masters (`ssh -S none` for the second… no — use a second terminal creating a slave; instead run `ssh -O cancel -R …` to kill the adopted forward) → paste → shim reconciles onto the surviving live socket with no ssh, verified by `~/.ssh/clipboard-over-ssh.log`.
  - Zero forwards: `ssh -O exit` all masters from x1c, then on an existing tmux pane `xclip -o` → loud stderr line within ~7 s, real-xclip fallthrough, log line written.
  - Concurrent pastes: two panes `xclip -o` simultaneously during breakage → log shows one reconcile doing work (others `busy`/no-op).
  - scp-first-master limitation: `ssh -O exit`, then `scp file ten64:` , then `ssh ten64` (slave) → documented: no forward until a new master; paste falls through loudly.
  - ProxyJump regression (review C2): `ssh logs.timvideos.us true` (jumps via underwood.mithis.com) → connection works, no protocol corruption, no stray output.
  - `git fetch` / `rsync` against a `*.mithis.com` host → no corruption, no stray output.
- [ ] 7. Watch `~/.ssh/clipboard-over-ssh.log` on ten64 for a day; then roll monarto + remaining remotes (step 3 list) and update the memory file `reference_ssh_clipboard_proxy.md` to describe the new scheme.

---

## Self-review (done at plan-writing time)

- **Spec coverage**: liveness model → T1; logging/rotation → T2; GC/selection/last-writer-wins → T3; symlink/legacy-migration/flock → T4; reconcile+status CLI, SSH_CONNECTION gate, retry → T5; ensure-forward, fd discipline, bootstrap mkdir, full remote path, recursion env → T6; shim deadlines/heal/loud fallthrough, CLIPBOARD_SOCK override skip → T7; installer/docs rot → T8; config flip incl. HAOS `PermitLocalCommand no`, %j warning comment, zprofile, guard deletion → T9; rollout preconditions, ClientAliveInterval, manual E2E incl. C2 regression → T10.
- **Placeholders**: none — every code step contains the code.
- **Type consistency**: `Probe/State/Live/Undead/Dead` (T1) used in T3/T4; `Result{LiveName,Legacy,Cleaned}` (T3/T4) consumed in T5/T7; `defaultSSHDir()` defined T5, used T6/T7; `runner` interface defined and consumed in T6; `request/requestWithHeal` target-threading noted in T7 step 3.
