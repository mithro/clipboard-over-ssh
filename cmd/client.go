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
	req, shouldHandle := parseClientArgs(invocationName, args)
	if !shouldHandle {
		return fallThrough(invocationName, args)
	}

	sockPath := os.Getenv("CLIPBOARD_SOCK")
	if sockPath == "" {
		// Default: check ~/.ssh/clipboard.sock (same path as SSH RemoteForward target)
		home, err := os.UserHomeDir()
		if err == nil {
			sockPath = filepath.Join(home, ".ssh", "clipboard.sock")
		}
	}
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
	if _, err := fmt.Fprintf(conn, "%s\n", req.target); err != nil {
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

	data := resp.Data

	// wl-paste --list-types only shows MIME types (entries containing '/'),
	// filtering out X11 atoms like TARGETS, TIMESTAMP, STRING, etc.
	if req.filterX11Atoms {
		data = filterX11Atoms(data)
	}

	// Write raw data to stdout (no protocol header)
	if _, err := os.Stdout.Write(data); err != nil {
		fmt.Fprintf(os.Stderr, "clipboard-over-ssh: writing output: %v\n", err)
		return 1
	}

	return 0
}

type clientRequest struct {
	target       string
	filterX11Atoms bool // when true, filter TARGETS output to only MIME types (for wl-paste --list-types)
}

// parseClientArgs extracts the clipboard target from xclip/wl-paste arguments.
// Returns the request and whether this invocation should be handled
// (vs. falling through to the real binary).
func parseClientArgs(invocationName string, args []string) (clientRequest, bool) {
	switch invocationName {
	case "xclip":
		target, ok := parseXclipArgs(args)
		return clientRequest{target: target}, ok
	case "wl-paste":
		return parseWlPasteArgs(args)
	case "clipboard-over-ssh":
		target, ok := parseDirectArgs(args)
		return clientRequest{target: target}, ok
	default:
		return clientRequest{}, false
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
func parseWlPasteArgs(args []string) (clientRequest, bool) {
	var target string

	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--list-types":
			return clientRequest{target: "TARGETS", filterX11Atoms: true}, true
		case args[i] == "--type" && i+1 < len(args):
			i++
			target = args[i]
		case strings.HasPrefix(args[i], "--type="):
			target = args[i][len("--type="):]
		}
	}

	if target != "" {
		return clientRequest{target: target}, true
	}

	return clientRequest{}, false
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

// x11SelectionAtoms are X11 selection metadata atoms that xclip includes in
// TARGETS output but wl-paste --list-types omits. These are not real clipboard
// content types.
var x11SelectionAtoms = map[string]bool{
	"TARGETS":   true,
	"TIMESTAMP": true,
}

// filterX11Atoms removes X11 selection metadata atoms from a newline-separated
// list of clipboard targets. This matches the behavior of wl-paste --list-types
// which only returns actual content types, not X11 selection metadata.
func filterX11Atoms(data []byte) []byte {
	var result []byte
	for _, line := range strings.Split(string(data), "\n") {
		if line != "" && !x11SelectionAtoms[line] {
			result = append(result, []byte(line+"\n")...)
		}
	}
	return result
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
