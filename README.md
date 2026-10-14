# icbfs

I Can't Believe this isn't already a Filesystem.

A filesystem built directly on versioned object storage — in the spirit of
[JuiceFS](https://juicefs.com/), but simpler: no chunking, no bespoke block
format. An object is an object.

## What it is

- **Instant snapshotting**, by requiring the underlying object store to
  have versioning enabled and relying on that native version history,
  rather than building a separate versioning layer on top.
- **A full POSIX-like metadata set** — permissions, ownership, timestamps,
  hard links, symlinks — plus a parallel path to solid Windows support.
- **Three backends, on purpose**: AWS S3, Azure Blob Storage, and MinIO
  (S3 API). No generic multi-cloud abstraction.

Files are never named by their filename in the underlying store. Every
file and directory is a plain object addressed by UUID; directories are
objects too, holding rows of `name → UUID`. A write to a file's content
never has to touch its parent directory, because identity (the UUID) is
decoupled from both the name and the metadata — see `ARCHITECTURE.md` for
why that one choice is what makes the rest of the design work.

## Status

Design is settled; implementation is in progress, and the two aren't
fully in step yet. See:

- **`ARCHITECTURE.md`** — the settled design: object model, metadata
  placement, concurrency, snapshotting, directory sharding, locking,
  change notifications, multi-filesystem support, FUSE/WinFsp access
  layers, and Windows compatibility.
- **`ROADMAP.md`** — concrete, sequenced development tasks closing the
  gap between what's shipped and what `ARCHITECTURE.md` describes. A
  working FUSE driver, object-store layer, and directory-sharding logic
  already exist and are tested — but they predate several decisions the
  design has since evolved to (Protobuf instead of JSON, the `.metadata`
  consolidation, the master block for multi-filesystem support), so the
  code and the architecture doc are not currently describing the exact
  same thing. `ROADMAP.md` is what closes that gap.

## Supported object stores

- AWS S3
- MinIO (S3 API)
- Azure Blob Storage

All three require object versioning enabled on the bucket/container —
that's the mechanism snapshotting is built on.

## Creating a filesystem

Mounts don't create filesystems; `icbfs-cli` does, in a bucket with
versioning on:

```
icbfs-cli fs create -fs docs                                  # primary-POSIX, root owned by you
icbfs-cli fs create -fs share -primary-windows -root-owner S-1-5-21-…-1001
icbfs-cli fs list | describe -fs NAME | resize | archive | prune
```

Primary mode and max file size are fixed at creation; the declared size
can change (`resize`). On Windows, a primary-Windows filesystem's root
defaults to the creating account's permissions; elsewhere give
`-root-owner` (full control for that SID, SYSTEM and Administrators) or
`-root-sddl`. Store connection: `-endpoint`, `-bucket`, `-access-key`,
`-secret-key`, or `$ICBFS_ENDPOINT` and friends.

## Mounting the past

Either mount command can show a filesystem read-only as it was at an
earlier time:

```
icbfs mount -as-of 2026-10-12 ... /mnt/yesterday        # as the day ended, local time
icbfs mount -as-of 2026-10-12T15:04:05 ... /mnt/then    # local time
icbfs-winfsp -as-of 2026-10-12T22:04:05Z ... Y:         # RFC 3339
```

The mount fails if the filesystem didn't exist yet at that time, or if
the time is in the future. Unmount as usual (or Ctrl+C the command); its
local version cache is deleted then. See `ARCHITECTURE.md`'s Snapshots
section for how it works and what it costs.

A moment can be given a name — a snapshot that copies nothing — and
mounted by it:

```
icbfs-cli names create -fs docs -name q3-signoff                 # now
icbfs-cli names create -fs docs -name before-import -at 2026-10-01T09:00
icbfs-cli names list -fs docs | delete -fs docs -name NAME
icbfs mount -as-of q3-signoff ... /mnt/q3
```

Names and the trash belong to a drive: here, with store credentials, the
filesystem `-fs` (or a directory in it, `-root /team`); through the daemon,
host's drive, `-drive ID` (see below).

## Trash

Every delete — from a mount or the API — that removes a file's last name,
or a directory, is recorded in the filesystem's trash, kept forever. A
restore brings the item back with its whole history:

```
icbfs-cli trash list -fs docs [-path /reports]
icbfs-cli trash restore -fs docs -id ENTRY [-to /new/path [-parents]] [-replace]
```

Permanently deleting an item (the drive API's purge) erases every version
of it and its trash entry; pruning a filesystem erases every version of
everything in it.

## Logging through a daemon (Linux)

On an host machine a FUSE mount names a **drive**, and gets
everything else — the store's credentials, the filesystem and directory
the drive is, whether this machine may only read it — through a local
daemon that relays over one websocket to the server, the same shape as
host's `hostd`; it logs what happens on it the same way:

```
icbfs-mockserver -secret S -log events.jsonl -drives docs=docs,team=docs:/team:ro -endpoint ... -bucket ... -access-key ... -secret-key ...
icbfs-logd -socket /run/icbfs-logd.sock -server ws://server:8090/ws/daemon -secret S
icbfs mount -log-socket /run/icbfs-logd.sock -drive team /mnt/team     # add -log-async to never wait
```

`icbfs-cli` can go through the daemon too: with `-daemon SOCKET` (or
`$ICBFS_DAEMON`) instead of store credentials, each command goes to the
server, which checks it and runs it as the machine's account — the one
the daemon sees on its socket, whatever the client claims — and logs it
(`icbfs-mockserver -allow-commands` limits which). Names and trash
commands then name the drive (`-drive team`), never a filesystem: only
the server knows which filesystem and directory that is, and its
permissions are on drives. The `fs` commands (administering a bucket's
filesystems, which drives are made on) keep `-fs`.

With `-log-socket` the mount takes its store endpoint, bucket and keys
from the server (and can't mount without them), mounts the drive's
directory as its root (read-only if the server says so), and logs a `read` when a
file is opened for reading, a `write` (with the size) when a file that
was written is closed, a `delete` for each unlink and rmdir, and an `ls`
for each directory listing — each with the caller's uid, gid and pid. By
default every logged operation waits until the server has written the
event; with `-log-async` events are queued instead, and dropped (and
counted) while the daemon is unreachable. `icbfs-mockserver` is a mock:
one shared secret, one set of credentials, a JSON-lines log.

## Keeping a folder in step: icbfs-sync

`icbfs-sync` keeps folders on a Windows machine in step with icbfs drives:
**one-way** (the folder is the truth; the drive mirrors it) or **two-way**
(changes go both ways; when both sides changed, the folder wins). Nothing is
lost on the drive: an overwrite is a new version, a delete goes to its
trash.

It works from listings, not change events, so it works on folders that
can't report changes, such as a WebDAV drive, and catches up after any time
away. On NTFS it uses stable file ids (renames are seen as renames) and
copies ACLs. Where there are no ids, a new file the size of one that's gone
is hashed, and if it matches it is moved on the drive, history and all, not
uploaded again. A pass that would remove more than a set number of files
waits for confirmation, and a folder that lists as empty, or lacks a marker
file, is taken as disconnected and left alone.

```
icbfs-sync init          # write a settings file to edit (paths: icbfs-sync paths)
icbfs-sync set-secret    # store the secret key encrypted for this account
icbfs-sync once          # one pass of every pair, from a command prompt
icbfs-sync install       # start the tray at login
icbfs-sync               # the tray: status, traffic, pause, confirm, log
```

It runs in the user's session, not as a service, because a mapped network
drive belongs to the user's logon and exists only while they're logged in.
To start a pair from a copy already in hand, copy it onto the drive first
through a mount (robocopy keeps the times), then start the sync: files whose
path, size and time match are adopted without being copied again.

## The drive API

Host's document API, as a Go library host-server mounts
(`apiproposal.md` is the design, `spec/openapi/drive.yaml` the contract):

- `pkg/drive` — the `Drive` interface (with per-backend capabilities),
  copying between drives, diffs, drafts in a side store;
- `pkg/drive/icbfsdrive` — the icbfs backend: a drive on a filesystem or a
  directory in one, checking each item's own permissions as the actor's
  profile (Windows ACLs on primary-Windows filesystems, evaluated in Go,
  with inheritance; POSIX modes on primary-POSIX ones);
- `pkg/drive/driveapi` — Fiber v3 handlers (`Mount`), plus the generated
  models and Go client (`go generate ./pkg/drive/driveapi`);
- `pkg/icbfs` — opening a bucket and administering its filesystems.

`drive-api -config drive-api.json` serves it standalone for testing: a
static token, drives, profiles and permissions from the file
(`driveapi.Standalone`); requests carry `Authorization: Bearer TOKEN` and
`X-Drive-Actor: NAME`.

## Windows mount: install WinFsp first

`cmd/icbfs-winfsp` does **not** carry or install
[WinFsp](https://github.com/winfsp/winfsp) itself — it's a real,
separate prerequisite (a kernel-mode driver plus a user-mode DLL) that
has to be installed on the target Windows machine *before* a mount is
attempted, not something icbfs's own binary bundles. Skip this and a
mount attempt fails outright with a DLL-not-found-style error, not a
helpful message pointing back here.

Install it one of two ways, either requiring administrator rights (it
installs a Windows service and files under `Program Files`):

- Download and run the installer from
  [WinFsp's latest release](https://github.com/winfsp/winfsp/releases/latest)
  (`winfsp-<version>.msi`), or
- `choco install winfsp`, if [Chocolatey](https://chocolatey.org/) is
  already set up.

Supported on Windows 7 through 11, x86/x64/ARM64 (per WinFsp's own
documentation). Verified end to end on a real Windows Server VM — see
`test/windows/README.md`'s "Installing WinFsp itself" section for the
exact commands and a real gotcha found along the way (the installer
doesn't add its own `bin` directory to `Path`).

## Third-party notice

The Windows mount (`cmd/icbfs-winfsp`) is built on
[WinFsp](https://github.com/winfsp/winfsp), licensed GPLv3 with a FLOSS
exception. icbfs qualifies under that exception (MIT-licensed, meeting
the Open Source Definition; never bundled or distributed alongside
proprietary software) and carries the exception's required notice:

> WinFsp - Windows File System Proxy, Copyright (C) Bill Zissimopoulos
> https://github.com/winfsp/winfsp

The same notice is printed by `cmd/icbfs-winfsp` itself, at every mount
and in its `--help` output — not just noted here.
