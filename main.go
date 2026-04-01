// Copyright 2026 Tim 'mithro' Ansell
// SPDX-License-Identifier: Apache-2.0

// clipboard-over-ssh forwards clipboard access over SSH connections.
//
// It operates in multiple modes depending on how it is invoked:
//
//   - "clipboard-over-ssh server": serve clipboard requests on stdin/stdout
//     (used by systemd socket activation)
//   - "clipboard-over-ssh install-local": install systemd units on the local machine
//   - "clipboard-over-ssh install-remote": set up shim directory on a remote machine
//   - Invoked as "xclip" or "wl-paste" (via symlink): act as a transparent
//     clipboard shim that forwards requests over a Unix socket
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/mithro/clipboard-over-ssh/cmd"
)

func main() {
	os.Exit(run())
}

func run() int {
	basename := filepath.Base(os.Args[0])

	// Symlink-based dispatch: if invoked as xclip or wl-paste, run client mode
	switch basename {
	case "xclip", "wl-paste":
		return cmd.RunClient(basename, os.Args[1:])
	}

	// Subcommand-based dispatch
	if len(os.Args) < 2 {
		printUsage()
		return 1
	}

	switch os.Args[1] {
	case "server":
		return cmd.RunServer()
	case "client":
		return cmd.RunClient("clipboard-over-ssh", os.Args[2:])
	case "install-local":
		return cmd.RunInstallLocal()
	case "install-remote":
		return cmd.RunInstallRemote()
	default:
		fmt.Fprintf(os.Stderr, "clipboard-over-ssh: unknown command %q\n", os.Args[1])
		printUsage()
		return 1
	}
}

func printUsage() {
	fmt.Fprintln(os.Stderr, `Usage: clipboard-over-ssh <command>

Commands:
  server          Serve clipboard requests on stdin/stdout (systemd socket activation)
  client          Query clipboard via forwarded socket
  install-local   Install systemd socket activation units on the local machine
  install-remote  Set up xclip/wl-paste shims on a remote machine

When invoked as 'xclip' or 'wl-paste' (via symlink), acts as a transparent
clipboard shim that forwards requests over $CLIPBOARD_SOCK.`)
}
