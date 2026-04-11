# clipboard-over-ssh

Forward clipboard access from remote machines to your local desktop over SSH.

Programs on remote machines that call `xclip` or `wl-paste` (tmux, neovim, etc.) transparently get your local clipboard contents — no configuration changes needed in those programs.

## How It Works

```
Remote machine                          Local machine (has display)
─────────────────────────────────────   ─────────────────────────────────
Program calls xclip/wl-paste
  → symlink hits clipboard-over-ssh
  → connects to ~/.ssh/clipboard.sock
  ─── SSH RemoteForward tunnel ───────→ ~/.ssh/clipboard-over-ssh.sock
                                          → systemd socket activation
                                          → clipboard-over-ssh server
                                          → reads real clipboard (xclip/wl-paste)
  ← response over tunnel ←──────────── ← sends data back
  → writes to caller's stdout
```

The binary serves double duty:

- **On the local machine**, it runs as a systemd socket-activated server that reads the real clipboard.
- **On the remote machine**, it installs as symlinks named `xclip` and `wl-paste` that forward requests over the SSH-forwarded Unix socket. If the socket isn't available (no SSH session), it falls through to the real binaries.

## Setup

### 1. Install on the Local Machine

Download or build the binary, then run:

```bash
clipboard-over-ssh install-local
```

This installs systemd user units (`~/.config/systemd/user/clipboard-over-ssh.socket` and `clipboard-over-ssh@.service`) and enables the socket at `~/.ssh/clipboard-over-ssh.sock`.

Requires `xclip` or `wl-paste` to be installed locally.

### 2. Configure SSH

Add to your `~/.ssh/config`:

```
Host *.example.com
    RemoteForward ${HOME}/.ssh/clipboard.sock ${HOME}/.ssh/clipboard-over-ssh.sock
    StreamLocalBindUnlink yes
```

`${HOME}` is expanded by SSH on the client side. This works when the remote home directory path matches the local one (e.g., both `/home/tim`).

`StreamLocalBindUnlink yes` removes stale sockets from previous sessions.

### 3. Install on Remote Machines

Copy the binary to the remote machine (matching its architecture), then run:

```bash
clipboard-over-ssh install-remote
```

This copies the binary to `~/.local/bin/` and creates `xclip` and `wl-paste` symlinks pointing to it. `~/.local/bin` must be in your `PATH` (it usually is by default).

The shims connect to `~/.ssh/clipboard.sock` by default. Override with `$CLIPBOARD_SOCK` if needed.

## Building

Requires Go 1.22+. All builds are fully static (no CGO).

```bash
# Build for current platform
make build

# Cross-compile for linux/amd64, linux/arm64, linux/arm
make
```

Output goes to `dist/`.

## License

Apache 2.0 — see [LICENSE](LICENSE).
