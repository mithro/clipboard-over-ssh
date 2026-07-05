# Self-healing clipboard forwards: unique sockets + symlink reconciliation

Date: 2026-07-06
Status: draft (pending review)
Branch: `self-healing-forward`

## Problem

The clipboard forward uses a single well-known socket path on the remote
(`~/.ssh/clipboard.sock`), and every failure mode observed in production is a
symptom of multiple writers contending for that one path:

1. **Slave clobber** — with `ControlMaster auto`, a second multiplexed
   connection re-requesting the config `RemoteForward` +
   `StreamLocalBindUnlink` unlinks the master's live socket and rebinds a dead
   one. The `clipboard-forward-on-master` bash guard exists solely to prevent
   this.
2. **Stale-socket lies** — the guard's `[ -S ]` test checks *existence*, not
   *liveness*. After a master dies uncleanly (laptop suspend, wifi roam), the
   stale control socket makes the next connection think it is a slave, so it
   never requests the forward (observed 2026-07-05: live master with no
   clipboard forward; shim silently fell through to real xclip, "Can't open
   display").
3. **Bind failures** — a stale `clipboard.sock` file blocks a new bind unless
   the remote sshd sets `StreamLocalBindUnlink yes` (ten64 does; x1c-work does
   not, so the laptop-bound direction fails on every connection with
   "Warning: remote port forwarding failed").
4. **Silence** — the shim treats any dial failure as "no SSH session" and
   falls through without a word, so breakage is discovered by a human minutes
   or days later.

## Core idea

Forward each connection to a **unique** socket path and make the well-known
path a **symlink** maintained by the tooling. Contention disappears by
construction; the read side reconciles the symlink to whatever is alive.
(Same pattern as ssh-agent-mux: stable name in front, ephemeral sockets
behind.)

Because ssh_config tokens cannot express per-connection uniqueness, the
forward moves out of the config entirely: it is requested at runtime with
`ssh -O forward` by a `LocalCommand` hook, which fires exactly once per new
mux master (verified empirically 2026-07-05: `LocalCommand` does not run for
mux slave connections).

Consequences:

- The `Match exec` guard is **deleted, replaced by nothing** — no master/slave
  decision exists any more because no path is ever requested twice.
- `StreamLocalBindUnlink` is no longer needed on either end (incidentally
  fixing the x1c-work direction without touching its sshd).
- Stale files cannot block anything; they are garbage-collected on the remote
  by the same routine that maintains the symlink.

## Components

### Local/initiating machine (any host ssh'ing out)

`clipboard-over-ssh ensure-forward <user> <host> <port>`

Triggered by `LocalCommand` (see config below). Self-backgrounds immediately
(fork+exit) so ssh startup latency is unaffected. Then:

1. Generate a unique name
   `~/.ssh/clipboard.d/<local-shorthostname>.<nonce>.sock` and request
   `ssh -O forward -R <unique>:$HOME/.ssh/clipboard-over-ssh.sock -l <user> -p <port> <host>`.
   Every master unconditionally creates its own forward — uniqueness makes
   this collision-free, and it guarantees the clipboard survives any *other*
   master exiting. (Piggybacking on an existing live socket was considered
   and rejected: with reverse heal out of scope, a master without its own
   forward has no repair path once the socket it borrowed dies.)
2. Run `ssh -l <user> -p <port> <host> clipboard-over-ssh reconcile --adopt
   <name>` so the symlink points at the newest connection (and dead sockets
   are collected).
3. Log one line to `~/.ssh/clipboard-over-ssh.log` on any action or failure.

Timeouts: 10 s total budget. All nested ssh calls set
`CLIPBOARD_OVER_SSH_INTERNAL=1` (recursion belt-and-braces; recursion is
already structurally impossible because nested calls are mux slaves or `-O`
control operations, neither of which runs `LocalCommand`).

### Remote machine (where panes live)

`clipboard-over-ssh reconcile [--adopt <name>]` — the single shared
primitive. Under `flock ~/.ssh/clipboard.d/.lock`:

1. Scan `~/.ssh/clipboard.d/*.sock`; probe each with a real `connect()`
   (2 s timeout); **unlink** any that refuse (`ECONNREFUSED` = provably
   stale). Only paths inside `clipboard.d/` matching `*.sock` are ever
   unlinked.
2. Choose a live socket: `--adopt <name>` if given and alive, else the most
   recently created live one. If the `~/.ssh/clipboard.sock` symlink does not
   point at the choice, flip it atomically (create temp symlink, `rename(2)`).
   Migration: if `clipboard.sock` is a regular (stale) socket file from the
   old scheme, remove it and replace with the symlink.
3. Exit 0 with `live <name>` on stdout, or exit 1 with `none`.

`clipboard-over-ssh status` — thin wrapper for login shells: runs reconcile,
prints one human line: `clipboard: OK (via x1c-work)` or
`clipboard: no live forward`. Never fails the login shell (always exit 0),
completes within ~2 s worst case.

**Shim self-heal** (change to existing client): when a clipboard read fails
to dial (or the socket answers but the protocol errs), run reconcile, retry
the read once, and only then fall through to the real binary — now with a
loud one-line stderr message
(`clipboard-over-ssh: no live forward (N stale sockets cleaned); falling back to real xclip`)
and a log line. The common heal (symlink hop to another live master's socket)
involves **no ssh at all**.

Concurrent pastes during breakage serialize on the flock; reconcile is
idempotent; symlink flips are atomic — no grace periods or backoff needed.

### Explicitly out of scope (removed during design)

- **Reverse heal** (remote sshing back to the laptop to re-request the
  forward): removed for now. If zero live forwarded sockets exist there is no
  tunnel to paste through anyway; the failure is loud and the next new
  connection repairs it. Revisit only if practice shows a need.
- Creating new master connections; the write/copy direction; per-connection
  `.meta` sidecar files (origin is derivable from the socket filename for
  display purposes).

## Configuration changes (rcfiles)

Delete:

```
Match host *.mithis.com exec "~/rcfiles/ssh/bin/clipboard-forward-on-master %h %p"
    RemoteForward ${HOME}/.ssh/clipboard.sock ${HOME}/.ssh/clipboard-over-ssh.sock
    StreamLocalBindUnlink yes
```

and the `ssh/bin/clipboard-forward-on-master` script.

Add:

```
Host *.mithis.com
    PermitLocalCommand yes
    LocalCommand ~/.local/bin/clipboard-over-ssh ensure-forward %r %h %p
```

The HAOS hosts block (`ha.welland` / `ha.monarto`) additionally gets
`PermitLocalCommand no` (they already have `ClearAllForwardings yes`).

zprofile gains one guarded line (after the agent-mux block, before the tmux
exec): run `~/.local/bin/clipboard-over-ssh status` if the binary exists,
else silent no-op.

`install-local` / `install-remote` create `~/.ssh/clipboard.d/` (0700).

## Error handling & observability

- Every heal/GC/flip/forward action and every failure appends one timestamped
  line to `~/.ssh/clipboard-over-ssh.log` (truncate when >1 MB). The
  2026-07-05 incident was expensive because breakage was silent; it must
  never be silent again.
- All network operations carry explicit timeouts (probe 2 s, ensure-forward
  10 s). A hung heal can never hang a paste: fallthrough still happens.
- `status` gives login-time visibility; the shim stderr line gives paste-time
  visibility.

## Testing

First tests in the repo (`go test`, stdlib only, CGO disabled):

- `reconcile`: decision matrix against fake sockets in a temp dir — live
  listener / stale file / mixed / none; adopt-name vs newest; symlink flip
  atomicity; migration from regular-file `clipboard.sock`; GC never touches
  paths outside `clipboard.d/`.
- `ensure-forward`: state machine with an injected `runner` interface faking
  the ssh invocations (records commands, returns scripted results) — no real
  network in unit tests.
- Shim: dial-fail → reconcile → retry → fallthrough ordering, with a fake
  socket server.
- Manual end-to-end plan (x1c ↔ ten64): kill master uncleanly → reconnect →
  verify auto-repair; kill the forward mid-session with a second live master
  present → paste → verify sshless symlink hop; zero live masters → verify
  loud fallthrough; two panes pasting simultaneously → single reconcile.

## Rollout

1. Implement on branch `self-healing-forward` in this repo (worktree
   `.worktrees/self-healing-forward` on ten64), small discrete commits.
2. CI builds artifacts → `rcfiles/ssh/bin/update-clipboard-over-ssh`
   refreshes the cached binaries → `setup.sh` installs.
3. rcfiles ssh-config change lands **after** binaries are deployed (config
   references the binary; wrong order breaks all `*.mithis.com` ssh).
4. Validate on x1c + ten64 (welland) per the manual plan, then roll to
   monarto ten64 and remaining remotes.
5. The repaired-by-hand forward currently live on ten64 keeps working
   throughout (old scheme) until the config flip; the migration step in
   reconcile absorbs the leftover `clipboard.sock` file.
