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
  One object store can host multiple independent filesystems; how they're
  created, identified, and discovered is covered in its own section below
  (Multiple filesystems per bucket: the master block) — it's more
  involved than a simple naming convention.
- **Hard links** are simply multiple directory rows pointing at the same
  UUID — this falls out of the model for free, with no special-casing,
  because identity (UUID) is already decoupled from name. The link count
  (nlink) itself needs real compare-and-swap protection against
  concurrent Link/Unlink calls — see the Metadata model section below for
  where it actually lives and why.
- **Symlinks** store their target as the object's *content* (same shape as
  a tiny regular file); the row's type flag marks it as a symlink.

## Multiple filesystems per bucket: the master block

One object store can host several independent filesystems. Discovering,
creating, and deleting them goes through a single **master block**: one
fixed, well-known object per bucket (bootstrapped on first use, same
create-if-missing pattern already used for a filesystem's own root).

**The master block's body is just a growable list of entries** — no
separate ID field, no separate counter:

```protobuf
message FilesystemEntry {
  string name = 1;     // empty = this slot is free, available for reuse
  uint64 size = 2;      // declared max size in bytes (the df "Total")
  bool archived = 3;    // see Lifecycle, below
}
message MasterBlock {
  repeated FilesystemEntry filesystems = 1;
}
```

**A filesystem's ID is its position in this list, not a stored value.**
Creating a filesystem means: CAS-read the master block, scan the list
*already in memory* (no extra round trip — this is a pure in-memory loop
over data already fetched for the write) for the lowest-indexed free
(`name == ""`) slot, reuse it if one exists, otherwise append; write the
whole list back conditioned on the ETag just read, retrying on a lost
race the same way every other CAS operation in this codebase does. The
chosen index, zero-padded to 4 hex digits (16 bits — 65,536 *concurrently
existing* filesystems per bucket, not a lifetime cap, since freed slots
are reused), is that filesystem's ID.

**One correctness-critical constraint this depends on: the list can only
ever grow in length, never shrink or reorder.** Deleting a filesystem
blanks its entry in place (`name = ""`) rather than removing it — if an
entry's position ever moved, its ID would change out from under it, but
every object it already created is permanently keyed with its *original*
ID and would silently stop matching. Tombstone in place; a later
filesystem can safely reuse a freed position, since reusing a slot
overwrites its content without moving anyone else's position.

**Every object a filesystem owns — root included — is prefixed with its
ID.** Not just the UUID-keyed objects (files, directories, `.metadata`,
`.lock`): the root itself, which previously lived at a bare `root/<name>`
naming-convention key, is now keyed as `<id>-root-<name>` — a derived
key, not separately stored anywhere, since it's always reconstructable
from the master block entry's position (the ID) and its `name` field.
Making root follow the same prefix convention as everything else (rather
than being a structural exception) means a single prefix-scoped listing
captures a filesystem's *entire* footprint, root included, with nothing
left uncounted.

**Why uniform prefixing, concretely — `df`/`du` without a fast primitive
in either backend's API:** neither S3/MinIO nor Azure Blob has a
real-time, O(1) "total size of this bucket/container" call (both expose
only slow, stale, batch-computed account-or-bucket-level metrics —
CloudWatch-style and Azure-Monitor-style respectively — unsuitable for a
live `df`). `du` was already free: it's a tree walk scoped to one
filesystem's own root by construction. `df`'s "Used" needed the same
scoping for a bucket shared by multiple filesystems, and that's exactly
what the ID prefix buys: `ListObjectsV2`/`List Blobs` with the filesystem's
ID as the prefix returns every object it owns (lightweight listing
metadata, no bodies fetched), summed for "Used." "Total" is just the
master block entry's `size` field. This is O(pages in that filesystem's
own object count) — not O(1), but correctly scoped and not proportional
to anything *else* sharing the bucket — "slow but not unreasonable," the
same cost class already accepted for other full-tree operations in this
design (reconstructing a snapshot, `du` itself). Changing a filesystem's
declared `size` after creation is expected to need a separate
administrative tool, not a normal mount-time operation — acknowledged,
not yet designed.

**Lifecycle: active → archived → pruned.** `archived = true` means the
filesystem and all its data still physically exist — nothing has been
deleted — but writes are no longer allowed; it stays readable so it can
be backed up before the next step actually destroys anything. Pruning is
a separate, explicit operation: delete every object under the
filesystem's ID prefix (the same prefix-scoped listing `df`/`du` uses,
iterated and deleted rather than summed), then blank the master block
entry. The `archived` flag is checked at mount/session start, not on
every operation — a deliberate choice, not an oversight: checking it on
every single write would tax the hot path for a flag that changes
through a rare, deliberate admin action, so archiving an already-mounted
filesystem takes effect on remount, not instantly mid-session.

## Metadata model

Every file/symlink has **two objects, not one**: the content object (the
actual bytes a reader gets back) and a dedicated `<uuid>.metadata` side
object holding every mutable attribute — mode, uid, gid, nlink, and any
future Windows-compatibility fields (ACL, file attribute bits). The
content object never carries any attribute metadata at all. Directories
don't get a `.metadata` object — they aren't hard-linkable, so "last
writer wins" on a directory's own mode/uid/gid is an accepted
simplification, and their attributes stay in native object metadata on
the directory block object itself, same as before.

**Why two objects instead of native object-store metadata on the content
object itself (the original design):** discovered empirically, not
foreseen — ETag on an S3-compatible store is a hash of the object's
*body*. A metadata-only update doesn't touch the body, so it doesn't
change the ETag, which means ETag-based CAS (the mechanism this whole
design relies on) gives **zero protection** to anything stored as native
metadata on an object whose body isn't otherwise changing. `.metadata`'s
own body *is* the attribute data (a small encoded record), so any real
change to it is a real content write, and ETag CAS becomes meaningful
again. Reaching nlink == 0 writes a tombstone value under that same CAS
check before the target is physically deleted, so a concurrent `Link`
can never race past a decrement that's already decided to delete.

**Why one consolidated object instead of a separate side object per
field** (e.g. `.nlink`, `.acl`, separately): CAS protects an object's
*state as a whole*, not a single field in isolation — any change to
*any* field in `.metadata` moves its ETag, so concurrent writers touching
different fields of the same inode (a `chmod` racing a `Link`) still
correctly serialize via the normal CAS-retry loop, just against one
shared object instead of several. The right test for whether two kinds of
data belong in the same object isn't "could two unrelated operations
coincidentally collide" — it's whether they're read together by the same
caller and change at a similar rate. mode/uid/gid/nlink pass both tests
(`stat()` wants all of them together, and all of them change rarely), so
they're consolidated. Locking (see below) and extended attributes/ACLs
fail at least one of those tests — different caller, and potentially
much higher change frequency for locks — so they get their own objects
instead, for the same reason `.nlink` was originally split out.

**What's derived from the content object, never duplicated into
`.metadata`:**
- **Size** is the content object's own `Content-Length` — storing it
  separately would mean two non-atomic writes (content, then a metadata
  update) that could disagree if one succeeds and the other doesn't, for
  a field the object store already gives us for free, authoritatively,
  at zero cost.
- **mtime** (POSIX: "content changed") is the content object's own
  `Last-Modified`. This is actually *correct* now in a way it wasn't in
  the original single-object design: since nothing but a real content
  write ever touches the content object anymore (all metadata changes
  go to `.metadata` instead), its `Last-Modified` can no longer be
  bumped by a `chmod`/`chown`/`Link`/`Unlink` — it only reflects real
  content changes, which is exactly what mtime is supposed to mean. (The
  original design had mtime and ctime conflated as the same value,
  caught by testing, not foreseen at design time — this consolidation
  is what actually resolves that for good, rather than papering over it
  with a separately-tracked mtime field.)
- **ctime** (POSIX: "*any* change, content or metadata") is
  `max(contentObject.LastModified, metadataObject.LastModified)` — since
  either object changing counts as "something changed," and both values
  are already in hand from the two fetches a full `stat()` needs anyway
  (see below), this costs nothing extra to compute.
- **atime** is still not tracked at all (no backend bumps a timestamp on
  read), reported as equal to mtime.
- **birthtime** is still free, decoded from the content object's own
  UUIDv7 key.

**Cost of a full `stat()`:** two independent object-store calls — one
`HEAD`/`GET` on `.metadata` (mode/uid/gid/nlink), one `HEAD` on the
content object (size, mtime, and btime via its key) — issued
concurrently, since neither depends on the other's result; the wall-clock
cost is `max()` of the two, not the sum, though it's still two requests
(two connections, 2x cost on a per-request-billed backend). No
best-effort cache was added to avoid this — the earlier instinct to mirror
a cached copy of attributes back onto the content object (mirroring the
original `.nlink` cache trick) was deliberately not taken, specifically
to avoid reintroducing a second, non-atomic copy of data that could drift
out of sync; two always-correct parallel fetches were judged better than
one fast, occasionally-stale one.

Field size: mode/uid/gid/nlink easily fit in a few hundred bytes even as
a small encoded record — no cap exists now that the ceiling is the
content-free `.metadata` object's own size, not S3's 2KB/Azure's ~8KB
native-metadata limits, which was the original constraint this design
was built under. Whether extended attributes and Windows ACLs also live
in `.metadata` or their own object is not yet decided — see
`MISSING_FEATURES.md`.

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
metadata-only fields — see the Metadata model section above for the
actual fix (a dedicated `.metadata` object whose body really does change).

## Content writes: CAS, bounded retry, and escalation to locking

**Content writes are CAS-protected, not unconditional.** A client opens a
file, remembers the version (ETag) it read, makes its edit in memory, and
on flush writes the whole object back conditioned on that ETag. If
rejected (someone else wrote first), the client does not resubmit its
stale buffer — it re-fetches the current content, re-applies *only its
own specific edit* (the offset/length/data it was asked to write) onto
that fresh copy, and retries the conditional write against the new
version. This fixes a real lost-update case that the whole-object write
model is otherwise exposed to and a real local filesystem isn't: on a
real filesystem, a low-level `pwrite(offset, length)` only ever touches
exactly those bytes, so two processes writing different, non-overlapping
ranges of the same file never lose each other's change. icbfs can't do
a true partial write — every flush rewrites the entire object — so
without this retry-and-reapply step, two non-conflicting concurrent
writers could silently destroy each other's edit purely because the
second one's write was based on stale content, not because anything
actually overlapped.

**Retries are bounded, and exhausting the budget escalates to the
locking mechanism below, rather than retrying forever.** This is the
standard optimistic-with-pessimistic-fallback pattern: stay lock-free for
the common case (most writes never collide, so well-behaved writers never
pay locking overhead), but under sustained contention between two
genuinely concurrent writers, don't let CAS retries thrash indefinitely —
after a bounded number of failed attempts (a count or time budget; tuned
separately from the directory/nlink retry budget, since a content-write
retry can mean re-sending a potentially large object, unlike a small
block), fall back to acquiring the file's lock (see Locking, below) and
make one final attempt while holding it. That final attempt should still
be CAS-protected, as a cheap defensive backstop, even though a
*cooperating* concurrent writer can no longer be racing it at that point.

**This is a cross-client requirement, not an implementation detail of
one driver.** The guarantee "a writer under contention eventually makes
progress" only holds if *every* client write path follows this same
protocol — a writer that keeps blindly retrying CAS forever, never
escalating, could starve a writer that does escalate correctly. This
holds today only because the FUSE driver is the only way to write to
icbfs; it must continue to hold for the WinFsp driver and any future
access layer (a REST API, etc.) — each new client implementation **must**
implement this same CAS-retry-reapply-escalate sequence, and each should
carry its own test proving it (reject-on-stale-ETag, correct reapply of
the caller's specific edit on retry, and actual escalation once the
retry budget is exhausted), the same way `internal/fuseserver`'s mount
tests prove it for the FUSE path today.

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

Locking is addressed below, not out of scope — superseding the earlier
decision to defer it entirely.

## Locking

Self-contained, optional, best-effort — not a real distributed lock
service. A genuinely strongly-consistent distributed lock (the JuiceFS
model: a separate coordination service like Redis or etcd) remains
explicitly out of scope; what's described here is built entirely from
object-store primitives already in use elsewhere in this design, with
real, named limitations rather than a false promise of full correctness.
If this ever proves inadequate in practice, wrapping a real coordination
service is the escalation path — deliberately not built up front.

- **Off by default, a mount-time flag.** Locking has real costs (polling,
  an extra object per lock-holding file) for a feature most mounts won't
  use; it's opt-in, not a default burden on every filesystem.
- **State lives in its own `<uuid>.lock` object** (itself prefixed with
  the owning filesystem's ID, per Multiple filesystems per bucket,
  above, same as every other object), not in `.metadata` —
  same reasoning as why `.metadata` itself is separate from the content
  object: locks are read/written by a different caller than `stat()`
  ever touches, and can churn far more frequently under real contention
  (e.g. a database doing per-transaction byte-range locks) than
  attributes ever do. Mixing them would mean routine `chmod` calls
  compete for writes against lock churn for as long as any workload is
  using locks heavily — not a rare coincidence, sustained avoidable
  contention.
- **Every lock has a TTL/lease — multiple seconds, not milliseconds** (on
  the order of 30+ seconds). A lease, not a bare flag, so a holder that
  crashes or disconnects before releasing doesn't lock a file forever;
  anyone else can treat an expired lease as free.
- **Lease timestamps are anchored to the object store's clock, not the
  client's, where it matters for safety.** At acquire/renew time,
  `expires_at` is computed from the lock write's own resulting
  `Last-Modified` (server-assigned, already fetched as part of every
  write in this codebase) plus the TTL — not the acquiring client's local
  "now," which could be skewed relative to other clients. At
  steal/expiry-check time, the checking client's own local clock is used
  to compare against that `expires_at`; this is accepted rather than
  closed, because clock skew here can only shift *when* a lease looks
  expired by a few seconds, never cause two clients to both successfully
  steal it — the steal itself is still a CAS write, so only one can land
  regardless of whose clock said what. (Closing this fully would mean
  capturing the raw HTTP `Date` response header via SDK middleware rather
  than relying on typed fields like `LastModified` — verified not to be
  exposed by the normal typed S3 calls already in use; not worth building
  until the soft edge above actually matters.)
- **Blocking lock acquisition is implemented as client-side polling, not
  true wake-on-release — a known, accepted performance cost, not an
  oversight.** Object storage has no notification/pub-sub/long-poll
  primitive; "wait for the lock" can only mean "retry the CAS-acquire in
  a loop." This converges correctly but means handoff latency is bounded
  by the poll interval rather than being instant, and every waiter burns
  real API calls for as long as it waits. Accepted explicitly: this
  feature prioritizes correctness over performance, for an expected small
  number of users, with the real coordination-service escalation path
  above if that tradeoff ever stops being acceptable.
- **Byte-range locks are a list, not a single holder field:**
  `.lock`'s body holds `{range, holder, expires_at}` entries; acquiring a
  range means checking it against every existing entry for overlap before
  CAS-writing your own claim in. This is advisory, same as a whole-file
  lock — nothing stops an uncooperative writer from ignoring it, which is
  fine, since POSIX `fcntl` locks are advisory by definition. Treating
  every byte-range request as if it were a whole-file lock was considered
  and rejected: it would defeat the actual reason an application reaches
  for byte-range locks in the first place (letting different processes
  work different parts of a shared file concurrently) by serializing
  exactly the access pattern the feature exists to avoid serializing.
- **Stronger-than-advisory enforcement, applicable to both FUSE and
  WinFsp, same mechanism:** before issuing a content write, the client
  diffs its modified buffer against the version it originally read (a
  conservative single contiguous span covering the first-to-last differing
  byte is enough — over-approximating the touched range can only make a
  conflict check *more* likely to catch something real, never less),
  re-fetches current lock state, and refuses the write if its touched
  range overlaps a held lock. This is not full mandatory enforcement — a
  narrow window remains between that re-check and the write actually
  landing, during which a brand-new conflicting lock could in principle
  be acquired, since there is no way to atomically check one object and
  write another across S3/Azure. Accepted as a bounded, sub-round-trip
  gap given the feature's explicit correctness-over-performance,
  best-effort framing throughout — not a real guarantee at the level a
  local filesystem's kernel-enforced locking gives you. True OS-level
  *mandatory* byte-range enforcement (Windows' `LockFileEx` as the OS
  itself would enforce it) is not attempted; this client-side check is
  the closest practical approximation given a whole-object write model.
- **One mechanism, two triggers.** The same `.lock` object and
  acquire/release/lease logic serves both an application's explicit lock
  request (`fcntl`/`LockFileEx`) and the internal escalation path from
  the Content writes section above, where the driver grabs a lock purely
  to guarantee forward progress under CAS contention, with no application
  having asked for a lock at all.

See `ROADMAP.md` for this broken into concrete implementation tasks.

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
- **A true, strongly-consistent distributed lock service** (a separate
  coordination service like Redis or etcd, the JuiceFS model) — not
  built; see the Locking section above for the self-contained,
  weaker-but-real alternative that *is* planned.
- **Generic multi-cloud object store abstraction** — only MinIO (S3 API)
  and Azure Blob are supported.
- **Views and the REST API** — acknowledged as wanted, not yet designed.
