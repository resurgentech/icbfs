# icbfs architecture

This is the settled design, synthesized from the discussion in
`questions.md`. That file has the rationale, pros/cons, and rejected
alternatives for each decision; this document states what was decided.

## Goals and scope

icbfs is a filesystem built directly on object storage, in the spirit of
JuiceFS but deliberately simpler: no chunking, no bespoke block format — an
object is an object. The two things it's built around:

- **Instant snapshotting**, by requiring the underlying object store to
  have versioning enabled and relying on that native version history rather
  than building a separate versioning layer.
- **A full POSIX-like metadata set**, including hard links and symlinks,
  plus a path to good Windows support alongside the POSIX/Linux path.

**Backend scope is deliberately narrow:** MinIO (S3 API) and Azure Blob
Storage are the only first-class object stores. No generic multi-cloud
abstraction is being built.

## Core object model

- **No chunking.** Each file's content is a single whole object. Writing a
  file means writing one object.
- **Blobs are identified by UUID, not by filename.** Specifically **UUIDv7**
  — its embedded 48-bit millisecond timestamp gives a free, zero-storage
  `birthtime` and makes keys naturally time-ordered.
- **Directory blocks are objects too.** A directory block's body is a set
  of rows: `name → UUID (+ type: file / directory / symlink)`. Nothing more
  — metadata does not live here (see below).
- **Root blocks are versioned objects**, same as directories and files.
  Since a root has no parent to be referenced from, roots are discovered by
  **naming convention** (e.g. `root/<fsname>`) rather than a random UUID.
  This also means **one object store can host multiple independent
  filesystems**, each anchored by its own named root.
- **Hard links** are simply multiple directory rows pointing at the same
  UUID — this falls out of the model for free, with no special-casing,
  because identity (UUID) is already decoupled from name. The link count
  (nlink) itself, however, needs real compare-and-swap protection against
  concurrent Link/Unlink calls, and — per the ETag limitation in the
  Concurrency section below — that can't live in the target's own object
  metadata the way the rest of this section implies. It lives instead in
  a tiny dedicated side object (`<uuid>.nlink`) whose *body* is the
  decimal count, so every change is a real content write and ETag CAS is
  meaningful again; reaching zero writes a tombstone value under the same
  CAS check before the target is physically deleted, so a concurrent Link
  can never race past a decrement that's already decided to delete. The
  target's own object metadata still carries a best-effort cached copy of
  nlink so `stat()` stays a single round trip in the common (never
  hardlinked) case.
- **Symlinks** store their target as the object's *content* (same shape as
  a tiny regular file); the row's type flag marks it as a symlink.

## Metadata model

Mutable POSIX metadata — **mode, uid, gid, nlink, ctime** — lives in the
blob's own **native object metadata** (S3/MinIO user-metadata via
`x-amz-meta-*`, Azure Blob metadata), not in the directory row and not
encoded in the key.

This single decision resolves several things at once:

- **Hard links stay consistent.** nlink and permissions live with the
  shared UUID, so multiple names pointing at one object can't drift out of
  sync the way per-row-duplicated metadata could.
- **Metadata changes don't propagate up the tree.** A `chmod`/`chown` is a
  metadata-only operation on the object itself (S3: `CopyObject` with
  `x-amz-metadata-directive: REPLACE`; Azure: `Set Blob Metadata`) — neither
  requires rewriting the object body, and neither touches the parent
  directory block.
- **ctime rides the snapshot mechanism for free.** On both backends, a
  metadata-only update creates a new object version when versioning is
  enabled — exactly like a content write — so permission/ownership changes
  show up in version history automatically, with no separate side channel.
- **Size isn't stored redundantly.** Size is the object's `Content-Length`.
  A single HEAD request returns size + all custom metadata together.
- **mtime and ctime are distinct fields, not the same value reported
  twice.** ctime (POSIX: "any change, content or metadata") is exactly
  the current version's `Last-Modified`, since a metadata-only update
  bumps it the same way a content write does. mtime (POSIX: "content
  changed") is *not* the same thing and needs its own explicitly stored
  field, set only by operations that actually rewrite content — otherwise
  a `chmod` would incorrectly look like a content change too. This was a
  bug in the first implementation pass (both were reported as the same
  value) caught by testing, not foreseen at design time.
- **atime is not tracked.** Neither backend bumps a timestamp on read, and
  tracking it ourselves would require a write on every read — exactly the
  propagation cost this model avoids elsewhere. Report it as equal to mtime.

Field size budget: mode/uid/gid/nlink/ctime/birthtime easily fit in a few
hundred bytes — well under S3's 2KB user-metadata cap and Azure's ~8KB
metadata cap. Generic, unbounded user xattrs are out of scope; this channel
is sized for the fixed POSIX field set, not arbitrary attribute storage.

## Concurrency: compare-and-swap on writes

All directory block and root block writes use **conditional PUT via ETag**
(`If-Match` on S3/MinIO and Azure Blob, both natively). A client reads a
block, modifies it, and writes back conditioned on the ETag it read; a
mismatch (someone else wrote first) is rejected and the client re-reads and
retries. No external lock service, no new infrastructure — both target
backends support this as a first-class primitive. This is proven correct
against real MinIO (not just assumed): a conditional write with a stale
ETag is rejected with 412, and the rejected write never applies.

**Important limitation, discovered through that same testing, not
foreseen at design time: ETag is a hash of the object's *body*.** A
metadata-only update does not change the body, so it does not change the
ETag either — two different metadata states can share the exact same
ETag. This means ETag-based CAS is only meaningful for writes that change
content; it provides **no protection at all** for a metadata-only update
racing against another metadata-only update (e.g. two concurrent nlink
changes). It was also confirmed that MinIO does not enforce
`DeleteObject`'s `If-Match` header at all — a conditional delete against
a deliberately stale ETag still succeeds. Directory/root block writes are
unaffected (every mutation there is a real content write, so ETag always
moves), but this ruled out using the same ifMatch mechanism for
metadata-only fields — see hard links, below, for the actual fix.

## Snapshots: point-in-time reconstruction

Reconstructing the tree as of some time T is a **timestamp-based walk**:
at each node (root, then each directory block, down to the leaf), list
that object's versions and pick the most recent one with `LastModified ≤ T`,
then descend into that version's children and repeat.

This works because:
- Every mutation — content or metadata-only — creates a new version with a
  server-assigned timestamp, on both backends.
- Deletes are generally non-destructive under versioning (S3 delete
  markers, Azure's version retention on delete), so a T before a later
  delete still resolves correctly.

Cost model, explicitly accepted:
- A single-path lookup (what did `/a/b/c.txt` look like at T) costs
  O(depth) — one version-list-and-scan per level. Cheap.
- Reconstructing the **entire** tree at T costs O(tree size) — there's no
  native "give me the version as of T" filter on either backend, so this
  is proportional to a full walk, not a cheap pointer dereference.
- Fallback if this proves too slow in practice: bolt on an external
  optimization (e.g. a cache of resolved as-of-T lookups, or
  periodically-materialized snapshot indexes) rather than changing the
  mechanism itself.

The alternative (storing explicit version-id pointers in directory rows,
git-tree style) was explicitly rejected: it would make point-in-time reads
cheaper, but at the cost of forcing every write anywhere to propagate a
pointer update up to the root — reintroducing the exact write-amplification
problem the metadata model above was built to avoid.

## Large directories: median-key B-tree sharding

Implemented. A directory block holds `name → UUID` rows up to a size cap.
On overflow, it splits into two children at the **median key actually
present in the block** — not a fixed alphabetic boundary (real names
cluster by prefix, e.g. `IMG_*`, `2026-*`, which skews fixed-range splits
badly) and not a hash (which would destroy lexicographic ordering). The
parent's rows become range pointers (`< median → child A`, `≥ median →
child B`), recursively, forming a standard B-tree keyed on filename. A
directory's own key (its UUID, or the filesystem's well-known root key)
always stays the entry point into its own tree, however many levels deep
that tree currently is — a split keeps the overflowing node's existing
key for the left half and mints a new UUID only for the right half,
except at the very top of a directory's own tree, where splitting instead
mints two new UUIDs for both halves and rewrites the fixed key as a new
Internal node, since that key's identity can't be reassigned to either
half.

- Lookup is a descent comparing the target name against each node's
  boundary — O(log n) in the number of splits, not a linear scan (this is
  why a doubly-linked overflow-block chain was rejected — O(n) point
  lookups don't scale).
- Lexicographic order is preserved, so sorted listing and prefix scans stay
  cheap (walk the relevant subtree instead of fanning out to every shard).
- Splitting on insert-overflow is implemented and proven (against real
  MinIO) to keep lookup, full listing, and removal all correct across
  however many shard levels a directory has grown. **Merging underfull
  nodes back together on delete is not implemented** — a directory that
  sharded once and then had most of its entries removed stays as tall as
  it grew, rather than being compacted back down. Not a correctness
  problem, just an accepted, explicit simplification.
- Concurrent writers racing on the same split are handled by retrying the
  *whole* top-down operation from scratch on any lost CAS race at any
  level, rather than per-node retries — simpler to reason about, at the
  cost of occasionally orphaning an already-written split half from an
  abandoned attempt (harmless: nothing ever comes to reference it).
- What this actually solves is **write amplification**, not storage
  capacity — a directory block is cheap to store even at a million rows
  (~50-100 bytes/row, well within either backend's object size limits); the
  real cost is rewriting a whole large block for one small change. Split
  thresholds should be tuned around mutation cost, not object size — the
  current threshold is deliberately small (8 entries) to make splitting
  easy to exercise in tests; production tuning is a separate, later
  concern.

## FUSE inode numbers

`st_ino` is derived from the UUID via a **high-quality 64-bit hash** (not
naive truncation, to minimize collision clustering). This is stateless — no
allocation table, no persistence.

Inode numbers only need to be unique and stable **within one running
mount's lifetime** — the kernel combines `st_ino` with the mount's `st_dev`
for system-wide uniqueness, so independent concurrent mounts (different
clients, or the same client remounting later) never need to agree with each
other. A hash is preferred over a purely ephemeral in-memory map mainly
because it's also stable across remounts for free, at no extra cost, which
benefits any tooling that persists and compares inode numbers over time.

## Access layers

- **FUSE driver** (Linux/POSIX) — the primary access path discussed in
  depth; implements the core object model directly.
- **WinFsp driver** (Windows) — confirmed as the model for Windows support,
  buildable in parallel with the FUSE driver, not sequentially after it.
- **Views** (browsable/mountable point-in-time snapshots) and a **REST
  API** (S3-shaped, but with real hierarchical directories and without
  S3's filename constraints) are both **deferred** — acknowledged as needed
  eventually, not designed yet.

Locking (POSIX advisory or Windows mandatory share-mode/byte-range) is
**explicitly out of scope for v1** — not a priority for this use case. If
ever needed, the right place for it is emulation local to the WinFsp driver
process, not a true cross-client distributed lock — a real distributed
lock (the JuiceFS model) would require a shared, strongly-consistent
coordination point, which this design has deliberately avoided adding on
top of plain object storage.

## Windows compatibility: primary mode

Several Windows-vs-POSIX gaps resolve to the same underlying rule: **at
root/filesystem creation time, the filesystem is marked primary Windows or
primary POSIX.** Dual-format fields are stored with full native fidelity
for the primary mode, and emulated/derived for the non-primary access path
when mounted. This is the general pattern — not a one-off rule per field.

Applied:

- **ACLs vs. POSIX mode bits** — both formats are supported natively in the
  schema (not a lossy one-way approximation). Primary mode determines which
  is authoritative at write time; the other is derived on access.
- **Reserved names/characters** — a primary-Windows filesystem disallows
  Windows-reserved names and characters (`< > : " / \ | ? *`, trailing
  space/period, `CON`/`AUX`/`COM1`...) outright at write time. (Primary-POSIX
  behavior — presumably allow the write and handle presentation via
  escaping on the Windows access path — follows the same pattern but is an
  inference, not yet explicitly confirmed.)
- **File attribute bits** (Hidden/System/ReadOnly/Archive) — fit the same
  model: the schema always carries the field, populated/used per primary
  mode.
- **Case sensitivity** — not governed by primary mode; resolved
  separately. Storage stays case-sensitive (case-preserving) as the ground
  truth regardless of primary mode; case-insensitive comparison is a
  WinFsp-access-layer-only behavior, following the standard NTFS/Samba/
  WSL2 pattern (case-preserving storage, case-folded comparison only where
  the consuming side needs it).
- **Creation time** — already solved via UUIDv7 (see core object model
  above); no Windows-specific work needed.
- **Reparse points** — no new work needed beyond symlink support already
  planned; WinFsp provides the mechanical translation to present a symlink
  as a reparse point. Directory junctions are out of scope (redundant given
  symlinks + hard links).
- **Delete/rename semantics on open files** — Windows's pending-delete and
  share-mode behavior is emulated in the WinFsp driver layer; not a core
  object-model change.

## Explicitly out of scope

- **8.3 short filenames** — not supported.
- **Alternate Data Streams** — not supported. Technically feasible at
  fairly low cost given the object model (each stream would just be
  another UUID-prefixed blob plus one WinFsp callback), but declined.
- **True distributed locking** — not supported (see Access layers above).
- **Generic multi-cloud object store abstraction** — only MinIO (S3 API)
  and Azure Blob are supported.
- **Views and the REST API** — acknowledged as wanted, not yet designed.
