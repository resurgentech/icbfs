# Testing

icbfs is tested in layers:

- its own Go tests, against a real MinIO and real FUSE mounts;
- independently written conformance suites, against real mounts on
  disposable VMs;
- end-to-end runs of the real binaries.

## Latest results

Each run against a fresh MinIO. The first six are from 2026-10-16; the
last three from 2026-10-14.

| suite | where | result |
|---|---|---|
| Go tests (`-race`) | Linux host, Docker | all 19 packages with tests pass |
| pjdfstest | Ubuntu 24.04 VM, FUSE, root | 238 files, 8,798 assertions; 0 unexpected failures, 4 accepted |
| xfstests `-g quick` | Ubuntu 24.04 VM, FUSE | 647 tests: 169 pass, 467 don't run, 11 fail, each listed with its reason |
| several machines | Linux VM and two small VMs, one MinIO | 3 machines: all 6 workloads pass; `fs check` clean |
| winfsp-tests `--external`, case-insensitive | Windows Server 2025 VM | 78 / 78 pass |
| winfsp-tests `--external`, case-sensitive | Windows Server 2025 VM | 77 / 78 pass; `getfileinfo_name_test` fails, as on WinFsp's memfs |
| logging chain | Ubuntu 24.04 VM | 5 / 5 checks pass |
| icbfs-sync end to end | Windows Server 2025 VM | 12 / 12 checks pass; its Go tests pass on NTFS |
| large files (6 GiB, opt-in) | Ubuntu 24.04 VM | 7 / 7 checks pass (6 GiB written in 73 s; a one-byte edit in 22 s) |

## Go tests

```sh
just test                     # go test ./... (needs Docker; the FUSE tests need /dev/fuse and fusermount3)
just test-short               # skips the code-generator check
just test-ci                  # with -race, as CI does
just test-windows             # the packages that run on Windows without Docker
```

CI runs `just ci` on every push and pull request, on GitHub's Linux
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
| `internal/winfspserver` | path resolution and case folding (portable parts); the driver itself is tested by winfsp-tests |
| `internal/acl` | the Windows access check and inheritance |
| `internal/notify/...` | MinIO notifications for real; SQS and Azure against fakes |
| `internal/logd`, `logserver`, `fslog`, `sidecmd` | the daemon chain |
| `pkg/drive/...` | the drive library, the icbfs backend, the HTTP handlers, drafts, copy, diff |
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
| storm | creates 300 names in one shared directory | all are there, and every machine lists the same |
| exclusive | creates the same 100 names with `O_EXCL` | each is created exactly once |
| counter | increments one file 40 times under `flock` | none is lost |
| append | appends 100 lines to one file under `flock` | none is lost or torn |
| rename | moves 50 files between two shared directories 4 times | none is lost or doubled |
| fsstress | 2,000 random operations in a directory of its own | `fs check` |

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

From the repository root, `just pjdfstest`, `just xfstests [tests]`,
`just mount-helper-test`, `just logging-test`, `just largefile-test`,
`just winfsp-tests [icbfs-ci|icbfs-cs|cmemfs-ci]` and `just sync-test`
run the VM suites (the Linux ones find the VM's address themselves), and
`just vm-tests` runs all but the large-file one and xfstests, which takes
most of a day.

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
  directory that grew and then emptied keeps its height.
- **Case-sensitive Windows mounts** fail winfsp-tests'
  `getfileinfo_name_test`, as WinFsp's own memfs does.

## Not tested

- **Real AWS S3.** It is the same S3-API code as for MinIO. Things that
  might differ: `If-Match` on `PutObject`, one-second `Last-Modified`
  resolution for as-of views, and SQS notifications.
- **Azure Blob Storage.** There is no store implementation; the
  change-feed adapter is tested against a fake.
- **xfstests and fsx.** fuse-xfstests is built for gocryptfs's mount
  conventions and would need its own wrapper.
- **A real WebDAV drive** for icbfs-sync. The test VM has no WebDAV
  client, so a folder that behaves as one stands in.
- **The icbfs-sync tray's menus**, by hand.
- **Scale and performance:** directories of millions of entries, long
  histories, many machines writing at once, and throughput.
