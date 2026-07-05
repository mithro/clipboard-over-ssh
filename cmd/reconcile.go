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

// statusRetryBudget bounds the retry loop by wall clock, not attempt count.
// A fresh login races the connection's own backgrounded ensure-forward
// reconcile; that race is normally over in well under a second, so 1.5s is
// generous. Bounding by wall clock (rather than a fixed attempt count) keeps
// this a bounded delay even when an undead socket makes each individual
// Reconcile call slow: a probe against an undead forward blocks for
// forward's own probeTimeout (2s), and 5 attempt-counted retries against
// such a socket used to serialize into an ~11s login stall. See also the
// per-call slow-reconcile check in runStatus below, which stops retrying
// immediately once a single call already outran the race window.
const (
	statusRetryBudget = 1500 * time.Millisecond
	statusRetryDelay  = 250 * time.Millisecond
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

	deadline := time.Now().Add(statusRetryBudget)
	for attempt := 1; ; attempt++ {
		start := time.Now()
		res, err := forward.Reconcile(sshDir, "")
		slow := time.Since(start) > statusRetryDelay
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
		// Stop as soon as either the wall-clock budget is spent, or this
		// call alone already took longer than the race we're absorbing —
		// a slow Reconcile (e.g. probing an undead socket) means the
		// sub-second fresh-login race is long since over, so further
		// retries would only add more of the same slow probes.
		if slow || time.Now().After(deadline) {
			fmt.Fprintln(out, "clipboard: no live forward")
			forward.Logf(sshDir, "status", "no live forward after %d attempts", attempt)
			return 0
		}
		time.Sleep(statusRetryDelay)
	}
}
