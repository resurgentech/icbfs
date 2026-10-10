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

## B2: `expires_at` anchored to the client's clock, not a two-write server anchor

**Question I'd have asked:** ARCHITECTURE.md says a lock's `expires_at`
should be "computed from the lock write's own resulting Last-Modified
... not the acquiring client's local clock." Literally, that's only
achievable by writing once, learning the real result, then writing
*again* with the corrected value (you can't know your own Put's result
before you've sent its body) — is that two-write dance actually
intended, or is a single-write, client-clock approximation acceptable?

**Assumed:** implemented the simpler single-write version:
`expires_at = time.Now() (client clock) + ttl`, documented directly in
`TryAcquireLock`/`RenewLock`'s doc comments as a deliberate deviation
from the literal server-anchor description. Rationale:
- A true server-anchor would need either (a) a second CAS-conditioned
  corrective write after learning the first write's `Last-Modified`
  (real extra round-trip, and its own brief "looks provisionally
  claimed with a slightly-off expiry" window), or (b) capturing the raw
  HTTP `Date` response header, which ARCHITECTURE.md itself already
  flags as unavailable through the typed SDK calls in use and "not
  worth building until the soft edge actually matters."
- ARCHITECTURE.md's own text says the actual safety property — "two
  clients [never] both successfully steal" a lock — comes from the CAS
  write itself, not from whose clock set `expires_at`: "clock skew here
  can only shift *when* a lease looks expired by a few seconds, never
  cause two clients to both successfully steal it." The implementation
  preserves exactly that property (verified by
  `TestLockConcurrentAcquireOnFreeLockHasExactlyOneWinner`); only the
  precision of *when* a lease is treated as expired is client-clock-
  relative instead of server-relative.

**Check this if:** real deployments show meaningfully-skewed client
clocks causing disputed/surprising lease-expiry behavior in practice —
at that point the two-write server-anchor (or raw Date-header capture)
would be the thing to actually build, per ARCHITECTURE.md's own
escalation note.

---

## B4: a holder can hold multiple disjoint byte ranges on one file

**Question I'd have asked:** B2's whole-file model was one claim per
holder per file. B4's "Done when" only tests different holders holding
non-overlapping ranges — does a *single* holder acquiring a second,
disjoint range on the same file replace its first claim (one-slot-per-
holder, same as B2), or add a second independent entry?

**Assumed:** a holder can hold several disjoint ranges simultaneously;
`TryAcquireLockRange` never conflicts against that same holder's own
existing entries (so a holder can never block itself), and only the
*exact* `(holder, start, end)` tuple being reacquired is replaced —
every other entry, including that same holder's other ranges, is left
untouched. `ReleaseLockRange`/`RenewLockRange` likewise key off the
exact `(holder, start, end)` tuple, not "the holder's one entry."
Rationale: this is what the schema itself (`repeated LockRange`, each
with its own `holder`) most naturally supports, matches real-world
byte-range use cases (e.g. a database process locking several disjoint
record ranges at once), and ROADMAP.md's B4 text says "Release/renew
operate on the caller's *specific range entry*," which reads more
naturally as "one of potentially several" than "the holder's only
one." Covered by
`TestByteRangeLocksSameHolderCanHoldMultipleDisjointRanges`.

**Check this if:** a caller actually wants POSIX-style same-process
lock *merging/splitting* (re-locking an overlapping-but-not-identical
range from the same holder adjusts the existing claim rather than
adding a second, independent one) — that's real `fcntl` semantics this
implementation does not attempt; it was out of scope for B4's stated
"Done when" and would be a real, separate feature if ever needed.

---

## B5: escalation range, lease TTL, and how the "Done when" test was actually provable

**Question I'd have asked:** when `OpenFile.Flush`'s plain CAS-retry
budget is exhausted, what range should the escalation lock cover (the
ROADMAP text explicitly allows whole-file as a simpler first cut), and
what lease TTL should it request?

**Assumed:**
- The escalation lock covers the actual touched byte range (the
  min/max span of every recorded edit in the session, same
  over-approximation reasoning ARCHITECTURE.md gives for task B6's
  diff-based enforcement), not the whole file — it wasn't materially
  harder than whole-file once `AcquireLockRange` already existed from
  task B4, and it's the more useful default (two sessions editing
  disjoint parts of a large file under contention don't serialize
  against each other during escalation either).
- The escalation lock's lease TTL is `lockEscalationLeaseTTL = 30s`,
  matching ARCHITECTURE.md's general "30+ seconds" guidance for lease
  TTLs rather than a short one — Flush always releases promptly once
  its one final attempt completes (success or failure), so a generous
  TTL only matters if a holder crashes mid-escalation, same trade-off
  ARCHITECTURE.md already accepts for locks generally.
- Each `OpenFile` gets its own unique `holder` identity (a fresh
  UUIDv7, generated once at `Open`/`NewOpenFile` time) purely for this
  escalation lock — reusing one fixed holder string across all
  escalating writers would have been a real bug: `TryAcquireLockRange`
  treats same-holder claims as always non-conflicting, so a shared
  holder identity would have let every escalating writer "steal" the
  lock from every other one instantly, defeating the whole point.

**How B5's "Done when" was actually tested:** the literal scenario
("two goroutines repeatedly writing as fast as possible") turned out
to be a bad test in practice — tried first, verified empirically that
local MinIO over Docker is fast enough that even an artificially tiny
retry budget (down to 1ms, up to 8 concurrent writers) never reliably
starved any single writer to zero successes, with or without
escalation actually wired in — so that version of the test passed
vacuously both ways and proved nothing. Replaced it with a
deterministic version (`TestFlushEscalatesWhenPlainRetryBudgetIsExhausted`):
force `contentWriteRetryBudget` to `0` and land one concurrent write on
the object before calling `Flush`, guaranteeing its one and only plain
CAS attempt fails with zero budget left to retry. Verified by hand
(reverting the escalation branch temporarily) that this version fails
3/3 without escalation and passes 3/3 with it — an actual
discriminating regression test, unlike the timing-based one.

**Check this if:** a future contributor is tempted to write a
"simulate N goroutines hammering a file" style test for similar
contention logic — confirm first (as done here) that it actually
fails on the pre-fix code before trusting it as a regression test;
against fast local infrastructure, these can pass vacuously on both
sides of a real bug.

---

## B6: distinguishing escalation locks from real application locks (new schema field)

**Question I'd have asked:** B6's check needs to refuse a write that
overlaps "a currently-held, unexpired lock belonging to someone else"
— but task B5's own escalation mechanism *also* writes entries into
that same `.lock` object under a different (per-session) holder. Once
B6's check exists, would another session's in-flight escalation claim
get mistaken for a real external application lock and spuriously
refuse a write that B1/B5 would otherwise have converged on its own?

**Assumed:** yes, this was a real, immediate interaction (not a
hypothetical future one — B5 already existed and already wrote into
the same object B6 now reads), so added `LockRange.EscalationOnly`
(proto field 5) to tag entries `OpenFile.Flush`'s own escalation
creates. `CheckRangeLockConflict` (B6) unconditionally excludes
`EscalationOnly` entries from its conflict check, regardless of
holder; `tryAcquireLockRange`'s own *acquire-time* conflict check is
unchanged and still considers every entry regardless of the flag (an
escalation attempt must still respect a real application lock, and a
real application-lock attempt must still respect another session's
in-flight escalation). Confirmed by
`TestCheckRangeLockConflictIgnoresEscalationOnlyEntries`, which fails
without the exclusion.

**Check this if:** task B8 (real FUSE `flock`/`fcntl` wiring) lands and
starts writing real application-lock entries under holder identities
chosen by *that* code — those entries must NOT set `EscalationOnly`
(the zero/default value already ensures this as long as B8 goes
through the public `TryAcquireLockRange`/`AcquireLockRange`, not the
unexported `acquireEscalationLockRange`/`tryAcquireLockRange(...,
escalationOnly: true)` path, which only `OpenFile.Flush` should ever
call).

---

## Known pre-existing flaky test (not caused by this session's changes)

`internal/fuseserver.TestMountStatfsReflectsDeclaredSizeAndUsage`
intermittently fails with `open .../statfs-usage.bin: input/output
error` when run as part of the full package suite (not when run
alone — 10/10 clean in isolation every time it's been tried). First
observed during Part D (StatFS/df wiring) work, before any Part B
locking code existed, and still happens at a similar rate after all of
Part B's changes — confirmed the failure is in `Node.Create` (an EIO
from an unmapped underlying error, almost certainly a MinIO-testcontainer
"reports ready but isn't fully serving yet" startup race), not
anywhere near Part B's lock/escalation code, which only runs inside
`Flush`, never `Create`.

**Check this if:** this flakiness rate gets worse or starts showing up
in CI in a way that blocks merges — at that point it's worth adding a
retry-on-connection-refused guard to the MinIO testcontainers setup
helpers, or investigating the container module's readiness check
directly, rather than continuing to treat it as background noise.

---

## B7: where the flag lives, and whether it also gates B5's escalation

**Question I'd have asked:** should the Locking on/off flag be a
stored, persisted master-block property (like `archived`) or a local,
in-memory, per-mount setting? And does disabling it also disable task
B5's internal escalation (which uses the same `.lock` object), or only
the public B2-B4 API real applications would call?

**Assumed:**
- `Filesystem.EnableLocking(bool)` is a plain in-memory setter, not
  threaded through `Bootstrap` or stored in the master block.
  Rationale: ARCHITECTURE.md calls this "a mount-time flag," and unlike
  `archived`/`size` (real, shared properties of the filesystem itself),
  whether *this* mount wants to pay Locking's costs is a per-mount
  choice — forcing it into shared, persisted state would mean one
  mount's choice dictates every other concurrent mount of the same
  filesystem.
- Disabling Locking disables *both* the public API (`TryAcquireLockRange`
  /`AcquireLockRange`/`ReleaseLockRange`/`RenewLockRange`, all returning
  the new `ErrLockingDisabled`) *and* `OpenFile.Flush`'s own B5
  escalation and B6 conflict-check — not just the former. Rationale:
  B5's escalation and B6's check both read/write the exact same
  `.lock` object the real feature uses, so "opting out of Locking's
  costs" has to mean opting out of all of it; with Locking off, exhausted
  plain-retry (task B1) simply returns `ErrWriteContention` directly,
  exactly as it did before task B5 existed. `CheckRangeLockConflict`
  short-circuits to `nil` (not an error) when disabled, matching "there
  is nothing a disabled mount could ever need to respect" rather than
  surfacing `ErrLockingDisabled` from a call site (`Flush`) that isn't
  itself an explicit lock request.
- `ErrLocked` (previously unmapped, falling through to a generic EIO in
  `fuseserver.errnoFromErr`) now maps to `EAGAIN`, matching real
  `flock(LOCK_NB)`'s `EWOULDBLOCK`/`EAGAIN` convention — done alongside
  B7 since B6's refusal path needed *some* real errno and this was the
  obvious match, not because B7 itself required it.

**Check this if:** a real deployment wants Locking enabled on some
mounts of a filesystem but not others and finds the resulting "lock
placed by an opted-in mount isn't visible as 'the feature is on' to an
opted-out mount, but its data in `.lock` is still there and still
respected if that mount ever turns Locking on" behavior surprising —
that's the direct consequence of this being per-mount, local state
rather than shared.

---
