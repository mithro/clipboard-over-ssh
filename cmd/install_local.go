// Copyright 2026 Tim 'mithro' Ansell
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

const socketUnit = `[Unit]
Description=Clipboard-over-SSH socket

[Socket]
ListenStream=%h/.ssh/clipboard-over-ssh.sock
Accept=yes
SocketMode=0600

[Install]
WantedBy=sockets.target
`

// serviceUnitTemplate has a placeholder for the binary path.
const serviceUnitTemplate = `[Unit]
Description=Clipboard-over-SSH handler

[Service]
Type=simple
ExecStart=%s server
StandardInput=socket
StandardOutput=socket
StandardError=journal
Environment=DISPLAY=:0
Environment=WAYLAND_DISPLAY=wayland-0
TimeoutStopSec=10
`

// RunInstallLocal installs systemd socket activation units on the local machine.
func RunInstallLocal() int {
	binPath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "clipboard-over-ssh install-local: cannot determine binary path: %v\n", err)
		return 1
	}
	binPath, err = filepath.EvalSymlinks(binPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "clipboard-over-ssh install-local: resolving binary path: %v\n", err)
		return 1
	}

	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "clipboard-over-ssh install-local: cannot determine home directory: %v\n", err)
		return 1
	}

	unitDir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(unitDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "clipboard-over-ssh install-local: creating unit directory: %v\n", err)
		return 1
	}

	// Write socket unit
	socketPath := filepath.Join(unitDir, "clipboard-over-ssh.socket")
	if err := os.WriteFile(socketPath, []byte(socketUnit), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "clipboard-over-ssh install-local: writing socket unit: %v\n", err)
		return 1
	}
	fmt.Printf("Wrote %s\n", socketPath)

	// Write service unit template
	servicePath := filepath.Join(unitDir, "clipboard-over-ssh@.service")
	serviceContent := fmt.Sprintf(serviceUnitTemplate, binPath)
	if err := os.WriteFile(servicePath, []byte(serviceContent), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "clipboard-over-ssh install-local: writing service unit: %v\n", err)
		return 1
	}
	fmt.Printf("Wrote %s\n", servicePath)

	// Reload and enable
	cmds := []struct {
		desc string
		args []string
	}{
		{"Reloading systemd", []string{"systemctl", "--user", "daemon-reload"}},
		{"Enabling socket", []string{"systemctl", "--user", "enable", "--now", "clipboard-over-ssh.socket"}},
	}

	for _, c := range cmds {
		fmt.Printf("%s...\n", c.desc)
		cmd := exec.Command(c.args[0], c.args[1:]...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "clipboard-over-ssh install-local: %s failed: %v\n", c.desc, err)
			return 1
		}
	}

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
	// Held in a variable rather than passed as a literal: `go vet`'s printf
	// checker flags Println literals containing %-sequences as a likely
	// Printf/Println mix-up, even though these are literal shell %-escapes
	// (%r %h %p), not Go format verbs.
	localCommandLine := "        LocalCommand sh -c 'test -x ~/.local/bin/clipboard-over-ssh && exec ~/.local/bin/clipboard-over-ssh ensure-forward %r %h %p; true'"
	fmt.Println(localCommandLine)
	fmt.Println()
	fmt.Println("Each new connection forwards its own uniquely-named socket into")
	fmt.Println("~/.ssh/clipboard.d/ on the remote; 'reconcile' points the")
	fmt.Println("~/.ssh/clipboard.sock symlink at a live one. No RemoteForward or")
	fmt.Println("StreamLocalBindUnlink config is needed (or wanted) any more.")
	fmt.Println()
	fmt.Println("Home directory paths must match on both ends (e.g. both /home/tim).")

	return 0
}
