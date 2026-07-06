// Copyright 2026 Tim 'mithro' Ansell
// SPDX-License-Identifier: Apache-2.0

// Package forward maintains the pool of per-connection forwarded clipboard
// sockets in ~/.ssh/clipboard.d/ and the ~/.ssh/clipboard.sock symlink that
// shims dial through.
package forward

import (
	"io"
	"net"
	"time"

	"github.com/mithro/clipboard-over-ssh/protocol"
)

// State classifies a forwarded socket.
type State int

const (
	// stateUnknown is the zero value: an accidentally-unset State (e.g. a
	// zero-valued struct field never assigned by a bug) reads as this, not
	// as Live. The selection switch in reconcile.go only special-cases Live
	// and Dead explicitly, so stateUnknown falls through like Undead —
	// skipped: never adopted, never unlinked. Fail closed, not open.
	stateUnknown State = iota
	// Live: completed a TARGETS round trip — safe to adopt.
	Live
	// Undead: accepts connections but never answers (half-open forward,
	// e.g. after laptop suspend, until sshd reaps the session). Not
	// adoptable, not collectable.
	Undead
	// Dead: connection refused or path missing — collectable.
	Dead
)

// Probe classifies the socket at path. A bare connect() is NOT liveness:
// sshd accepts on a forwarded socket even when the tunnel's far end is
// gone, so only a full application-level round trip counts as Live.
func Probe(path string, timeout time.Duration) State {
	conn, err := net.DialTimeout("unix", path, timeout)
	if err != nil {
		return Dead
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(timeout))
	if _, err := io.WriteString(conn, "TARGETS\n"); err != nil {
		return Undead
	}
	if uc, ok := conn.(*net.UnixConn); ok {
		uc.CloseWrite()
	}
	if _, err := protocol.ReadResponse(conn); err != nil {
		return Undead
	}
	return Live
}
