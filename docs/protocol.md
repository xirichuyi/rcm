# Wire protocol v1

Transport: ordered, bidirectional bytes on one OpenSSH process's stdin/stdout. stderr is
reserved for diagnostics. No per-file SSH command, RPC or request/response read API.

Each frame is `uint32 big-endian JSON-length | UTF-8 JSON | binary body`.
Maximum JSON header: 32 MiB; maximum single body: 1 TiB. The complete manifest is bounded
by the header limit. Receive buffers hold metadata and small copy buffers, not whole files.
Backpressure uses disk-spooled received entries; queued metadata can grow under sustained
backlog. There is no total disk quota in V1. Resource exhaustion is an error, not an ACK.

Example metadata (whitespace for readability):

```json
{
  "type": "PUSH",
  "generation": 12,
  "path": "src/main.go",
  "entry": {"kind": "file", "size": 123, "hash": "<64 hex SHA-256>", "mode": 420},
  "base": {"kind": "file", "hash": "<previous SHA-256>", "mode": 420},
  "body": 123
}
```

`generation` is a per-sender connection sequence for diagnostics; durable content identity,
not the generation or mtime, determines correctness. Supported kinds are file, dir, link;
an empty kind denotes absence. Equal means same kind, hash, normalized mode and link target.
Regular-file body length must equal entry.size; SHA-256 is checked before applying it.
Links carry their target in metadata and SHA-256 of the target bytes. Directories have no
body. Regular modes are 0644/0755; directory mode is 0755. Ownership is never transferred.

## Startup

1. Client → HELLO `{version:1, config:...}`.
2. Agent validates the version, loads policy, registers watches and scans.
3. Agent → HELLO with effective config and ordered ignore sources, then MANIFEST.
4. Client registers local watches, scans, compares its baseline and returns **one WANT**
   containing every remote path whose content it needs.
5. Agent → ENTRY for each requested path, followed by END. Entries are read again before
   sending; they may be newer than the manifest. Watcher events cover subsequent changes.
6. Client finishes three-way reconciliation, then pipelines any local-only PUSH entries.

A missing path in a complete manifest is a tombstone relative to the saved baseline;
ignored paths are excluded from deletion decisions. Creating parents precedes children;
deleting children precedes parents. Rename is delete+create, not a speculative inode match.

## Steady state

- Agent → ENTRY: current contents or deletion, after event coalescing.
- Client → PUSH: candidate plus the expected remote base.
- Agent → ACK: the requested change was applied/revalidated; client advances its baseline.
- Agent → CONFLICT: current remote snapshot after failed validation/application.

An event batch is an ordered run of ENTRY/PUSH frames. There is no additional batch RPC.
Multiple PUSH paths may be outstanding simultaneously; one outstanding upload per path
prevents an old ACK from being confused with a new upload. Receive-to-disk runs separately
from the engine so simultaneous large sends cannot deadlock the duplex transport.

Malformed metadata, invalid paths, incompatible versions, checksum mismatch, truncated
bodies, policy changes, watcher overflow or I/O errors end the session. The client logs the
error, reconnects with backoff, and reconciles. Diagnostics use stderr instead of a separate
ERROR frame. SSH ServerAlive replaces application PING. There are no resume offsets or
persisted event replay log in V1; an interrupted file is retransferred in full.

The SSH peer is the authenticated user's agent. The protocol still validates all entry
paths and uses root-scoped filesystem capabilities; absolute, noncanonical, traversal and
reserved paths are rejected. There is no second authentication scheme over SSH.
