# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build Commands

```bash
# Build for current platform (static binary, no CGO)
make build

# Build for all supported platforms (linux/amd64, linux/arm64, linux/arm)
make

# Clean build artifacts
make clean

# Run go vet (GOROOT may need explicit setting, see Makefile)
GOROOT=/usr/lib/go-1.22 go vet ./...

# Run the test suite (includes ~1s and ~5s deadline tests; ~6s total)
go test ./...
```

Go 1.22+ required. All builds are fully static (`CGO_ENABLED=0`, `-ldflags="-s -w"`). No external Go dependencies (only stdlib).

`go test ./...` (suite includes deadline tests at ~1s and ~5s, so the run
takes a few seconds — don't mistake that for a hang), `go vet ./...`, and
`go build ./...` are the checks. CI runs via `.github/workflows/build.yml`.

## Architecture

clipboard-over-ssh forwards clipboard read access from remote machines to a local desktop over SSH Unix socket forwarding. It is a single static Go binary dispatched by symlink name or subcommand.

### Data Flow

Each new SSH connection's `LocalCommand` gives itself a uniquely-named
forward, and a background `reconcile` keeps a symlink pointed at whichever
forward is currently live:

```
Remote machine                          Local machine (has display)
─────────────────────────────────────   ─────────────────────────────────
New connection: LocalCommand runs
ensure-forward
  ─── ssh -O forward (-R) ────────────→ ~/.ssh/clipboard-over-ssh.sock
      binds ~/.ssh/clipboard.d/            → systemd socket activation
      <shorthost>.<nonce>.sock              → clipboard-over-ssh server
  ← detached `reconcile --adopt` ────── ← reads real clipboard (xclip/wl-paste)
      flips ~/.ssh/clipboard.sock
      symlink → clipboard.d/<name>.sock

Program calls xclip/wl-paste
  → symlink hits clipboard-over-ssh
  → connects to ~/.ssh/clipboard.sock (symlink → clipboard.d/<live>.sock)
  ─── forwarded tunnel ────────────────→ ~/.ssh/clipboard-over-ssh.sock
                                          → clipboard-over-ssh server
                                          → reads real clipboard via xclip/wl-paste
  ← response over tunnel ←──────────── ← sends data back over stdout
  → writes to caller's stdout
```

### Modes (dispatched in `main.go`)

1. **Server** (`cmd/server.go`): Reads one clipboard request from stdin, calls the real clipboard tool, writes response to stdout. Designed for systemd `Accept=yes` socket activation — one process per connection, no persistent daemon.

2. **Client/Shim** (`cmd/client.go`): When the binary is invoked as `xclip` or `wl-paste` (via symlink) or via `clipboard-over-ssh client`, it parses the CLI arguments, connects to `$CLIPBOARD_SOCK` (or `~/.ssh/clipboard.sock`), sends the request, and prints the result. Requests have a 5s deadline (an "undead" forward that accepts but never answers would otherwise hang forever). On failure against the default socket it calls `forward.Reconcile` (a symlink hop — no ssh involved) and retries once before **falling through** to the real binary by searching PATH past its own directory, logging the fallthrough loudly.

3. **install-local** (`cmd/install_local.go`): Writes systemd user units (socket + template service) to `~/.config/systemd/user/` and enables the socket at `~/.ssh/clipboard-over-ssh.sock`. Also creates `~/.ssh/clipboard.d/` and prints the `ssh-config.snippet` advice.

4. **install-remote** (`cmd/install_remote.go`): Copies the binary to `~/.local/bin/`, creates `xclip`/`wl-paste` symlinks pointing to it, and creates `~/.ssh/clipboard.d/`.

5. **reconcile** (`cmd/reconcile.go` → `forward.Reconcile`): Scans `~/.ssh/clipboard.d/*.sock`, GCs dead ones, and flips the `~/.ssh/clipboard.sock` symlink at a live one (`--adopt <name>` prefers that name if it's live; otherwise newest-by-mtime wins). Serialized via `flock` on `clipboard.d/.lock`; a busy lock is not an error (`ErrBusy`, "someone else is already healing").

6. **ensure-forward** (`cmd/ensure_forward.go`): The `LocalCommand` hook (`ensure-forward %r %h %p`). Generates a unique remote socket name, runs `ssh -O forward -R <remote>:<local>` synchronously against the already-established mux master (self-bootstrapping `mkdir ~/.ssh/clipboard.d` and retrying once if the remote is a fresh install), then detaches a remote `reconcile --adopt <name>` so the symlink flips without blocking login.

7. **status** (`cmd/reconcile.go` → `RunStatus`): A login-shell one-liner (`clipboard: OK (via <host>)` / `no live forward`). Gated on `$SSH_CONNECTION` being set (never fires on a local desktop login) and retries briefly (~1s total) to avoid racing its own connection's backgrounded `ensure-forward`/`reconcile`.

### Packages

- **`protocol/`**: Wire protocol — request is `"<target>\n"`, response is `"OK <len>\n<data>"` or `"ERR <msg>\n"`. One request per connection.
- **`clipboard/`**: Reads from the real system clipboard by shelling out to `xclip` (tried first) or `wl-paste` (fallback). Has a 5-second timeout.
- **`forward/`**: The liveness/reconciliation model shared by `reconcile`, `status`, and the client's heal-retry.
  - `Probe` (`forward/probe.go`) classifies a socket path: **Live** (a full `TARGETS` request/response round trip succeeds — a bare `connect()` is not enough, since sshd accepts on a forwarded socket even after the tunnel's far end is gone), **Undead** (accepts but never answers — a half-open forward, e.g. after laptop suspend, until sshd reaps the session; not adoptable, not collectable), or **Dead** (connection refused or the path is missing).
  - `Reconcile` (`forward/reconcile.go`) GCs Dead sockets, but only once they're older than 5s (`minAge`) — sshd's own bind()-then-listen() window makes a just-born socket look refused; collecting it would orphan a live forward on an invisible inode.
  - Also handles legacy migration: a live old-scheme regular-file `clipboard.sock` is kept as-is (mixed-version rollout safety) until it goes dead, only then replaced by the symlink.

### Key Design Decisions

- Symlink-based dispatch (argv[0] detection) — the binary changes behaviour based on its filename, like busybox.
- Client falls through to real binaries when no forward is available, so installing the shims is safe even without an active SSH session.
- Server validates that non-TARGETS requests look like MIME types (must contain exactly one `/`).
- `wl-paste --list-types` filtering: X11 selection atoms (TARGETS, TIMESTAMP) are stripped from output since wl-paste doesn't include them.
- Socket paths: local side uses `~/.ssh/clipboard-over-ssh.sock` (systemd `%h` specifier); remote side uses uniquely-named sockets under `~/.ssh/clipboard.d/`, with `~/.ssh/clipboard.sock` a symlink that `reconcile` keeps pointed at whichever one is live. `ssh -O forward -R` (issued by `ensure-forward` from a `LocalCommand`) bridges each pair.
- **fd discipline (`ensure-forward` and anything it spawns)**: `LocalCommand`'s stdout is interleaved into the connection's own data stream — for `-W`/`ProxyJump` masters, and for protocols like git/rsync that use stdout as the wire — so nothing this process runs may ever write to stdout (or read stdin). `RunEnsureForward` redirects both to `/dev/null` before doing anything else; detached children (`startDetached`) get `/dev/null` on stdin/stdout too, with stderr going to a log file. Apply the same rule to any future code reachable from `LocalCommand`.
