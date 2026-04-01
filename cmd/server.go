// Copyright 2026 Tim 'mithro' Ansell
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/mithro/clipboard-over-ssh/clipboard"
	"github.com/mithro/clipboard-over-ssh/protocol"
)

// RunServer handles a single clipboard request on stdin/stdout.
// Designed to be invoked by systemd socket activation (Accept=yes).
func RunServer() int {
	target, err := protocol.ReadRequest(os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "clipboard-over-ssh server: %v\n", err)
		return 1
	}

	ctx := context.Background()
	var data []byte

	if target == "TARGETS" {
		data, err = clipboard.GetTargets(ctx)
	} else {
		data, err = clipboard.GetContent(ctx, target)
	}

	if err != nil {
		if werr := protocol.WriteError(os.Stdout, err.Error()); werr != nil {
			fmt.Fprintf(os.Stderr, "clipboard-over-ssh server: writing error response: %v\n", werr)
			return 1
		}
		return 0
	}

	if err := protocol.WriteOK(os.Stdout, data); err != nil {
		fmt.Fprintf(os.Stderr, "clipboard-over-ssh server: writing OK response: %v\n", err)
		return 1
	}
	return 0
}
