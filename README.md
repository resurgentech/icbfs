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
