# Testing

icbfs is tested in layers:

- its own Go tests, against a real MinIO and real FUSE mounts;
- independently written conformance suites, against real mounts on
  disposable VMs;
- end-to-end runs of the real binaries.

## Latest results

Each run against a fresh store: MinIO unless the row says otherwise.

| suite | when | where | result |
|---|---|---|---|
| Go tests (`-race`) | 2026-10-24 | Linux host, Docker | all 24 packages with tests pass |
| pjdfstest | 2026-10-16 | Ubuntu 24.04 VM, FUSE, root | 238 files, 8,798 assertions; 0 unexpected failures, 4 accepted |
| pjdfstest, on AWS S3 and on Azure | 2026-10-18 | Linux VM, FUSE, root | the same on each: 0 unexpected failures, 4 accepted |
| xfstests `-g quick` | 2026-10-16 | Ubuntu 24.04 VM, FUSE | 647 tests: 169 pass, 467 don't run, 11 fail, each listed with its reason |
| several machines | 2026-10-17 | Linux VM and two small VMs, one MinIO | 3 machines: every workload passes (14 checks); `fs check` clean |
| several machines, on AWS S3 and on Azure | 2026-10-18 | 3 Linux VMs | all 8 workloads on each, fsstress at 2,000 operations per machine; `fs check` clean |
| Connectathon (cthon04) | 2026-10-17 | Ubuntu 24.04 VM, FUSE | basic, general, special and lock tests all pass; one warning (lock 13.1, mandatory locking) |
| Connectathon, on AWS S3 and on Azure | 2026-10-18 | Linux VM, FUSE | all groups pass on each; the same warning |
| winfsp-tests `--external`, case-insensitive | 2026-10-17 | Windows Server 2025 VM | 78 / 78 pass, at the default `-meta-cache` and at 0 |
| winfsp-tests `--external`, case-sensitive | 2026-10-16 | Windows Server 2025 VM | 77 / 78 pass; `getfileinfo_name_test` fails, as on WinFsp's memfs |
| logging chain | 2026-10-14 | Ubuntu 24.04 VM | 5 / 5 checks pass |
| icbfs-sync end to end | 2026-10-14 | Windows Server 2025 VM | 12 / 12 checks pass; its Go tests pass on NTFS |
| large files (6 GiB, opt-in) | 2026-10-14 | Ubuntu 24.04 VM | 7 / 7 checks pass (6 GiB written in 73 s; a one-byte edit in 22 s) |
| metadata copies (test/metadata) | 2026-10-24 | Ubuntu 24.04 VM, FUSE, root | `cp -a`, `cp -rp`, `rsync -aAXH(U)`, `tar --xattrs --acls`: all keep exactly what they keep on tmpfs |
| metadata copies (test/metadata) | 2026-10-24 | Windows 11 VM, WinFsp | robocopy (`/COPY:DAT /DCOPY:DAT` and `/COPY:DATSO`), Copy-Item and DISM keep exactly what they keep on NTFS; hard links not compared (WinFsp can't make them); xcopy `/O` fails: WinFsp drops the inherited entries of an ACL given at create, leaving an empty DACL |

A later xfstests quick run (2026-10-18, MinIO) stopped at its 8-hour limit
partway through the group: 98 passed, and generic/103 and generic/213
failed, both a preallocation past the space left, which want `ENOSPC` and
got `EFBIG`. That is fixed since (2026-10-19, with a Go test of its own),
but xfstests hasn't been run again to confirm it.

## Go tests

```sh
just test                     # go test ./... (needs Docker; the FUSE tests need /dev/fuse and fusermount3)
just test-short               # skips the code-generator check
just test-ci                  # with -race, as CI does
just test-windows             # the packages that run on Windows without Docker
```

CI runs `just ci` on every push to `main` and every pull request, on GitHub's Linux
runners (Docker and FUSE available), and `just build-windows` and
`just test-windows` on Windows runners.

- Each test that needs storage starts its own MinIO container through
  testcontainers (`internal/testutil/miniotest`), with a fresh versioned
  bucket.
- `internal/fuseserver`'s tests mount real FUSE filesystems in the test
  process, and run helper processes for the cross-process lock tests.
- `pkg/drive/driveapi`'s `TestGeneratedMatchesSpec` regenerates the API
  code and fails if `api.gen.go` is out of date.

What they cover:

| package | covers |
|---|---|
| `internal/block` | block encoding, splits, case-folded order |
| `internal/objstore` | conditional writes, versions, delete markers, as-of views, the server clock |
| `internal/icbfs` | the filesystem core: directories, links, rename, CAS races, retry and escalation, locks and leases, the master block, names, trash, purge, streams, max file size, multipart edits, the disk cache |
| `internal/fuseserver` | real mounts: permissions, POSIX ACLs (enforcement, inheritance, `chmod`), extended attributes, rename, `flock`/`fcntl` across processes, lease renewal, notifications, logging |
| `internal/winfspserver` | path resolution and case folding (portable parts); on Windows only, ACL control flags and inheritance marks (`security_test.go`, run by `test/metadata/run-windows.sh`); the driver itself is tested by winfsp-tests |
| `internal/acl` | the Windows access check and inheritance |
| `internal/notify/...` | MinIO notifications for real; SQS and Azure against fakes |
| `internal/logd`, `logserver`, `fslog`, `sidecmd` | the daemon chain |
| `pkg/drive/...` | the drive library, the icbfs backend, the HTTP handlers, drafts, copy, diff, mirroring, times |
| `pkg/history` | a filesystem's past for programs browsing it, and that revisiting a moment costs no store requests |
| `internal/namecache` | the mounts' name cache |
| `internal/storeconfig`, `internal/storeflags` | store configurations and the store flags |
| `cmd/icbfs` | the mount(8) helper's options |
| `internal/syncer/...`, `cmd/icbfs-sync` | the sync planner and engine, the folder and drive sides, the command's pass |

## Conformance suites

### pjdfstest (POSIX)

[pjdfstest](https://github.com/pjd/pjdfstest) checks POSIX syscall
behavior, down to the errno, for `chmod`, `chown`, `link`, `mkdir`,
`mkfifo`, `mknod`, `open`, `rename`, `rmdir`, `symlink`, `truncate`,
`unlink` and `utimensat`, as root and as other users.

```
test/linux/run-pjdfstest.sh <vm-ip>
```

A run passes when every failing assertion is listed in
`test/linux/pjdfstest-known.txt`. One is listed:

- `rename/24.t` 4–5, 8–9: a directory's link count is always 1, as on
  Btrfs. icbfs stores no `.` or `..` entries, and directories can't be
  hard-linked. pjdfstest marks the same assertions as expected failures
  on Btrfs.

### xfstests (Linux)

[xfstests](https://git.kernel.org/pub/scm/fs/xfs/xfstests-dev.git) (fstests)
is the Linux filesystems' own regression suite. It runs against icbfs's
FUSE mount on the Linux VM, through the mount helper, with a test and a
scratch filesystem on a throwaway MinIO, mounted `-o allow_other,dev,suid`
(the tests use device nodes and setuid binaries):

```
test/linux/run-xfstests.sh <vm-ip> [check arguments]   (default: -g quick)
```

The first run builds xfstests (pinned to a commit) on the VM. Failures
not listed, with a reason, in `test/linux/xfstests-known.txt` fail the
run. It is slow against an object store: the quick group takes most of a
day, as every directory operation is a few store round trips.

Of the quick group's 647 tests, 169 pass and 467 don't run: reflink and
dedupe, block-device and shutdown tests, quotas, encryption, fs-verity,
swap files and the like. 11 fail, each listed with its reason, all
outside icbfs's reach: opening by file handle (go-fuse has no FUSE export
support), FIBMAP, the kernel FUSE side's setgid rule on writes, FUSE's
asynchronous release of OFD locks, `/proc/locks`, and the declared size
not being enforced.

### Connectathon (NFS)

[cthon04](https://git.linux-nfs.org/?p=steved/cthon04.git), the
Connectathon NFS test suite, pinned to a commit: basic (create, remove,
lookups, setattr, read and write, readdir, links, rename, symlinks,
statfs), general (compiles programs on the filesystem, a large file),
special (open-then-delete, renames, holes, truncation, exclusive create,
negative seeks) and lock tests (fcntl ranges up to the last possible
byte, between a parent and a child process).

```
test/linux/run-cthon.sh <vm-ip>                 # one machine; CTHON_TESTS=-b etc. for one group
test/multi/run-multi.sh cthon                   # every machine at once, a directory each
```

The first run clones and builds it on the Linux test VM; the
multi-machine workload copies that build to every machine. Like
pjdfstest, `ICBFS_TEST_STORE=s3` or `azure` runs on a real store.

On 2026-10-17, on MinIO: every group passes on one machine, with one
warning (lock test 13.1: mapping a locked file isn't refused, which is
mandatory locking, as on Linux's NFS). It found two bugs: rewinding an
open directory showed it as it was at the open, not as it is
(rewinddir), and a lock range at the last possible byte, 2^63-1, wasn't
told apart from one to the end of the file.

### Several machines

Several Linux machines mount one filesystem on a throwaway MinIO, locking
on, and run each workload at the same time; each workload's result is
checked from every machine, and at the end, unmounted, `icbfs-cli fs
check` must find nothing wrong. The machines are the Linux VM and small
VMs (1 vCPU, 1 GiB) made for it:

```
just multi-vms [n]          # icbfs-multi-1..n, once (default 2)
just multi-test [workload]  # test/multi/run-multi.sh
```

| workload | each machine | checked |
|---|---|---|
| smoke | one machine creates, writes, renames, links, symlinks, `chmod`s, sets an xattr and an ACL, takes a `flock`, and writes a 20 MB file | each step, and another machine reads the 20 MB file intact |
| storm | creates 300 names in one shared directory | all are there, and every machine lists the same |
| exclusive | creates the same 100 names with `O_EXCL` | each is created exactly once |
| counter | increments one file 40 times under `flock` | none is lost |
| append | appends 100 lines to one file under `flock` | none is lost or torn |
| rename | moves 50 files between two shared directories 4 times | none is lost or doubled |
| fsstress | 2,000 random operations in a directory of its own | `fs check` |
| cthon | Connectathon's basic, general, special and lock tests, in a directory of its own (built by `test/linux/run-cthon.sh`) | every group passes everywhere |

### Real stores: AWS S3 and Azure Blob Storage

The object store's conformance tests (`internal/objstore`: conditional
writes and their 412, put-if-absent, multipart writes under `If-Match`,
version listings and deletes, past views, the server's clock) run against
a real bucket or container as well as MinIO:

```
test/aws/run-conformance.sh     # .env.aws names the bucket; keys from the aws CLI
test/azure/run-conformance.sh   # .env.azure names the account; key from the az CLI
```

`.env.aws` and `.env.azure` are gitignored and hold names only. Each test
writes under a prefix of its own, and the bucket's and account's lifecycle
rules expire what's left after two days. The multi-machine tests run on
either with `ICBFS_MULTI_STORE=s3` or `azure`. `MULTI_MACHINES` limits how
many machines take part, `ICBFS_MULTI_ONLY=1` leaves out the Linux VM (the
small VMs alone), and `STORM`, `EXCL`, `COUNT`, `APPEND`, `RENAMES` and
`FSSTRESS_OPS` size the workloads.

**Shared test state.** What the test scripts keep outside git (`.env.aws`,
`.env.azure`, and each VM kind's `work/`: the Windows VMs' password and
answer ISO, downloaded images, logs) is under `ICBFS_TEST_HOME`, by
default the checkout itself (`test/home.sh`). Another checkout of icbfs,
a submodule in a repo built on it, shares one set by naming the first in
a gitignored `.env.test` at its root: `ICBFS_TEST_HOME=/path/to/icbfs`.

On 2026-10-17, against AWS S3 (us-east-1) and Azure (westus2): all 14
conformance tests pass on each, and on two machines the smoke, storm,
exclusive and counter workloads pass, with `fs check` clean. On
2026-10-18, on each: pjdfstest and Connectathon on a FUSE mount, and the
full multi-machine set on three machines, all pass (see Latest results). Where the
stores differ (Azure moves the ETag on a metadata change and has no
minimum part size; AWS and Azure enforce `If-Match` on deletes, MinIO
doesn't), the tests record each behaviour. Locks are slow on a remote
store: a locked increment took about 5 seconds on S3, every lock, unlock
and close-to-open read being a round trip.

### winfsp-tests (Windows)

[winfsp-tests](https://github.com/winfsp/winfsp/wiki/WinFsp-Testing) is
WinFsp's own conformance suite. In `--external` mode it runs against a
third-party filesystem, with NTFS behavior as the reference. It covers:

- create, open, overwrite and delete, including pending deletes;
- file information, attributes and times;
- security descriptors;
- directory queries and change notifications;
- reparse points and symlinks;
- named streams;
- rename;
- reads and writes: cached, uncached, overlapped and memory-mapped.

```
test/windows/run-winfsp-tests.sh             # case-insensitive
test/windows/run-winfsp-tests.sh icbfs-cs    # case-sensitive
```

**Case-sensitive:** `getfileinfo_name_test` fails, because it reopens a
file under a different case. WinFsp's own memfs fails it the same way when
case-sensitive.

## End-to-end runs

- **Logging chain** (`test/linux/run-logging-test.sh`): `icbfs-mockserver`,
  `icbfs-logd` and a mount, all real binaries. Checks:
  - exactly the expected events are logged, synchronously and with
    `-log-async`;
  - an unknown drive can't mount;
  - a read-only directory drive mounts read-only;
  - `icbfs-cli` works through the daemon.
- **icbfs-sync** (`test/windows/run-sync-test.sh`): the real
  `icbfs-sync.exe`, checked through a real mount. Twelve checks:
  - capability detection;
  - seeding;
  - content and ACLs arriving;
  - renames by ID and by hash;
  - an editor's save becoming a new version;
  - a missing marker holding the pass;
  - an unchanged pass doing nothing.
- **Large files** (`test/linux/run-largefile-test.sh`, opt-in): a 6 GiB
  multipart file, a one-byte edit in the middle, and the max-file-size
  limit.
- **Metadata copies** ([test/metadata](../test/metadata/README.md)):
  `test/metadata/run-linux.sh <vm-ip>` and `test/metadata/run-windows.sh`
  copy a reference tree onto a mount and onto a local disk with each
  platform's ordinary tools, and compare the copies field by field.
- **Two Windows mounts as two machines** (`test/windows/run-consistency.sh`):
  what one changes, an open on the other sees at once, and the other's
  listings within `-meta-cache`.
- **Timings on Windows** (`test/windows/run-bench.sh`): what Explorer and
  Office do to a mount (listing many files, opening small ones, a deep
  path, an Office-style save, copying a folder in) and the mount's peak
  memory, once per `-meta-cache` value.

From the repository root, `just pjdfstest`, `just xfstests [tests]`,
`just mount-helper-test`, `just logging-test`, `just largefile-test`,
`just winfsp-tests [icbfs-ci|icbfs-cs|cmemfs-ci]` and `just sync-test`
run the VM suites (the Linux ones find the VM's address themselves).
`just vm-tests` runs pjdfstest, the mount-helper test, the logging chain,
the multi-machine set, both winfsp-tests runs and the sync test: not the
large-file test, xfstests (most of a day), the single-machine Connectathon
run (`test/linux/run-cthon.sh`), or the metadata copies.

The VMs, and how to set them up, are described in
[test/linux](../test/linux/README.md) and
[test/windows](../test/windows/README.md). Every VM run starts its own
MinIO on the host (`test/minio/with-minio.sh`) and removes it afterwards,
however the run ends, so Linux and Windows runs can go at the same time.
For a MinIO to use by hand:

```
eval "$(test/minio/up.sh)"     # prints ICBFS_MINIO_ENDPOINT, _VM_ENDPOINT, _BUCKET, _ACCESS_KEY, _SECRET_KEY
test/minio/down.sh             # remove it; --all removes any left behind
```

## Known issues

- **Locks on Windows mounts are local to the machine** (see
  [architecture.md](architecture.md#locking)).
- **Directories don't shrink.** Blocks are split, never merged, so a
  directory that grew and then emptied keeps all its blocks: listing it
  stays slow (see [architecture.md](architecture.md#files-and-directories)
  for why, and the planned fix).
- **Case-sensitive Windows mounts** fail winfsp-tests'
  `getfileinfo_name_test`, as WinFsp's own memfs does.

## Not tested

- **xfstests on AWS S3 and Azure:** it has run against MinIO only. SQS
  notifications and Azure's change feed are tested against fakes.
- **xfstests beyond the quick group**, and long fsx and fsstress runs.
- **A real WebDAV drive** for icbfs-sync. The test VM has no WebDAV
  client, so a folder that behaves as one stands in.
- **The icbfs-sync tray's menus**, by hand.
- **Scale and performance:** directories of millions of entries, long
  histories, many machines writing at once, and throughput.
