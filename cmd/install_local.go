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
ListenStream=%t/clipboard-over-ssh.sock
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

	// Print status
	xdg := os.Getenv("XDG_RUNTIME_DIR")
	if xdg == "" {
		xdg = fmt.Sprintf("/run/user/%d", os.Getuid())
	}
	localSock := filepath.Join(xdg, "clipboard-over-ssh.sock")

	fmt.Println("\nInstalled successfully. Socket is active.")
	fmt.Println("\nAdd this to your ~/.ssh/config for remote hosts:")
	fmt.Println()
	fmt.Println("    Host <hostname-pattern>")
	fmt.Printf("        RemoteForward ${HOME}/.ssh/clipboard.sock %s\n", localSock)
	fmt.Println("        StreamLocalBindUnlink yes")
	fmt.Println()
	fmt.Println("${HOME} is expanded by SSH on the client side. This works when the")
	fmt.Println("remote home directory path matches the local one (e.g. both /home/tim).")

	return 0
}
