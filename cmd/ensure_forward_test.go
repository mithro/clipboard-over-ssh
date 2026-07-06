// Copyright 2026 Tim 'mithro' Ansell
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
)

type fakeCall struct {
	name string
	args []string
}

type fakeRunner struct {
	calls    []fakeCall
	failures map[int]error // call index (0-based) -> error to return from run
	detached []fakeCall
}

func (f *fakeRunner) run(name string, args ...string) ([]byte, error) {
	idx := len(f.calls)
	f.calls = append(f.calls, fakeCall{name, args})
	if err, ok := f.failures[idx]; ok {
		return []byte("mux_client_forward: forwarding request failed"), err
	}
	return nil, nil
}

func (f *fakeRunner) startDetached(name string, args ...string) error {
	f.detached = append(f.detached, fakeCall{name, args})
	return nil
}

func TestEnsureForwardHappyPath(t *testing.T) {
	r := &fakeRunner{}
	name, err := ensureForward(r, "/home/tim/.ssh", "x1c-work", "tim", "ten64.welland.mithis.com", "22")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^x1c-work\.[0-9a-f]{6}\.sock$`).MatchString(name) {
		t.Errorf("socket name = %q, want x1c-work.<hex6>.sock", name)
	}

	if len(r.calls) != 1 {
		t.Fatalf("got %d sync calls, want 1 (-O forward): %v", len(r.calls), r.calls)
	}
	fwd := strings.Join(r.calls[0].args, " ")
	wantFwd := fmt.Sprintf("-O forward -R /home/tim/.ssh/clipboard.d/%s:/home/tim/.ssh/clipboard-over-ssh.sock -l tim -p 22 ten64.welland.mithis.com", name)
	if fwd != wantFwd {
		t.Errorf("-O forward args:\n got %q\nwant %q", fwd, wantFwd)
	}

	if len(r.detached) != 1 {
		t.Fatalf("got %d detached calls, want 1 (reconcile): %v", len(r.detached), r.detached)
	}
	adopt := strings.Join(r.detached[0].args, " ")
	wantAdopt := fmt.Sprintf("-o BatchMode=yes -l tim -p 22 ten64.welland.mithis.com ~/.local/bin/clipboard-over-ssh reconcile --adopt %s", name)
	if adopt != wantAdopt {
		t.Errorf("reconcile args:\n got %q\nwant %q", adopt, wantAdopt)
	}
}

func TestEnsureForwardBootstrapsClipboardDir(t *testing.T) {
	// First -O forward fails (clipboard.d missing on a never-installed
	// remote) -> mkdir over the mux -> retry succeeds.
	r := &fakeRunner{failures: map[int]error{0: errors.New("exit status 255")}}
	_, err := ensureForward(r, "/home/tim/.ssh", "x1c-work", "tim", "ten64.welland.mithis.com", "22")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 3 {
		t.Fatalf("got %d sync calls, want 3 (forward, mkdir, forward): %v", len(r.calls), r.calls)
	}
	mkdir := strings.Join(r.calls[1].args, " ")
	want := "-o BatchMode=yes -l tim -p 22 ten64.welland.mithis.com mkdir -p -m 700 .ssh/clipboard.d"
	if mkdir != want {
		t.Errorf("mkdir call:\n got %q\nwant %q", mkdir, want)
	}
}

func TestEnsureForwardFailsAfterRetry(t *testing.T) {
	r := &fakeRunner{failures: map[int]error{
		0: errors.New("exit status 255"),
		2: errors.New("exit status 255"),
	}}
	_, err := ensureForward(r, "/home/tim/.ssh", "x1c-work", "tim", "ten64.welland.mithis.com", "22")
	if err == nil {
		t.Fatal("want error when -O forward fails twice")
	}
	if len(r.detached) != 0 {
		t.Error("must not run reconcile after forward failure")
	}
}

func TestEnsureForwardUniqueNames(t *testing.T) {
	r := &fakeRunner{}
	a, _ := ensureForward(r, "/home/tim/.ssh", "x1c-work", "tim", "h", "22")
	b, _ := ensureForward(r, "/home/tim/.ssh", "x1c-work", "tim", "h", "22")
	if a == b {
		t.Errorf("two calls produced the same socket name %q", a)
	}
}
