// Copyright 2026 Tim 'mithro' Ansell
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const bashrcMarker = "# clipboard-over-ssh"
const bashrcSnippet = `# clipboard-over-ssh
export CLIPBOARD_SOCK="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}/clipboard.sock"
export PATH="$HOME/.local/bin/clipboard-shims:$PATH"
`

// RunInstallRemote sets up the clipboard shim directory and shell config
// on a remote machine. Run this after copying the binary to the remote.
func RunInstallRemote() int {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "clipboard-over-ssh install-remote: cannot determine home directory: %v\n", err)
		return 1
	}

	shimDir := filepath.Join(home, ".local", "bin", "clipboard-shims")
	if err := os.MkdirAll(shimDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "clipboard-over-ssh install-remote: creating shim directory: %v\n", err)
		return 1
	}
	fmt.Printf("Created %s\n", shimDir)

	// Determine our binary path
	binPath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "clipboard-over-ssh install-remote: cannot determine binary path: %v\n", err)
		return 1
	}
	binPath, err = filepath.EvalSymlinks(binPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "clipboard-over-ssh install-remote: resolving binary path: %v\n", err)
		return 1
	}

	// Copy binary into shim directory if not already there
	shimBin := filepath.Join(shimDir, "clipboard-over-ssh")
	if binPath != shimBin {
		data, err := os.ReadFile(binPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "clipboard-over-ssh install-remote: reading binary: %v\n", err)
			return 1
		}
		if err := os.WriteFile(shimBin, data, 0755); err != nil {
			fmt.Fprintf(os.Stderr, "clipboard-over-ssh install-remote: copying binary: %v\n", err)
			return 1
		}
		fmt.Printf("Copied binary to %s\n", shimBin)
	}

	// Create symlinks for xclip and wl-paste
	for _, name := range []string{"xclip", "wl-paste"} {
		link := filepath.Join(shimDir, name)
		// Remove existing symlink if present
		os.Remove(link)
		if err := os.Symlink("clipboard-over-ssh", link); err != nil {
			fmt.Fprintf(os.Stderr, "clipboard-over-ssh install-remote: creating %s symlink: %v\n", name, err)
			return 1
		}
		fmt.Printf("Created symlink %s -> clipboard-over-ssh\n", link)
	}

	// Update .bashrc if needed
	bashrcPath := filepath.Join(home, ".bashrc")
	if err := updateBashrc(bashrcPath); err != nil {
		fmt.Fprintf(os.Stderr, "clipboard-over-ssh install-remote: updating .bashrc: %v\n", err)
		return 1
	}

	fmt.Println("\nInstalled successfully.")
	fmt.Println("Run 'source ~/.bashrc' or reconnect to activate.")

	return 0
}

func updateBashrc(path string) error {
	content, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	if strings.Contains(string(content), bashrcMarker) {
		fmt.Printf("%s already configured, skipping\n", path)
		return nil
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	if len(content) > 0 && !strings.HasSuffix(string(content), "\n") {
		if _, err := f.WriteString("\n"); err != nil {
			return err
		}
	}

	if _, err := f.WriteString("\n" + bashrcSnippet); err != nil {
		return err
	}

	fmt.Printf("Updated %s\n", path)
	return nil
}
