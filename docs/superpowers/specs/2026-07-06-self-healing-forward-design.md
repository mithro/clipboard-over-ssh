# Self-healing clipboard forwards: unique sockets + symlink reconciliation

Date: 2026-07-06
Status: draft v2 (post adversarial review, pending user review)
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
mux master created by plain `ssh` (verified empirically 2026-07-05/06:
`LocalCommand` does not run for mux slaves; it DOES run for remote-command
and `-W`/ProxyJump masters; `scp`/`sftp` hard-code `PermitLocalCommand=no` —
see Known limitations).

Consequences:

- The `Match exec` guard is **deleted, replaced by nothing** — no master/slave
  decision exists any more because no path is ever requested twice.
- `StreamLocalBindUnlink` is no longer needed on either end (incidentally
  fixing the x1c-work direction without touching its sshd).
- Stale files cannot block anything; they are garbage-collected on the remote
  by the same routine that maintains the symlink.

## Liveness model (review finding I1)

A bare `connect()` is NOT a liveness test for a forwarded socket: sshd
accepts the connection even when the tunnel's far end is gone (half-open TCP
after laptop suspend persists until sshd notices — hours with default
`ClientAliveInterval 0`). Therefore:

- **Live** (selectable): completes an application-level round trip — send
  `TARGETS`, receive a valid `OK`/`ERR` header — within 2 s.
- **Dead** (collectable): `connect()` refused (`ECONNREFUSED`) **and** file
  age > 5 s (see GC race, below). Only these are unlinked.
- **Undead** (timeout: connects but no response): not selectable, not
  collectable — skipped. It resolves to dead once sshd reaps the session.

Deployment recommendation (rollout step): set `ClientAliveInterval 30` /
`ClientAliveCountMax 4` on receiving sshds so undead listeners become
collectable in ~2 minutes, not hours.

## Components

### Local/initiating machine (any host ssh'ing out)

`clipboard-over-ssh ensure-forward <user> <host> <port>`

Triggered by `LocalCommand` (see config below). **fd discipline (review
finding C2):** the process immediately `setsid()`s and reopens fds 0/1/2 to
`/dev/null` before any other work, and never writes to inherited stdout —
LocalCommand's stdout is interleaved into the connection's data stream for
`-W`/ProxyJump masters (verified), and git/rsync use stdout as the protocol
pipe; one stray byte corrupts the session. Nested ssh children get
stdout/stderr redirected to the log. Holding inherited fds would also delay
EOF for the parent session by up to the 10 s budget.

Sequence (total budget 10 s):

1. **Synchronously** (before backgrounding; it is a local control-socket
   round trip, milliseconds — review finding I5): generate
   `~/.ssh/clipboard.d/<local-shorthostname>.<nonce>.sock` and request
   `ssh -O forward -R <unique>:$HOME/.ssh/clipboard-over-ssh.sock -l <user> -p <port> <host>`.
   `-O forward` returning success guarantees the listener is bound (verified).
   Every master unconditionally creates its own forward — uniqueness makes
   this collision-free, and it guarantees the clipboard survives any *other*
   master exiting. (Piggybacking on an existing live socket was considered
   and rejected: with reverse heal out of scope, a master without its own
   forward has no repair path once the socket it borrowed dies.)
   If the bind fails with ENOENT (missing `clipboard.d/` on a
   never-installed remote — review finding I2), run
   `ssh -l <user> -p <port> <host> mkdir -p -m 700 .ssh/clipboard.d` and
   retry once — self-bootstrapping, not install-order-dependent.
2. In the background: run
   `ssh -l <user> -p <port> <host> '~/.local/bin/clipboard-over-ssh reconcile --adopt <name>'`
   so the symlink points at the newest connection and dead sockets are
   collected. The binary path is spelled out because `~/.local/bin` is not
   on the non-interactive sshd PATH (verified — review finding I3).
3. Log one line to `~/.ssh/clipboard-over-ssh.log` on any action or failure.

All nested ssh calls set `CLIPBOARD_OVER_SSH_INTERNAL=1` (recursion
belt-and-braces; recursion is already structurally impossible because nested
calls are mux slaves or `-O` control operations, neither of which runs
`LocalCommand`).

### Remote machine (where panes live)

`clipboard-over-ssh reconcile [--adopt <name>]` — the single shared
primitive. Under `flock ~/.ssh/clipboard.d/.lock` (acquire with ~3 s
timeout; on timeout, exit reporting "busy" rather than pile up):

1. Scan `~/.ssh/clipboard.d/*.sock`; probe all **in parallel** per the
   liveness model above; unlink dead ones. Only paths inside `clipboard.d/`
   matching `*.sock` are ever unlinked, never anything a symlink points at
   outside it, and never files younger than 5 s (a just-`bind()`ed socket
   briefly refuses before `listen()` — unlinking it would orphan the forward
   on an invisible inode; review finding M1).
2. Choose a live socket: `--adopt <name>` if given and live, else the one
   with the newest mtime (= bind time). **Semantics: last writer wins** — on
   a remote shared by several laptops, pastes follow whichever connected (or
   healed) most recently; this matches the old scheme's rebind behavior and
   is intended (review finding M8). If the `~/.ssh/clipboard.sock` symlink
   does not point at the choice, flip it atomically (temp symlink +
   `rename(2)`).
   Migration (review finding C1): if `clipboard.sock` is a regular socket
   file from the old scheme, **probe it first**; replace it with the symlink
   only if it refuses connection. A live old-scheme forward is left
   untouched (and reported as the live path) so a mixed-version fleet keeps
   working mid-rollout.
3. Exit 0 with `live <name>` on stdout, or exit 1 with `none`.

`clipboard-over-ssh status` — thin wrapper for login shells: only acts when
`$SSH_CONNECTION` is set (a local desktop login has a real clipboard; alarms
there are noise — review finding M3). Runs reconcile; on "none", retries for
~1 s (a fresh login races the connection's own backgrounded reconcile —
review finding I5) before printing `clipboard: no live forward`. Success
prints `clipboard: OK (via x1c-work)` (origin parsed from the socket
filename). Always exits 0; bounded by the flock timeout + probe deadline,
~5 s absolute worst case, typical <100 ms.

**Shim self-heal** (change to existing client): the normal paste path gets
read/write deadlines (5 s — today `ReadResponse` can block forever on an
undead socket, violating "a hung heal can never hang a paste"; review
finding I1). On dial failure, deadline expiry, or protocol error: run
reconcile, retry the read once, and only then fall through to the real
binary — with a loud one-line stderr message
(`clipboard-over-ssh: no live forward; falling back to real xclip`) and a
log line. The common heal (symlink hop to another live master's socket)
involves **no ssh at all**.

Concurrent pastes during breakage serialize on the flock; reconcile is
idempotent; symlink flips are atomic.

### Explicitly out of scope (removed during design)

- **Reverse heal** (remote sshing back to the laptop to re-request the
  forward): removed for now. If zero live forwarded sockets exist there is no
  tunnel to paste through anyway; the failure is loud and the next new
  connection repairs it. Revisit only if practice shows a need.
- Creating new master connections; the write/copy direction; per-connection
  `.meta` sidecar files (origin is derivable from the socket filename for
  display purposes).

## Known limitations (accepted, documented)

- **scp/sftp-created masters** hard-code `PermitLocalCommand=no` (verified):
  if an scp/sftp is the first connection to a host, that master gets no
  forward and later interactive slaves cannot add one. Pre-existing hole
  (the old scheme's config forward was also cleared for scp), residual by
  design; the failure is now loud instead of silent. rsync/git/ProxyJump
  masters DO fire LocalCommand and are covered.
- **`-S none` / `ControlMaster no` sessions**: previously the config
  `RemoteForward` applied; now `-O forward` has no master to attach to and
  the session gets no forward (log-only failure). Documented opt-out cost.
- **ProxyJump `%j` ControlPath variant**: the commented-out
  `master-%j-%r@%h:%p` ControlPath (rcfiles ssh/config TODO) would make the
  master socket path unpredictable for ensure-forward's re-invocation. A
  warning comment goes at both sites; revisit if `%j` is ever restored.
- **Undead window**: between a laptop dying and the remote sshd reaping the
  session, pastes time out (5 s) and fall through loudly rather than heal —
  bounded by `ClientAliveInterval` on the receiving sshd.

## Configuration changes (rcfiles)

Delete:

```
Match host *.mithis.com exec "~/rcfiles/ssh/bin/clipboard-forward-on-master %h %p"
    RemoteForward ${HOME}/.ssh/clipboard.sock ${HOME}/.ssh/clipboard-over-ssh.sock
    StreamLocalBindUnlink yes
```

and the `ssh/bin/clipboard-forward-on-master` script.

Add (existence-guarded so machines that pull rcfiles before running
`setup.sh` don't spray errors on every ssh — review finding I6; an old
binary would print usage errors, a missing one "not found", on the stderr of
every connection):

```
Host *.mithis.com
    PermitLocalCommand yes
    LocalCommand sh -c 'test -x ~/.local/bin/clipboard-over-ssh && exec ~/.local/bin/clipboard-over-ssh ensure-forward %r %h %p; true'
```

With a comment noting that `ClearAllForwardings` does NOT suppress
LocalCommand-driven forwards (semantics change — review finding M5): hosts
that must not receive forwards need `PermitLocalCommand no`, which the HAOS
block (`ha.welland` / `ha.monarto`, already `ClearAllForwardings yes`) gets
explicitly.

zprofile gains one guarded line (after the agent-mux block, before the tmux
exec): run `~/.local/bin/clipboard-over-ssh status` if the binary exists,
else silent no-op.

Documentation/tooling updated in the same change (review finding M6 — all
currently teach the old scheme, and following them would re-create the
clobber bug): `install-local`'s printed ssh-config advice, README.md
"Configure SSH", `ssh-config.snippet`, repo CLAUDE.md.
`install-local`/`install-remote` create `~/.ssh/clipboard.d/` (0700).

## Error handling & observability

- Every heal/GC/flip/forward action and every failure appends one timestamped
  line (single `write(2)` on an `O_APPEND` fd — many concurrent writers) to
  `~/.ssh/clipboard-over-ssh.log`; at >1 MB rotate to
  `clipboard-over-ssh.log.old` (truncation would discard exactly the evidence
  generated during an incident — review finding M7). The 2026-07-05 incident
  was expensive because breakage was silent; it must never be silent again.
- All network operations carry explicit timeouts (liveness probe 2 s, shim
  paste deadline 5 s, ensure-forward budget 10 s, flock acquire 3 s). A hung
  anything can never hang a paste: fallthrough still happens.
- `status` gives login-time visibility; the shim stderr line gives paste-time
  visibility.

## Testing

First tests in the repo (`go test`, stdlib only, CGO disabled):

- `reconcile`: decision matrix against fake sockets in a temp dir — live
  (responding server) / undead (accepting, never responding) / stale
  (refusing) / too-young-to-collect / mixed / none; adopt-name vs newest;
  symlink flip atomicity; migration cases (live regular file left alone,
  dead regular file replaced); GC never touches paths outside
  `clipboard.d/`; flock timeout behavior.
- `ensure-forward`: state machine with an injected `runner` interface faking
  the ssh invocations (records commands, returns scripted results) — no real
  network in unit tests; ENOENT→mkdir→retry path; fd discipline (no stdout
  writes) asserted by construction.
- Shim: dial-fail / deadline-expiry / protocol-error → reconcile → retry →
  fallthrough ordering, with fake socket servers (including an undead one).
- Manual end-to-end plan (x1c ↔ ten64): kill master uncleanly → reconnect →
  verify auto-repair; kill the forward mid-session with a second live master
  present → paste → verify sshless symlink hop; zero live masters → verify
  loud fallthrough within the deadline; two panes pasting simultaneously →
  single reconcile; scp-first-master → verify documented limitation; ProxyJump
  through underwood.mithis.com → verify no stream corruption (C2 regression
  test).

## Rollout

1. Implement on branch `self-healing-forward` in this repo (worktree
   `.worktrees/self-healing-forward` on ten64), small discrete commits.
2. CI builds artifacts → `rcfiles/ssh/bin/update-clipboard-over-ssh`
   refreshes the cached binaries → `setup.sh` installs.
3. **Precondition for the config flip: new binaries deployed to ALL
   machines** — initiators need `ensure-forward`; remotes need the new
   `reconcile`/shim (an old-binary remote fails the nested reconcile and
   heals nothing, silently — review finding I6). rcfiles is shared, so the
   config change activates fleet-wide on next pull + cannot be staged per
   host; the existence-guarded LocalCommand line keeps not-yet-setup
   machines harmless.
4. rcfiles ssh-config change lands last; recommend `ClientAliveInterval 30`
   / `ClientAliveCountMax 4` on receiving sshds in the same change.
5. Validate on x1c + ten64 (welland) per the manual plan, then monarto.
6. The repaired-by-hand forward currently live on ten64 keeps working
   throughout: reconcile's migration step probes it and leaves it in place
   while live (C1), replacing it with the symlink only once it dies.
