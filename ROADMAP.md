# Roadmap

Concrete development tasks for icbfs's active work. "Done when" is the
acceptance bar for each task — write the test first if that's
practical, same as the rest of this codebase's testing style (real
MinIO via testcontainers-go, not mocks).

Parts A through F (serialization, locking, permission enforcement,
multiple filesystems per bucket, change notifications, and the WinFsp
driver) shipped and are done — see git history for their full task
breakdown and verification notes. This file tracks only the current,
active workstream.

---

## Part G: Closing real findings from the actual certification runs

Four items, each discussed directly with Jared before scoping (see that
discussion's own record for the full reasoning) and each confirmed
buildable with no hard blockers found during that discussion — not
aspirational. The explicit goal for all four, stated directly: pass the
real test, don't just document why it's excluded. Usefulness of the
underlying feature to a real user is **explicitly not a factor** in
scoping any of these — confirmed directly, more than once, for G2 and
G4 specifically.

**Not yet covered by this part, worth a decision before it's assumed
in scope:** the rest of the open-findings backlog `test/windows/
README.md` and `test/linux/README.md` already catalogue (16 WinFsp
findings beyond the two reparse ones below — `create_readonlydir_test`,
the three privilege-gated `create_*` tests, `getfileattr_test`,
`getfileinfo_name_test`, `rename_test`/`rename_standby_test`,
`setsecurity_test`, the three `rdwr_*` mmap/overlapped findings,
`querydir_test`, `dirnotify_test`; plus Linux's `open/06.t` residual
finding). Those aren't part of this round — raised here so the roadmap
doesn't quietly imply they are, not because they're out of scope
forever.

### G1. Real atime tracking — strict semantics

Today `fillAttr`'s own doc comment is explicit that atime is just
reported as a copy of mtime; there's no separate atime storage at all.
Mtime is free (derived from the object's own S3 `LastModified`); atime
has no backend-native equivalent, so this needs its own explicit
storage and an explicit write on every read.

- New `Atime time.Time` field on `icbfs.Attr`; a new `.metadata` xattrs
  entry (or equivalent explicit metadata field) to persist it, since
  there's no free ride from the object store the way mtime gets one.
- **Strict semantics, decided directly, cost understood and accepted**:
  every read bumps atime via a real `Store.UpdateMetadata` call — not
  `relatime`-style approximation. Jared's own words: "I understand the
  cost... a user would be insane to actually use it nevertheless. We
  will put it in there to be able to pass the tests cleanly." Document
  this cost plainly wherever the write-on-every-read path lives, the
  same way other deliberately-expensive-but-correct choices in this
  project are documented, not left for a future reader to discover by
  profiling.
- Wire into `Stat`, the explicit `utimensat`/`SetAttr` path (setting
  atime directly), and both access layers' read paths (`fuseserver`'s
  `FileHandle.Read`, `winfspserver`'s `Read`) so a real read actually
  triggers the update, not just `Getattr`/`GetFileInfo` reporting
  whatever's already stored.
- `fillAttr` (FUSE)/`fillFileInfo` (WinFsp) report the real stored
  value instead of copying mtime.
- **Done when:** a FUSE read measurably bumps atime (confirmed via
  `stat` before/after, not just code review); a WinFsp read does the
  same through `GetFileInfo`; `winfsp-tests`' `getfileinfo_test`/
  `setfileinfo_test` (currently excluded, `test/windows/README.md`) and
  whichever `pjdfstest` `utimensat`/`open`/`ftime`-style subtests
  currently fail on atime grounds specifically (not the already-
  explained device-type-loop failures) both pass.

**Done.** New `Atime time.Time` field on `icbfs.Attr`; files/symlinks
store it as a new `atime_unix_nano` field on `.metadata`'s own Protobuf
schema (`spec/proto/icbfs/v1/metadata.proto` — a plain additive field,
regenerated via a reconstructed `protoc`/`protoc-gen-go` invocation
confirmed byte-identical against the existing generated files before
trusting it); directories store it as a new `atime` native-metadata
key, same pattern as the existing `mtime` one. `Filesystem.TouchAtime`
(new) bumps it to now for either type, called by both access layers'
real read paths — `fuseserver`'s `FileHandle.Read`/`Node.Readdir`,
`winfspserver`'s `Read`/`ReadDirectory` — never from `Stat`/`Getattr`
itself, matching real POSIX semantics (`stat(2)` must not update
atime). `SetAttr` also gained a `atime *time.Time` parameter for
`utimensat`'s explicit-set path, wired through go-fuse's real
`SetAttrIn.GetATime()` and a new `timeFromFiletime` helper
(`filetimeFromTime`'s inverse) for WinFsp's `SetBasicInfo` — which,
found while wiring this, previously ignored its own
`creationTime`/`lastAccessTime`/`lastWriteTime`/`changeTime`
parameters entirely, silently no-opping any `SetFileTime` call.

Verified for real, not just via portable tests (though those exist
too — `TestTouchAtimeBumpsFileAtime`/`TestTouchAtimeBumpsDirAtime`/
`TestSetAttrSetsExplicitAtime`, each confirmed discriminating by
temporarily breaking the code path and watching the expected failure):
rebuilt and redeployed to both certification VMs. `winfsp-tests`'
`getfileinfo_test`/`setfileinfo_test` both now pass against a real
mount, confirmed alongside all 41 previously-passing default tests
with zero regressions. `pjdfstest`'s `utimensat` category improved
measurably across every file in it (e.g. `00.t`: 24→22 failed,
`02.t`: 3→1, `05.t`: 7→4, `06.t`: 2→0, `09.t`: a parse-error crash →
a real, lesser failure) — not fully clean, because a good chunk of
`utimensat`'s own remaining failures are explained by a related but
genuinely separate gap this task deliberately did not fix: explicit
**mtime**-setting (as opposed to atime) is still a no-op for files,
since mtime is currently derived entirely from the content object's
own S3 `LastModified` with no override field — a real, new finding,
flagged here rather than silently folded into this task's scope or
silently left undocumented.

**One unrelated, pre-existing finding surfaced while verifying this,
not caused by it:** `winfsp-tests`' `create_test` fails
(`ASSERT(ERROR_INVALID_NAME == GetLastError())`, actually
`STATUS_NOT_A_DIRECTORY`) on a trailing-backslash-past-a-file path
check — confirmed via a direct side-by-side run against the
*pre-G1* binary, which fails identically, so this isn't a regression
from this task. Not yet root-caused; likely resolver-layer behavior
(`resolvePath`'s existing `ErrNotDir` path) disagreeing with this
specific Win32 path-validation convention. Logged here as a new, real,
open finding rather than silently left for whoever next runs the full
suite to rediscover from scratch.

### G2. FIFO, device-node, and Unix-socket special files

Confirmed, during discussion, to be much closer to "just metadata" than
it sounds: for FIFOs, device nodes, and Unix domain sockets alike, the
**kernel** services the actual runtime I/O once the special-file type is
reported correctly — none of this needs the FUSE driver to implement
pipe or device semantics itself. AF_UNIX `bind()` also creates its
socket's filesystem entry via the kernel's own `vfs_mknod()` with
`S_IFSOCK`, the identical path FIFOs and device nodes use — confirmed,
not assumed, before scoping this as one task rather than three.

- New `EntryType` values (`spec/proto/icbfs/v1/block.proto` — a plain,
  additive proto enum change, not a migration) for FIFO, char device,
  block device, and socket.
- One new `Mknod` callback in `internal/fuseserver/node.go` (go-fuse's
  real `NodeMknoder` interface: `Mknod(ctx, name, mode, dev, out)`),
  dispatching on `mode & S_IFMT` to create the right entry type. Char/
  block device nodes additionally need the `dev` (rdev: encoded major/
  minor) stored and reported back correctly; FIFOs and sockets don't.
- **POSIX/FUSE-only, confirmed** — NTFS has no equivalent concept, so
  there's nothing to build on `internal/winfspserver`'s side. Flag this
  plainly wherever the WinFsp side's own test/doc coverage might
  otherwise imply parity that doesn't exist.
- **Explicitly scoped to include device nodes, not just FIFOs/sockets,
  at Jared's direct instruction** — the real security/policy question
  this raises (should a shared, possibly-multi-mounted, object-store-
  backed filesystem let a client create a node pointing at a real host
  device?) was raised and explicitly set aside as out of scope for this
  task's own goal: "any judgment about the usefulness of this feature
  is not in scope... if we can support it, we will, simply for the
  benefit of passing the tests."
- **Done when:** `pjdfstest`'s `mkfifo`/`mknod` categories pass (today,
  13 files fail outright — `test/linux/README.md`'s own "already
  out-of-scope" table), and the device-type branches of every other
  type-looping test file (`rename`, `open`, `link`, `chmod`, `chown`,
  `mkdir`, `rmdir`, `unlink`, `symlink`, `utimensat` — currently ~55
  files with partial failures explained by exactly this gap) pass too.

### G3. Local disk cache for file content, replacing the in-RAM buffer

Scoped deliberately narrow after discussion corrected an initial
over-scoping on my part: this is the local-buffering layer only. The
backend protocol (`objstore.Store`'s whole-object `Put`/`Get`) does not
change in this task — no range-GET, no multipart upload. Jared's own
correction: "I don't understand the need for [a backend protocol
change]... did you think I asked for that?" — confirmed no, scope is
local-cache-only.

- `OpenFile` (`internal/icbfs/openfile.go`)'s `data []byte` field —
  today, the *entire* file is always materialized as one in-memory
  slice on `Open` (the root cause of this session's three crash-bug
  fixes, see below) — becomes a local on-disk cache file; `ReadAt`/
  `WriteAt` operate against it via streaming I/O instead of slice
  indexing.
- `Flush` keeps reading the local cache file whole and `Put`-ing it
  whole to the backend — identical write protocol to today, just
  sourced from disk instead of RAM.
- New config: a maximum total local cache size (a `cmd/icbfs` flag,
  needs a sensible default since this isn't optional functionality the
  way `--locking`/`--notify` are) and an LRU eviction policy at
  **per-file granularity** — evict the whole locally-cached copy of the
  least-recently-accessed open file once the total budget is exceeded.
  Not sub-file chunking: there's no backend partial-write path to make
  finer-grained eviction pay off, since this task deliberately doesn't
  touch the backend protocol.
- **Verify, don't assume, that the existing CAS-retry-and-reapply
  `writeOp` log (task B1) carries over unchanged** — it already tracks
  `{offset, data}` edits independent of whatever backs `o.data`, so it
  should be agnostic to this change, but this needs actual confirmation
  against the existing Part B test suite, not just a reasonable-sounding
  assumption.
- **Not a "Done when" requirement, but a real follow-up worth tracking
  once this lands:** this was raised as the reason mmap "should" get
  more straightforward (a local file can be mmap'd directly via the
  OS's own mechanism, rather than emulated over a Go slice) — revisit
  the open `rdwr_mmap_test`/`rdwr_mixed_test` findings
  (`test/windows/README.md`) once G3 ships, to see whether that hope
  actually paid off, rather than assuming it did.
- **Done when:** a large file's `ReadAt`/`WriteAt` traffic no longer
  shows up as a large RSS spike in the mounting process (confirmed via
  the same `runtime.MemStats`-logging technique already used to
  diagnose this session's OOM bug, not just reasoned about); total
  local cache usage never exceeds the configured budget under sustained
  multi-file access; `go test ./...` — especially Part B's existing
  CAS-retry/escalation suite — still passes unmodified.

### G4. Generic (non-symlink) reparse-point support

`internal/winfspserver/reparse.go` already handles symlinks correctly
(confirmed by reading it directly, not re-guessed) — this task is
specifically the *other* reparse-tag case `winfsp-tests`' own
`reparse_guid_test`/`reparse_nfs_test` exercise: setting an arbitrary,
non-symlink reparse tag (a custom GUID, or an NFS-style tag) with
opaque data on any file, confirmed against the real test source
(`tst/winfsp-tests/reparse-test.c`), not assumed from the test name
alone.

- A new, separate `SetReparsePoint`/`GetReparsePoint` path (distinct
  from the existing symlink-specific one) for any tag other than
  `IO_REPARSE_TAG_SYMLINK`: store the raw tag + GUID (when present) +
  opaque data blob as a new `.metadata` xattrs entry — the same pattern
  `windows.acl` (task F8) and `windows.attributes` (task F7) already
  established, not a new storage mechanism.
- A normal `Open` (no `FILE_FLAG_OPEN_REPARSE_POINT`) against a file
  carrying a tag this driver doesn't know how to transparently follow
  must fail distinctly — the real test expects `ERROR_CANT_ACCESS_FILE`
  on the Win32 side; confirm the exact NTSTATUS WinFsp's own header
  maps that to before assuming it, same practice as every other
  external API in this project. Opening *with* that flag must still
  succeed and return the raw reparse point, unresolved.
- **Wire up `BehaviourDeleteReparsePoint`** — confirmed, while scoping
  this task, that `go-winfsp` exposes this as its own separate hook and
  `reparse.go` doesn't implement it at all today, for symlinks or
  anything else. `FSCTL_DELETE_REPARSE_POINT` likely doesn't work for
  *any* reparse point currently — a bonus finding from this task's own
  research, not something `reparse_guid_test`/`_nfs_test` alone forced
  into view, but squarely in scope since both tests exercise delete as
  part of their own flow.
- **Done when:** `winfsp-tests`' `reparse_guid_test` and
  `reparse_nfs_test` both pass against a real mount.

### G5. Fix: symlink creation fails under `SYMBOLIC_LINK_FLAG_ALLOW_UNPRIVILEGED_CREATE`

A real, concrete driver bug — **not** a missing capability, correcting
an earlier assumption surfaced during discussion that this would need
access to the caller's privilege/security token, which `go-winfsp`
genuinely does not expose. Reading the real test source
(`tst/winfsp-tests/winfsp-tests.h`'s `BestEffortCreateSymbolicLinkW`)
showed the actual mechanism: it calls `CreateSymbolicLinkW` with the
modern `SYMBOLIC_LINK_FLAG_ALLOW_UNPRIVILEGED_CREATE` flag first (the
ordinary, no-privilege-needed way Windows creates symlinks today), and
only falls back to the privileged variant if that specific attempt
fails with exactly `ERROR_INVALID_PARAMETER`. This driver currently
returns some other error under that flag (observed: file-not-found),
which breaks the test's fallback logic and its final
`ERROR_PRIVILEGE_NOT_HELD` assertion — a bug in how `Create`/
`SetReparsePoint`'s two-step sequence handles whatever this flag
actually changes about the request, not an unbuilt feature.

- Reproduce directly on the Windows VM with targeted logging — the same
  method that found the `Rename` case-folding bug (task F9) and the
  `applyLocked` OOM trigger (`internal/icbfs`, this session) — to find
  exactly where the `Create`-then-`SetReparsePoint` sequence diverges
  under this specific flag.
- **Done when:** `winfsp-tests`' `reparse_symlink_test` and
  `reparse_symlink_relative_test` both pass against a real mount.
