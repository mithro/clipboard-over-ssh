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
	"time"

	"github.com/mithro/clipboard-over-ssh/forward"
)

// internalEnv marks nested ssh children spawned by this tool. Recursion is
// already structurally impossible (mux slaves and -O control ops never run
// LocalCommand — verified 2026-07-05/06); this is belt-and-braces.
const internalEnv = "CLIPBOARD_OVER_SSH_INTERNAL"

// ensureForwardBudget bounds the entire RunEnsureForward call. ssh runs
// LocalCommand synchronously as part of establishing every new session, so a
// hang here (a wedged mux master, a stuck -O forward) doesn't just break one
// clipboard forward — it blocks every new ssh connection to this host,
// fleet-wide, until something intervenes. 10s is generous for a local
// control-socket round trip plus a detached spawn.
const ensureForwardBudget = 10 * time.Second

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

	// Watchdog: if anything below wedges (a hung -O forward, a hung mux
	// master), this process must not hang the connection's session setup
	// forever. os.Exit here reaps nothing extra — the detached reconcile
	// child, already Setsid'd, has already reparented to init and survives
	// on its own regardless of what happens to us.
	time.AfterFunc(ensureForwardBudget, func() {
		if sshDir, err := defaultSSHDir(); err == nil {
			forward.Logf(sshDir, "ensure-forward", "budget exceeded (%s); aborting", ensureForwardBudget)
		}
		os.Exit(1)
	})

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
