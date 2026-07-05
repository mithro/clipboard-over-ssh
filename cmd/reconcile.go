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
