# clipboard-over-ssh

Forward clipboard access from remote machines to your local desktop over SSH.

Programs on remote machines that call `xclip` or `wl-paste` (tmux, neovim, etc.) transparently get your local clipboard contents — no configuration changes needed in those programs.

## How It Works

```
Remote machine                          Local machine (has display)
─────────────────────────────────────   ─────────────────────────────────
New SSH connection (mux master)
  → LocalCommand runs ensure-forward
  ─── ssh -O forward (-R) ────────────→ ~/.ssh/clipboard-over-ssh.sock
      binds a uniquely-named socket        → systemd socket activation
      ~/.ssh/clipboard.d/<client>.<nonce>.sock → clipboard-over-ssh server
      (<client> = local machine's short
      hostname, not the SSH target)
  ← detached `reconcile --adopt` ────── ← reads real clipboard (xclip/wl-paste)
      points ~/.ssh/clipboard.sock
      (symlink) at the new socket

Program calls xclip/wl-paste
  → symlink hits clipboard-over-ssh
  → connects to ~/.ssh/clipboard.sock (symlink → clipboard.d/<live>.sock)
  ─── forwarded tunnel ────────────────→ ~/.ssh/clipboard-over-ssh.sock
                                          → clipboard-over-ssh server
                                          → reads real clipboard (xclip/wl-paste)
  ← response over tunnel ←──────────── ← sends data back
  → writes to caller's stdout
```

The binary serves double duty:

- **On the local machine**, it runs as a systemd socket-activated server that reads the real clipboard.
- **On the remote machine**, it installs as symlinks named `xclip` and `wl-paste` that forward requests over the socket that `~/.ssh/clipboard.sock` currently points at. If no forward is available (no SSH session, or nothing live), it falls through to the real binaries.

### Self-healing

Each connection gets its own uniquely-named socket in `~/.ssh/clipboard.d/`,
so two overlapping sessions (e.g. a laptop that suspends/resumes, or two
concurrent logins) never clobber each other's forward — the old scheme's
single shared `clipboard.sock` could. `clipboard-over-ssh reconcile` keeps
the `~/.ssh/clipboard.sock` symlink pointed at whichever forward actually
answers, garbage-collecting dead ones as it goes. If the shim's request
fails, it reconciles and retries once before falling through — loudly, via
stderr and a small log — rather than hanging or silently reverting to the
remote clipboard. Run `clipboard-over-ssh status` (or drop it in your shell
rc) for a one-line forward-health check at login time.

## Setup

### 1. Install on the Local Machine

Download or build the binary, then run:

```bash
clipboard-over-ssh install-local
```

This installs systemd user units (`~/.config/systemd/user/clipboard-over-ssh.socket` and `clipboard-over-ssh@.service`) and enables the socket at `~/.ssh/clipboard-over-ssh.sock`.

Requires `xclip` or `wl-paste` to be installed locally.

### 2. Configure SSH

Add to your `~/.ssh/config` (see [`ssh-config.snippet`](ssh-config.snippet)):

```
Host *.example.com
    PermitLocalCommand yes
    LocalCommand sh -c 'test -x ~/.local/bin/clipboard-over-ssh && exec ~/.local/bin/clipboard-over-ssh ensure-forward %r %h %p; true'
```

Each new connection's `LocalCommand` forwards its own uniquely-named socket
into `~/.ssh/clipboard.d/` on the remote and detaches a `reconcile` there to
adopt it onto the `~/.ssh/clipboard.sock` symlink. This works when the
remote home directory path matches the local one (e.g., both `/home/tim`).

**Do NOT** add a `RemoteForward` for `clipboard.sock` or
`StreamLocalBindUnlink yes` — that is the old single-shared-socket scheme,
and mixing it with the above re-creates the clobber bug this design
removed. Note also that `ClearAllForwardings` does **not** suppress
`LocalCommand`-driven forwards; hosts that must never get one need
`PermitLocalCommand no`.

### 3. Install on Remote Machines

Copy the binary to the remote machine (matching its architecture), then run:

```bash
clipboard-over-ssh install-remote
```

This copies the binary to `~/.local/bin/` and creates `xclip` and `wl-paste` symlinks pointing to it, and creates `~/.ssh/clipboard.d/` (the receiving side of forwards — most machines are both sides, so `install-local` creates it too). `~/.local/bin` must be in your `PATH` (it usually is by default).

The shims connect to `~/.ssh/clipboard.sock` by default (a symlink maintained by `reconcile`). Override with `$CLIPBOARD_SOCK` if needed.

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
