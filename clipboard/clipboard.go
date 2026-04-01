// Copyright 2026 Tim 'mithro' Ansell
// SPDX-License-Identifier: Apache-2.0

// Package clipboard reads from the local system clipboard using
// xclip or wl-paste, whichever is available.
package clipboard

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

const timeout = 5 * time.Second

// GetTargets returns the list of available clipboard targets (MIME types).
func GetTargets(ctx context.Context) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Try xclip first
	out, err := exec.CommandContext(ctx, "xclip", "-selection", "clipboard", "-t", "TARGETS", "-o").Output()
	if err == nil {
		return out, nil
	}

	// Fallback to wl-paste
	out, err = exec.CommandContext(ctx, "wl-paste", "--list-types").Output()
	if err == nil {
		return out, nil
	}

	return nil, fmt.Errorf("no clipboard tool available (tried xclip, wl-paste): %w", err)
}

// GetContent returns the clipboard content for the given MIME type target.
func GetContent(ctx context.Context, target string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Validate target looks like a MIME type (contains exactly one slash)
	if !isValidTarget(target) {
		return nil, fmt.Errorf("invalid target %q: expected MIME type like image/png", target)
	}

	// Try xclip first
	out, err := exec.CommandContext(ctx, "xclip", "-selection", "clipboard", "-t", target, "-o").Output()
	if err == nil {
		return out, nil
	}

	// Fallback to wl-paste
	out, err = exec.CommandContext(ctx, "wl-paste", "--type", target).Output()
	if err == nil {
		return out, nil
	}

	return nil, fmt.Errorf("failed to get clipboard content for %q (tried xclip, wl-paste): %w", target, err)
}

func isValidTarget(target string) bool {
	parts := strings.SplitN(target, "/", 3)
	if len(parts) != 2 {
		return false
	}
	return len(parts[0]) > 0 && len(parts[1]) > 0
}
