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
- **Only two backends, on purpose**: MinIO (S3 API) and Azure Blob Storage.
  No generic multi-cloud abstraction.

Files are never named by their filename in the underlying store. Every
file and directory is a plain object addressed by UUID; directories are
objects too, holding rows of `name → UUID`. A write to a file's content
never has to touch its parent directory, because identity (the UUID) is
decoupled from both the name and the metadata — see `ARCHITECTURE.md` for
why that one choice is what makes the rest of the design work.

## Status

Design phase is complete. See:

- **`ARCHITECTURE.md`** — the settled design: object model, metadata
  placement, concurrency, snapshotting, directory sharding, FUSE/WinFsp
  access layers, and Windows compatibility.
- **`questions.md`** — historical design log. Full rationale, options
  considered, and tradeoffs behind each decision in `ARCHITECTURE.md`.

Implementation is starting now.

## Supported object stores

- MinIO (S3 API)
- Azure Blob Storage

Both require object versioning enabled on the bucket/container — that's
the mechanism snapshotting is built on.
