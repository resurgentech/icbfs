# Missing features: the POSIX/Windows superset

This is a working wishlist, not a settled design — unlike `ARCHITECTURE.md`,
nothing here is decided. It exists to collect, in one place, every feature
needed to reach the full superset of what POSIX and Windows filesystems
support, grouped logically, with the real tradeoffs discussed rather than
just listed. Where Windows and POSIX versions of a feature overlap enough
to share a mechanism, they're discussed together.

Some of these were raised earlier and are already decided in
`ARCHITECTURE.md` but **not yet implemented in code** — those are called
out explicitly as "decided, not built," which is a different and smaller
problem than "not yet designed at all."

---

## 1. Locking — POSIX advisory + Windows mandatory

These are different-sized problems that happen to share a storage
mechanism, so they're discussed together.

**What they have in common.** Both need an atomic "claim this if nobody
else holds it" operation, which is exactly the CAS pattern already proven
correct elsewhere in this codebase: read the current lock state, check
it's free (or that your requested range doesn't overlap an existing
holder), write your claim conditioned on what you read, retry on a lost
race.

**Where they diverge, and why that matters:**

- **POSIX advisory locks (`flock`, `fcntl`)** are opt-in — only a caller
  that explicitly asks to lock is affected. Everyone else can ignore the
  lock entirely and read/write the file anyway. This keeps the blast
  radius small: implementing this touches only the explicit lock/unlock
  code path, nothing else.
- **Windows mandatory locking (share-mode, byte-range)** is enforced on
  *every* open/read/write, even for callers who never call a lock
  function. That means every I/O path would need to consult lock state,
  not just an explicit lock call — a materially larger scope than the
  POSIX version, not just "the same thing plus enforcement."
- **`fcntl` byte-range locks** add a structural wrinkle beyond a single
  whole-file flag: multiple non-overlapping ranges can be held by
  different owners simultaneously, so lock state becomes a small list of
  `{range, holder, lease}` entries, and acquiring requires checking your
  requested range against every existing entry for overlap before you
  can write your claim.

**The genuinely hard part isn't the CAS, it's blocking.** A non-blocking
lock attempt (`flock(LOCK_EX | LOCK_NB)`) is easy — try the CAS once, fail
immediately if it's held. A *blocking* attempt is supposed to make the
caller wait until the lock becomes free, and get woken up promptly when
it does. Object storage has no notification primitive — no pub/sub, no
long-poll, nothing that says "tell me when this object changes." The only
way to implement blocking acquisition with pure object-store primitives
is to poll: loop, attempt the claim, sleep, retry. This is correct — it
converges — but it trades real-time wake-up for simplicity: handoff
latency is bounded by the poll interval instead of being near-instant,
and every waiter burns real API calls for as long as it waits. Under
real contention this is a materially worse experience than a real lock
service (Redis, etcd) gives for free via genuine blocking primitives —
which only exist here if we introduce a coordination service, which
`ARCHITECTURE.md` already deliberately declined to do.

**Dead holders need a lease, not just a flag.** If a holder crashes or
disconnects before releasing, a bare "holder: X" never clears on its own.
The fix is a lease (`holder: X, expires_at: T`) that the holder must
periodically renew, and that anyone else can treat as free once `T`
passes. Buildable, but it's real additional machinery — renewal
heartbeats, and a preference for the object store's own server-assigned
timestamps over client wall clocks, to avoid different machines
disagreeing about whether a lease has expired.

**Where the state should live:** its own dedicated object
(`<uuid>.lock`), not folded into a shared attribute object. The reason
isn't "it might collide with an unrelated operation" — it's that lock
state fails both tests that justified combining mode/uid/gid/nlink into
one object in the first place: it's read and written by a *different*
caller (the lock/unlock path, never `stat()`), and it can change at a
*much* higher rate under real use (a database doing per-transaction
byte-range locks) than attributes ever do. Mixing them means every
unrelated `stat()`/`chmod` on a heavily-locked file would compete for
writes on the same object as the lock churn, for the entire duration any
workload is actually using locks heavily — not as a rare coincidence, but
as sustained, avoidable contention.

**Bottom line:** POSIX advisory locking (including byte-range) is
buildable today with no new infrastructure, at the cost of polling-based
blocking instead of real wake-on-release. Windows mandatory locking is a
substantially bigger lift, since it has to be consulted on every I/O, not
just explicit lock calls. Both remain explicitly out of scope per
`ARCHITECTURE.md`'s existing decision ("not a priority for this use
case") — this section exists so that decision is an informed one, not a
default.

---

## 2. Extended attributes and ACLs

Grouping these together because, on Linux, they're actually the same
mechanism: POSIX ACLs (`getfacl`/`setfacl`) are implemented *as* xattrs
(`system.posix_acl_access`, `system.posix_acl_default`). Windows ACLs
(security descriptors — owner SID, group SID, DACL) are conceptually
richer and are not xattrs on Windows, but they're the same "flexible,
possibly large, per-file side data" shape as a POSIX xattr.

- **POSIX xattrs** are arbitrary user-defined name/value pairs attached
  to a file — used for SELinux labels, user-defined tags, and (via the
  ACL xattrs above) permissions beyond plain mode bits. Typical limits on
  real filesystems are generous (tens of KB per value, similar total per
  file) — well beyond what S3's 2KB or Azure's ~8KB native metadata cap
  could ever hold, which is exactly why metadata moved off native
  object-store metadata in the first place.
- **Windows ACLs** were already decided in `ARCHITECTURE.md` (the
  "primary mode" section): store both formats natively, with primary mode
  determining which is authoritative at write time and which is derived
  on access. That decision stands — but **zero code exists for it yet**.
  There's no ACL field anywhere in `internal/icbfs`, no primary-mode flag
  stored on the filesystem at all. This is "decided, not built," not an
  open design question.

**Where this data should live:** its own object (`<uuid>.xattrs` /
folding ACLs into it), not the same object `stat()` reads routinely. The
reasoning mirrors the locking section: xattrs and ACLs are read by a
different caller (`getxattr`/`listxattr`, or a security-descriptor query —
never plain `stat()`), so there's no reason to make every routine `stat()`
call pay for fetching data it didn't ask for. Unlike locks, xattrs aren't
necessarily high-churn — the separation here is purely about not
coupling a hot, frequently-read path to a cold, occasionally-large one.

---

## 3. Permission enforcement — verified, not speculative

This surfaced while writing this document, and was checked directly
against a real mount rather than left as a guess — two distinct findings
came out of it, one a real bug (now fixed), one a real gap (still open).

**Finding 1 (bug, fixed): `chmod 0` was silently rewritten back to
644/755 on every stat.** `chmod 000 locked.txt` followed immediately by
`stat` reported `644`, not `0` — even though the backend's own stored
metadata genuinely had `mode: "0"` (confirmed by reading the object
directly). The cause: go-fuse has a built-in safety default — "if
`Getattr` reports a zero mode, treat it as 644 (files) / 755 (dirs)
instead," specifically so a buggy filesystem can't accidentally lock
someone out of `chdir`ing into their own mount. That default is only
disabled by setting `fs.Options.NullPermissions = true`, which neither
`cmd/icbfs` nor the test mount setup was doing. Fixed in both places, with
`TestMountChmodToZeroIsReportedAccurately` added as a regression test
(it would have failed before the fix, confirmed before committing).

**Finding 2 (real gap, still open): stored mode bits are not actually
enforced on read/write.** Separately verified: `chmod 400 locked.txt`
(owner read-only) correctly *reports* `400` via `stat` — the display bug
above was specific to the all-zero case — but a subsequent write to that
same file, as its own owner, **still succeeds**. Nothing in
`internal/fuseserver`'s `Open`/`Write`/`Create` path checks the caller's
requested access against the file's mode bits at all. FUSE only performs
automatic permission checking when the kernel calls the `ACCESS` opcode
(explicit `access(2)` calls, and directory-traversal checks) — it does
**not** automatically gate `open`/`read`/`write` unless the filesystem
either sets the `default_permissions` mount option (deferring enforcement
to the kernel, simplest) or implements `NodeAccesser`/checks itself in
`Open`/`Create` (more control, more code). Neither is done. Right now,
permission bits in this filesystem are cosmetic for file content access:
correctly stored, correctly reported, not enforced.

---

## 4. Filesystem space reporting (`statfs` / `df`)

Already observed, not hypothetical: the manual CLI demo earlier in this
project showed `df -hT` reporting `0.0K` for size, used, and available
space on the mounted filesystem. That's because `internal/fuseserver`
never implements `StatFs` (go-fuse's `NodeStatfser` interface) — there's
simply no answer being given for "how much space is here."

This is a real design question, not just a missing method: object stores
like S3/Azure don't have a fixed "capacity" the way a local disk does —
from the client's perspective they're effectively unlimited, modulo
account-level quotas that aren't queryable in real time. Options worth
weighing: report a large placeholder capacity (simple, but meaningless to
anything that acts on the number); query actual bucket usage via a
backend-specific API if one exists (likely async/cached, not live, and
differs between S3 and Azure); or report "unknown"/zero deliberately and
accept that `df`-style tooling won't show anything meaningful. Any real
answer needs this decided explicitly, not left as an accidental zero.

---

## 5. Filesystem change notifications

Not previously discussed. Both platforms have a mechanism for "tell me
when something in this directory changes," used heavily by real
tooling: Linux's `inotify`, Windows' `ReadDirectoryChangesW` (what
Explorer and most file-watching apps use under the hood).

This is a genuinely awkward gap for a FUSE-backed filesystem: `inotify`
is a kernel mechanism that watches the kernel's own page/inode cache, and
FUSE's interaction with it is notoriously unreliable unless the FUSE
driver itself explicitly triggers kernel-level invalidation
(`Inode.NotifyContent`/`NotifyEntry` in go-fuse's API) for every change —
and since every write here could come from a *different* client mount
entirely (this is a shared, versioned, multi-client filesystem), one
mount's kernel cache has no way to know about another mount's write
without us explicitly telling it to. Supporting this for real would mean
every mount polling or otherwise discovering remote changes and pushing
invalidations into its own kernel cache — a real feature, not a small
one, and it's never been scoped at all until now.

---

## 6. Windows-specific gaps: decided but not built

Recapping what's already settled in `ARCHITECTURE.md`'s "Windows
compatibility: primary mode" section, since none of it has a line of code
behind it yet:

- The primary-mode flag itself (Windows vs. POSIX, set at filesystem
  creation) — not stored anywhere.
- Reserved-name/character enforcement for a primary-Windows filesystem.
- Windows file attribute bits (Hidden/System/ReadOnly/Archive).
- Delete/rename-on-open-file emulation.
- **The WinFsp driver itself does not exist.** Only the FUSE/Linux access
  layer has been built. This is the largest "decided, not built" item by
  far — everything above is a field or a check; this is an entire second
  access layer.

---

## 7. Explicitly excluded — listed here for completeness, not reopened

These were already decided against or ruled out of scope earlier, and
aren't reopened by this document:

- **Alternate Data Streams** — declined (technically feasible, judged not
  worth it).
- **8.3 short filenames** — declined.
- **Directory junctions / volume mount points** (beyond plain symlinks) —
  declined as redundant.
- **Device/special files** (block/char devices, sockets, FIFOs) — out of
  scope from the original README framing; this was never meant to be a
  full root filesystem.
- **Disk quotas** — not discussed, assumed out of scope given the project
  targets object storage, not a fixed-capacity disk.

---

## 8. Minor or cosmetic gaps

- **`mmap`** — FUSE supports it, and since `FileHandle` already buffers
  the whole file in memory, this plausibly already works without extra
  code, but it hasn't been tested.
- **Sparse files** — writing past EOF then seeking back will work
  correctly under the current whole-object model (the gap just becomes
  real zero bytes), it's just not space-efficient the way a local
  filesystem's true sparse-file support is. Not a correctness gap, just
  not an optimization we have.
