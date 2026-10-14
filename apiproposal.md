# Drive API — proposal

Status: **built** — see "As built" at the end for where it lives and where
it departs from this text. A call nobody has confirmed yet is marked
**(proposed)**. The consumable spec is `spec/openapi/drive.yaml`.

## Purpose

The document API of host's backend, replacing host's current doc
service. Through it host's web app lets its users:

- browse drives as trees;
- open a file — shown in-app when the web app can (documents, mostly), else
  offered as a download; the API delivers bytes and their type, no
  conversion;
- look at a file's older versions, at a drive as it was at a past time or at
  a named version, and at deleted files, and bring them back;
- edit documents in an in-app editor, with drafts and conflict detection;
- copy documents between drives.

It's also what host's own code (agents' reviews, say) uses to reach
drives, through the Go interface underneath the HTTP API.

## Terms

| term | meaning |
|---|---|
| **drive** | what users and the API deal with: a connection to a place in some backend's storage — a whole icbfs filesystem or a directory in one, a SharePoint library, a Git repository — with a kind, a writable policy, and an owning org. Host keeps the list of drives. |
| **backend** | a kind of storage a drive can be on, implementing the `Drive` interface: icbfs first; SharePoint, a network file share, Git later. |
| **icbfs filesystem** | icbfs's own storage unit, in the bucket's master block, what a mount attaches to (`-fs`). The engine keeps this name; one filesystem can back several drives (the whole tree, subtrees). |
| **registered bucket** | a bucket host knows: endpoint, bucket name, credentials. icbfs filesystems are created in registered buckets. |
| **profile** | an identity a drive's storage knows (a Windows SID and its groups, or a POSIX uid and its gids) that an actor acts as on that drive. |
| **version** | one state of a file's content: shown as a time, carried with an id. |
| **named version** | a name for a time, on a drive or a whole filesystem: a snapshot that copies nothing. |
| **side store** | an icbfs-backed store where host keeps its own data about documents — drafts, review information — whatever drive the document is on. |

## Decisions

| | |
|---|---|
| Form | A Go library: a `Drive` interface (the superset of what drives can do), backends implementing it (icbfs first), thin Fiber handlers over it, the side store, and icbfs storage administration. host-server mounts the handlers. A standalone binary serves them for testing. |
| Transferability | Each drive declares its capabilities; an operation its backend can't do answers `not_supported`, and the web app hides it. |
| Registry | Host owns drives (and their role assignments); the library serves whatever drive host hands it. |
| Storage | Host registers buckets. icbfs filesystems are created through the API (from host's admin panel) or with `icbfs-cli`, in a registered bucket, with all their settings given at creation. Mounts never create a filesystem. |
| Machines | Gone from the API. A machine is granted permission to mount a drive's storage. |
| Compatibility | None with host's current doc endpoints. |
| Permissions | Two layers: host's role check (kind-dot-action permissions, role assignments, inheritance) gates the action; then the file's own permissions, checked as the actor's profile — Windows ACLs on primary-Windows filesystems, POSIX modes on primary-POSIX ones. |
| Default permissions | A file or folder without an ACL of its own inherits from its nearest ancestor that has one; the root starts with the creating admin having full control. |
| Versions | Shown as times; each also carries an id, which requests use where they can (it saves the backend lookups). |
| Named versions | On a drive (its subtree) or a whole filesystem; created for now or a past time; create and delete only. Read through the same as-of logic as times. |
| Drafts | In the side store, per user and file; publishing is conditional on the version the draft started from. |
| Trash | Every delete is recorded, forever; a deleted item can be restored anywhere with its history. Permanent delete, one item at a time, removes its content and metadata (every version), its trash entry and its drafts; the name may remain in old versions of the parent directory; the audit log remains. |
| Presence | Host's, not the drive API's: host already knows its users and has a live channel to them. |
| Viewing | No conversion; bytes and content type only. |
| Toolchain | Like host: Fiber v3, OpenAPI spec-first, oapi-codegen. |

## Architecture

```
host-server
  ├── its drive registry, role assignments, users/groups/orgs, admin panel
  └── mounts  pkg/drive (library)
                ├── HTTP handlers  /api/v1/drives/{drive}/…, /api/v1/storage/…
                ├── Drive interface + capabilities
                │     └── backends: icbfs (first), later SharePoint, file share, Git
                ├── side store (drafts, review data) on an icbfs filesystem
                └── icbfs storage administration (create, describe, list, resize, archive, prune)

icbfs-logd ── the machine's daemon: mounts and icbfs-cli reach host through it
icbfs mount, icbfs-winfsp ── mounts; icbfs-cli ── side commands and storage admin
```

- **Library**: `pkg/drive`, public so host's module can
  import it; the icbfs core exposes what it needs through `pkg/icbfs`,
  with the access layers staying in `internal/`.
- host-server calls `driveapi.Mount(app, cfg)` with its Fiber app, its
  authentication, its permission check and its drive registry.
- **Standalone**: `cmd/drive-api` serves the same handlers
  with a static token, a drive list and a profile map from configuration,
  for testing and the VM runs.

### The `Drive` interface (sketch)

```go
type Drive interface {
    Capabilities() Capabilities

    List(ctx context.Context, dir ItemRef, opts ListOptions) (Page, error)
    Stat(ctx context.Context, item ItemRef, at At) (Item, error)
    Read(ctx context.Context, item ItemRef, at At, rng *Range) (io.ReadCloser, Item, error)
    Write(ctx context.Context, path string, body io.Reader, cond Condition, opts WriteOptions) (Item, error)
    Mkdir(ctx context.Context, path string, parents bool) (Item, error)
    Move(ctx context.Context, from ItemRef, to string, replace bool) (Item, error)
    Delete(ctx context.Context, item ItemRef, recursive bool, cond Condition) error

    Versions(ctx context.Context, item ItemRef) ([]Version, error)   // capability: versions
    Names(ctx context.Context) ([]Name, error)                       // capability: named versions
    CreateName(ctx context.Context, name string, at time.Time) (Name, error)
    DeleteName(ctx context.Context, name string) error
    Trash(ctx context.Context, opts TrashOptions) (TrashPage, error) // capability: trash
    Restore(ctx context.Context, entry string, to string, opts RestoreOptions) (Item, error)
    Purge(ctx context.Context, item ItemRef, allNames bool) error    // permanent delete
    Changes(ctx context.Context) (<-chan Change, error)               // capability: events
    Identities(ctx context.Context) (Job, error)                      // capability: permissions
    Permissions(ctx context.Context, item ItemRef) (ACL, error)
}
```

`ItemRef` is a path or an id; `At` is now, a time, a named version, or a
version id; `Condition` is "only if still this version" or "only if absent".
Copy, between drives or within one, is the library's own: read from the
source, write to the target, and record where it came from (`origin`: drive,
id, version) on targets that can keep it; a diff is the library's own too,
unless a backend does it natively (Git).

### Capabilities

Every drive reports them (`GET …/drives/{drive}`):

| capability | icbfs | SharePoint | file share | Git |
|---|---|---|---|---|
| `ids` — stable across renames | yes | yes | no | no |
| `versions` | yes | yes | no | yes |
| `asOf` — the tree at a time | yes | no | snapshots, if any | yes |
| `names` — named versions | yes | no | snapshots, if any | yes (tags) |
| `trash` | yes | yes | no | no |
| `purge` | yes | yes | yes | hard |
| `conditionalWrite` | yes | yes | no | yes |
| `events` | yes | yes | no | polling |
| `permissions` | `windows` or `posix` | its own | `posix` | none |
| `diff` | computed | computed | computed | native |

This document specifies the full set, as the icbfs backend does it.

## Storage: buckets and icbfs filesystems

Infrastructure provides buckets (with versioning on) and their credentials;
host registers them. Everything else is done here.

`/api/v1/storage/{bucket}/…` (host's registered bucket id), for those
holding the storage permissions:

- `GET  …/filesystems` — the icbfs filesystems in the bucket (its master
  block), with their settings.
- `GET  …/filesystems/{name}` — one filesystem: primary mode, max file size,
  declared size, used, archived, created.
- `POST …/filesystems` `{"name", "primaryMode", "maxFileSize", "size"}` —
  create. Every setting is given explicitly; the root starts with the
  creating admin's profile holding full control (that's just the root folder's
  ACL, changeable like any folder's).
- `PATCH …/filesystems/{name}` `{"size"}` — the declared size is the one
  setting that changes later (primary mode and max file size are fixed at
  creation).
- `POST …/filesystems/{name}/archive` — read-only from then on.
- `DELETE …/filesystems/{name}` — prune: every object and version of it
  (archived filesystems only; irreversible).

`icbfs-cli` does the same with store credentials given directly, for use
without host (`icbfs-cli fs create|list|describe|resize|archive|prune`).
A mount of a filesystem that doesn't exist fails; it no longer creates one.

Drives are then created in host on registered storage: a filesystem and
a root path in it (`/` for the whole filesystem).

## Identity and permissions

**Authentication** is host's; the library never sees credentials. An
actor is a user or an agent.

**Host's check** gates every operation: the library names a permission
(kind-dot-action) per operation, host checks the actor's role
assignments on the drive and its ancestors, and refuses as not-found where
the actor may not learn the drive exists. The permission set the library
declares for the drive kind (**proposed** names):

| permission | operations |
|---|---|
| `drive.read` | list, stat, content, versions, names, as-of reads, diff, trash list |
| `drive.write` | write, mkdir, move, copy into, publish a draft, restore a version |
| `drive.delete` | delete (to the trash) |
| `drive.restore` | restore from the trash |
| `drive.purge` | permanent delete |
| `drive.name` | create and delete named versions |
| `drive.permissions.read` | the identities scan, an item's ACL |
| `storage.read`, `storage.admin` | the storage endpoints |

**Profiles.** For each drive, host maps an actor to a profile: a
Windows SID and its group SIDs on a primary-Windows filesystem, a uid and its
gids on a primary-POSIX one. The library asks through an interface host
implements, and refuses with `no_access` without one.

**The file's own check**, by the filesystem's primary mode, never mixing the
two:

- Windows: the file's security descriptor (stored by icbfs as SDDL) against
  the profile's SIDs — deny entries before allow, well-known SIDs, generic
  rights mapped. Mounts get this check from Windows itself; the API needs its
  own, in Go, tested against Windows' decisions.
- POSIX: mode bits, owner and group against the profile's uid and gids.

A file or folder without an ACL inherits from its nearest ancestor that has
one; new items get the parent's inheritable entries, owned by the profile.

**Identities.** `POST …/identities` starts a scan of every ACL (or every
owner and group) on the drive; `GET …/identities/{job}` reports progress and,
when done, every SID or uid/gid found, with how many items each appears on.
icbfs holds SIDs, not names (names live in Active Directory), so host
resolves names its own way. `GET …/permissions?id=` returns one item's ACL
(SDDL and parsed), or its owner, group and mode.

**Audit.** Every operation goes through the logging chain (`icbfs-logd` →
server) with the actor and the profile.

## Versions

Each content write to a file is a new version with a store-assigned time.
The API shows versions as times and carries an id with each:

```json
{"id": "0000-01a2…", "versions": [
  {"time": "2026-10-12T15:04:05.214Z", "version": "3f5e…", "size": 4211, "current": true,
   "names": ["q3-signoff"]},
  {"time": "2026-10-12T14:51:40.002Z", "version": "a19d…", "size": 4102}
]}
```

- Requesting a specific version uses its id (`version=3f5e…`) — one call to
  the store, where a time means listing the file's versions first.
- `ETag: "<version id>"` on content responses; `If-Match: "<version id>"` on
  a write: only if the file is still that version, else 412 with the current
  one in `x-drive-current-version`. `If-None-Match: *` only if absent.
- Only content writes make versions; a rename, move or permission change
  doesn't, so renaming a document doesn't make an open editor see a conflict.
- Precision is the backend's: MinIO and Azure to the millisecond or finer,
  AWS S3 to the second (two saves in one second share a time; the ids still
  tell them apart).

**As of a time.** `asOf=<time>` on `list`, `stat` and `content` shows the
drive as it was then, read-only: the newest version of everything at or before
that time. Before the filesystem existed → 404; in the future → 400 (a time
up to about a second ahead is waited for).

**Named versions.** A name is a label for a time:

- `GET …/names` — the drive's names, and the filesystem's (they apply in any
  subtree); a sibling drive's are not shown.
- `POST …/names` `{"name": "q3-signoff", "at": "2026-09-30T23:59:59Z"}` — `at`
  omitted means now: the name's time is the store's own timestamp of writing
  it, so everything finished before the request is included, with no wait.
  `scope: "filesystem"` for a filesystem-wide name.
- `DELETE …/names/{name}`. Names are created and deleted, never moved.
- `version=<name>` anywhere `asOf` is accepted resolves the name to its time.
- A file version carries a name in `versions` when it was the file's current
  version at the name's time.

Mounts read them too: `icbfs mount -as-of q3-signoff`. Later,
a hidden, read-only `.versions/<name>/` directory at a mount's root, like ZFS's
`.zfs/snapshot`, to reach named versions from a live mount (not in v1).

## HTTP endpoints

Base `/api/v1/drives/{drive}` (host's drive id). Items by `path=` (from
the drive's root) or `id=`; JSON for metadata, raw bytes for content, RFC 3339
times. Paths are looked up case-insensitively on primary-Windows filesystems
and exactly on primary-POSIX ones.

### The drive

`GET /api/v1/drives/{drive}` — name, kind, writable, capabilities, the
actor's profile and permissions. (Listing drives is host's.)

### Browsing and reading

- `GET …/list?path=` — one directory; `depth`, `limit`, `cursor`, `sort`,
  `asOf` / `version`. Entries: name, path, id, type (`file`, `dir`,
  `symlink`), size, contentType, the current version (time and id),
  modified, changed, created, `hasDraft`, `draftsBy`. FIFOs, sockets,
  devices and anything the profile may not see are left out.
- `GET …/stat?path=` — one item: the same, plus every path to it (hard
  links), parentId, owner, and the actor's effective rights on it (`read`,
  `write`, `delete`, `changePermissions`), so the web app can grey out what
  can't be done.
- `GET …/content?path=` — the bytes, streamed, with `Content-Type`,
  `Content-Length`, `ETag`, `Last-Modified`, `x-drive-item-id`; `Range` → 206;
  `If-None-Match` → 304; `version=`, `asOf=`; `download=1` for
  `Content-Disposition: attachment`. `HEAD` for headers only.
- `GET …/diff?id=&from=&to=` — a unified diff of two versions of a text file.

### Writing

- `PUT …/content?path=` — the body becomes the content; `If-Match` /
  `If-None-Match`, or neither for an unconditional write; `parents=1`; up to
  the filesystem's max file size (413 past it).
- `POST …/mkdir` `{"path", "parents"}`
- `POST …/move` `{"from", "to", "replace"}` — the id doesn't change.
- `POST …/copy` `{"from", "version", "to", "toDrive"}` — within the drive, or
  to another drive (`toDrive`), optionally of an old or named version; the
  copy records its origin where the target can keep it.
- `DELETE …/content?path=` — to the trash; `recursive=1` for a non-empty
  directory (one trash batch); `If-Match` honored.
- `POST …/restore-version` `{"id", "version"}` — that version's content
  becomes current again, as a new version.

### Trash

A delete — from the API or any mount — records a trash entry when a file's
last name goes or a directory is removed: the id, the name, the original
parent and path, when, by whom, the last version and size. Kept forever, in a
hidden per-filesystem index (one object per entry, listed newest first, outside the visible
tree). Restoring works because icbfs keeps a file under its id: its versions
stayed put, so restoring makes the last one current and gives it a name again,
and its history simply continues.

- `GET …/trash?path=` — deleted items, newest first, optionally only from
  under a directory. Versions and content of trashed items can be read before
  restoring (`versions?id=`, `content?id=&version=`).
- `POST …/trash/{entry}/restore` `{"to", "parents", "replace"}` — to its
  original path without `to`; 409 if the name is taken. A directory restores
  its whole batch.
- A file that still has another name (hard link) was never deleted and isn't
  in the trash.

### Permanent delete

`DELETE …/purge?id=` — a live or trashed item, irreversible: every version
of its content and metadata objects and streams, its trash entry, and every
user's drafts of it; a directory recursively, its own directory blocks
included. The name stays in old versions of the parent directory (an `asOf`
view of the parent still lists it, unopenable); the audit log stays. A file
with other names is refused (409 `has_other_names`) unless `allNames=1`.

### Drafts

Autosave goes to drafts, not versions:

- `PUT …/drafts?id=` — the body; `x-drive-base-version: <version id>`.
- `GET …/drafts?id=` — the actor's draft and its base version.
- `POST …/drafts/publish?id=` — becomes the file's content if the file is
  still at the base version; else 412 with both versions (`force=1` to
  overwrite).
- `DELETE …/drafts?id=` — discard.

Drafts live in the side store, one object per user and file; publishing or
discarding a draft removes its old versions too, so autosaves don't pile up.

### Permissions

- `POST …/identities`, `GET …/identities/{job}` — the identities scan.
- `GET …/permissions?id=` — one item's ACL, or owner/group/mode.

### Change events

`GET …/events` — Server-Sent Events: `changed` with the directory (path and
id) that changed, where known, or a bare `changed`. Fed by the API's own
writes and by icbfs change notifications for writes from mounts. Host may
merge these into its own stream.

## The side store

An icbfs filesystem set aside for host (**proposed**: one per registered
bucket, or one per deployment), behind a small interface of its own: keyed
objects with metadata, versioned. Holds drafts now, and review information —
comments, feedback, the base version a review started from, the provenance of
versions an agent writes — for any drive, whatever its backend. Its own
history and as-of views come with it.

## Daemon side commands: `icbfs-cli`

A separate binary for what isn't a filesystem operation. Two ways to run:

- **Through the daemon** (on host machines): `icbfs-cli` talks to
  `icbfs-logd`'s socket like a mount; the daemon relays to host with the
  actor (the agent on the machine, from the socket's peer); host checks
  the permission and does it through the library. No store credentials on the
  machine.
- **With credentials directly**, for use without host.

Commands: `names create|delete|list` now; `fs create|list|describe|resize|
archive|prune` (direct credentials, or through host's storage
permissions); more later (trash, restore). (The VM test scripts currently
build the FUSE mount command as `icbfs-cli`; they'll be renamed.)

## Errors

JSON `{"error", "code"}`:

| condition | status | code |
|---|---|---|
| no such drive, item, version or name; or the actor may not see it | 404 | `not_found` |
| allowed to see, not to do this | 403 | `forbidden` |
| no profile on this drive | 403 | `no_access` |
| the backend can't do this | 501 | `not_supported` |
| exists | 409 | `exists` |
| directory not empty | 409 | `not_empty` |
| not a directory / is a directory | 409 | `not_dir` / `is_dir` |
| other names exist (purge) | 409 | `has_other_names` |
| version stale (`If-Match`, a draft's base) | 412 | `precondition_failed` |
| bad name or path | 400 | `invalid_name` |
| past the max file size | 413 | `too_large` |
| write to an as-of view or an archived filesystem | 409 | `read_only` |
| write contention exhausted retries | 503 | `busy` |

## Consistency

Writes are compare-and-swap, as from mounts. A mount's unflushed writes are
visible only on that machine until the file is closed there. A move between
directories is a remove then an insert, so a listing in between can show the
item in neither or both.

## What it needs from icbfs

- Mounts no longer create filesystems; an explicit create (library, API,
  `icbfs-cli`).
- A trash index written by every delete (mounts included), and restore.
- Deleting specific versions (for purge, prune, and draft cleanup).
- Named versions: a per-filesystem names object; as-of by name.
- A Windows access check in Go, and ACL inheritance.
- A public package (`pkg/icbfs`) for the library.
- `icbfs-cli`, and a new daemon request type for its side commands.

## Not for now

- Signed links (browser fetches bytes directly); host proxies everything.
- Document conversion, thumbnails, previews; search; real-time co-editing.
- Several API instances; quotas; editing ACLs or modes through the API
  (Windows mounts do that); streams; locks.
- Logging from the Windows mount (there's no daemon on Windows).
- `.versions/` in mounts.
- Testing a robocopy migration (`/COPYALL`, `/SECFIX`) onto a Windows mount —
  before the first real migration.

## Open questions

1. **Side store placement**: one side-store filesystem per registered bucket,
   or one per deployment?
2. **Permission names**: the `drive.*` and `storage.*` set above is a
   proposal for host's registry; does it match how host names and
   groups permissions?

## As built

- `pkg/drive` (the interface, capabilities, `Copy`, `Diff`, `Drafts`),
  `pkg/drive/icbfsdrive` (the backend, and the side store on an icbfs
  filesystem of its own), `pkg/drive/driveapi` (`Mount(router, Config)`:
  the Fiber v3 handlers; the generated models and client; `Standalone`),
  `pkg/icbfs` (buckets and filesystem administration), `cmd/drive-api`.
  The Windows access check is `internal/acl`.
- `Config` is what host supplies: `Drive` (its registry), `Authorize`
  and `AuthorizeStorage` (its role check, answering the actor and the
  actor's profile), `Bucket`, `Drafts`, and `Audit`, told of every
  operation (actor, profile, route, item, status) for the logging chain.
- `version=` is a named version when the drive has a name by that value,
  else a version id.
- A trashed item has no directory left to ask, so the drive's root is
  checked instead: read there to see its versions, write to purge it.
- Events carry the API's own changes. `icbfsdrive.Drive.Notify` takes
  change signals (icbfs change notifications, for mounts' writes) for
  host to wire to its notification source; nothing feeds it yet.
- A copy across drives reads as the actor's profile on the source and
  writes as its profile on the target, checked for `drive.write` there.
- A file the API creates starts with one version, its content (the empty
  version that creating it leaves is removed).
- `icbfs-cli` runs every command through `internal/sidecmd`: itself, with
  store credentials, or on the server through the daemon (`-daemon`), as
  the account the daemon sees on its socket.
