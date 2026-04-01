// Copyright 2026 Tim 'mithro' Ansell
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"fmt"
	"os"
	"path/filepath"
)

// RunInstallRemote installs the clipboard-over-ssh binary and xclip/wl-paste
// symlinks into ~/.local/bin/. Since ~/.local/bin is typically already in PATH,
// no shell config changes are needed.
func RunInstallRemote() int {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "clipboard-over-ssh install-remote: cannot determine home directory: %v\n", err)
		return 1
	}

	binDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "clipboard-over-ssh install-remote: creating directory: %v\n", err)
		return 1
	}

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

	// Copy binary into ~/.local/bin if not already there
	destBin := filepath.Join(binDir, "clipboard-over-ssh")
	if binPath != destBin {
		data, err := os.ReadFile(binPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "clipboard-over-ssh install-remote: reading binary: %v\n", err)
			return 1
		}
		if err := os.WriteFile(destBin, data, 0755); err != nil {
			fmt.Fprintf(os.Stderr, "clipboard-over-ssh install-remote: copying binary: %v\n", err)
			return 1
		}
		fmt.Printf("Copied binary to %s\n", destBin)
	}

	// Create symlinks for xclip and wl-paste
	for _, name := range []string{"xclip", "wl-paste"} {
		link := filepath.Join(binDir, name)
		// Remove existing symlink if present (but don't remove real binaries)
		if info, err := os.Lstat(link); err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				os.Remove(link)
			} else {
				fmt.Fprintf(os.Stderr, "clipboard-over-ssh install-remote: %s exists and is not a symlink, skipping\n", link)
				continue
			}
		}
		if err := os.Symlink("clipboard-over-ssh", link); err != nil {
			fmt.Fprintf(os.Stderr, "clipboard-over-ssh install-remote: creating %s symlink: %v\n", name, err)
			return 1
		}
		fmt.Printf("Created symlink %s -> clipboard-over-ssh\n", link)
	}

	fmt.Println("\nInstalled successfully.")
	fmt.Printf("Ensure %s is in your PATH.\n", binDir)
	fmt.Println("The clipboard socket defaults to ~/.ssh/clipboard.sock (no env var needed).")

	return 0
}
