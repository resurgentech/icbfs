# Open design questions

> **Status: historical.** This document captured the design discussion as
> it happened — options considered, rejected alternatives, and rationale.
> All decisions reached here have been folded into `ARCHITECTURE.md`, which
> is the current source of truth for the design. Kept for the "why," not
> maintained as an active list going forward.

## 1. Concurrent writes to directory/root blocks

### The issue

A directory block (and the root block) is a single object. Reading it, modifying
it, and writing it back is a classic read-modify-write cycle. If two clients do
this concurrently against the same block — e.g. two processes each creating a
different file in the same directory at nearly the same instant — the second
write simply overwrites the first in the object store, with no error raised to
either client. The first client's change is silently gone. This isn't a
hypothetical: it's the default behavior of a plain `PUT` to S3 or Azure Blob,
and it gets more likely the busier a directory (or the root) is.

This matters more for root/high-traffic directories than for a quiet leaf
directory, but any directory with more than one writer is exposed to it.

### Option A — Ignore it (last-writer-wins)

Clients read, modify, and `PUT` the block with no precondition at all.

- **Pros:** Zero extra implementation, zero extra latency, zero extra API
  calls. Fine for single-writer or effectively-single-writer workloads (one
  mount, one user, batch jobs with no real concurrency).
- **Cons:** Silent, unrecoverable data loss under real concurrent writers —
  not a crash, not an error, just a vanished file with no trace. This is the
  kind of bug that's invisible in testing and shows up as "where did my file
  go" in production. I wouldn't ship this as the only mechanism even for a v1,
  given the project's whole premise is reliable history/versioning.

### Option B — Conditional write / compare-and-swap via ETag

Read the block and note its ETag (S3) / ETag (Azure, same mechanism). Modify
in memory. Write back with a conditional header: S3 supports `If-Match` on
`PutObject` (and MinIO mirrors this, since it implements the S3 API); Azure
Blob supports `If-Match` on `Put Blob` / `Set Blob Metadata` natively and has
for a long time. If the ETag on the server no longer matches what you read,
the write is rejected (412 Precondition Failed) instead of silently
succeeding. The client re-reads, re-applies its change, and retries.

- **Pros:** Correct — no silent lost updates, ever. Failure is explicit and
  retriable. No new infrastructure: both target backends support this
  natively, so it stays inside the "only MinIO S3 and Azure Blob" scope.
  Only contended blocks pay any retry cost; quiet directories are free.
- **Cons:** Requires a retry loop in every client that writes a directory
  block. Under heavy contention on one hot directory, you can get retry
  storms — not incorrect, but a client wanting to write into a very busy
  shared directory could back off repeatedly before it lands. Also, every
  single logical change (one file added) still means rewriting the *entire*
  serialized directory block — this is the same write-amplification concern
  that shows up in the large-directory question below, and the two problems
  compound on each other for a big, busy directory.

This is the mechanism I'd pick as the baseline. It's correct, it requires no
infrastructure beyond what's already in scope, and both backends support it
as a first-class primitive.

### Option C — External lock / lease service

Acquire an explicit lock (a lease object with a TTL, or a real coordination
service like etcd/ZooKeeper, or a conditional-write table) before touching a
directory block, release after.

- **Pros:** Can coordinate multi-object transactions that a single-object CAS
  fundamentally can't — e.g. an atomic cross-directory rename that needs to
  update two directory blocks together.
- **Cons:** Introduces a dependency outside the two blessed backends
  (MinIO S3, Azure Blob), which the project has deliberately scoped down to.
  Adds latency and a whole new failure-mode class (stale/expired leases,
  split-brain on lock ownership). Overkill for the common case, which
  single-object CAS already handles correctly.

### Option D — Append-only delta log + periodic compaction

Instead of read-modify-write on the whole block, each mutation is written as
its own small delta object ("add name X → UUID", "remove name Y") appended to
a per-directory log; the log is periodically compacted into a fresh
consolidated directory block.

- **Pros:** Writes never collide — each mutation is an independent object, so
  no CAS and no retries are needed for the common case. This also directly
  solves the "rewrite the whole block for one entry" amplification problem,
  which ties it to the large-directory question below.
- **Cons:** Reads get materially more complex — you now have to merge a base
  block with however many deltas have accumulated since the last compaction,
  in order. It also complicates the point-in-time tree-walk mechanism
  (question 2, below): you'd be reconstructing "base + deltas as of T"
  instead of just picking one version. This is a bigger structural departure
  from "it's just whole objects," which cuts against the project's explicit
  goal of staying simple.

### Decision

**Option B (CAS via ETag).** Final — correct, native to both backends, no
new infrastructure, no new complexity beyond a retry loop. Option D
(append-only delta log) stays on the shelf as a future step if contention or
write amplification on a hot, large directory ever becomes a real bottleneck
— now more likely to matter given large-directory sharding (below) is being
built rather than deferred.

---

## 2. Point-in-time tree reconstruction by timestamp

### The question

Your framing: this whole approach hinges on being able to get a timestamp for
every version of every object. If that holds, you can walk the tree —
starting at the root, picking at each node the version that was current as of
some target time T, then descending into that version's children and
repeating — and reconstruct the state of the filesystem (or any single file
within it) as of T. Is that correct?

### Short answer

Yes, correct in principle, and it's the right mechanism given everything else
already decided — but it's worth being precise about what it costs and what
it depends on.

### Why it holds

- **Both backends expose a timestamp per version.** S3's `ListObjectVersions`
  returns `LastModified` for every version of a key. Azure's `List Blobs`
  with `include=versions` returns the same per version — and on Azure the
  version ID *is* literally a timestamp string (e.g.
  `2026-10-08T22:58:12.3027905Z`), so the timestamp is intrinsic to the
  identifier, not a side attribute.
- **Every mutation creates a new version, including metadata-only ones.**
  We already established (metadata-in-metadata discussion) that a
  metadata-only update — a `chmod`, a `chown`, an nlink change — triggers a
  new version on both S3 (via `CopyObject` with `REPLACE` metadata directive)
  and Azure (via `Set Blob Metadata`), when versioning is enabled. That means
  the timestamp-walk approach captures *all* state changes to an inode, not
  just content writes.
- **Deletes are generally non-destructive under versioning.** S3 writes a
  delete marker as the new current version rather than destroying history;
  Azure similarly preserves prior versions under blob versioning. So walking
  to a T that predates a later delete correctly resolves to the live version
  at that time, not a tombstone.
- **Timestamps are server-assigned, not client-supplied**, so there's no
  client clock-skew risk corrupting the ordering.

### What it costs — the part worth being explicit about

- **There's no native "give me the version as of T" filter.** Neither API
  has an "AsOf" query parameter. You list a key's versions (paginating if
  there are many) and scan for the first one with `LastModified ≤ T`. That's
  a real API call (or several, if paginated) per object, every time, not a
  free server-side filter.
- **A single-path lookup is cheap: O(depth).** "What did `/a/b/c.txt` look
  like at T" costs one version-list-and-scan per directory level on the way
  down, plus one for the file itself. Fine.
- **Reconstructing the *whole* tree at T is O(total nodes in that tree),
  not O(1).** "Walk the entire filesystem to reconstruct it at T" is correct,
  but it is not a cheap snapshot-pointer operation — it's proportional to the
  size of the snapshot, closer in cost to a full `rsync`/tree-diff than to
  "open a pointer." If the intended use is mounting a historical view and
  resolving paths on demand (with caching), that's exactly the right shape
  and the cost is paid incrementally, per path touched. If the intended use
  is "instantly materialize an entire historical snapshot," this approach
  does not give you that for free — it gives you a (possibly expensive) full
  walk.

### The alternative, and why the timestamp-walk is still the right call

The other option would be storing explicit version-id pointers in each
directory row (git-tree style): a directory block would record not just
`name → UUID` but `name → UUID @ version-id`. That makes point-in-time
resolution O(depth) with zero scanning — direct GET-by-known-version-id, no
listing. But it means *every* write anywhere forces a pointer update in its
immediate parent, and that parent's own version change forces an update in
*its* parent, all the way to the root — exactly the up-the-chain propagation
problem the rest of the design (UUID-stable blobs, metadata-in-metadata) was
built specifically to avoid.

So the timestamp-scan approach you described is the one consistent with
every other decision made so far: writes stay cheap and local, and the cost
of that tradeoff shows up at read time for historical queries rather than at
write time for every single mutation. That's the right side to be on for this
project, given it's named for not reinventing a filesystem with heavyweight
machinery.

### Decision

**Accepted as-is, closed for now.** The cost profile (cheap point lookups,
expensive full-tree reconstruction) is understood and acceptable. If it
turns out to be too slow in practice, the fallback is to bolt on an
optimization externally (e.g. a cache of resolved as-of-T lookups, or
periodically-materialized snapshot indexes) rather than changing the
underlying mechanism.

---

## 3. Multiple filesystems in one object store — resolved

A single bucket/container can host more than one independent filesystem.
Each one is anchored by its own root block, keyed by a naming convention
(e.g. `root/<fsname>`) rather than a random UUID, since a root has no parent
to be referenced from — the name itself is the discovery mechanism. Settled.

---

## 4. Large directory sharding — decided, building now

Reopened: this is in scope for v1, not deferred. Going with **median-key
B-tree splitting**, not fixed alphabetic ranges and not hashing:

- A directory block holds `name → UUID` rows up to a max count/size. On
  overflow, the block splits into two children at the **median key actually
  present in the block** — not a pre-assigned boundary like "A–M / N–Z".
  Real-world names cluster (`IMG_*`, `2026-*`, timestamp- or
  prefix-heavy naming), so a fixed alphabetic boundary leaves some ranges
  empty and others overloaded; splitting at the observed median keeps both
  halves balanced regardless of the naming pattern.
- The parent block's entries become range pointers (`< median → child A`,
  `≥ median → child B`), recursively, forming a standard B-tree keyed on
  filename. Lookup is a descent comparing the target name against each
  node's boundary — O(log n) in the number of splits, not a linear scan.
  This is explicitly preferred over a linked list of overflow blocks, which
  degrades to O(n) for point lookups.
- Lexicographic order is preserved (unlike hash-based sharding), so a sorted
  listing or a prefix scan stays efficient — walk the relevant subtree
  instead of fanning out to every shard.
- Real cost to account for when this is built: split/merge logic for both
  insert-overflow and delete-underflow, and split operations racing with
  concurrent writers — i.e. this interacts directly with the CAS mechanism
  in question 1 above, and should be designed alongside it, not
  independently.
- Worth remembering what this is actually solving: a directory block is just
  serialized rows (~50-100 bytes each), so even a million entries is on the
  order of 100MB — well within either backend's object size limits. The real
  problem is write amplification (rewriting a whole large block for one
  entry change), not storage capacity, so the split threshold should be
  tuned around mutation cost, not object size.

---

## 5. Access layer / client surfaces — open, unaddressed

Not yet designed. Three distinct client surfaces have been raised:

- **Views.** Wants the ability to expose point-in-time state (the
  mechanism from question 2) as something a client can actually browse or
  mount — e.g. a ZFS-`.zfs/snapshot`-style read-only view of the tree as of
  some T, rather than the timestamp-walk only being usable internally.
  Needs clarification: is this strictly historical/snapshot browsing, or
  something broader (e.g. user-defined virtual views over the namespace)?
- **Windows support**, described as needing to be "fairly good" — not yet
  decided what that means concretely: a native Windows filesystem driver
  (the WinFsp/Dokan equivalent of FUSE on Linux), an SMB gateway in front of
  icbfs, or something else. Affects the whole access-layer design, since a
  POSIX/FUSE-only story on Linux won't carry over to Windows as-is.
  This is the broader "how do clients actually mount/talk to icbfs"
  question from the earlier discussion — still unresolved, and now has
  concrete requirements attached to it.
- **A REST API**, closer to S3's shape than POSIX, but needs two things S3
  itself doesn't give you: real hierarchical directories (S3 is a flat
  keyspace with prefix-emulated "folders" — no atomic directory ops, no
  directory rename) and filenames that aren't limited the way S3's are.
  Needs clarification on the actual filename-length concern (S3 object keys
  already go up to 1024 bytes/UTF-8 — worth pinning down whether the
  concern is about that limit specifically, a different limit from
  experience with another system, or something else like path-component
  length on Windows).

All three point at the same underlying open question: what access layer(s)
sit in front of the object-store-backed core, and how many of them need to
be built for v1 vs. later.

Note: views and the REST API are explicitly tabled for later discussion.
Windows support is confirmed as WinFsp, buildable in parallel with the FUSE
driver — no open question there. What's still open from this family is the
FUSE inode-number problem and the Windows-vs-POSIX feature gap, both broken
out below.

---

## 6. FUSE inode number mapping — open, recommendation given

### The issue

FUSE's `st_ino` is a plain integer (64-bit on modern Linux). It has to be
stable across lookups, and — since hardlinks are in scope — *identical* for
every directory entry that points at the same underlying UUID. Our identity
is a 128-bit UUID, so something has to narrow that down to a 64-bit inode
number.

### Option A — Truncate/hash the UUID to 64 bits, stateless

Take the low 8 bytes of the UUID (or run it through a fast 64-bit hash) and
use that directly as the inode number.

- **Pros:** Zero extra state, computed identically on every lookup.
  Hardlinks are automatically correct, since every name pointing at the same
  UUID derives the same inode number with no extra bookkeeping.
- **Cons:** Nonzero collision probability — birthday-bound math puts a 50%
  collision chance around ~4 billion live objects in a 64-bit space. Not
  zero, but this is the same bet effectively every S3-backed FUSE
  implementation already makes in production (s3fs, goofys, rclone mount).
  Cheap mitigation: keep a small in-memory override table only for a
  detected collision, so the common case stays fully stateless and only the
  rare pathological case pays for a real allocation.

### Option B — Persistent UUID → sequential-integer allocation table

A real mapping table, assigned on first lookup, persisted.

- **Pros:** No collisions, ever, by construction.
- **Cons:** A new shared, mutable, ever-growing structure that every client
  needs to read/write consistently — i.e. it needs the same CAS treatment as
  directory blocks, plus a lookup-on-stat and an allocation-on-create cost.
  This is exactly the kind of extra moving part the rest of the design has
  deliberately avoided (metadata-in-metadata instead of a side table,
  timestamp-walk instead of a pointer index).

### Option D — Let the FUSE kernel module assign inode numbers itself

Don't set the `use_ino` mount option; let the kernel generate its own
inode numbers from its dentry cache.

- **Pros:** Zero design work.
- **Cons:** Disqualified. The kernel assigns inode numbers per-path, not
  per-object, so two hardlinked names would get *different* inode numbers,
  breaking `ls -i` / hardlink detection outright. Not viable given hardlinks
  are explicitly in scope.

### Decision

**Option A — hash-based, confirmed.** Use a high-quality 64-bit hash
function (not naive truncation of the UUID bytes) to minimize collision
clustering and non-uniformity. Consistent with everything else decided so
far — uses only the object store's own primitives, no new shared mutable
structure, and the failure mode (a rare collision) is well-precedented and
cheaply mitigated.

Side note from the FUSE discussion that led here: inode numbers only need
to be unique and stable *within one running mount's lifetime* (the kernel
combines `st_ino` with the mount's `st_dev` for system-wide uniqueness), so
there's no requirement that independent concurrent mounts — or even the
same client remounting later — agree with each other. A hash is still
preferred over a purely ephemeral in-memory map because it also happens to
be stable across remounts for free, which some backup/dedup tooling
benefits from, at no extra cost over the ephemeral approach.

---

## 7. Windows-specific feature gaps vs. POSIX — decided

WinFsp is confirmed as the model for Windows support, buildable in parallel
with the FUSE driver.

### Foundational concept: primary mode, set at creation

Several of the items below are resolved by the same underlying rule, so
it's worth stating once: at root/filesystem creation time, the filesystem
is marked **primary Windows** or **primary POSIX**. Dual-format fields
(ACLs, reserved-name policy, file attributes, and generally anything with a
native Windows shape and a native POSIX shape) are stored with full
fidelity for the primary mode, and *emulated* for the non-primary access
path when mounted. This is the general pattern — "duplicate what's
required" — rather than a one-off rule for any single field.

- **Locking — decided: skip for v1.** POSIX advisory locking and Windows
  mandatory share-mode/byte-range locking are both out of scope for now —
  not a priority for this use case. Note for later: if it's ever needed,
  the right place to emulate it is locally within the WinFsp driver code
  (not a real cross-client distributed lock, which — per the JuiceFS
  comparison — would need a shared coordination point we've deliberately
  avoided adding).
- **ACLs vs. mode bits — decided: store both, governed by primary mode.**
  The format supports real Windows ACLs *and* POSIX mode bits natively
  (not a lossy one-way approximation). Primary mode determines which one
  is authoritative at write time; the other is emulated/derived when
  accessed from the non-primary side.
- **Case sensitivity — no decision needed.** Already resolved: storage
  stays case-sensitive (case-preserving) as the ground truth, and
  case-insensitive comparison is a WinFsp-access-layer behavior only,
  following the NTFS/Samba/WSL2 pattern (see discussion above). Nothing
  further to decide here.
- **Reserved names and characters — decided: governed by primary mode.**
  If the filesystem is primary Windows, Windows-reserved names/characters
  (`< > : " / \ | ? *`, trailing space/period, `CON`/`AUX`/`COM1`...) are
  disallowed outright at write time. (Primary-POSIX behavior — presumably
  allow the write and handle presentation via escaping/mangling on the
  Windows access path — follows from the same primary-mode pattern but
  hasn't been explicitly confirmed; flagging as an inference, not a
  confirmed decision.)
- **File attribute bits — decided: fits the ACL model.** Stored per the
  primary-mode rule; the schema carries the field always, but it's only
  meaningfully populated/used when relevant to the primary mode.
- **Reparse points — resolved, no new work needed.** This is the
  mechanism underneath symlinks (already planned) and WinFsp provides the
  mechanical translation to present one as a reparse point. Directory
  junctions (an NTFS-only, no-cross-volume variant) are redundant given
  symlinks + hardlinks are already planned, so out of scope unless wanted
  explicitly. One Windows-side constraint, not ours to solve: creating a
  symlink on native Windows normally needs Developer Mode or elevation.
- **Creation time — decided, already solved.** UUIDv7 blob keys give this
  for free (embedded creation timestamp), per the earlier metadata
  discussion. No further work.
- **Delete/rename semantics on open files — decided: emulate in the
  WinFsp driver.** Windows's pending-delete/share-mode behavior around
  open files is implemented as WinFsp-layer emulation, not a core-model
  change.
- **8.3 short names / MAX_PATH=260 — decided: ignore.** Not supporting
  legacy short names; path length is an OS/opt-in concern, not something
  icbfs needs to solve.
- **Alternate Data Streams — decided: not supported.** NTFS lets a file
  carry extra named streams (`file.txt:streamname`) beyond its main
  content, invisible in a normal directory listing — used for things like
  the `Zone.Identifier` mark-of-the-web stream (triggers the Windows
  SmartScreen "this file came from the internet" warning) and some
  enterprise backup/AV tooling. Technically feasible at fairly low effort
  (each stream would just be another UUID-prefixed blob plus one WinFsp
  callback, consistent with the rest of the object model), but explicitly
  declined — out of scope.
