// Copyright 2026 Tim 'mithro' Ansell
// SPDX-License-Identifier: Apache-2.0

// Package protocol implements the clipboard-over-ssh wire protocol.
//
// The protocol is one request per connection:
//
// Request (client → server): a single line "<target>\n"
//   - "TARGETS\n" to list available clipboard types
//   - "image/png\n" to get clipboard as PNG, etc.
//
// Response (server → client):
//   - Success: "OK <byte-length>\n" followed by raw bytes
//   - Error: "ERR <message>\n"
package protocol

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// ReadRequest reads a single-line target request from r.
func ReadRequest(r io.Reader) (string, error) {
	scanner := bufio.NewScanner(r)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return "", fmt.Errorf("reading request: %w", err)
		}
		return "", fmt.Errorf("reading request: empty input")
	}
	target := strings.TrimSpace(scanner.Text())
	if target == "" {
		return "", fmt.Errorf("reading request: empty target")
	}
	return target, nil
}

// WriteOK writes a success response: "OK <len>\n" followed by data.
func WriteOK(w io.Writer, data []byte) error {
	header := fmt.Sprintf("OK %d\n", len(data))
	if _, err := io.WriteString(w, header); err != nil {
		return fmt.Errorf("writing OK header: %w", err)
	}
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("writing OK data: %w", err)
	}
	return nil
}

// WriteError writes an error response: "ERR <message>\n".
func WriteError(w io.Writer, msg string) error {
	line := fmt.Sprintf("ERR %s\n", msg)
	if _, err := io.WriteString(w, line); err != nil {
		return fmt.Errorf("writing error: %w", err)
	}
	return nil
}

// Response holds a parsed server response.
type Response struct {
	OK   bool
	Data []byte
	Err  string
}

// ReadResponse reads and parses a server response from r.
func ReadResponse(r io.Reader) (*Response, error) {
	br := bufio.NewReader(r)
	line, err := br.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("reading response header: %w", err)
	}
	line = strings.TrimRight(line, "\n")

	if strings.HasPrefix(line, "ERR ") {
		return &Response{OK: false, Err: line[4:]}, nil
	}

	if !strings.HasPrefix(line, "OK ") {
		return nil, fmt.Errorf("unexpected response header: %q", line)
	}

	lengthStr := line[3:]
	length, err := strconv.Atoi(lengthStr)
	if err != nil {
		return nil, fmt.Errorf("parsing response length %q: %w", lengthStr, err)
	}

	data := make([]byte, length)
	if _, err := io.ReadFull(br, data); err != nil {
		return nil, fmt.Errorf("reading response data (%d bytes): %w", length, err)
	}

	return &Response{OK: true, Data: data}, nil
}
