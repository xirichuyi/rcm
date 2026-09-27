# Remote Code Mirror V1 — architecture decision record

Status: implementation contract. RCM is a disk mirror, never a remote filesystem.

## Components and transport

A single Go 1.25+ executable exposes `connect`, `agent`, and lifecycle/conflict commands.
The local supervisor calls the installed OpenSSH client, inheriting aliases, identities,
ports, ProxyJump, known_hosts, agent forwarding policy and authentication from ssh_config.
We do not parse ssh_config or weaken host-key checking. ControlMaster is optional and
inherited from user configuration; no custom multiplexing socket is required. There are
setup round trips per connection, but no per-file request/response loop.

Release builds embed Linux amd64/arm64 agents into macOS amd64/arm64 and Linux clients.
The client probes uname, uploads its embedded agent to a version/content-addressed private
cache, and execs `rcm agent --root ...` on a persistent SSH stdin/stdout stream. Development
builds accept an agent directory. No remote Go, rsync, Python or daemon service is required.
Remote shell parameters are POSIX-quoted. Protocol output exclusively uses stdout; logs stderr.

## Wire protocol and reconciliation

Versioned frames: 4-byte big-endian JSON header length, JSON metadata, then exactly the
announced binary body bytes. Headers and bodies have bounds. File bodies are streamed to
private disk spool files and SHA-256 checked, never accumulated in RAM. Messages include
HELLO, MANIFEST, WANT, ENTRY, END, PUSH, ACK, CONFLICT. Streams are ordered, writes
serialized. Session generation is monotonic within one connection; SHA-256 identity and
persistent common baseline provide correctness across sessions, including deletions.

Both sides install watchers before enumeration. Agent sends a complete metadata manifest;
client returns one WANT list; agent streams requested bodies and END, then pushes changes.
Client compares baseline/local/remote (including tombstones), sends local-only edits with
expected remote identity, preserves divergent versions as conflicts. Restart repeats this
three-way reconciliation. No network polling and no trust in mtime as content identity.
SSH ServerAlive detects dead links; reconnect uses capped exponential delay. Overflow or
watch failure causes a fresh reconciliation rather than assuming nothing changed.

## Filesystem and event pipeline

fsnotify provides inotify on Linux and kqueue on macOS. Every directory is registered;
new subtrees are scanned after registration. The event collector never blocks on network
I/O. A fixed debounce window (150ms default) coalesces paths without starvation under
continuous writes. Changed subtrees are rescanned; a full scan is reserved for startup,
reconciliation, ignore-rule changes and overflow. Hashes, not sleeps or suppression timers,
prevent feedback: applying an ENTRY advances the baseline; matching watcher observations
are no-ops. Rename is delete+create, including directory trees. Directory deletion uses
non-recursive removal so unknown/untracked local contents are never silently removed.

SHA-256 covers file bytes or symlink target; entry identity also includes type and execute
permission. mtime and size are informational. Intermediate file states may be observed;
unstable reads are retried, eventual final state is obtained from queued events.

## Conflict and write safety

Base A / local C / remote B is always a conflict. A conflict is durable before acknowledging
or changing a baseline. Both versions are retained in the root’s reserved private recovery directory; later remote versions update the remote snapshot without replacing
local edits. Delete/modify and ancestor file/directory conflicts also block destructive
writes. A failed or interrupted transfer cannot advance the baseline. Repeated identical
changes are idempotent. Explicit `resolve --remote/--local` records the chosen identities;
if either side changed again, resolution is rejected/reconflicted instead of force applied.

All replacement content is staged, checked and fsynced, then atomically renamed. The
actual previous inode is moved to a private recovery path and rehashed. Installation uses
Linux renameat2(RENAME_NOREPLACE) / Darwin renameatx_np(RENAME_EXCL), so any newly appearing
destination causes a conflict instead of an overwrite. Failed transactions restore the
old path only if still absent; crash recovery follows the durable journal. Directory
deletion uses descriptor-relative rmdir, never recursive removal or unlink of a file.
This deliberately allows a brief missing-path interval between capture and installation.
It is NOT a linearizable content CAS: already-open external file descriptors can continue
writing the archived inode, and their changes remain recoverable there. This boundary is
explicit; use read-only uploads for the most conservative remote-primary workflow.
Resolution is manual; no automatic merge. Local writes may be disabled independently.

## Boundaries and symlinks

All protocol paths are canonical relative slash paths: no absolute paths, '..', empty
components, reserved internal names or NUL. Operations use Go os.Root to enforce the
root capability against concurrent symlink escapes. Parent symlinks are rejected, not
traversed. Symlinks themselves are mirrored, never followed when scanning. Absolute or
lexically escaping symlink targets are rejected (stronger than merely not following them).
Special files are skipped; mode bits are normalized and ownership is never imported.
State/spool/recovery directories have owner-only permissions. One local worker per project
is protected by an OS lock; overlapping local roots and ambiguous project names are rejected.

## Ignore and Git

Rules: built-in defaults, hierarchical .gitignore, then root .rcmignore, then configured
patterns; later matches win, `!` negation supported, ignored parents must be re-included.
Internal RCM temporary paths are unconditionally excluded. `.git` is unconditionally
excluded unless --include-git; when enabled it is remote-to-local only. This is an opt-in
full metadata mirror, not a fabricated local Git repository. It can be costly for large
object stores and is not a transactionally consistent live Git database. Do not run local
Git writes against it. Partial metadata would produce misleading index/object references;
local git-init would invent a history, so neither is the default. Linked worktree .git
files are not supported as self-contained Git mirrors.

## Lifecycle, configuration, performance

`connect` registers and starts a foreground worker by default; --background and `start`
run detached. status/logs/stop/remove operate on private XDG state. remove unregisters,
retaining mirror and recovery data. .rcm.yaml and XDG config.yaml expose only ignore,
debounce, local_write.enabled and conflict.strategy=manual; flags take precedence.
Project configuration is read from the remote root during handshake.

Target: 1k–20k source files; 10–500KB edits normally one debounce + one network flight +
disk I/O. <500ms–1s is a target, not a claim without WAN measurement. No idle scans over
SSH. Streamed content means a 100MB file does not allocate 100MB in memory. SSH compression
is optional through ssh_config; no custom compression in V1. macOS kqueue descriptor
limits and Linux inotify watch limits must produce actionable errors.

## Validation

Unit tests: framing limits/truncation, ignore precedence, hash/type identity, traversal,
symlink escapes, three-way decision table, atomic replacement and safe deletion.
Integration tests over duplex pipes: initial sync, CRUD/rename/directories, 100-file bursts,
10/100MB streaming, concurrent edits, feedback suppression, disconnect/reconcile with
20 modifications + 5 deletes + 10 creates, restart persistence and conflict resolution.
CLI tests use a fake ssh executable to exercise installation and argument quoting without
requiring real credentials. Cross-compile all four target platforms. Real WAN/macOS runtime
measurements remain separate from tests runnable on a Linux development host.

## Implementation references

- [Go os.Root](https://pkg.go.dev/os#Root): root-scoped operations; Go 1.25 added required
  write operations. Directory-fd operations implement no-clobber rename and directory-only removal.
- [fsnotify](https://github.com/fsnotify/fsnotify): directories need recursive registration;
  Linux inotify and macOS kqueue limits remain operational constraints.
- [OpenSSH ssh(1)](https://man.openbsd.org/ssh) and [ssh_config(5)](https://man.openbsd.org/ssh_config):
  system transport, existing aliases and optional user-managed multiplexing.

See README for the implemented ignore syntax subset, linked-worktree limitations, retention
policy and the explicit missing-path / already-open-descriptor write tradeoffs. These are
V1 boundaries, not guarantees silently delegated to a later implementation.
