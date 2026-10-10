# Roadmap

Concrete development tasks, broken out by workstream. Unlike
`MISSING_FEATURES.md`, everything in this document is already decided —
this is sequencing and task breakdown, not open design. "Done when" is
the acceptance bar for each task — write the test first if that's
practical, same as the rest of this codebase's testing style (real MinIO
via testcontainers-go, not mocks).

Two workstreams so far:

- **Part A: Serialization — JSON to Protobuf.** All of this project's own
  object bodies (directory/root blocks, the not-yet-built `.metadata`
  object, the not-yet-built `.lock` object) move to Protobuf instead of
  JSON. Do this one first, or at least its schema/toolchain setup —
  Part B's `.lock` object should be defined in Protobuf from the start
  rather than built in JSON and migrated a second time.
- **Part B: Locking.** Implements the Locking section (and the Content
  writes: CAS, bounded retry, and escalation to locking section) of
  `ARCHITECTURE.md`.

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
minimum that `.lock` follows the same side-object pattern. Task 2 below
should define `.lock`'s body directly in Protobuf per Part A's schema
conventions, not in JSON — there's no shipped JSON version of `.lock` to
migrate from either, same situation as `.metadata`.

---

## 1. Content writes become CAS-protected

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

## 2. `.lock` object: non-blocking acquire/release/renew, whole-file only

The core primitive, scoped to whole-file first — byte ranges come later
(task 5) once the simple case is solid.

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

## 3. Blocking acquisition via polling

Layered on top of task 2's non-blocking primitive.

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

## 4. Byte-range locks

Generalizes task 2's single-holder body to a list, with overlap
checking.

- Object body becomes `{ranges: [{start, end, holder, expires_at}, ...]}`.
- Acquire checks the requested `[start, end)` against every existing
  entry for overlap; rejects (or blocks, via task 3's loop) only if a
  conflicting range is actually held and unexpired.
- Release/renew operate on the caller's specific range entry, not the
  whole object.
- **Done when:** tests cover: two non-overlapping ranges can be held
  concurrently by different holders; a request for an overlapping range
  is rejected while the conflicting range is held; the same request
  succeeds once that range's lease expires or is released.

## 5. Escalation wiring: content-write retry exhaustion → lock → retry

Connects task 1 to tasks 2-4.

- When task 1's bounded retry budget is exhausted, acquire a lock (via
  task 3's blocking acquire) covering the byte range the write actually
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

## 6. Client-side stronger-than-advisory enforcement

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
- This is distinct from task 5: task 5 is about *this driver's own*
  write succeeding eventually under contention with itself; this task
  is about respecting a lock an *external, cooperating* caller
  explicitly placed via `fcntl`/`LockFileEx`.
- **Done when:** a test holds an explicit lock on a range from one
  simulated client, attempts a write overlapping that range from a
  second simulated client, and confirms it's refused — then confirms a
  write to a genuinely non-overlapping range from the second client
  succeeds normally while the lock is still held.

## 7. Mount-time opt-in flag

- A CLI flag (`cmd/icbfs`, e.g. `--locking`) and the equivalent for
  whatever WinFsp's mount invocation ends up being, defaulting to off.
- When off, lock/unlock calls should return "not supported" (or
  whatever the correct POSIX/Windows errno is for an unimplemented
  lock operation) rather than silently no-op — a caller that thinks
  it's holding a lock when it isn't is worse than a caller that gets a
  clear error.
- **Done when:** a test confirms lock acquisition fails cleanly when
  the mount wasn't started with the flag, and succeeds when it was.

## 8. FUSE wiring

- Investigate go-fuse's actual API surface for `flock`/`fcntl` before
  assuming a specific interface shape — not yet checked against the
  installed go-fuse version the way every other API assumption in this
  project has been checked first.
- Wire whatever that interface is onto tasks 2-6's primitives.
- **Done when:** a real mount test (same style as the existing
  `internal/fuseserver/mount_test.go` suite) exercises `flock`/`fcntl`
  through the actual kernel syscalls against a real mount, not just the
  `Filesystem`-level API directly.

## 9. WinFsp wiring

Blocked on the WinFsp driver existing at all (see `MISSING_FEATURES.md`
— it doesn't yet). Once it does, wire its lock-related callbacks onto
the same tasks 2-6 primitives used by FUSE. Not further broken down
here since the driver itself isn't scoped yet.
