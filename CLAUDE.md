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
```

Go 1.22+ required. All builds are fully static (`CGO_ENABLED=0`, `-ldflags="-s -w"`). No external Go dependencies (only stdlib).

There are no tests yet — `go vet` and `go build` are the only checks. CI runs via `.github/workflows/build.yml`.

## Architecture

clipboard-over-ssh forwards clipboard read access from remote machines to a local desktop over SSH Unix socket forwarding. It is a single static Go binary that operates in four modes:

### Data Flow

```
Remote machine                          Local machine (has display)
─────────────────────────────────────   ─────────────────────────────────
Program calls xclip/wl-paste            
  → symlink hits clipboard-over-ssh     
  → connects to ~/.ssh/clipboard.sock   
  ─── SSH RemoteForward tunnel ───────→ ~/.ssh/clipboard-over-ssh.sock
                                          → systemd socket activation
                                          → clipboard-over-ssh server
                                          → reads real clipboard via xclip/wl-paste
  ← response over tunnel ←──────────── ← sends data back over stdout
  → writes to caller's stdout           
```

### Modes (dispatched in `main.go`)

1. **Server** (`cmd/server.go`): Reads one clipboard request from stdin, calls the real clipboard tool, writes response to stdout. Designed for systemd `Accept=yes` socket activation — one process per connection, no persistent daemon.

2. **Client/Shim** (`cmd/client.go`): When the binary is invoked as `xclip` or `wl-paste` (via symlink) or via `clipboard-over-ssh client`, it parses the CLI arguments, connects to `$CLIPBOARD_SOCK` (or `~/.ssh/clipboard.sock`), sends the request, and prints the result. If the socket is missing or args don't match a read operation, it **falls through** to the real binary by searching PATH past its own directory.

3. **install-local** (`cmd/install_local.go`): Writes systemd user units (socket + template service) to `~/.config/systemd/user/` and enables the socket. The socket listens at `~/.ssh/clipboard-over-ssh.sock`.

4. **install-remote** (`cmd/install_remote.go`): Copies the binary to `~/.local/bin/` and creates `xclip`/`wl-paste` symlinks pointing to it.

### Packages

- **`protocol/`**: Wire protocol — request is `"<target>\n"`, response is `"OK <len>\n<data>"` or `"ERR <msg>\n"`. One request per connection.
- **`clipboard/`**: Reads from the real system clipboard by shelling out to `xclip` (tried first) or `wl-paste` (fallback). Has a 5-second timeout.

### Key Design Decisions

- Symlink-based dispatch (argv[0] detection) — the binary changes behaviour based on its filename, like busybox.
- Client falls through to real binaries when socket is unavailable, so installing the shims is safe even without an active SSH session.
- Server validates that non-TARGETS requests look like MIME types (must contain exactly one `/`).
- `wl-paste --list-types` filtering: X11 selection atoms (TARGETS, TIMESTAMP) are stripped from output since wl-paste doesn't include them.
- Socket paths: local side uses `~/.ssh/clipboard-over-ssh.sock` (systemd `%h` specifier), remote side uses `~/.ssh/clipboard.sock`. SSH `RemoteForward` bridges them.
