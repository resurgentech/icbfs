# Roadmap

Concrete development tasks, broken out by workstream, implementing the
design already settled in `ARCHITECTURE.md` — this is sequencing and
task breakdown, not open design. "Done when" is the acceptance bar for
each task — write the test first if that's practical, same as the rest
of this codebase's testing style (real MinIO via testcontainers-go, not
mocks).

**Status: Parts A-E are done and shipped on `main`.** Part F (the
WinFsp driver) is the only remaining workstream — see its section
below for the current plan, now grounded in a real, verified Windows
test VM (`TESTING.md`, `test/windows/`) that didn't exist when this
part was first scoped.

Five workstreams so far:

- **Part A: Serialization — JSON to Protobuf.** All of this project's own
  object bodies (directory/root blocks, the not-yet-built `.metadata`
  object, the not-yet-built `.lock` object, the not-yet-built master
  block) move to Protobuf instead of JSON. Do this one first, or at least
  its schema/toolchain setup — every object defined in Parts B and D
  should be born in Protobuf rather than built in JSON and migrated a
  second time.
- **Part B: Locking.** Implements the Locking section (and the Content
  writes: CAS, bounded retry, and escalation to locking section) of
  `ARCHITECTURE.md`.
- **Part C: Permission enforcement.** Mode bits are stored and reported
  correctly but not actually enforced on read/write — this closes that
  gap. Independent of A and B; can be done in any order relative to them.
- **Part D: Multiple filesystems per bucket — the master block.**
  Implements the Multiple filesystems per bucket section of
  `ARCHITECTURE.md`: the master block itself, uniform ID-prefixing of
  every object (a real migration — the current code has no such prefix
  at all), the archive/prune lifecycle, and `df`/`du`. Depends on Part A
  for its schema (task D1), and its prefixing change affects every other
  part's object-naming — sequence it early relative to B and C's own
  object-creation code, not as an afterthought bolted on at the end.
- **Part E: Change notifications.** Implements the Change notifications
  section of `ARCHITECTURE.md` — three backend-specific event sources
  behind one internal interface, wired to go-fuse's notification API.
  Depends on Part D's ID-prefixing (notifications are scoped by it) and,
  for MinIO, on Part A's Protobuf toolchain not being required at all —
  this part's wire format is each backend's own event schema, not
  something icbfs defines.
- **Part F: WinFsp driver and Windows compatibility.** Everything in
  `ARCHITECTURE.md`'s "Windows compatibility: primary mode" section has
  zero code behind it today — this part builds the WinFsp driver itself
  (the largest single item in this whole roadmap) and wires the
  already-decided primary-mode fields into it. Parts B and C each have
  a task that's "blocked on the WinFsp driver existing" (Part B's task
  B9, Part C's task C3) — those are this part, not separately scoped
  tasks of their own.

---

## Part A: Serialization — JSON to Protobuf

Everything in this part applies to objects this project defines itself —
directory/root blocks, `.metadata`, `.lock`. It does not apply to, and
cannot apply to, the native S3/Azure object-metadata header mechanism
(`x-amz-meta-*`), which is a fixed, external, string-only API outside our
control. Once `.metadata` exists with its own Protobuf body (task A4),
it's likely those native headers won't be needed for much of anything —
worth confirming once A4 lands, not assumed here.

### A1. Decide where `.proto` files and generated code live, and how

A real decision with consequences for every contributor, not a detail to
default silently:

- Where do `.proto` source files live in the repo (e.g. `/proto`, or
  alongside each package)?
- Are generated `.pb.go` files checked into git, or regenerated at build
  time (`go:generate`, a Makefile target)? Checked-in means `go build`
  keeps working for anyone without `protoc` installed; regenerated-only
  means no risk of committed code drifting from its `.proto` source but
  requires `protoc` + `protoc-gen-go` in every contributor's and CI's
  environment.
- **Done when:** the decision is written down (a short note in this
  section or a comment at the top of the first `.proto` file is enough),
  the toolchain is installed and confirmed working in this dev
  environment, and a trivial "hello world" message round-trips through
  `go generate`/`protoc` successfully.

### A2. Define the directory/root block schema

- A `.proto` message set covering what `internal/block.Block` currently
  represents in JSON: `Kind` (leaf/internal), `Entries` (name, UUID,
  type), `Children` (min key, UUID).
- Decide whether the generated Protobuf types become the public Go API
  for this package directly, or whether `block.Block`/`block.Entry`/
  `block.Child` stay as hand-written Go types with Protobuf purely as
  the wire format underneath (a translation layer in `Encode`/`Decode`).
  Leaning toward the latter for decoupling — callers throughout
  `internal/icbfs` shouldn't need to care that the wire format changed —
  but this is a real choice to make explicitly, not assume.
- **Done when:** the schema is defined and generates cleanly; this task
  doesn't yet require wiring it into `internal/block` (that's A3).

### A3. Migrate `internal/block` from JSON to Protobuf

- Replace `Block.Encode`/`block.Decode`'s JSON implementation with the
  A2 schema. If using the translation-layer approach, this means
  converting between the hand-written types and the generated ones on
  every encode/decode.
- Update every caller in `internal/icbfs/filesystem.go` that currently
  assumes JSON-compatible byte handling (there shouldn't be many, if the
  `Encode`/`Decode` function signatures stay the same — `[]byte` in,
  `[]byte` out — but confirm this rather than assume it).
- **Rewrite `internal/block/block_test.go`'s format-specific tests.**
  `TestEncodeDecodeRoundTrip` and `TestDecodeEmptyIsLeaf` are JSON-shaped
  today (they construct/inspect JSON specifically) and need rewriting
  against the new format. The pure-logic tests — `Insert`/`Find`/`Remove`/
  `SplitLeaf`/`SplitInternal`/`ChildFor`/`InsertChild` — operate on the
  in-memory `Block` type regardless of wire format and shouldn't need to
  change at all; confirm that's actually true rather than assume it.
- **Audit which tests get easier or redundant, which is a real property
  of switching to a schema-backed format, not just a migration
  side-effect worth ignoring.** A hand-rolled JSON struct with
  `map[string]string`-style fields (see `internal/icbfs/attr.go`'s
  `parseMetaUint`/`metadataFromAttr`) can silently hold a malformed or
  wrong-typed value that only surfaces as a bug at parse time; a
  generated Protobuf struct with a `uint32` field structurally cannot
  hold a string, so a class of "did we parse this correctly" tests
  becomes unnecessary — the type system already guarantees it. Document
  which existing/hypothetical tests this removes the need for. This cuts
  the other way too: Protobuf's wire format has its own correctness
  surface that JSON didn't (unknown-field handling, schema evolution
  across old-writer/new-reader or new-writer/old-reader combinations) —
  if this schema is expected to keep evolving the way it already has
  across this project's own design discussion, at least one test
  exercising decoding a message with an unexpected/missing field is
  worth adding, not just tests ported over from the JSON era.
- **Confirm `internal/icbfs` and `internal/fuseserver`'s existing test
  suites still pass unmodified** (`filesystem_test.go`,
  `mount_test.go`) — they exercise the `Filesystem`/mount-level API, not
  the wire format directly, so they shouldn't need code changes, only a
  clean run after the migration. `internal/objstore`'s tests are
  unaffected entirely (that package only ever deals in raw bytes) and
  should need no changes or even a re-review.
- **Done when:** `go test ./...` passes end to end, including the new
  large-directory-sharding tests already in the suite
  (`TestDirectorySplitsAndStaysConsistent`,
  `TestMountLargeDirectorySharding`) — these specifically prove the
  B-tree logic still functions correctly after changing the one thing
  (encoding) underneath it.

### A4. Build `.metadata` directly in Protobuf (not a migration — this object doesn't exist in code yet)

This is where the `.metadata` consolidation from `ARCHITECTURE.md`
(mode/uid/gid/nlink, replacing the current `<uuid>.nlink`-only design)
and the extended-attributes/ACL union design actually get built — in one
pass, directly in Protobuf, since there's no JSON version of this object
shipped to migrate away from.

- Schema: `mode`, `uid`, `gid`, `nlink` as native typed fields (replacing
  `internal/icbfs/attr.go`'s current string-encoded
  `map[string]string` metadata, which exists only because S3/Azure's
  *native* object metadata headers are string-only — `.metadata`'s own
  body has no such constraint), plus an `xattrs` field: `map<string,
  bytes>`. POSIX ACLs are just an entry in that map under the key
  `system.posix_acl_access` (mirroring Linux's own xattr-based ACL
  representation exactly); Windows ACLs are just another entry under a
  reserved key like `windows.acl`. No separate ACL-specific schema field,
  no separate object.
- Replace the current `<uuid>.nlink` side object and its tombstone
  protocol with the same mechanism applied to `.metadata`'s `nlink`
  field instead — the CAS/tombstone logic in
  `internal/icbfs/filesystem.go`'s `adjustNlink` carries over
  conceptually unchanged, just targeting `.metadata` instead of
  `.nlink`.
- Wire `Stat`/`SetAttr`/`Create`/`Symlink`/`Link`/`Unlink` to read/write
  `.metadata` instead of native object metadata headers for
  mode/uid/gid/nlink.
- **Done when:** a new test suite (mirroring the existing
  `TestConcurrentLinkRaceDoesNotLoseUpdates` style) proves the migrated
  nlink CAS/tombstone protocol still closes the concurrent-link race
  against real MinIO, plus new tests for the `xattrs` map round-tripping
  arbitrary binary values (confirming no base64-style corruption or
  truncation) and for a POSIX-ACL-shaped entry and a Windows-ACL-shaped
  entry coexisting in the same map without interference.

### A5. CI / build confirmation

- Confirm `go build`/`go test` work the way A1's decision intended for a
  contributor who hasn't touched this before — if generated code is
  checked in, confirm building from a clean checkout with *no* `protoc`
  installed still works; if not checked in, confirm a clean checkout
  *fails* clearly (not confusingly) without `protoc`, and document the
  install step.
- **Done when:** both of the above are actually tried, not assumed.

---

## Part B: Locking

Implements the Locking section (and the Content writes: CAS, bounded
retry, and escalation to locking section) of `ARCHITECTURE.md`.

Tasks are ordered; each one after the first two depends on the ones
before it.

**Prerequisite context:** Part A (task A4 specifically) builds
`.metadata` for the first time — tasks below assume it exists, or at
minimum that `.lock` follows the same side-object pattern. Task B2 below
should define `.lock`'s body directly in Protobuf per Part A's schema
conventions, not in JSON — there's no shipped JSON version of `.lock` to
migrate from either, same situation as `.metadata`.

---

### B1. Content writes become CAS-protected

Right now `Filesystem.WriteFile` does an unconditional `Put` (`ifMatch:
""`). This task makes it conditional, with retry-and-reapply on
rejection — independent of locking, and valuable on its own (it's the
fix for the lost-update case described in ARCHITECTURE.md, which exists
today for any two processes writing non-overlapping ranges of the same
file concurrently).

- `FileHandle` needs to track the version (ETag) it read at `Open`/
  `Create` time.
- `FileHandle.Write` currently mutates its in-memory buffer directly as
  `pwrite`-style calls arrive. For retry-and-reapply to work, the
  specific edits (offset, data) need to be replayable onto a freshly
  fetched base, not just baked into a buffer that's already lost track
  of what the original content was. Simplest approach: keep the
  pristine buffer read at `Open` time *separate* from a list of
  `{offset, data}` writes applied since, and only merge them into one
  buffer at flush time — on a CAS rejection, re-fetch fresh content and
  replay the same edit list onto that instead of the stale pristine copy.
- `Filesystem.WriteFile` takes an expected ETag and uses it as `ifMatch`
  on the `Put`; on `objstore.IsPreconditionFailed`, the caller (the
  `FileHandle`/`Flush` path) re-fetches and retries.
- Bound the retry count/time budget — a separate constant from
  `maxTreeRetries`, since a content-write retry can mean resending a
  large object, not a small block. A time budget (not just a count)
  is worth considering so large and small files get comparable retry
  windows rather than comparable attempt counts.
- **Done when:** a test spawns two concurrent writers against the same
  file editing non-overlapping byte ranges (mirroring
  `TestConcurrentLinkRaceDoesNotLoseUpdates`'s style) and confirms both
  edits survive in the final content — this test should fail against
  today's code and pass after this task.

### B2. `.lock` object: non-blocking acquire/release/renew, whole-file only

The core primitive, scoped to whole-file first — byte ranges come later
(task B4) once the simple case is solid.

- Object body: `{holder: string, expires_at: int64}` (holder is an
  opaque client-generated id, not necessarily a uid/pid — it just needs
  to be unique enough to tell "is this still the same holder renewing,
  or someone else").
- `expires_at` is computed from the *write's own resulting*
  `Last-Modified` (already available from every `Put` in this codebase)
  plus the TTL — not the client's local clock. See ARCHITECTURE.md's
  Locking section for why.
- Acquire: read current state (or treat a missing object as free);
  if free or `expires_at` has passed (checked against the reading
  client's local clock — accepted, see ARCHITECTURE.md), CAS-write your
  own claim; on a lost CAS race, re-read and retry a bounded number of
  times before reporting "currently held."
- Release: CAS-write back to an empty/absent state, conditioned on
  still being the current holder (don't release a lock you no longer
  hold because your lease already expired and someone else took it).
- Renew: same as acquire, but only succeeds if you're still the current
  holder — bumps `expires_at` forward.
- **Done when:** tests cover: acquire-then-release round trip; a second
  acquire attempt while the first holder's lease is still valid is
  rejected; an acquire attempt *after* the TTL has passed succeeds even
  without an explicit release (expired-lease steal); two concurrent
  acquire attempts against the same free lock result in exactly one
  winner.

### B3. Blocking acquisition via polling

Layered on top of task B2's non-blocking primitive.

- A blocking acquire loops: attempt the non-blocking acquire, and if
  held, sleep (with backoff — don't hammer the object store at a fixed
  tight interval) and retry, until it succeeds or an optional timeout
  elapses.
- Expose both a non-blocking (`LOCK_NB`-equivalent) and blocking variant,
  since callers need both.
- **Done when:** a test holds a lock, starts a blocking acquire attempt
  from a second "client" in a goroutine, releases the first lock, and
  confirms the second acquire completes promptly after release (not
  just eventually) and definitely before the lease TTL would have
  expired on its own — proving it's reacting to the release, not just
  waiting out the lease.

### B4. Byte-range locks

Generalizes task B2's single-holder body to a list, with overlap
checking.

- Object body becomes `{ranges: [{start, end, holder, expires_at}, ...]}`.
- Acquire checks the requested `[start, end)` against every existing
  entry for overlap; rejects (or blocks, via task B3's loop) only if a
  conflicting range is actually held and unexpired.
- Release/renew operate on the caller's specific range entry, not the
  whole object.
- **Done when:** tests cover: two non-overlapping ranges can be held
  concurrently by different holders; a request for an overlapping range
  is rejected while the conflicting range is held; the same request
  succeeds once that range's lease expires or is released.

Done — including a later fix (shared/exclusive semantics, and real
`fcntl(2)` same-owner merge/split on re-locking an overlapping range)
raised directly after initial shipping. See `ASSUMPTIONS.md`'s B4 and
"Real POSIX shared/exclusive lock semantics" entries.

### B5. Escalation wiring: content-write retry exhaustion → lock → retry

Connects task B1 to tasks B2-B4.

- When task B1's bounded retry budget is exhausted, acquire a lock (via
  task B3's blocking acquire) covering the byte range the write actually
  touches (or the whole file, if that's simpler to ship first — see
  ARCHITECTURE.md's note that even a whole-file fallback here is
  defensible as a first cut), then make one final CAS-protected write
  attempt while holding it, then release.
- This must be implemented identically by every client write path. For
  now that's only the FUSE driver; this task includes making sure the
  escalation logic lives somewhere shared (not copy-pasted per driver)
  so the eventual WinFsp driver gets it automatically rather than by
  re-implementation.
- **Done when:** a test simulates sustained contention — e.g. two
  goroutines each repeatedly writing to the same file as fast as
  possible for a bounded duration — and confirms both make real forward
  progress (neither is starved indefinitely), where without this task's
  escalation the same test would be expected to show one writer
  thrashing against the other's retries.

### B6. Client-side stronger-than-advisory enforcement

The diff-against-what-I-read-then-check-locks mechanism from
ARCHITECTURE.md, built on everything above.

- Before issuing a content write (not just on retry-exhaustion — every
  write), diff the modified buffer against the version read at open
  time to get the touched range (a single contiguous min/max span is
  sufficient — see ARCHITECTURE.md for why over-approximating is safe
  here).
- Re-fetch current lock state and check the touched range against it;
  refuse the write (return the appropriate error to the caller) if it
  overlaps a currently-held, unexpired lock belonging to someone else.
- This is distinct from task B5: task B5 is about *this driver's own*
  write succeeding eventually under contention with itself; this task
  is about respecting a lock an *external, cooperating* caller
  explicitly placed via `fcntl`/`LockFileEx`.
- **Done when:** a test holds an explicit lock on a range from one
  simulated client, attempts a write overlapping that range from a
  second simulated client, and confirms it's refused — then confirms a
  write to a genuinely non-overlapping range from the second client
  succeeds normally while the lock is still held.

### B7. Mount-time opt-in flag

- A CLI flag (`cmd/icbfs`, e.g. `--locking`) and the equivalent for
  whatever WinFsp's mount invocation ends up being, defaulting to off.
- When off, lock/unlock calls should return "not supported" (or
  whatever the correct POSIX/Windows errno is for an unimplemented
  lock operation) rather than silently no-op — a caller that thinks
  it's holding a lock when it isn't is worse than a caller that gets a
  clear error.
- **Done when:** a test confirms lock acquisition fails cleanly when
  the mount wasn't started with the flag, and succeeds when it was.

### B8. FUSE wiring

- Investigate go-fuse's actual API surface for `flock`/`fcntl` before
  assuming a specific interface shape — not yet checked against the
  installed go-fuse version the way every other API assumption in this
  project has been checked first.
- Wire whatever that interface is onto tasks B2-B6's primitives.
- **Done when:** a real mount test (same style as the existing
  `internal/fuseserver/mount_test.go` suite) exercises `flock`/`fcntl`
  through the actual kernel syscalls against a real mount, not just the
  `Filesystem`-level API directly.

Done — but the "done when" bar above was passing for the wrong reason
for a long time: `fuse.MountOptions.EnableLocks` was never set, so the
kernel silently handled every lock locally and never actually
dispatched to this driver at all. Also added real lease renewal
(tied to the FUSE file handle's lifetime) once that was fixed and a
much shorter TTL became safe. **Required reading before touching
Part F's own lock-callback wiring (B9, below) or any other FUSE mount
option:** `ASSUMPTIONS.md`'s "Real lease renewal, and the much bigger
bug it exposed" entry — the same category of "an option exists that
gates whether the kernel actually calls your driver at all" mistake is
worth checking for explicitly on the WinFsp side too, not assumed away
because the FUSE side eventually got it right.

### B9. WinFsp wiring

Blocked on the WinFsp driver existing at all — see Part F. Once it
does, wire its lock-related callbacks onto the same tasks B2-B6 primitives
used by FUSE. Not further broken down here since Part F owns scoping
the driver itself.

### B10. Optional: use Change Notifications to accelerate blocking acquisition

Cross-cutting with Part E — depends on both B3 (blocking acquisition)
and Part E existing, and is itself optional on top of two already-
optional features (Locking and Change Notifications each have their own
mount flag; this task only does anything when both happen to be on).

- When Change Notifications is enabled, B3's polling loop can
  additionally subscribe to notifications scoped to the exact `.lock`
  key it's waiting on, and retry the CAS-acquire as soon as a matching
  notification arrives rather than waiting out the next poll interval.
  Scoping this is simpler than Part E's general per-watch UUID tracking
  (task E3) — the key being waited on is already known exactly, no
  path-to-UUID resolution needed.
- **Gate this per backend — do not wire it in for Azure.** MinIO's
  adapter (task E4) is genuinely near-real-time, a real win here. Azure
  Change Feed (task E5) is minutes-scale; using it as a lock wake-up
  signal would make blocking acquisition slower than plain polling, not
  faster. AWS S3 (task E6) is a plausible, modest win depending on the
  polling interval being compared against.
- The plain-polling fallback from B3 must stay fully correct and be the
  actual behavior whenever a notification is missed, delayed, never
  arrives, or Change Notifications isn't enabled at all — this task
  only ever sharpens the retry trigger, it never becomes a load-bearing
  dependency for Locking's correctness.
- **Done when:** a test with both features enabled against MinIO shows
  lock handoff latency measurably better than B3's plain-polling
  baseline; a second test with Change Notifications disabled (or
  pointed at a fake Azure-shaped adapter with injected multi-minute
  delay) confirms blocking acquisition still converges correctly via
  the B3 fallback, just without the speedup — proving the dependency is
  genuinely optional, not just untested.

---

## Part C: Permission enforcement

Mode bits are correctly stored and correctly reported via `stat()`, but
nothing currently gates actual access against them — a `chmod 400`
(read-only) file can still be written by its own owner, verified
directly against a real mount, not assumed. (A related, already-fixed
bug: go-fuse's `NullPermissions` default used to silently rewrite a
genuinely-stored `0` mode back to 644/755 on every `stat()` — that one's
done; this part is about the separate, still-open enforcement gap.)

### C1. Enable kernel-level enforcement via `default_permissions`

- Set the `default_permissions` FUSE mount option in `cmd/icbfs` and in
  the test mount setup in `internal/fuseserver/mount_test.go` — the same
  two places `NullPermissions` was added for the earlier bug fix.
- This defers enforcement to the kernel: for every open/read/write (not
  just the explicit `access(2)`-triggered checks go-fuse's own default
  `Access()` fallback already handles today), the kernel compares the
  calling process's uid/gid — and its *full* set of supplementary group
  memberships, not just a primary gid — against whatever `Getattr`
  reports for mode/uid/gid. Preferred over implementing `NodeAccesser`
  or manual checks in `Open`/`Create` ourselves: this reuses the
  kernel's own permission logic, which already correctly handles root
  bypass and group membership, instead of re-implementing a
  correctness-sensitive check by hand.
- **Done when:** tests cover both directions on the owner's own
  access, not just the write case already verified manually: a `chmod
  000` file cannot be read *or* written even by its own owner; a
  `chmod 444` file can be read but a write attempt fails with a
  permission error rather than silently succeeding.

### C2. Decide how to test group/other-bit enforcement, not just owner

- The straightforward test in C1 only exercises the *owner* bits, since
  the test process's uid is the same as the file's recorded owner uid.
  Actually exercising group/other-bit enforcement needs a second
  identity — either running part of the test as a different uid (real
  environment complexity: needs root or an equivalent privilege to
  `setuid`, not just a few more lines of Go) or explicitly accepting
  owner-bit coverage as the automated bar and documenting group/other
  enforcement as verified manually rather than continuously. A real
  decision, not a default to skip silently.
- **Done when:** the decision is made and whichever path was chosen is
  actually exercised — either a genuine differing-uid test, or an
  explicit note in the test file stating what's covered, what isn't,
  and why.

### C3. WinFsp equivalent (blocked on the driver existing)

Windows doesn't use POSIX mode bits as its native enforcement
mechanism — it's ACL/security-descriptor-based. Once the WinFsp driver
exists (Part F), it needs its own enforcement path: real ACL checking
if `ARCHITECTURE.md`'s primary-mode ACL design has been built by then
(see Part F), or at minimum an approximated mode-bit check equivalent to
C1's for a POSIX-primary filesystem mounted on Windows. Not further
broken down here since Part F owns scoping the driver itself.

---

## Part D: Multiple filesystems per bucket — the master block

Implements the Multiple filesystems per bucket section of
`ARCHITECTURE.md`. Depends on Part A (task A1 at minimum) for the
Protobuf toolchain — the master block schema should be defined in
Protobuf from the start, same reasoning as `.metadata`/`.lock`.

### D1. Define the master block schema and bootstrap logic

- `FilesystemEntry{name, size, archived}` / `MasterBlock{repeated
  filesystems}`, per `ARCHITECTURE.md`.
- Create-if-missing bootstrap at a single fixed key, one per bucket —
  mirrors `Filesystem.Bootstrap`'s existing logic for a root block
  almost exactly; worth checking whether that function can be
  generalized/reused rather than duplicated.
- **Done when:** a fresh bucket's master block can be bootstrapped, and
  bootstrapping an already-existing one is a no-op (same bar
  `Bootstrap` already meets for roots).

### D2. Filesystem creation through the master block

- CAS-read the master block; scan the in-memory list for the
  lowest-indexed free (`name == ""`) slot, reuse it if found, else
  append; write back conditioned on the ETag read, retrying on a lost
  race (the standard pattern, applied to a new object).
- Derive the ID from the chosen index, zero-padded to 4 hex digits;
  create the root at `<id>-root-<name>`.
- **Done when:** tests cover: creating a filesystem allocates the
  expected ID; two concurrent creation attempts both succeed with
  distinct IDs (one retries after losing the CAS race, not both landing
  on the same slot); after a filesystem is deleted (D5) and its slot
  tombstoned, a new creation reuses that same freed slot rather than
  only ever appending.

### D3. Uniform ID-prefixing of every object — a real migration, not new-code-only

**This is the task with the widest blast radius in this part.** Every
key-generation call site in `internal/icbfs` currently produces a bare
UUID (or `<uuid>.metadata`, `<uuid>.lock`, etc.) with no filesystem-ID
prefix at all — `Create`, `Mkdir`, `Symlink`, the nlink side-object key
helper, and anything added by Parts B/D's own new code. All of them need
the owning filesystem's ID prepended, which means `Filesystem` needs to
know its own ID (learned at open time, by finding its entry's position
in the master block) and thread it into every key it generates.
- **Done when:** a test creates a filesystem, writes a file, a
  directory, and (once Part B exists) a lock, and confirms every one of
  their keys in the backend actually carries the expected `<id>-`
  prefix — not just that the filesystem still works end to end (the
  existing test suite already proves that), but that the prefix
  convention is actually present on disk, since that's the thing this
  task is actually for.

### D4. `df`/`du` via prefix-scoped listing

- `du`: already free, a tree walk from the filesystem's own root — if
  this isn't already exposed as a callable operation independent of
  `ReadDir`'s existing recursive traversal, this task includes exposing
  it as one.
- `df`: "Total" is the master block entry's `size` field; "Used" is a
  `ListObjectsV2`/`List Blobs` call scoped to the filesystem's `<id>-`
  prefix, summing each returned entry's size across pagination.
- Wire into FUSE's `StatFs` — investigate go-fuse's actual
  `NodeStatfser` interface shape before assuming it, the same way every
  other go-fuse API assumption in this project has been checked first.
- **Done when:** a test creates two filesystems in the same bucket, adds
  differently-sized content to each, and confirms each filesystem's `df`
  "Used" reflects only its own content, not the other's — this is the
  test that actually proves the multi-tenant scoping problem is solved,
  not just that a number comes back.

### D5. Archive and prune lifecycle

- Archive: set `archived = true` on the filesystem's master block
  entry (a CAS write, like any other). Enforcement is checked at
  mount/session start, not per-operation — a mounted session doesn't
  need to notice an archive that happens mid-session; the next mount
  does. Document this explicitly wherever it's implemented, since it's
  a deliberately chosen gap, not an accident.
- Prune: list every object under the filesystem's `<id>-` prefix (the
  same listing D4's "Used" calculation uses) and delete each one, then
  blank the master block entry (`name = ""`), making its slot eligible
  for D2's reuse.
- **Done when:** tests cover: writes against an archived filesystem are
  rejected (checked at the point this project chooses to enforce it —
  mount/session start, per above — not necessarily mid-session); reads
  against an archived filesystem still succeed; pruning actually removes
  every object under the prefix (confirmed by listing after, not just
  trusting the delete calls didn't error) and frees the slot for reuse.

### D6. A tool to change a filesystem's declared size after creation — done

`icbfs.Resize(ctx, store, fsName, newSize)` (`master.go`): a CAS write to
the master block entry's `size` field, same pattern as `Archive`. Takes
effect for every mount of that filesystem immediately (`StatFS` always
re-reads the master block fresh, never caches the value from
`Bootstrap`). No validation against current "Used" — shrinking below
what's already stored is allowed and simply makes `df` report negative
headroom, same as any real filesystem resized smaller than its contents
without a tool checking first. No CLI wiring (same as `Archive`/`Prune` —
all three are library-level admin operations today, not `icbfs mount`
flags).

---

## Part E: Change notifications

Implements the Change notifications section of `ARCHITECTURE.md`.
Depends on Part D (notifications are scoped by the filesystem's ID
prefix — build D's prefixing first). Once this part exists, see Part
B's task B10 — Locking's blocking acquisition can use it to wake up
faster than polling, on MinIO specifically, as an optional enhancement
in the other direction.

### E1. Internal event-source interface

- Define one internal interface all three backend adapters implement:
  something like "a channel of wake-up signals, each naming the
  key/prefix that changed," deliberately minimal — per
  `ARCHITECTURE.md`'s governing principle, a signal is never expected to
  carry authoritative payload data, so the interface shouldn't be
  designed as if it needs to.
- **Done when:** the interface is defined and at least one trivial fake
  implementation exists for testing the consumer side independent of
  any real backend.

### E2. Mount-time opt-in flag, and the explicit "works with none of these" test

- A CLI flag (`cmd/icbfs`), defaulting to off — same pattern as
  Locking's flag.
- **This needs its own explicit, permanent regression test, not an
  assumption that "optional" naturally holds**: mount with no
  notification backend configured at all, and confirm every existing
  operation (create, read, write, mkdir, etc.) works identically to a
  mount that predates this feature entirely — no hang, no error, no
  behavioral difference. This was specifically called for, not left
  implicit.
- **Done when:** that test exists and passes, run as part of the normal
  suite, not a one-off manual check.

### E3. Per-watch UUID tracking and go-fuse wiring

- A FUSE watch gets established via a path lookup, which already
  resolves to a UUID at that moment (`Lookup`'s existing behavior) — the
  driver records "this active watch corresponds to UUID X" at setup
  time, rather than needing any general reverse index from UUID back to
  path.
- Filter incoming wake-up signals (from whichever backend adapter is
  active) against the set of currently-active watched UUIDs; on a
  match, push the notification via go-fuse's `Inode.NotifyContent`/
  `NotifyEntry` — investigate the actual current API shape before
  assuming it, same practice as every other go-fuse assumption in this
  project.
- Renames are the known remaining wrinkle: inotify's `MOVED_FROM`/
  `MOVED_TO` semantics don't fall out of plain "a UUID changed" signals
  — not solved here, flagged for whoever picks this up.
- **Done when:** a real mount test establishes a watch, triggers a
  change through a *second*, independent client against the same
  backend, and confirms the watching mount receives a notification —
  this is the test that actually proves cross-mount delivery works, not
  just that the internal plumbing compiles.

### E4. MinIO adapter

- New dependency: `github.com/minio/minio-go/v7` (confirmed non-standard,
  not reachable via the `aws-sdk-go-v2` client already used everywhere
  else — this is an exception to the project's otherwise-single-SDK
  approach, worth a comment at the import site saying why).
- Use prefix-scoped `ListenBucketNotification`, scoped to the
  filesystem's own ID prefix (MinIO supports this server-side).
- Prefer **sync** delivery mode over async: async can silently drop
  events under sustained queue overload (documented), and since this
  feature is already latency-tolerant by design, sync's slower send rate
  is a good trade, not a real cost.
- **Done when:** a real MinIO container test (testcontainers-go, same
  pattern as the rest of this project) proves an actual change produces
  an actual received notification — this is the one backend that can be
  tested the way everything else in this project is.

### E5. Azure adapter

- Change Feed is a pull-and-resume log — this task includes cursor/
  checkpoint persistence (where does "I've read up to here" live
  between polls?), not just a one-shot read.
- No server-side filtering exists — read the whole account's feed and
  filter client-side for this filesystem's own ID prefix.
- Given latency is minutes-scale and this environment has no way to
  provision a real Azure Storage Account (see `ARCHITECTURE.md`'s
  Testing reality note), this task's test strategy needs its own
  decision: a mocked/fake Change Feed reader exercising the
  checkpoint-and-filter logic in isolation is realistic here; an actual
  end-to-end Azure test is not, and shouldn't be assumed into this
  project's automated suite the way MinIO's can.
- **Done when:** the checkpoint/filter logic has a passing test against
  a fake feed, and the gap (no automated real-Azure test exists) is
  documented plainly in the code, not silently absent.

### E6. AWS S3 adapter

- Bucket notification configuration → SQS; consume via long-polling
  `ReceiveMessage` (`WaitTimeSeconds` up to 20).
- Stays within the existing SDK family (`aws-sdk-go-v2/service/sqs`) —
  no new dependency, unlike MinIO's adapter.
- Same testing-reality constraint as Azure: no real AWS account
  available in this environment, so this task's automated coverage is
  necessarily a fake/mocked SQS consumer, with real-account testing
  documented as a manual, not automated, gap.
- **Done when:** the consumption logic has a passing test against a
  fake queue, same bar as E5.

---

## Part F: WinFsp driver and Windows compatibility

Implements `ARCHITECTURE.md`'s "Windows compatibility: primary mode"
section. The largest single workstream in this roadmap — everything
else Windows-related (Part B's task B9, Part C's task C3) is blocked on
this part, not separately scoped.

**The environment gap that blocked this part is closed.** A previous
version of this section said flatly that no Windows machine or VM was
available to test against, the same category of gap as Part E's "no
real Azure/AWS account" but with no partial workaround. That's no
longer true: a real, fully-unattended, verified Windows Server 2025
eval VM now exists on this host (`TESTING.md`, `test/windows/` — real
KVM/libvirt, confirmed SSH access, a reusable `clean-base` snapshot,
eval license good until **2027-04-08** per `slmgr`'s own authoritative
check). Every "done when" below that says "against a real mount" is
now actually actionable, not aspirational. Two things it does **not**
yet include, confirmed while writing this update:
- **WinFsp itself** (the kernel-mode driver + user-mode DLL every
  WinFsp-backed filesystem needs, task F11) is not installed on the VM
  yet — only OpenSSH and WinSCP are, per `test/windows/README.md`. This
  needs doing once, before F1-level code can be exercised at all, and
  the resulting state re-snapshotted into `clean-base` the same way the
  WinSCP install already was.
- **`winfsp-tests`** (the prebuilt conformance suite `TESTING.md`
  identifies as the real certification bar, `--external` mode) hasn't
  been downloaded onto the VM yet either — straightforward once WinFsp
  itself is installed, ships as a ready-to-run zip on WinFsp's GitHub
  releases, no build step needed.

Both are now unblocked, cheap, one-time setup — not attempted here
without checking first whether that's wanted, since it means booting a
real VM and changing its saved snapshot.

### F1. Choose a Go binding — now fully enumerated, not just researched

Two current, real options — both downloaded and actually read directly
(not inferred from docs), both confirmed to cross-compile clean from
this Linux host to a real Windows `.exe` with `CGO_ENABLED=0` (a trivial
program against each built and verified), so neither costs anything in
dev-loop friction:

- **`github.com/winfsp/go-winfsp`** (root package, v1.0.6) — a direct
  binding to WinFsp's *native* C API. Far more actively maintained (a
  real release as recent as **2026-10-04**, six days before this
  writing, vs. cgofuse's **2025-01-05**). Its full interface is now
  enumerable — `winfsp_windows.go`'s `FSP_FILE_SYSTEM_INTERFACE` struct
  is a direct, complete mirror of WinFsp's real C struct (36 callback
  slots: `Create`, `Open`, `Overwrite`, `Cleanup`, `Read`, `Write`,
  `Flush`, `GetFileInfo`, `SetBasicInfo`, `SetFileSize`, `CanDelete`,
  `Rename`, `GetSecurity`, `SetSecurity`, `ReadDirectory`,
  `Get`/`Set`/`DeleteReparsePoint`, `GetEa`/`SetEa`, ...), each backed by
  a Go-idiomatic opt-in `Behaviour*` interface
  (`BehaviourSetSecurity`, `BehaviourGetReparsePoint`, ...) our
  filesystem type implements only the ones it needs. This maps
  **directly** onto F7 (`BehaviourSetBasicInfo`'s `FILE_ATTRIBUTE_*`),
  F8 (`BehaviourGetSecurity`/`SetSecurity`), and F9
  (`BehaviourCanDelete`/`SetDelete`) below — no translation through a
  FUSE-shaped approximation needed for any of them. It also ships a
  higher-level `gofs` sub-package (an `io/fs`-shaped
  `OpenFile`/`Mkdir`/`Stat`/`Rename`/`Remove` convenience wrapper, with
  its own `memfs` reference implementation) — **not the right layer for
  icbfs**: it has no `SetSecurity`/`SetReparsePoint`/attribute-bit
  control at all, exactly what F7-F9 need, so this driver should bind
  directly against the root package's `Behaviour*` interfaces, not
  `gofs`.
- **`github.com/winfsp/cgofuse`** (v1.6.0) — a FUSE-*compatible* shim:
  the same callback shape across Windows/WinFsp, macOS/macFUSE, and
  Linux/libfuse. Its `FileSystemInterface` **does** have a `Link`
  (hardlink) method, which go-winfsp's native interface has no slot for
  at all — the one real capability asymmetry found. But this is likely
  illusory, not a real advantage: both bindings sit on the exact same
  underlying WinFsp driver, whose native `FSP_FILE_SYSTEM_INTERFACE`
  (confirmed above) has no hard-link callback either — cgofuse's
  `Link()` almost certainly just surfaces as `ENOSYS` on the Windows
  backend regardless, not a real, working hardlink. Worth a direct
  five-minute check against the VM before relying on this read, not
  assumed either way.

**Decided, at Jared's direction: `go-winfsp`'s root package** — the
semantic-fit and maintenance-currency reasons above, confirmed rather
than defaulted to.

**Neither gives you what go-fuse's high-level `fs` package gives
`internal/fuseserver`** — a cached node tree where `Lookup` returns a
reusable `*Inode`. Both are flatly path-string-based. This means
whichever is chosen, `internal/fuseserver`'s `Node` structs are **not
portable** to this driver; only `icbfs.Filesystem`'s core logic is
(it's already key/UUID-based and access-layer-agnostic by design — see
task F2).

- **Done when:** one is chosen and the choice is recorded here with its
  reasoning (done above), and a trivial "hello world" WinFsp mount
  using it is actually proven against the real VM — the same "don't
  just compile it, run it" bar every other external API in this
  project has been held to.

**Done, for real, against the native interfaces the real driver
needs.** `cmd/icbfs-winfsp-hello`, built and run on the VM, proves an
actual mount through `go-winfsp`'s native `Behaviour*` interfaces: a
real `dir J:\` listing (volume label and all), `[System.IO.File]::
ReadAllText` returning the exact expected bytes, a not-found path
correctly surfacing `FileNotFoundException`, and `Get-Acl` round-
tripping the security descriptor. Not `gofs` — the production driver
can't use `gofs` (F7-F9 need `SetSecurity`/`SetReparsePoint`/attribute
control it doesn't expose), so proving the smoke test through `gofs`
would have proven the wrong layer.

**It did not work the first time, and the reasons are load-bearing for
F2-F9, not quirks of the smoke test.** The first native attempt failed
with `ERROR_INVALID_FUNCTION` ("Incorrect function") on every single
I/O, despite the mount reporting success. Two real root causes, each
verified against the actual source rather than guessed:
1. **WinFsp refuses every create/open unless `Create`, `Open`, and
   `Overwrite` are all wired.** Confirmed in WinFsp's own `fsop.c`
   (`FspFileSystemOpCreate`): `STATUS_INVALID_DEVICE_REQUEST` if
   `Create`/`CreateEx` is NULL, or `Open` is NULL, or
   `Overwrite`/`OverwriteEx` is NULL. That NTSTATUS is exactly what
   Win32 renders as "Incorrect function." A filesystem that only wires
   `Open` never has a single callback invoked — which is why
   `Win32_Volume` never saw a volume either — while `Mount()` itself
   succeeds, which is what made this so hard to see. A read-only
   filesystem can simply refuse both; they still have to exist.
2. **The security descriptor must carry an Owner and Group, not just
   a DACL.** With `Create`/`Overwrite` wired, directory listing worked
   immediately, but a *file* open failed with "The security descriptor
   structure is invalid" (`STATUS_INVALID_SECURITY_DESCR`): the
   kernel's access check on a file open rejects a DACL-only SD. `gofs`
   never hits this because it hands WinFsp the current process's own
   SD (`procsd.Load()`), which naturally has both. Directly relevant to
   F8's ACL design — whatever `windows.acl` stores has to be a
   complete SD, or the mount-side translation has to supply the
   missing parts.

A third, latent bug fixed on the way: `GetOrNewDirBuffer` must return
the *same* `DirBuffer` for the same open directory across calls — the
marker-based continuation reads back out of it — not a fresh zero
buffer per call, which leaks the native allocation every time.

**Cross-compilation was ruled out before any of this was found**: at
Jared's direction, native builds were set up on the VM first (Go
1.27.2 installed there — see `test/windows/README.md`), and the
failing code rebuilt natively failed identically. That eliminated the
whole toolchain/ABI class of hypothesis cleanly and pointed at the
Go-level wiring, which is where both real causes turned out to be.
`go-winfsp`'s own module ships no native-interface example or test
anywhere (only a `gofs`-based one, `winfsp_test.go`), so there was
nothing to diff against — the answer came from reading WinFsp's own
C source for what its dispatcher demands, which is the right reference
for F2/F3 too.

**Standing workflow going forward: build on the VM, not cross-compiled
from Linux** — this was Jared's direct instruction after the
cross-compilation question came up, not just this investigation's own
conclusion; see `test/windows/README.md` for the mechanics (source
synced over as a tarball, `go build` run over SSH).

### F2. Path-resolution layer

- Since neither binding provides go-fuse's node-tree/`Lookup`-caches-an-
  `Inode` convenience, this driver needs its own layer resolving a path
  string to an `icbfs.Filesystem` UUID on every call — either a fresh
  tree walk per call, or a driver-owned cache. This is genuinely new
  architecture, not a port of anything already built.
- **Done when:** path resolution is correct for nested directories and
  for multiple hard-linked names resolving to the same UUID — provable
  at the Go unit-test level, independent of a real WinFsp mount, since
  this layer sits entirely on the `icbfs.Filesystem` side of the binding
  boundary.

**Done.** `internal/winfspserver/resolve.go`'s `resolvePath` walks a
WinFsp path string from `fsys.RootKey()` down via repeated
`icbfs.Filesystem.Lookup` calls — a fresh walk per call, no
driver-owned cache in this first cut, same "every call re-walks from
the root" model described above. Deliberately not Windows-only (no
`go-winfsp`/`golang.org/x/sys/windows` import at all): it's pure
`icbfs.Filesystem` logic, so it builds and tests on any platform,
proven by `go test ./internal/winfspserver/...` running clean on this
Linux dev machine with no VM involved — exactly the point of this
task's own "independent of a real WinFsp mount" bar.

Covered by real tests, not just the happy path: nested directories,
two hardlinked names resolving to the identical UUID (confirmed by
reverting the intermediate-directory-type check and watching the
through-a-file test fail, not just trusting it compiles), a genuinely
missing path surfacing `icbfs.ErrNotFound`, and a path that tries to
descend through a plain file surfacing `icbfs.ErrNotDir` rather than a
confusing not-found or a silent wrong answer. Symlink-following through
intermediate components is deliberately out of scope here — not asked
for by this task's own "Done when," and a real design question for F3
(WinFsp represents symlinks as reparse points, a different mechanism
from POSIX's always-transparent-except-at-the-leaf convention).

### F3. Core operation wiring

- Wire the chosen binding's callbacks to `icbfs.Filesystem` via F2's
  path resolution, covering the same basic operation set already proven
  on the FUSE side: create, read, write, mkdir, rmdir, unlink, readdir,
  getattr/setattr, symlink, readlink, link. If `go-winfsp` is the chosen
  binding (task F1): `Create`/`Open`/`Read`/`Write`/`GetFileInfo`
  map directly; `readdir` goes through `BehaviourReadDirectory`;
  symlink/readlink go through `BehaviourSetReparsePoint`/
  `GetReparsePoint` (Windows represents symlinks as reparse points, not
  a separate native concept) — confirm the exact reparse-tag/buffer
  format WinFsp expects against its own header/docs before assuming
  it, same practice as every other external API in this project.
- **Likely real gap, confirm before building around it either way:**
  hardlinks may simply not be supported on this access path at all —
  WinFsp's native `FSP_FILE_SYSTEM_INTERFACE` (task F1) has no hardlink
  callback regardless of which binding wraps it. If confirmed, `Link`
  on a primary-Windows (or Windows-mounted) filesystem should return a
  clear "not supported" error, not silently no-op or corrupt nlink
  bookkeeping that assumes it happened.
- **Reuse `icbfs.Ino()` for file identity — a genuine, confirmed point
  of code reuse, not a Windows-specific reimplementation.** WinFsp's
  `FSP_FSCTL_FILE_INFO.IndexNumber` is a direct analog to FUSE's
  `st_ino`, and — same as FUSE — WinFsp does not generate one for you;
  the existing hash-of-UUID function plugs directly into this different
  struct field.
- **Wire `Create`, `Open`, and `Overwrite` together, from the very
  first commit — not incrementally.** WinFsp's dispatcher
  (`fsop.c`, `FspFileSystemOpCreate`) refuses *every* open with
  `STATUS_INVALID_DEVICE_REQUEST` until all three are non-NULL, while
  the mount itself still reports success. Found the hard way in F1;
  an "Open first, Create later" sequencing of this task would
  reproduce it exactly and look like a mysterious total failure.
- **Done when:** basic file lifecycle (create/read/write/mkdir/rmdir)
  *and* symlink create/read work against a real WinFsp mount; hardlink
  either works for real or fails with a clear, intentional error —
  not an untested unknown either way.

### F4. Case-sensitivity mount flag

- Confirmed as a single whole-volume setting
  (`VolumeParams.CaseSensitiveSearch`) — WinFsp explicitly does not
  support NTFS's newer per-directory mixed-sensitivity model, so there's
  no finer-grained option to consider. This maps cleanly onto the
  already-decided design (case-preserving storage, case-folded
  comparison only at the access layer, from `ARCHITECTURE.md`'s Windows
  compatibility section): set this flag, do the case-fold in this
  driver's own lookup path.
- **Done when:** case-insensitive lookup behaves correctly against a
  real mount while the underlying stored names remain unchanged.

### F5. Primary-mode flag

- Store whether a filesystem is primary-Windows or primary-POSIX, set
  once at creation and immutable afterward — add this to the master
  block's `FilesystemEntry` (Part D) or the root block; either works,
  pick one and be consistent.
- **Done when:** creating a filesystem lets the caller specify primary
  mode, and it's correctly retrievable afterward.

### F6. Reserved-name/character enforcement

- For a primary-Windows filesystem: `Create`/`Mkdir`/`Symlink`/`Link`
  reject Windows-reserved names and characters outright
  (`< > : " / \ | ? *`, trailing space/period, `CON`/`AUX`/`COM1`...).
- **Primary-POSIX behavior, confirmed at Jared's direction** (this was
  only an inference in `ARCHITECTURE.md` before — "presumably allow the
  write and handle presentation via escaping on the Windows access
  path" — now a real decision, not just an unconfirmed guess carried
  forward): a primary-POSIX filesystem **allows** any POSIX-legal name,
  Windows-reserved or not. A Windows client that later accesses such a
  filesystem needs its own escaping/presentation handling for whatever
  it finds — not designed here, since it's a Windows-access-path
  concern against an already-POSIX-primary filesystem, not something
  this task's enforcement logic itself needs to solve.
- **Done when:** tests cover both primary modes' actual behavior for a
  reserved name — Windows-primary rejects it; POSIX-primary accepts it
  — not just the Windows-primary rejection case.

### F7. Windows file attribute bits

- Hidden/System/ReadOnly/Archive — storage (likely another entry in
  `.metadata`'s `xattrs` map, consistent with how ACLs landed there) and
  wiring to WinFsp's `FILE_ATTRIBUTE_*` reporting/setting. If
  `go-winfsp` is the chosen binding (task F1): `BehaviourGetFileInfo`
  reports them, `BehaviourSetBasicInfo` sets them — confirmed real,
  named callbacks on the actual installed interface, not inferred.
- **Done when:** round-trips correctly through a real WinFsp mount.

### F8. ACL wiring

- The `xattrs` map itself is built in Part A's task A4 (`.metadata`'s
  Protobuf schema); this task is specifically about implementing
  WinFsp's `GetSecurity`/`SetSecurity` callbacks (confirmed real,
  FUSE-side-has-no-analog operations) to read/write the `windows.acl`
  entry, and — if `getfacl`/`setfacl`-style POSIX ACL support is ever
  built on the FUSE side — the `system.posix_acl_access` entry
  correspondingly.
- **Every SD handed back to WinFsp must carry an Owner and Group, not
  just a DACL** — the kernel's access check on a file open fails with
  `STATUS_INVALID_SECURITY_DESCR` otherwise (found in F1; directory
  listing tolerates a DACL-only SD, a file open does not). So this
  task has to decide what Owner/Group a `.metadata` entry with no
  stored `windows.acl` reports — mapped from the POSIX uid/gid, a
  fixed well-known SID, or the mounting process's own identity (what
  `gofs` does via `procsd.Load()`) — before anything else here, since
  nothing opens at all until that's settled.
- **Done when:** a real security descriptor round-trips through a real
  WinFsp mount.

### F9. Delete/rename-on-open-file emulation

- Windows's pending-delete and share-mode semantics around a file open
  elsewhere, emulated at this driver's layer per `ARCHITECTURE.md` —
  not a core object-model change. If `go-winfsp` is the chosen binding
  (task F1): `BehaviourCanDelete` is where a pending delete gets
  refused if some other emulated condition says it should be (WinFsp
  itself already handles the core "can't delete while another handle
  has it open without `FILE_SHARE_DELETE`" share-mode enforcement
  natively — confirm exactly how much of this is already free from
  WinFsp itself vs. needs emulating here before assuming it's all this
  task's responsibility).
- **Done when:** tests cover deleting/renaming a file that's open
  elsewhere behaving per Windows semantics, against a real mount.

### F10. Licensing compliance

WinFsp is GPLv3 with a FLOSS exception, verified against its own
`License.txt`, not assumed from general familiarity with GPL-family
licenses — the exception requires **all three** of: (1) icbfs meeting
the Free Software Definition or Open Source Definition; (2) including
WinFsp's specific attribution notice and a link to its repo, in icbfs's
own UI and user-facing docs; (3) **never** linking or distributing
WinFsp together with any proprietary software while relying on this
exception — mixing FLOSS and proprietary under it is not permitted. A
commercial license exists as a fallback if that constraint ever doesn't
fit (per-organization or per-developer terms, confirmed current
pricing available on WinFsp's own site, not reproduced here since
pricing changes independently of this project).
- **Done when:** the attribution notice and repo link actually appear
  in icbfs's own docs/UI wherever a user would encounter the WinFsp-
  backed mount, not just noted here and forgotten.

### F11. Deployment / installer story

- WinFsp requires its own kernel-mode driver and user-mode DLL installed
  on the target Windows machine, **separately from icbfs's own binary**,
  requiring admin rights — confirmed, not assumed. icbfs cannot silently
  carry or install this itself; it's a real, separate prerequisite a
  user or an installer script has to satisfy via WinFsp's own installer.
- **Done when:** icbfs's own install documentation says this plainly and
  points to WinFsp's installer, rather than a user discovering the
  missing dependency only when a mount attempt fails.
