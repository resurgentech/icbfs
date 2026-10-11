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
- `.proto` sources live under `/proto/icbfs/v1/*.proto` (moved to
  `/spec/proto/icbfs/v1/*.proto` later, at Jared's direction, to nest
  under a top-level `spec/` directory).
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

**Resolved later, at Jared's direction:** the "no way to change it
after the fact" gap above is closed — `icbfs.Resize(ctx, store,
fsName, newSize)` (master.go) updates the master-block entry's `Size`
field via the same CAS pattern `Archive` already uses. Like `archived`
(and unlike `EnableLocking`'s per-mount flag), declared size is a
real, shared property of the filesystem itself, so a resize takes
effect for every mount of that filesystem immediately — confirmed by
`TestResizeChangesDeclaredSizeForEveryMount`, which calls `StatFS` on
a second, already-bootstrapped handle after resizing and checks it
sees the new total (it does, because `StatFS` always re-reads the
master block fresh rather than caching the value from `Bootstrap`).
100 GiB remains the arbitrary first-creation default; there is still
no "unlimited" option, and no CLI subcommand wires `Resize` up yet
(same as `Archive`/`Prune`, which also have no CLI wiring — all three
are library-level admin operations, not `icbfs mount` flags).

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

## B2: `expires_at` anchoring — now a real server-clock anchor, not the client's

**Resolved later, at Jared's direction** (the drift below went
unnoticed for a while — this entry still described the original,
superseded decision after the real fix shipped; caught and corrected
when explaining this file's E3 entry surfaced the inconsistency): this
entry originally recorded a single-write, client-clock approximation
(`expires_at = time.Now() (client clock) + ttl`) as "good enough," with
raw-`Date`-header capture flagged as "not worth building until the
soft edge actually matters." That edge mattered, and the real fix was
built: `objstore.Store.ServerTime` captures the object store's actual
clock via a real request's raw HTTP `Date` response header (S3Store's
implementation uses `smithy-go`'s deserialize middleware against a
`HeadBucket` call, confirmed against real MinIO, not assumed — see its
own doc comment), and `tryAcquireLockRange`/`RenewLockRange` now anchor
every claim's `expires_at` to that, fetched once per call rather than
per retry attempt. The expiry *check* against already-held entries
still deliberately uses the local clock (`checkNow`) inside the retry
loop, not another store round trip — see ARCHITECTURE.md: clock skew
there can only shift *when* a lease looks expired by a few seconds,
never let two clients both successfully steal it, so this asymmetry is
accepted rather than every single retry attempt also paying for a
server round trip.

**Real constraint found while fixing this:** the HTTP `Date` header
only has whole-second resolution and can itself lag true time by up to
~1s — this broke a couple of existing tests that used sub-second TTLs
(150-200ms), since `expires_at` could already read as "in the past" by
the time a local check ran against it. Fixed by giving those tests
multi-second TTLs instead of chasing sub-second precision this
anchoring approach can't actually offer.

**Check this if:** a future backend adapter (Azure Blob, when that's
built — see `objstore.Store.ServerTime`'s own doc comment for a sketch
of the Azure-side equivalent) is added — it needs its own real,
verified `ServerTime` implementation, not an assumption that whatever
raw-header approach worked for S3/MinIO transfers unchanged.

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

**Resolved later, at Jared's direction:** the "check this if" below
was the wrong call to leave open — real POSIX `fcntl`/`flock`
same-owner merge/split isn't a separate, hypothetical feature, it's
part of what "a holder can hold multiple ranges" already implies for
a filesystem whose explicit ask is POSIX fidelity (same correction
already made for shared/exclusive semantics, above). Added
`mergeOwnRanges` (lock.go): acquiring a new range now adjusts every
existing entry this same holder already holds that overlaps it — an
overlapping entry of the *same* type (shared/exclusive) is absorbed
(the new claim's bounds widen to cover it), one of a *different* type
is trimmed to its non-overlapping remainder(s) — exactly matching real
`posix_lock_file` merge/split behavior for repeated `F_SETLK` from the
same owner, run to a fixed point since absorbing one entry can newly
overlap another. Covered by
`TestByteRangeLocksSameHolderOverlappingReacquireMergesAndSplits`,
confirmed to fail without the fix (reverted lock.go, ran it, got a
real failure) before trusting it as a regression test.

~~**Check this if:** a caller actually wants POSIX-style same-process
lock *merging/splitting* ... that's real `fcntl` semantics this
implementation does not attempt.~~ (superseded by the fix above)

**Still not attempted, deliberately out of scope for this fix:**
partial-range *unlock* splitting — real `fcntl` also lets `F_UNLCK`
release only part of a held range, splitting the remainder into
however many separate locked extents are left outside the unlocked
part. `ReleaseLockRange` still only matches an exact `(holder, start,
end)` tuple. Not attempted because it wasn't what was asked for here
(acquire-time merge/split specifically), but it's the same flavor of
gap and would need the same fixed-point-split treatment if a caller
ever needs it.

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

## B8: a fixed 24h lease with no renewal, and the cross-process test discovery

**Correction, at Jared's direction — see the standalone "shared/
exclusive lock semantics" entry below for the fix:** this entry
originally also recorded a decision to map `F_RDLCK` and `F_WRLCK`
onto the same exclusive-only primitive, reasoning that "building real
reader-writer semantics into the lock data model was never asked
for." That reasoning was wrong: the actual ask throughout this whole
effort is a POSIX-compatible filesystem, which already implies real
`fcntl`/`flock` shared-vs-exclusive semantics — it isn't a separate
feature someone has to request. Fixed for real; the remaining
decisions below (lease TTL, `owner` mapping) still stand unchanged.

**Question I'd have asked:** go-fuse's `Getlk`/`Setlk`/`Setlkw` pass a
real POSIX `Typ` (`F_RDLCK`/`F_WRLCK`/`F_UNLCK`) and a lock `owner`
token, plus the real kernel enforces real POSIX lock-ownership rules
(fcntl locks are per-(process, inode); flock locks are per-open-file-
description) — but a lease-based model has no "held until explicitly
released" concept real POSIX locks have. How should the FUSE wiring
reconcile these?

**Assumed, later corrected at Jared's direction — see the standalone
"Real lease renewal" entry near the end of this file:** FUSE-sourced
locks originally got a fixed `lockLeaseTTL = 24h`, with **no
background renewal** for as long an application holds the lock.
Rationale at the time: this project's lease model has no "forever"
concept at all (every claim needs an `expires_at`); implementing real
renewal would mean a goroutine tied to the FUSE file handle's
lifetime, explicitly out of scope for what B8's "Done when" asks for.
**Real consequence, correctly identified but wrongly left
unaddressed:** an application that holds an fcntl/flock lock longer
than 24h without this codebase ever renewing it could have that lock
silently stolen by another claimant — regardless of whether that
application was still alive and actively using it. Fixed for real: see
the later entry for the renewal goroutine, the much shorter resulting
TTL, and — more significantly — the real, previously-latent bug this
work exposed when actually verifying it (kernel lock forwarding had
never been enabled in any test or in `cmd/icbfs` itself).
- `owner` (the kernel-assigned FUSE lock-owner token) becomes this
  codebase's holder string directly (`fuse-owner-<owner>`) — no
  translation needed; it already has exactly the stability guarantee
  (same token = same lock context across calls) this codebase's
  `holder` concept needs.

**Real discovery, not a guess — the single-process fcntl test was
wrong:** the first version of the fcntl byte-range test opened two fds
*in the same test process* and expected a "second fd's overlapping
lock" to conflict with the first. It didn't — confirmed empirically
that real fcntl(2) ownership is per-(process, inode), not per-fd, so
the kernel reports the *same* owner for both fds, and this codebase's
"a holder's own re-claim never conflicts with itself" rule (needed for
idempotent re-acquire/renew) correctly treated the second request as
the same holder replacing its own lock — exactly matching real fcntl
semantics, where a second `F_SETLK` from the same process really does
just replace the first rather than erroring. Fixed by spawning a real
second OS process (`TestMain` intercepting an `ICBFS_FCNTL_HELPER=1`
re-exec of the test binary itself, operating directly on the parent's
already-mounted FUSE path — no second testcontainers/mount setup
needed) to get a genuinely different lock owner. `flock(2)`, by
contrast, is owned per-open-file-description, so the original
single-process, two-fd version of that test was correct as written
and needed no such fix.

~~**Check this if:** a real application legitimately needs shared
(read) lock semantics, or needs a lock held reliably longer than 24h
— both would require real design work...~~ (both resolved — shared/
exclusive semantics in the standalone entry further below, lease
renewal in the one after it).

---

## C1: root ownership had to change, not just the mount option

**Not a question so much as a bug C1 itself surfaced:** enabling
`default_permissions` (the literal C1 task) immediately broke every
single existing mount test with "permission denied" — `cmd/icbfs` and
the test helpers all hardcoded the root directory's owner to uid/gid
`0, 0` regardless of who actually runs the mount, which was only ever
safe because nothing enforced it before. Fixed by bootstrapping the
root with the *real* calling process's `os.Getuid()`/`os.Getgid()` in
both `cmd/icbfs/main.go` and `internal/fuseserver/mount_test.go`'s
helpers, rather than adding a new flag for it — nobody asked for
"mount as a different user," and the natural default (you own what
you mount) is the one every other FUSE filesystem uses too.

**Resolved later, at Jared's direction:** the "check this if" above was
the wrong call to leave open — a `--uid`/`--gid` override is standard,
not a hypothetical (sshfs, NFS, and effectively every other FUSE
filesystem expose exactly this). `cmd/icbfs` now takes `--uid`/`--gid`
(default `-1`, meaning "use the mounting user's own," same as before),
only consulted the first time a filesystem name is created, same as
`--size`.

---

## C2: the decision — explicit note, not a differing-uid test

**Decision ROADMAP.md explicitly asked to be made, not defaulted
silently:** owner-bit coverage (task C1) is the automated bar; group/
other-bit enforcement is exercised by a *different* mechanism, not a
genuine second real identity (no root/setuid available to this
autonomous session to actually run part of a test as a different real
uid). `TestMountDefaultPermissionsEnforcesOtherBits` creates a file
directly through the `Filesystem` API with a fabricated owner uid/gid
that's deliberately not the test process's own (something no real
`create(2)` syscall could do, but the kernel's `default_permissions`
check doesn't care how an inode's reported ownership got that way,
only what `Getattr` reports), then accesses it through the real
mounted path — genuinely exercising the "other" bits kernel-enforcement
code path, not just owner bits, without needing real privilege
escalation.

**What this does NOT cover** (documented directly in the test's own
doc comment too, per C2's "Done when"): the "group" bits specifically
(as distinct from "other"), and ROADMAP's called-out nuance that the
kernel checks a caller's *full* supplementary group list, not just a
primary gid — both would still need a genuine second real identity to
verify.

**Check this if:** this project ever gets a CI environment with real
root/container-user-namespace capability to spawn a genuinely
different uid — at that point the group-bits and supplementary-group
nuances above are worth closing for real, rather than continuing to
rely on the "other"-bits proxy this session chose.

---

## E3: a real bug (stale-nodeId registry) and two real dead ends (inotify, page cache)

**Real bug found and fixed, not a guess:** the first implementation of
`watchRegistry.register` overwrote its map entry unconditionally on
every `newChild` call. Confirmed by reading `(*fs.Inode).NewInode`/
`newInodeUnlocked` directly: when an explicit `StableAttr.Ino` is
given (which every call here does), it does **not** deduplicate
against an already-known Inode with that Ino at all — it always
allocates a fresh wrapper. The real, authoritative "is this Ino
already known, and if so, which Inode wins" resolution happens later,
inside go-fuse's own `bridge.addNewChild`/`addNewNode`, called by the
framework *after* our `Lookup`/`Create` method already returned. So a
*second* lookup of an already-known file (e.g. `os.Open` after the
`os.WriteFile`/`Create` that made it) builds a second, throwaway
Inode that the bridge discards in favor of the first — and the old
unconditional-overwrite `register` call stomped the correct,
already-registered entry with a reference to that soon-to-be-discarded
one. Caught directly via `fuse.MountOptions.Debug: true`'s raw
protocol trace: the `NOTIFY_INVAL_INODE` message targeted a nodeId the
kernel had never heard of, and the kernel's own write(2) response was
a literal `ENOENT`. Fixed by making `register` first-writer-wins (the
very first registration for any given Ino is, by construction, always
the one go-fuse's own dedup lets win later).

**Two real dead ends, tried and confirmed not to work, not left
unexamined:**
- Tried driving a real `inotify_add_watch`-observed `IN_MODIFY` event
  via `Inode.NotifyContent`, with both `(0,0)` and `(0, <real size>)`
  arguments. Confirmed via the same `Debug: true` trace that the
  kernel accepts the notify write (errno 0) but no inotify event ever
  arrives at a raw `syscall.InotifyInit1`/`InotifyAddWatch` watcher,
  even after forcing a subsequent re-read. `NotifyDelete`'s own go-fuse
  doc comment ("equivalent to NotifyEntry, but *also* sends an event to
  inotify watchers") in hindsight is the tell: it calls out sending an
  inotify event as a distinguishing feature, implying `NotifyContent`/
  `NotifyEntry` alone don't reliably do so on their own.
- Considered testing via kernel page-cache staleness instead (what
  `NotifyContent`'s doc literally promises: "content... flushed from
  buffers"). Checked this filesystem's own `Node.Open`/`FileHandle`:
  it never requests `FOPEN_KEEP_CACHE`, and `fs.Options.AttrTimeout`/
  `EntryTimeout` are both left nil (→ effectively zero caching) — so
  there isn't actually any kernel-side cache in this configuration for
  `NotifyContent` to meaningfully invalidate; a plain re-read would
  show fresh content regardless of whether any notification ever
  fired, so that comparison wouldn't have proven anything either.

**Assumed, given both of the above:** `TestMountNotifyDispatchesSignalToCorrectInode`
verifies this task's actual deliverable — the registry+dispatch
pipeline correctly turns a `notify.Source` signal into a `NotifyContent`
call against the right, kernel-validated Inode, accepted without
error — via a test-only hook (`notifyContentHook`, nil in production)
rather than an environment-dependent kernel side effect. Confirmed this
is a real regression test by reverting the registry fix and watching
it fail deterministically (3/3) with the exact `ENOENT` the real bug
produced, then pass 3/3 once restored.

**Check this if:** a real end-user-visible inotify/dnotify delivery
guarantee is ever actually required — this task proves the plumbing
*up to* the kernel accepting the notify call, not that a watching
userspace process observably reacts to it; closing that gap for real
would need either a different kernel/FUSE capability, opting into
`FOPEN_KEEP_CACHE` plus real cache timeouts to make staleness
observable, or revisiting whether `NotifyEntry`/`NotifyDelete`
(confirmed to explicitly fire inotify events) are better suited to
however this ends up being used in practice.

---

## E4: no sync/async delivery-mode parameter exists on this specific API

**Question I'd have asked:** ARCHITECTURE.md's Change notifications
section calls out explicitly preferring MinIO's sync delivery mode
over async (the latter can silently drop events under sustained queue
overload). Where does this project's MinIO adapter actually set that?

**Found while implementing, not assumed:** `minio-go/v7`'s
`ListenBucketNotification`/`ListenNotification` — the API
`internal/notify/miniosrc` wraps — is a direct, unqueued client HTTP
long-poll against the MinIO server; reading its implementation
directly shows no sync/async parameter anywhere in that call path at
all. The sync-vs-async distinction ARCHITECTURE.md describes applies
to MinIO's *separate* bucket-notification-to-external-target system
(webhook/AMQP/Kafka/NATS targets configured via the server's own
`notify_<type>` config, each with its own queue-store delivery mode) —
a different mechanism this project isn't using, since
`ListenBucketNotification` talks to the MinIO server directly with no
external target/queue in between at all.

**Assumed:** there is nothing for this code to configure — the
sync/async consideration doesn't apply to the API actually in use.
Documented directly in `miniosrc.New`'s doc comment rather than
silently doing nothing and leaving future readers to wonder whether
it was overlooked.

**Check this if:** a future deployment needs this feature on a MinIO
instance where `ListenBucketNotification` is unavailable/disabled and
the fallback is a real external notification target instead — *that*
path would need the sync-vs-async server config ARCHITECTURE.md
describes, configured on the MinIO server itself, outside anything
this Go code controls.

---

## E5: no real Azure SDK code at all, by deliberate choice

**Question I'd have asked:** should this task still attempt real
Azure SDK for Go integration code (even if untestable here), or
should it stop at the backend-agnostic checkpoint/filter logic plus a
documented gap?

**Assumed:** stopped at the backend-agnostic logic
(`internal/notify/azurecf`: `Source`, the pull-and-resume cursor
handling, client-side prefix filtering) plus a `Reader` interface a
real implementation would satisfy — no Azure SDK dependency was
added, and no concrete Azure-backed `Reader` was written at all.
Rationale: every other backend API used in this project (go-fuse's
`Notify*` methods, MinIO's `ListenBucketNotification`) was verified by
reading the actual installed package source before being used — this
project's own stated practice, reinforced by ARCHITECTURE.md's own
MinIO-docs-corruption anecdote ("even documentation fetches get
verified here, not trusted blind"). There is no way to do that for
Azure here (no SDK installed, no account to exercise it against), so
writing a "real" adapter would mean guessing at field names,
pagination semantics, and error types from training knowledge alone —
exactly the kind of unverified assumption this project has avoided
everywhere else, for a cloud SDK where a wrong guess would be
silently broken until someone with a real Storage Account found out
the hard way. Task E5's own text explicitly scopes the testable
deliverable to the checkpoint/filter logic and calls for the gap to be
"documented plainly in the code, not silently absent" — which is what
`azurecf`'s package doc comment does.

**Check this if:** someone with real Azure Storage Account access
picks this up — they'd add the real SDK dependency, implement `Reader`
against the actual Change Feed client, and verify the actual
continuation-token/`Cursor` representation against reality (this
package treats `Cursor` as opaque `[]byte` specifically so that
real shape, whatever it turns out to be, can slot in without changing
`Source`'s own logic).

**Related note, added at Jared's direction:** this entry is about the
Change-notifications backend specifically — the *object storage* side
(the actual put/get/list CRUD `objstore.Store` exposes, what files and
locks are actually stored through) is a separate, already-existing
abstraction with the same "no real Azure to verify against" problem.
Confirmed it's already structured so a real `AzureStore` is just a new
file implementing `objstore.Store`, not a new layer: every call site
in icbfs (`master.go`, `lock.go`, `filesystem.go`, ...) already only
ever talks to the `Store` interface, never to `s3.Client`/AWS types
directly. Added a per-method doc comment on `objstore.Store` itself
sketching the Azure Blob Storage SDK for Go call each one would make —
same explicit "unverified, not installed/testable here" caveat as
`azurecf` above, not claimed as confirmed fact.

---

## E6: a real adapter this time (unlike E5), plus why

**Question I'd have asked:** E5 deliberately stopped short of real
backend-SDK code because Azure's SDK isn't installed/verifiable here.
Should E6 do the same for AWS S3/SQS, or write a real adapter against
`aws-sdk-go-v2/service/sqs`?

**Assumed:** wrote a real, concrete adapter this time
(`internal/notify/sqssrc.Source`, directly against
`aws-sdk-go-v2/service/sqs`'s actual `ReceiveMessage`/`DeleteMessage`
API, verified by reading the installed package source the same way
every other backend API in this project has been). Rationale for the
asymmetry with E5: `aws-sdk-go-v2` is *already* a trusted, verified
dependency this project uses for every other S3-API call — adding
`service/sqs` just extends an SDK family already in use, and its real
API shape could be checked directly, unlike Azure's SDK (not
installed, no account to test against, would have meant guessing from
training knowledge alone). The "no real AWS account" testing
constraint (ARCHITECTURE.md's Testing reality note, same as E5) is
handled by defining a minimal `receiver` interface
(`ReceiveMessage`/`DeleteMessage`) a real `*sqs.Client` satisfies, and
testing against a fake implementation of just that interface instead
— real adapter code, fake-queue test, matching E6's own "Done when"
bar exactly.
- The S3-event-notification JSON schema (what an SQS message body
  actually contains) is parsed via a small struct defined locally in
  `sqssrc`, not by importing `minio-go/v7/pkg/notification` — even
  though that package's types were already confirmed (while building
  E4) to mirror this exact schema for MinIO's S3-compatibility, pulling
  a MinIO-labeled package into an AWS-specific adapter for this felt
  like the wrong dependency direction.
- Added a client-side prefix filter here too, even though
  ARCHITECTURE.md says AWS S3 supports server-side filtering on the
  bucket notification configuration itself (unlike Azure) — that
  configuration lives outside this package (task E6 scopes this
  adapter to the consumption side only, per its own text), so a
  redundant, cheap client-side check keeps `Source`'s filtering
  contract identical across all three backend adapters.

**Check this if:** a real AWS account becomes available to test
against — `PutBucketNotificationConfiguration` (the S3-to-SQS wiring
itself, including its own prefix/suffix filter) still isn't
implemented anywhere in this project and would need to be, alongside
verifying this adapter's parsing against a genuine S3-generated event
body rather than the hand-constructed JSON this task's test uses.

---

## B10: a real architectural gap found before implementing — one Source, two consumers

**Not a guess — a real problem B10's own premise creates:** B10 says
"this task only does anything when both [Locking and Change
notifications] happen to be on," meaning its core scenario is exactly
the case where task E3's FUSE watch dispatch and B10's own lock-
acquisition accelerator both need to observe the *same* backend
signal stream at once. But `notify.Source.Signals()` (as built for
tasks E1/E4-E6) is a single-consumption channel — a value read by one
consumer is gone for any other. Two independent readers of the same
`Source` would silently steal signals from each other, breaking
*both* E3's dispatch and B10's accelerator intermittently, exactly in
the scenario B10 is supposed to matter.

**Assumed/built:** added `notify.Broadcaster` (fans out one `Source`'s
signals to any number of independent `Subscribe()` channels, each
seeing every signal) rather than solving this inline in either
consumer. `fuseserver.Root` now takes a plain `<-chan notify.Signal`
instead of a `notify.Source` — it never needed anything beyond
ranging over a channel, and this decouples it from needing to know
`Broadcaster` exists at all; the caller (`cmd/icbfs`) owns the one
real `Source`, wraps it in one `Broadcaster`, and hands each consumer
its own `Subscribe()`. `Filesystem.EnableChangeNotifications` takes
the same shape. A dropped signal under a slow/stalled subscriber
(`Broadcaster`'s bounded per-subscriber buffer, non-blocking send) is
accepted the same way every other missed signal is throughout this
feature — never a correctness problem, just a slower wake-up.

**Check this if:** a future consumer of signals (beyond E3's dispatch
and B10's accelerator) gets added — it should get its own
`Subscribe()` too, never share a channel with an existing consumer,
or this exact bug reappears for that new pair.

---

## Real POSIX shared/exclusive lock semantics (correcting the B8 misstep above)

Added for real, at Jared's direction, after the earlier B8 entry
wrongly treated this as out of scope. `LockRange` (lock.proto) gained
a `shared bool` field (default false = exclusive, so every existing
B2-B6 caller's behavior is unchanged unless it explicitly asks for
shared). The conflict rule everywhere a *new claim* is being acquired
(`tryAcquireLockRange`, and `FindConflictingLockRange`'s simulation of
"would this request conflict," for `F_GETLK`) is real `fcntl`
semantics: two shared claims from different holders never conflict;
anything else (shared vs. exclusive, exclusive vs. exclusive) does.
New public API: `TryAcquireSharedLockRange`/`AcquireSharedLockRange`/
`TryAcquireSharedLock`/`AcquireSharedLock` alongside the existing
exclusive-only names, rather than adding a parameter to those and
touching every existing call site — lower blast radius, and reads
clearly at each call site which kind is being requested.
`RenewLockRange` preserves `Shared` across renewal, the same way it
already preserved `EscalationOnly`.

**One deliberate exception, not an oversight:** `CheckRangeLockConflict`
(task B6's stronger-than-advisory write check) stays
type-*insensitive* — a content write conflicts with *any* other
holder's claim, shared or exclusive. A shared-lock holder expects a
stable view of the data for as long as they hold it; letting an
uncoordinated write land underneath them because "shared locks don't
conflict with each other" would violate exactly that expectation,
even though two concurrent *readers* are genuinely compatible with
each other. Only the acquire-time conflict check (and `F_GETLK`'s
simulation of it) gets the shared-vs-shared exception.

FUSE wiring (`node.go`'s `setlk`/`Getlk`): `F_RDLCK` now routes to the
shared primitives, `F_WRLCK` to the exclusive ones; `Getlk` reports
back the real type of whatever it finds conflicting (`F_RDLCK` if the
holder's claim was shared, `F_WRLCK` otherwise) instead of always
claiming `F_WRLCK`. Verified through real kernel `fcntl`/`flock`
syscalls against a real mount, using the same cross-process helper
B8 built (shared locks are still owned per-(process, inode), so a
second real OS process is still required to prove two different
holders coexist, not just two fds in one process).

---

## Docker container readiness (the other direction B8/E4/etc.'s flakiness note should have gone)

An earlier session logged `internal/fuseserver`'s intermittent
container-related test failures (`TestMountStatfsReflectsDeclaredSizeAndUsage`,
during Part D; later also `TestMountFcntlByteRangeLocksAcrossProcesses`,
during Part B's task B8 — both failing with `open .../<newfile>:
input/output error` on a plain `os.WriteFile` creating a brand-new
file, only when run as part of the full suite, never alone) as an
accepted, documented flake rather than fixing it — the wrong call,
raised directly. Root cause:
`testcontainers-go`'s MinIO module waits on MinIO's own
`/minio/health/live` endpoint, a *liveness* probe ("the process
started"), not a readiness one — a real S3 API call immediately after
that succeeds can still intermittently fail for a brief window.
Fixed with `internal/testutil.RetryUntilReady`, wrapping the first
real API call (bucket creation) in all four MinIO-container test
helpers (`internal/objstore`, `internal/icbfs`, `internal/fuseserver`,
`internal/notify/miniosrc`) in a short retry instead of trusting the
container's own "ready" signal.

---

## Real lease renewal, and the much bigger bug it exposed (further correcting B8)

Added for real, at Jared's direction, after B8's original "no
renewal, 24h TTL" decision above was raised directly: a lease nothing
ever renews can't distinguish "holder crashed" from "holder has just
been holding this a while," which a real NFS/SMB-style lease (short
TTL, renewed periodically while alive, reclaimed only after a holder
goes dark for one full lease period) can. Added `FileHandle.
ensureRenewal`/`runRenewal` (node.go): one background goroutine per
FUSE lock-owner that currently holds a lock through that handle,
renewing everything that owner currently holds (`icbfs.Filesystem.
LockRangesHeldBy`, new) every `lockRenewInterval` for as long as the
handle stays open. `lockLeaseTTL` dropped from the old 24h placeholder
to 30s now that something actually renews it. Queries "what do I
currently hold" fresh on every tick rather than renewing whatever
bounds the triggering acquire call originally requested, specifically
because `mergeOwnRanges` (the B4 fix above) can widen or split a
holder's own entries after the fact — renewing a stale remembered
range would silently stop working the moment a later overlapping
acquire changed the real stored bounds.

**The much bigger bug this surfaced, not a hypothetical:** verifying
the renewal fix actually did anything meant adding temporary debug
prints to `tryAcquireLockRange` and confirming they fired during a
real cross-process test. They didn't — not for the new test, and not
for `TestMountFcntlByteRangeLocksAcrossProcesses`, a pre-existing test
specifically written to prove cross-process conflict detection works.
Root cause, found by reading go-fuse directly rather than guessing:
`fuse.MountOptions.EnableLocks` gates whether the kernel forwards
`FUSE_GETLK`/`SETLK`/`SETLKW` to this driver *at all* — `go-fuse/fuse/
opcode.go`'s `kernelFlags |= input.Flags64() & (CAP_FLOCK_LOCKS |
CAP_POSIX_LOCKS)` only happens `if server.opts.EnableLocks`. Neither
any test helper (`mountFSWithLocking`) nor `cmd/icbfs`'s own
`fuseMountOptions` ever set it. Without it, the kernel silently falls
back to its own local, single-host advisory lock table (the same one
every local filesystem uses) and never calls this driver's `Setlk`/
`Getlk`/`Setlkw`/`Getlk` at all — which, for a same-host test (both
"processes" share one kernel), reproduces *identical* pass/fail
behavior to this driver's real remote-`.lock`-object logic, with zero
indication anything was wrong. Every task B2-B8 FUSE-level lock test
that has ever passed in this project, before this fix, may have been
exercising the kernel's own local lock table, never this driver's
actual code — the byte-range/shared/exclusive *logic* itself was
separately verified at the `internal/icbfs` level (no FUSE/kernel
involved there), but the FUSE *wiring* on top of it was never really
proven end-to-end until now.

**Fixed:** `EnableLocks: true` added to `mountFSWithLocking`
(mount_test.go) and `cmd/icbfs`'s `fuseMountOptions`. Turning it on
immediately failed three existing/new tests for two more real reasons
(not flakes — each confirmed by tracing exact timestamps):
- `FileHandle` never actually released a lock on a plain fd close/
  process-exit without an explicit `F_UNLCK` — it had never needed to
  before, because the kernel's own local lock table was doing that for
  free. Added `FileHandle.Release` (`fs.FileReleaser`): stops every
  renewal goroutine for this handle's owners *and* actively releases
  whatever they still held, so a crash/plain-close still frees the
  lock immediately rather than waiting out a lease. Deliberately
  scoped to what *this* `FileHandle`'s own acquires registered, not
  every lock its owner holds anywhere — real `fcntl(2)` semantics are
  that closing *any* fd releases *all* of that process's locks on the
  inode (locks are owned per-process, not per-fd), which would need a
  registry shared across every `FileHandle` on a `Filesystem`, not
  attempted here.
- FUSE's `RELEASE` is dispatched to this driver asynchronously
  relative to the closing/exiting process's `close(2)`/exit actually
  returning — confirmed by tracing timestamps: `cmd.Wait()` returning
  (the helper process is fully gone) does not mean this driver's
  `Release` has run yet. Two existing cross-process tests asserted
  "succeeds immediately after `cmd.Wait()`" with no tolerance for
  this; fixed with `retryFcntlSetlkUntilNotEagain` (a short bounded
  retry, not a blind sleep) at both assertion points.
- The new renewal test itself had two bugs, both caught by actually
  running it rather than trusting it once it compiled: a TTL short
  enough to collide with `ServerTime`'s whole-second Date-header
  resolution (same class of issue as the B2 entry — fixed by using
  2s/500ms instead of sub-second values), and a sleep duration longer
  than `runFcntlLockHolderHelper`'s own hardcoded 10s give-up deadline
  (the helper was self-exiting from *its own* timeout, which looked
  identical to a real renewal failure until timestamps were compared
  — fixed by keeping the sleep at 3 lease windows, comfortably under
  10s).
- `-race` caught one more real bug in the fix itself: `Release`
  calling each renewal goroutine's `cancel()` and returning
  immediately, without waiting for the goroutine to actually observe
  it and stop, raced a test that mutates the shared `lockLeaseTTL`/
  `lockRenewInterval` vars during cleanup. Added a `sync.WaitGroup`
  so `Release` blocks until every goroutine it signaled has actually
  exited before returning (and before releasing their locks, so a
  goroutine's own in-flight renewal can't race the release either) —
  a real goroutine-lifecycle bug, not just a test-only concern, even
  though the race detector only caught it via the test.

**Check this if:** a process locks the same file through *multiple*
open fds and closes one without releasing — only the `FileHandle`
that actually acquired a given range releases it on its own close;
real `fcntl(2)` would release it on *any* of that process's fds
closing. Also check if any other mount configuration or driver
(a future WinFsp equivalent) needs the same `EnableLocks`-equivalent
capability turned on explicitly — it is never implied by implementing
the lock interfaces alone.

---
