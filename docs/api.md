# The drive API

A document API over **drives**, as a Go library with HTTP handlers. A host
application mounts the handlers in its own server and supplies
authentication, its drive registry and its permission check. The library
does the rest. `cmd/drive-api` serves the same handlers on their own, for
testing.

[`spec/openapi/drive.yaml`](../spec/openapi/drive.yaml) is the contract.
The Go models and client are generated from it into
`pkg/drive/driveapi/api.gen.go` (`go generate ./pkg/drive/driveapi`).

## Terms

| term | meaning |
|---|---|
| **drive** | a place in some backend's storage that users and the API deal with: a whole icbfs filesystem or a directory in one. The host keeps the list of drives. |
| **backend** | a kind of storage implementing the `Drive` interface. icbfs is the one built. |
| **filesystem** | an icbfs filesystem in a bucket's master block. One filesystem can back several drives (the whole tree, or subtrees). |
| **registered bucket** | a bucket the host knows: endpoint, name, credentials. Filesystems are created in registered buckets. |
| **actor** | who is acting: a user or an agent, named by the host. |
| **profile** | who the actor is on a drive's storage: a Windows SID and its groups' SIDs, or a POSIX uid and gids. |
| **version** | one state of a file's content: shown as a time, carried with an ID. |
| **named version** | a name for a time, on a drive or a whole filesystem: a snapshot that copies nothing. |
| **side store** | an icbfs filesystem where the library keeps its own data about documents (drafts), whatever drive the document is on. |

## Packages

```
pkg/drive                 the Drive interface, capabilities, Copy, Diff, Drafts, profiles
pkg/drive/icbfsdrive      the icbfs backend, and the side store on an icbfs filesystem
pkg/drive/driveapi        Fiber v3 handlers (Mount), generated models and client, Standalone
pkg/icbfs                 opening a bucket; creating and administering filesystems
internal/acl              the Windows access check
cmd/drive-api             the standalone server
```

The host calls `driveapi.Mount(router, cfg)`. `Config` is what it
supplies:

| field | what it is |
|---|---|
| `Drive` | finds a drive by its registry ID (`ErrNotFound` if there's none, or the actor may not know of it) |
| `Authorize` | may this request's actor do this permission on this drive? Answers the actor and the actor's profile on the drive. |
| `AuthorizeBucket` | the same for a registered bucket. The profile is who a new filesystem's root is owned by. |
| `Bucket` | opens a registered bucket |
| `Drafts` | a drive's drafts store: the side store in the drive's own bucket where it has one, otherwise the deployment's. `nil` means drafts are not supported. |
| `Stop` | when closed, ends every event stream |
| `Audit` | told of every drive operation once it's answered: actor, profile, route, item, status |

## The `Drive` interface

```go
type Drive interface {
    Capabilities() Capabilities

    List(ctx, dir ItemRef, opts ListOptions) (Page, error)
    Stat(ctx, item ItemRef, at At) (Item, error)
    Read(ctx, item ItemRef, at At, rng *Range) (io.ReadCloser, Item, error)
    Write(ctx, path string, body io.Reader, cond Condition, opts WriteOptions) (Item, error)
    Mkdir(ctx, path string, parents bool) (Item, error)
    Move(ctx, from ItemRef, to string, replace bool) (Item, error)
    Delete(ctx, item ItemRef, recursive bool, cond Condition) error

    Versions(ctx, item ItemRef) ([]Version, error)
    Names(ctx) ([]Name, error)
    CreateName(ctx, name string, at time.Time) (Name, error)
    DeleteName(ctx, name string) error
    Trash(ctx, opts TrashOptions) (TrashPage, error)
    Restore(ctx, entry string, to string, opts RestoreOptions) (Item, error)
    Purge(ctx, item ItemRef, allNames bool) error
    Changes(ctx) (<-chan Change, error)
    Identities(ctx) (Job, error)
    Permissions(ctx, item ItemRef) (ACL, error)
}
```

- An `ItemRef` is a path or an ID.
- An `At` is now, a time, a named version, or a version ID.
- A `Condition` is "only if still this version" or "only if absent".
- Who is acting, and as which profile, comes in the context
  (`drive.WithActor`, `drive.WithProfile`).

**Copy** (within a drive, or between drives) is the library's own: it
reads from the source, writes to the target, and records where the copy
came from (drive, ID, version) on targets that can keep it. It reads as
the actor's profile on the source and writes as the actor's profile on the
target. **Diff** is the library's own too.

### Capabilities

Every drive reports what its backend can do. An operation the backend
can't do answers `not_supported`, so a client can hide it. The icbfs
backend has them all:

| capability | meaning | icbfs |
|---|---|---|
| `ids` | IDs are stable across renames | yes |
| `versions` | a file's older versions | yes |
| `asOf` | the tree at a past time | yes |
| `names` | named versions | yes |
| `trash` | deleted items can be listed and restored | yes |
| `purge` | permanent delete | yes |
| `conditionalWrite` | `If-Match` / `If-None-Match` | yes |
| `events` | a change stream | yes |
| `origin` | a copy keeps where it came from | yes |
| `permissions` | `windows`, `posix`, or none | the filesystem's primary mode |
| `diff` | `computed` or `native` | computed |

## Permissions

There are two checks, in order.

1. **The host's check** gates the action. Each operation names the
   permission it needs (`x-permission` in the spec). The host checks it
   against the actor's roles, and refuses with not-found when the actor may
   not learn the drive exists.
2. **The item's own permissions**, checked as the actor's profile, by the
   filesystem's primary mode, never mixing the two:
   - **Windows:** the item's security descriptor (SDDL) against the
     profile's SIDs. The first entry that names a right decides it, the
     owner keeps READ_CONTROL and WRITE_DAC, and generic rights are mapped.
     An item with no descriptor of its own takes its nearest ancestor's.
     New items get the parent's inheritable entries (OI, CI, NP, IO,
     CREATOR OWNER), owned by the profile.
   - **POSIX:** mode bits, owner and group against the profile's uid and
     gids.

An actor without a profile on the drive is refused with `no_access`.

| permission | operations |
|---|---|
| `drive.read` | list, stat, content, versions, names, as-of reads, diff, trash list |
| `drive.write` | write, mkdir, move, copy into, publish a draft, restore a version |
| `drive.delete` | delete (to the trash) |
| `drive.restore` | restore from the trash |
| `drive.purge` | permanent delete |
| `drive.name` | create and delete named versions |
| `drive.permissions.read` | the identities scan, an item's ACL |
| `bucket.read` | list and describe a bucket's filesystems |
| `bucket.admin` | create, resize and archive filesystems |
| `bucket.prune` | prune a filesystem |

A drive has no "writable" switch. Whether an actor may write is whether it
holds `drive.write`.

A trashed item has no directory left to check against, so the drive's
root is checked instead: reading its versions needs read access there, and
purging it needs write access there.

## Versions

Each content write is a new version with a store-assigned time. Renames,
moves and permission changes don't make versions, so renaming a document
doesn't make an open editor see a conflict.

```json
{"id": "0000-01a2…", "versions": [
  {"time": "2026-10-12T15:04:05.214Z", "version": "3f5e…", "size": 4211, "current": true,
   "names": ["q3-signoff"]},
  {"time": "2026-10-12T14:51:40.002Z", "version": "a19d…", "size": 4102}
]}
```

- `version=<id>` fetches a specific version in one store call. A time means
  listing the file's versions first. `version=` is a named version when the
  drive has a name of that value, and a version ID otherwise.
- Content responses carry `ETag: "<version id>"`.
  - A write with `If-Match: "<version id>"` lands only if the file is still
    that version. Otherwise it answers 412, with the current version in
    `x-drive-current-version`.
  - `If-None-Match: *` means "only if absent".
- Precision is the store's: MinIO keeps milliseconds, and AWS S3 keeps
  seconds. Two saves in one second on S3 share a time, but their IDs still
  tell them apart.
- A file the API creates starts with one version, its content.

**As of a time.** `asOf=<time>` on `list`, `stat` and `content` shows the
drive read-only as it was then.

- A time before the filesystem existed answers 404.
- A time in the future answers 400. A time up to about a second ahead is
  waited for.

**Named versions.**

- `GET …/names` lists the drive's names and the filesystem-wide ones. A
  sibling drive's names are not shown.
- `POST …/names` with `{"name", "at"}` creates one. Without `at`, the time
  is the store's timestamp for writing the name, so everything finished
  before the request is included. `"scope": "filesystem"` makes a
  filesystem-wide name.
- `DELETE …/names/{name}` deletes one. Names are never moved.
- A file version carries a name in `versions` when it was the file's
  current version at that name's time.

Mounts read names too: `icbfs mount -as-of q3-signoff`.

## Endpoints

Drive paths are under `/api/v1/drives/{drive}`, where `{drive}` is the
host's drive ID.

- Items are named by `path=` (from the drive's root) or `id=`.
- Metadata is JSON, content is raw bytes, and times are RFC 3339.
- Paths are looked up case-insensitively on primary-Windows filesystems,
  and exactly on primary-POSIX ones.

### The drive

- `GET /api/v1/drives/{drive}` returns the drive's name, kind,
  capabilities, and the actor's profile and permissions.

### Reading

- `GET …/list?path=` lists one directory.
  - Options: `depth`, `limit`, `cursor`, `sort`, and `asOf` or `version`.
  - Each entry has: name, path, ID, type (`file`, `dir`, `symlink`), size,
    contentType, the current version (time and ID), modified, changed,
    created, `hasDraft` and `draftsBy`.
  - FIFOs, sockets, devices, and anything the profile may not see are left
    out.
- `GET …/stat?path=` returns one item: the same fields, plus every path to
  it (hard links), parentId, owner, and the actor's effective rights on it
  (`read`, `write`, `delete`, `changePermissions`).
- `GET …/content?path=` streams the bytes.
  - Headers: `Content-Type`, `Content-Length`, `ETag`, `Last-Modified` and
    `x-drive-item-id`.
  - `Range` answers 206, and `If-None-Match` answers 304.
  - Options: `version=`, `asOf=`, and `download=1` for an attachment.
  - `HEAD` returns the headers only.
- `GET …/versions?id=` lists a file's versions.
- `GET …/diff?id=&from=&to=` returns a unified diff of two versions of a
  text file.

### Writing

- `PUT …/content?path=` makes the body the file's content.
  - It takes `If-Match`, `If-None-Match`, or neither for an unconditional
    write; and `parents=1` to create missing directories.
  - The body can be up to the filesystem's max file size; anything larger
    answers 413.
- `POST …/mkdir` takes `{"path", "parents"}`.
- `POST …/move` takes `{"from", "to", "replace"}`. The item's ID doesn't
  change.
- `POST …/copy` takes `{"from", "version", "to", "toDrive"}`. It copies
  within the drive or to another drive, optionally from an old or named
  version.
- `DELETE …/content?path=` moves the item to the trash. `recursive=1`
  deletes a non-empty directory as one trash batch. `If-Match` is honored.
- `POST …/restore-version` takes `{"id", "version"}`. That version's content
  becomes current again, as a new version.

### Trash

A delete from the API or from any mount records a trash entry when a file's
last name goes or a directory is removed. An entry holds the ID, name,
original parent and path, when, by whom, and the last version and size.
Entries are kept forever.

- `GET …/trash?path=` lists deleted items, newest first, optionally only
  those from under a directory. A trashed item's versions and content can
  be read before restoring it (`versions?id=`, `content?id=&version=`).
- `POST …/trash/{entry}/restore` takes `{"to", "parents", "replace"}`.
  - Without `to`, the item goes back to its original path.
  - A taken name answers 409.
  - Restoring a directory restores its whole batch.
- A file that still has another name (a hard link) was never deleted, so it
  isn't in the trash.

### Permanent delete

`DELETE …/purge?id=` permanently deletes a live or trashed item. It erases:

- every version of its content, metadata and stream objects;
- its trash entry;
- every user's drafts of it;
- for a directory, everything under it, including its own blocks.

What remains:

- The name stays in old versions of the parent directory, so an `asOf`
  listing still shows it, but it can't be opened.
- The audit log remains.

A file with other names is refused (409 `has_other_names`) unless the
request has `allNames=1`.

### Drafts

An editor's autosave goes to drafts, not to versions.

- `PUT …/drafts?id=` saves the body, with `x-drive-base-version: <version
  id>`.
- `GET …/drafts?id=` returns the actor's draft and its base version.
- `POST …/drafts/publish?id=` makes the draft the file's content, if the
  file is still at the base version. Otherwise it answers 412 with both
  versions; `force=1` overwrites anyway.
- `DELETE …/drafts?id=` discards the draft.

Drafts live in the side store, one object per user and file. Publishing or
discarding a draft removes its old versions too, so autosaves don't pile
up.

A registered bucket can have a side store of its own: a filesystem in that
bucket, so its drives' drafts stay in the same storage as their documents.
Every other drive uses the deployment's side store.

### Permissions

- `POST …/identities` starts a scan of every ACL (or every owner and group)
  on the drive.
- `GET …/identities/{job}` reports the scan's progress. When it's done, it
  lists every SID or uid/gid found, with how many items each appears on.
  icbfs holds SIDs, not names, so the host resolves names its own way.
- `GET …/permissions?id=` returns one item's ACL (SDDL and parsed), or its
  owner, group and mode.

### Change events

`GET …/events` is a Server-Sent Events stream.

- Each `changed` event carries the directory (path and ID) that changed,
  where known; otherwise it is a bare `changed`.
- It carries the API's own writes.
- It can also carry changes made elsewhere, such as through mounts, as
  bare `changed` events: `pkg/icbfs.WatchMinIO` turns MinIO's bucket
  notifications for a filesystem into wake-ups (a burst of writes is one),
  and `icbfsdrive.Drive.Notify` takes them. The standalone server does this
  for a bucket with `"notify": {"<bucket>": "minio"}` in its
  configuration. A drive on a directory gets wake-ups for its whole
  filesystem.

### Storage administration

These are under `/api/v1/buckets/{bucket}/filesystems`, where `{bucket}` is
a registered bucket's ID.

- `GET …/filesystems` lists the bucket's filesystems and their settings.
- `GET …/filesystems/{name}` describes one: primary mode, max file size,
  declared size, used, archived, and created.
- `POST …/filesystems` takes `{"name", "primaryMode", "maxFileSize",
  "size"}` and creates a filesystem. The root starts out owned by the
  creating actor's profile, with full control.
- `PATCH …/filesystems/{name}` takes `{"size"}`. The declared size is the
  only setting that can change; primary mode and max file size are fixed.
- `POST …/filesystems/{name}/archive` makes the filesystem read-only from
  then on.
- `DELETE …/filesystems/{name}` prunes it, erasing every object and
  version. It works on archived filesystems only, and can't be undone.

## Errors

Errors are JSON, `{"error", "code"}`:

| condition | status | code |
|---|---|---|
| no such drive, item, version or name; or the actor may not see it | 404 | `not_found` |
| allowed to see, not to do this | 403 | `forbidden` |
| no profile on this drive | 403 | `no_access` |
| the backend can't do this | 501 | `not_supported` |
| already exists | 409 | `exists` |
| directory not empty | 409 | `not_empty` |
| not a directory / is a directory | 409 | `not_dir` / `is_dir` |
| other names exist (purge) | 409 | `has_other_names` |
| stale version (`If-Match`, a draft's base) | 412 | `precondition_failed` |
| bad name or path | 400 | `invalid_name` |
| past the max file size | 413 | `too_large` |
| write to an as-of view or an archived filesystem | 409 | `read_only` |
| write contention exhausted the retries | 503 | `busy` |

## Consistency

- Writes are compare-and-swap, as they are from mounts.
- A mount's unflushed writes are visible only on that machine until the
  file is closed there.
- A move between directories is a remove then an insert, so a listing made
  in between can show the item in neither directory or in both.

## The standalone server

```
drive-api -config drive-api.json [-listen :8080] [-audit]
```

The configuration (`driveapi.Standalone`) stands in for a host:

```json
{
  "listen": ":8080",
  "token": "a-static-token",
  "buckets": {"main": {"endpoint": "http://127.0.0.1:9000", "bucket": "icbfs",
                        "accessKey": "…", "secretKey": "…"}},
  "drives": {"docs": {"name": "Documents", "bucket": "main", "filesystem": "docs", "root": "/"}},
  "sideStore": {"bucket": "main", "filesystem": "drafts"},
  "profiles": {"alice": {"windows": {"user": "S-1-5-21-…-1001"}}},
  "permissions": {"alice": ["drive.*", "bucket.read"]}
}
```

- Requests carry `Authorization: Bearer <token>` and `X-Drive-Actor: <actor>`.
- An actor not listed in `permissions` has every permission.
- `driveProfiles` gives a drive its own profiles, and `bucketSideStores`
  gives a bucket its own side store.
- `notify` feeds a bucket's drives' event streams with changes made
  elsewhere (`"minio"`).
- `-audit` writes every operation to stdout as one JSON line.

## Not supported

- Signed links for fetching bytes directly from the store; the API proxies
  all content.
- Document conversion, thumbnails, previews, search, and real-time
  co-editing.
- Editing ACLs or modes through the API; Windows mounts can do that.
- Alternate data streams and locks.
- Running several API instances against one bucket has not been designed
  for.
