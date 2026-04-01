// Copyright 2026 Tim 'mithro' Ansell
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/mithro/clipboard-over-ssh/protocol"
)

// RunClient handles clipboard requests as a fake xclip or wl-paste.
// It connects to the forwarded Unix socket at $CLIPBOARD_SOCK, sends
// the request, and writes the raw response data to stdout.
//
// If the socket is unavailable or the invocation doesn't match a
// clipboard read operation, it falls through to the real binary.
func RunClient(invocationName string, args []string) int {
	target, shouldHandle := parseClientArgs(invocationName, args)
	if !shouldHandle {
		return fallThrough(invocationName, args)
	}

	sockPath := os.Getenv("CLIPBOARD_SOCK")
	if sockPath == "" {
		return fallThrough(invocationName, args)
	}

	if _, err := os.Stat(sockPath); os.IsNotExist(err) {
		return fallThrough(invocationName, args)
	}

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		return fallThrough(invocationName, args)
	}
	defer conn.Close()

	// Send request
	if _, err := fmt.Fprintf(conn, "%s\n", target); err != nil {
		fmt.Fprintf(os.Stderr, "clipboard-over-ssh: writing request: %v\n", err)
		return 1
	}

	// Close write side so server sees EOF after the request line
	if uc, ok := conn.(*net.UnixConn); ok {
		uc.CloseWrite()
	}

	// Read response
	resp, err := protocol.ReadResponse(conn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "clipboard-over-ssh: reading response: %v\n", err)
		return 1
	}

	if !resp.OK {
		fmt.Fprintf(os.Stderr, "clipboard-over-ssh: server error: %s\n", resp.Err)
		return 1
	}

	// Write raw data to stdout (no protocol header)
	if _, err := os.Stdout.Write(resp.Data); err != nil {
		fmt.Fprintf(os.Stderr, "clipboard-over-ssh: writing output: %v\n", err)
		return 1
	}

	return 0
}

// parseClientArgs extracts the clipboard target from xclip/wl-paste arguments.
// Returns the target string and whether this invocation should be handled
// (vs. falling through to the real binary).
func parseClientArgs(invocationName string, args []string) (string, bool) {
	switch invocationName {
	case "xclip":
		return parseXclipArgs(args)
	case "wl-paste":
		return parseWlPasteArgs(args)
	case "clipboard-over-ssh":
		return parseDirectArgs(args)
	default:
		return "", false
	}
}

// parseXclipArgs handles: xclip -selection clipboard -t <target> -o
func parseXclipArgs(args []string) (string, bool) {
	var hasOutput bool
	var selection string
	var target string

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-o", "-out":
			hasOutput = true
		case "-selection", "-sel":
			if i+1 < len(args) {
				i++
				selection = args[i]
			}
		case "-t", "-target":
			if i+1 < len(args) {
				i++
				target = args[i]
			}
		}
	}

	// Only intercept output mode for clipboard selection
	if !hasOutput || selection != "clipboard" {
		return "", false
	}

	if target == "" {
		target = "TARGETS"
	}

	return target, true
}

// parseWlPasteArgs handles: wl-paste --type <target> or wl-paste --list-types
func parseWlPasteArgs(args []string) (string, bool) {
	var target string

	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--list-types":
			return "TARGETS", true
		case args[i] == "--type" && i+1 < len(args):
			i++
			target = args[i]
		case strings.HasPrefix(args[i], "--type="):
			target = args[i][len("--type="):]
		}
	}

	if target != "" {
		return target, true
	}

	return "", false
}

// parseDirectArgs handles: clipboard-over-ssh client --target <target>
func parseDirectArgs(args []string) (string, bool) {
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--target" && i+1 < len(args):
			i++
			return args[i], true
		case strings.HasPrefix(args[i], "--target="):
			return args[i][len("--target="):], true
		}
	}
	return "", false
}

// fallThrough finds and executes the real binary, searching PATH entries
// past the directory containing our binary.
func fallThrough(name string, args []string) int {
	ourPath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "clipboard-over-ssh: cannot determine own path: %v\n", err)
		return 1
	}
	ourDir := filepath.Dir(ourPath)

	// Search PATH for the real binary, skipping our own directory
	pathDirs := filepath.SplitList(os.Getenv("PATH"))
	for _, dir := range pathDirs {
		if dir == ourDir {
			continue
		}
		candidate := filepath.Join(dir, name)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			// Found a real binary — exec it
			allArgs := append([]string{name}, args...)
			err := syscall.Exec(candidate, allArgs, os.Environ())
			// If exec returns, it failed
			fmt.Fprintf(os.Stderr, "clipboard-over-ssh: exec %s: %v\n", candidate, err)
			return 1
		}
	}

	fmt.Fprintf(os.Stderr, "clipboard-over-ssh: %s not found in PATH (excluding %s)\n", name, ourDir)
	return 1
}
