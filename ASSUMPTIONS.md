# Assumptions made during autonomous, unsupervised work

Each entry: the question I would have asked, what I assumed instead, why,
and what to check. Written so each one can be confirmed or overridden
after the fact without having to re-derive the context.

---

## A1/A2: Protobuf file layout, checked-in generated code, wrapped types

**Question I'd have asked:** where should `.proto` files live, should
generated code be committed, and should generated Protobuf types be the
public API of `internal/block`/`internal/icbfs`, or wrapped?

**Assumed:**
- `.proto` sources live under `/proto/icbfs/v1/*.proto`.
- Generated `.pb.go` files are **checked into git**, in their own package
  `internal/pb`, imported by whatever package needs them. Rationale:
  `go build`/`go test` keep working for anyone without `protoc` installed
  locally — matches this project's existing low-dependency-friction
  pattern (e.g. `internal/block`'s current JSON approach needs nothing
  extra to build).
- Generated types are **not** exposed as the public API of `internal/block`
  or `internal/icbfs` — those packages keep their existing hand-written
  types (`block.Block`, `block.Entry`, `icbfs.Attr`, etc.) and convert
  to/from the generated types only inside `Encode`/`Decode`. Rationale:
  callers throughout the codebase (and anyone building on these packages
  later) shouldn't need to know the wire format changed; this also means
  if Protobuf is ever swapped for something else again, it's a one-file
  change, not a propagating one.

**Check this if:** a different contributor has a strong existing
convention for where generated Go protobuf code lives in their other
projects — this is a style choice, not a correctness one, and easy to
move later since nothing outside `internal/pb` and each package's
`Encode`/`Decode` would need to change.

---

## D1-D3: Master block design decisions (location, idempotency, prefixing)

**Question I'd have asked:** should the master-block logic live in a new
package or inside `internal/icbfs`; should `Bootstrap` handle both
create-new and open-existing, or should those be separate entry points;
and what's the exact algorithm for slot reuse/idempotency?

**Assumed:**
- Master-block logic (`bootstrapMaster`, `readMaster`,
  `registerFilesystem`, `fsID`, `Archive`, `updateFilesystemEntry`,
  `Prune`) lives in a new file `internal/icbfs/master.go`, same package
  as `Filesystem` — not a separate package. Rationale: it's tightly
  coupled to `Filesystem.Bootstrap` (which calls `registerFilesystem`
  directly) and to the ID-prefixing scheme every `Filesystem` method
  relies on; splitting it into its own package would just mean constant
  cross-package calls with no real encapsulation benefit.
- `Filesystem.Bootstrap(ctx, size, mode, uid, gid)` handles **both**
  first-time creation and opening an already-existing filesystem,
  exactly like the existing (pre-Part-D) `Bootstrap` already did for root
  blocks. `registerFilesystem` is idempotent (returns the existing ID if
  `name` is already registered, `size`/mode/uid/gid are ignored in that
  case) rather than erroring if called again. Rationale: matches the
  established pattern in this codebase (same no-op-if-exists contract
  the root-block half of `Bootstrap` already had before Part D), and a
  `mount` command shouldn't need to know in advance whether a filesystem
  name is new.
- Slot reuse: `registerFilesystem` scans for the lowest-indexed entry
  with `Name == ""` (a pruned, freed slot) before appending a new one.
  Rationale: ARCHITECTURE.md requires the list to never shrink/reorder
  (so IDs, baked into every object key, never change), but it would be
  wasteful to only ever grow the list when pruned slots are sitting
  there unused.
- CAS retry bound reused `maxTreeRetries` (already defined for the
  directory-tree insert/remove loops) rather than defining a separate
  constant for master-block CAS loops. Rationale: same shape of
  problem (optimistic retry on a lost `If-Match` race), no reason for a
  different bound.

**Check this if:** multiple bucket-wide operations are expected to
contend heavily on the master block at once (e.g. many filesystems being
created/archived/pruned concurrently in automated tooling) — the single
`_master` object is a serialization point by design, and `maxTreeRetries`
(20) may need tuning or backoff if that contention becomes real instead
of theoretical.

---

## D-cleanup: `cmd/icbfs` default declared size

**Question I'd have asked:** what should the CLI default to for a
filesystem's declared capacity (the `size` argument `Bootstrap` now
needs), and should it be a flag at all?

**Assumed:** added a `--size` flag to `icbfs mount`, defaulting to
100 GiB (`100 << 30` bytes), only consulted the first time a given
filesystem name is created (ignored on every subsequent mount of the
same name, same as `registerFilesystem`'s existing idempotency). 100 GiB
is an arbitrary round placeholder, not derived from any real capacity
planning. Test helpers (`newTestFilesystem` in
`internal/icbfs/filesystem_test.go`, `mountTestFS` in
`internal/fuseserver/mount_test.go`) use a smaller 1 GiB placeholder
since the value is irrelevant to what those tests exercise.

**Check this if:** there's an intended real default (e.g. "unlimited"/no
declared cap, or a value tied to the actual backing bucket's quota) —
right now `StatFS`'s "Total" will just report whatever was passed at
first creation, with no way to change it after the fact (no `Resize`
operation exists yet).

---

## D1/D2: real bug found and fixed — `bootstrapMaster`'s race, not an assumption

Not a question I'd have asked — this is a correctness bug the D2 "two
concurrent creation attempts both succeed with distinct IDs" test
caught directly, recorded here because the fix adds a new primitive to
`objstore.Store` that wasn't in the original design.

**What was wrong:** `bootstrapMaster` checked `Head` for "does the
master block exist" and, if not, did an *unconditional* `Put` of an
empty block. Two concurrent first-time callers can both observe
"doesn't exist," and whichever one's unconditional write lands *after*
another caller's subsequent `registerFilesystem` CAS write silently
wipes that registration back to empty — a lost update, not a retried
one, because `registerFilesystem`'s CAS loop only protects against
races within itself, not against a late unconditional overwrite from
someone else's `bootstrapMaster`.

**Fix:** added `objstore.Store.PutIfAbsent(ctx, key, body, metadata)` —
a true create-if-absent CAS primitive, implemented in `S3Store` via
`PutObject`'s `IfNoneMatch: "*"`. Confirmed empirically against real
MinIO (not assumed from the S3 API docs) that this is honored and
returns a 412 `IsPreconditionFailed` on conflict, same signal `Put`'s
`ifMatch` already produces. `bootstrapMaster` now calls this directly
and treats a precondition failure as the expected "someone else already
created it" no-op case.

**Check this if:** a future backend adapter (Azure Blob, when that's
built) is added to `objstore.Store` — it needs a real `PutIfAbsent`
too, not a `Head`-then-`Put` shim, or this exact race reappears there.
Azure Blob's `If-None-Match: *` (or the `BlobClient.Upload` SDK
equivalent) should cover it, but that needs the same "confirmed against
a real instance" verification this fix got against MinIO before
trusting it.

---

## B1: retry budget, exhaustion error/errno, and where the shared logic lives

**Question I'd have asked:** what exactly should bound a content
write's CAS retry loop, what should happen when that bound is
exhausted (before task B5's lock-escalation exists to catch it), and
where should the buffering/retry logic itself live?

**Assumed:**
- `contentWriteRetryBudget = 5 * time.Second` (wall-clock, not an
  attempt count) — ROADMAP.md's task B1 explicitly suggested a time
  budget over a count for exactly this reason (large vs. small files
  getting comparable retry *windows*, not comparable attempt counts),
  but didn't name a number. 5 seconds is an arbitrary, unvalidated
  round value.
- Exhaustion returns a new sentinel, `icbfs.ErrWriteContention`,
  mapped to `syscall.EAGAIN` in `fuseserver.errnoFromErr`. Rationale:
  EAGAIN ("resource temporarily unavailable, try again") is the closest
  POSIX fit for "this didn't succeed because of contention, not because
  of a real failure" — matches the same errno `flock`'s `LOCK_NB` uses
  for "currently held," which is the same flavor of condition.
- The buffering/edit-log/CAS-retry machinery itself
  (`icbfs.OpenFile`) lives in `internal/icbfs`, not in
  `internal/fuseserver`. Rationale: ROADMAP.md's task B5 explicitly
  requires the eventual retry-then-lock-escalation logic to be
  implemented once and shared across access layers (FUSE today, WinFsp
  eventually) rather than copy-pasted per driver — putting the
  buffering/retry step itself here too, not just the future escalation
  call, means a WinFsp driver gets the whole thing for free by using
  `OpenFile` the same way `fuseserver.FileHandle` does, rather than
  reimplementing buffering and only sharing the escalation step.

**Check this if:** 5 seconds turns out to be badly miscalibrated once
task B5's lock-escalation exists (e.g. it fires so late that users
perceive a stall, or so early that it escalates to locking in cases
that would have resolved on their own with one more retry) — this was
never load-tested, just chosen as a reasonable round number.

---
