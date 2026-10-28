<p align="center"><img src="docs/icbfs.png" alt="icbfs logo" width="200"></p>

# icbfs

*I Can't Believe this isn't already a Filesystem.*

icbfs is a filesystem stored directly in a versioned S3 bucket (or Azure
Blob Storage container). It works in
the spirit of [JuiceFS](https://juicefs.com/), but is simpler: there is no
chunking and no block format of its own. A file is one object, and the
bucket's own version history is the filesystem's history.

It mounts on Linux (FUSE) and on Windows (WinFsp). It also opens the same
files to Go programs as drives, and keeps Windows folders in step with a
drive.

## Goals

- **Every past moment is readable.** Each write is a new object version, so
  a filesystem can be mounted read-only as it was at any time, or at a
  named version, without copying anything.
- **Nothing deleted is lost** until it is purged. Every delete goes to a
  trash, kept forever, and a restored item comes back with its whole
  history.
- **Full metadata on both platforms.**
  - POSIX: permissions, owners, POSIX ACLs, extended attributes, hard
    links, symlinks, special files.
  - Windows: security descriptors, attribute bits, alternate data streams,
    case-insensitive lookup.
- **No other infrastructure.** Concurrency is compare-and-swap on the
  store's conditional writes. There is no metadata database and no lock
  server.

## How it works

```mermaid
flowchart LR
  subgraph Clients
    L["Linux apps"]
    W["Windows apps"]
    H["Go programs"]
    F["Windows folders"]
  end
  subgraph icbfs
    FUSE["icbfs mount<br/>FUSE"]
    WFS["icbfs-winfsp<br/>WinFsp"]
    API["drive library<br/>pkg/drive"]
    SYNC["icbfs-sync"]
    CORE["filesystem core<br/>internal/icbfs"]
  end
  S3[("versioned bucket<br/>MinIO / S3 / Azure Blob")]
  L --> FUSE --> CORE
  W --> WFS --> CORE
  H --> API --> CORE
  F --> SYNC --> API
  CORE --> S3
```

- Every file and directory is an object named by a **UUID**, never by its
  name.
- **Directories** are B-trees of small blocks, holding `name → UUID` rows,
  and split at the median name.
- **Attributes** live in a separate `.metadata` object. Writing a file
  doesn't touch its directory, and renaming a file doesn't touch its
  content.
- **Every change is a conditional write** (`If-Match` on the ETag). A
  writer that loses a race re-applies only its own edit and retries.
- **One bucket holds many filesystems**, each under its own key prefix,
  listed in a master block.

[docs/architecture.md](docs/architecture.md) has the full design.

## Features

| | |
|---|---|
| **Mounts** | Linux via FUSE 3; Windows via WinFsp |
| **Past views** | mount read-only as of any time (`-as-of 2026-10-12T15:04`) or a named version |
| **Named versions** | a name for a moment: a snapshot that copies nothing |
| **Trash** | every delete recorded forever; restore with full history; permanent purge |
| **POSIX** | modes, owners, POSIX ACLs (enforced by the kernel, default ACLs inherited), extended attributes, hard links, symlinks, FIFOs, sockets, device nodes, `utimensat`, creation times (`statx`) |
| **Windows** | security descriptors (SDDL), ACL inheritance, attribute bits, alternate data streams, creation times, case-insensitive lookup, share modes |
| **Large files** | multipart uploads that send only the parts an edit changed; per-filesystem max file size (default 1 TiB) |
| **Locking** | `flock`/`fcntl` byte-range locks shared between Linux mounts, with leases (on by default) |
| **Drive library** | a Go library over drives: browse, read, write, versions, diffs, drafts, trash, copy between drives, mirror a source, permissions checked per item |
| **Folder sync** | `icbfs-sync`: one-way or two-way sync of Windows folders, including WebDAV drives, with a tray app |

## Tools

| command | what it does | docs |
|---|---|---|
| `icbfs mount` | mount a filesystem on Linux (FUSE) | [mount.md](docs/mount.md) |
| `icbfs-winfsp` | mount a filesystem on Windows (WinFsp) | [mount.md](docs/mount.md#windows-winfsp) |
| `icbfs-cli` | create and manage filesystems; named versions; trash | [cli.md](docs/cli.md) |
| `icbfs-sync` | keep Windows folders in step with drives | [sync.md](docs/sync.md) |

## Status

The core filesystem, both mounts, the drive library, and icbfs-sync are built
and tested against MinIO, and the Linux mount against AWS S3 and Azure
Blob Storage as well. What has been tested:

- **icbfs's own Go tests**, with the race detector, against a real MinIO
  and real FUSE mounts.
- **POSIX conformance:** [pjdfstest](https://github.com/pjd/pjdfstest)'s
  8,798 assertions on a FUSE mount all pass but 4 accepted (a directory's
  link count, as on Btrfs).
- **Linux's own regression suite:**
  [xfstests](https://git.kernel.org/pub/scm/fs/xfs/xfstests-dev.git)' quick
  group. Of its tests that apply (most of the rest need reflinks, block
  devices or quotas), all pass but 11, each for a listed reason outside
  icbfs.
- **NFS's Connectathon suite** (cthon04): the basic, general, special and
  lock tests pass.
- **Windows conformance:** [winfsp-tests](https://github.com/winfsp/winfsp/wiki/WinFsp-Testing)
  `--external` all pass case-insensitive, and case-sensitive all but
  `getfileinfo_name_test`, which WinFsp's own memfs fails too.
- **Several machines on one filesystem:** create storms, `O_EXCL`,
  counters and logs under `flock`, renames and fsstress, then a clean
  `fs check`.
- **Metadata through ordinary copies** ([test/metadata](test/metadata/README.md)):
  a tree of the hard cases (times before 1970 and after 2038, creation
  times, setuid and sticky modes, unnamed owners, extended attributes,
  POSIX and Windows ACLs, attribute bits, alternate data streams, symlinks,
  hard links) copied onto a mount and onto a local disk, and the copies
  compared field by field.
  - Linux: `cp -a`, `cp -rp`, `rsync -aAXH` and `tar --xattrs --acls`
    keep exactly what they keep on a local disk.
  - Windows: robocopy (`/COPY:DAT` and `/COPY:DATSO`), `Copy-Item` and DISM
    keep exactly what they keep on NTFS. `xcopy /O` fails: WinFsp drops
    the inherited entries of the ACLs it copies at create, leaving an
    empty DACL; use `robocopy /SEC`. Hard links can't be made on a Windows mount (WinFsp
    has no way to).
- **End to end:** icbfs-sync against a drive, and large files (6 GiB,
  multipart edits, the maximum size).
- **On AWS S3 and Azure Blob Storage:** the store layer's conformance
  suite, and on a FUSE mount of each, pjdfstest, Connectathon and the full
  multi-machine set, all passing as on MinIO.

**Not yet tested:**

- **xfstests on AWS S3 and Azure**: it has run against MinIO only.
- **The rest of xfstests** beyond the quick group, and long fsx and
  fsstress runs.
- **Scale:** millions of files, many concurrent machines, and performance
  generally.

[docs/testing.md](docs/testing.md) has the latest results, how to run each
suite, and the known issues.

## Building

Requirements:

- **Go 1.27** or later.
- To mount on Linux: FUSE 3 (`fuse3`).
- To mount on Windows: [WinFsp](https://github.com/winfsp/winfsp/releases/latest),
  installed on the machine. It isn't needed to build.
- To run the Go tests: Docker. The tests start their own MinIO containers.
- [just](https://github.com/casey/just), for the recipes below (optional:
  each is a plain `go` command).

Everything is pure Go (`CGO_ENABLED=0` works), and cross-compiles from any
OS:

```sh
just build          # Linux and Windows commands into bin/
just test           # the Go tests (Docker; the FUSE tests need /dev/fuse)
just ci             # what CI runs: fmt, tidy and vet checks, both builds, tests with -race
just                # every recipe, including the VM suites
```

Without just:

```sh
go build -o bin/ ./cmd/icbfs ./cmd/icbfs-cli
GOOS=windows go build -o bin/ ./cmd/icbfs-winfsp ./cmd/icbfs-cli
GOOS=windows go build -ldflags -H=windowsgui -o bin/icbfs-sync.exe ./cmd/icbfs-sync
go test ./...
```

CI ([.github/workflows/ci.yml](.github/workflows/ci.yml)) runs `just ci` on
Linux, with Docker and FUSE, and builds and runs the portable tests on
Windows. The VM suites are run by hand ([docs/testing.md](docs/testing.md)).

The generated code is checked in. `just generate` regenerates it after a
schema change: the Protobuf code from `spec/proto` (needs `protoc`; see
[spec/proto/README.md](spec/proto/README.md)).

## Quick start

Start a MinIO with a versioned bucket, create a filesystem, and mount it:

```sh
docker run -d --name minio -p 9000:9000 cgr.dev/chainguard/minio server /data
docker run --rm --network host -e MC_HOST_local=http://minioadmin:minioadmin@127.0.0.1:9000 \
  cgr.dev/chainguard/minio-client mb --with-versioning local/icbfs

export ICBFS_ENDPOINT=http://127.0.0.1:9000 ICBFS_BUCKET=icbfs \
       ICBFS_ACCESS_KEY=minioadmin ICBFS_SECRET_KEY=minioadmin
bin/icbfs-cli fs create -fs docs
mkdir -p /tmp/docs && bin/icbfs mount -fs docs /tmp/docs
```

Then, from another shell with the same variables set, use it, delete
something, and look back:

```sh
echo hello > /tmp/docs/a.txt
bin/icbfs-cli names create -fs docs -name first
rm /tmp/docs/a.txt
bin/icbfs-cli trash list -fs docs
mkdir -p /tmp/first && bin/icbfs mount -as-of first -fs docs /tmp/first
```

Every command takes the store's connection from the same flags or the
same `ICBFS_*` variables, so keys need not be on a command line.

## Documentation

| | |
|---|---|
| [architecture.md](docs/architecture.md) | the design: objects, directories, metadata, concurrency, locking, past views, trash, Windows compatibility |
| [mount.md](docs/mount.md) | mounting on Linux and Windows, installing WinFsp, mounting the past |
| [cli.md](docs/cli.md) | `icbfs-cli`: filesystems, named versions, trash |
| [api.md](docs/api.md) | the drive library: drives, permissions, versions, drafts, mirroring |
| [sync.md](docs/sync.md) | `icbfs-sync`: setup, settings, how it decides, safety, the tray |
| [testing.md](docs/testing.md) | the test layers, how to run them, latest results, known issues |
| [test/linux](test/linux/README.md), [test/windows](test/windows/README.md) | the disposable test VMs |

## Repository layout

```
cmd/            the commands above
internal/
  icbfs/        the filesystem core
  block/        directory B-tree blocks
  objstore/     the store interface; the S3 and Azure Blob stores, their
                conformance suite, and as-of views of them
  storeconfig/ storeflags/   which store, and how to reach it
  fuseserver/   the FUSE driver
  winfspserver/ the WinFsp driver
  acl/          the Windows access check
  notify/       change-notification sources (MinIO, SQS, Azure change feed)
  syncer/       icbfs-sync's engine, and its folder and drive sides
  pb/           generated Protobuf code
pkg/
  drive/        the drive library and its icbfs backend
  icbfs/        opening a store; filesystem administration
  tokencmd/     a token credential from a program the mount runs
  history/      a filesystem's past for programs that browse it: folders at
                any moment, a file's versions, search including the trash
  icbfstest/    a throwaway MinIO for other modules' tests
spec/           Protobuf schemas
test/           VM test environments and runners
  metadata/     the metadata copy tests: a reference tree and the tools that
                copy it
third_party/go-winfsp/   vendored WinFsp binding, patched
```

## License

MIT; see [LICENSE](LICENSE).

### Third-party notice

The Windows mount (`cmd/icbfs-winfsp`) is built on
[WinFsp](https://github.com/winfsp/winfsp), which is licensed GPLv3 with a
FLOSS exception. icbfs qualifies under that exception: it is MIT-licensed,
meets the Open Source Definition, and is never bundled or distributed with
proprietary software. It carries the exception's required notice:

> WinFsp - Windows File System Proxy, Copyright (C) Bill Zissimopoulos
> https://github.com/winfsp/winfsp

`icbfs-winfsp` prints the same notice at every mount and in its `-help`
output.

`third_party/go-winfsp` is a copy of
[go-winfsp](https://github.com/winfsp/go-winfsp), patched to expose the
callbacks icbfs needs. Its own license is in that directory.
