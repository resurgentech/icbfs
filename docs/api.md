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
| **profile** | who the actor is on a drive's storage: a Windows SID, its groups' SIDs and its domain's, or a POSIX uid and gids. `ByMode` is for an actor with no uid on a primary-POSIX drive (a Windows user's, say): items are checked by their mode bits alone, as a Windows mount does. `System` skips the item checks, for the library's own housekeeping, never for a user. |
| **version** | one state of a file's content: shown as a time, carried with an ID. |
| **named version** | a name for a time, on a drive or a whole filesystem: a snapshot that copies nothing. |
| **side store** | an icbfs filesystem where the library keeps its own data about documents (drafts), whatever drive the document is on. |

## Packages

```
pkg/drive                 the Drive interface, capabilities, Copy, Diff, Drafts, profiles
pkg/drive/icbfsdrive      the icbfs backend, and the side store on an icbfs filesystem
pkg/drive/driveapi        Fiber v3 handlers (Mount), generated models and client, Standalone
pkg/icbfs                 opening a bucket; creating and administering filesystems
pkg/history               a filesystem's past for a program browsing it (below)
pkg/icbfstest             a throwaway MinIO for tests of programs built on icbfs
internal/acl              the Windows access check
cmd/drive-api             the standalone server
```

The host calls `driveapi.Mount(router, cfg)`. `Config` is what it
supplies:

| field | what it is |
|---|---|
| `Drive` | finds a drive by its registry ID, with its name and kind (a `DriveEntry`); `ErrNotFound` if there's none, or the actor may not know of it |
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
    CreateName(ctx, name string, at time.Time, filesystemWide bool) (Name, error)
    DeleteName(ctx, name string) error
    Trash(ctx, opts TrashOptions) (TrashPage, error)
    Restore(ctx, entry string, opts RestoreOptions) (Item, error)
    Purge(ctx, item ItemRef, allNames bool) error
    Changes(ctx) (<-chan Change, error)
    Identities(ctx, progress func(scanned int)) ([]Identity, error)
    Permissions(ctx, item ItemRef) (Permissions, error)

    SetOrigin(ctx, item ItemRef, o Origin) error
}
```

- An `ItemRef` is a path or an ID.
- An `At` is now, a time, a named version, or a version ID.
- A `Condition` is "only if still this version" or "only if absent".
- Who is acting, and as which profile, comes in the context
  (`drive.WithActor`, `drive.WithProfile`).

**Copy** (within a drive, or between drives) is the library's own:
`drive.Copy(ctx, src, srcID, from, at, dst, to, cond) (Item, error)` reads
from the source, writes to the target, and records where the copy came
from (drive, ID, version) on targets that can keep it. It reads as the
actor's profile on the source and writes as the actor's profile on the
target. **Diff** is the library's own too: `drive.Diff(ctx, d, item, from,
to) (string, error)`, a unified diff of two versions of a text file.

`WriteOptions.ModTime`, `WriteOptions.Created` and `WriteOptions.Origin`
set a file's modified and creation times at its source and where it was
copied from in the same write, instead of a `SetTimes` and a `SetOrigin`
after it. The creation time stays through later writes; the modified time
is the write's again at the next write without one.

`WriteOptions.App`, `WriteOptions.Mode` and `WriteOptions.WindowsAttrs`
go in that same write too: app data the caller keeps on the item (keys
`<tool>.<name>`, `drive.CheckAppKey`; never `drive.*`, which are the
drive's own; a nil value removes a key), its permission bits, and its
Windows ReadOnly/Hidden/System/Archive bits. A source item locked at its
source is written with mode 0444 (0555 for a folder) and ReadOnly, and
every mount shows it read-only; a mount's own chmod still works. An item
reports them back as `Item.App`, `Item.Mode` and `Item.WindowsAttrs`, to
callers of the library only (not over HTTP).

**Writing without spooling.** `WriteOptions.Size` is the body's length
when the caller knows it: a download's reported length, a listing's size,
a local file's, an HTTP upload's `Content-Length` (the HTTP handler and
`drive.Copy` fill it in). Known, the body goes to the store as it
arrives: one put when it fits one part, else parts straight from the
body with one part in memory at a time, and nothing on disk. A body that
turns out shorter or longer than its size fails the write, which then
writes nothing; another write landing first fails a streamed write
rather than retrying it, as the body can't be sent twice. Checksums the
caller gives are checked once the body has gone by, and a mismatch
erases the version written. Without a size (0), the body is read first:
into memory up to 64 MiB, past that into a temporary file. A drive that
is a `FileWriter` also takes content the caller holds open:
`WriteFrom(ctx, path, content io.ReaderAt, size, cond, opts)` reads parts
from it at offsets as they go up, reads one again if it is retried, and
copies nothing; the caller's checksums are checked before anything is
written. The item that results is the same whichever way the bytes came.

A drive that's a `drive.InfoSetter` (icbfs's is) records all of that on
an existing item in one metadata write and no new version:
`SetInfo(ctx, item, ItemInfo{Modified, Created, App, Mode, WindowsAttrs,
Origin}) (Item, error)`, every field optional. `SetTimes` (a
`drive.TimeSetter`) is `SetInfo` with only the times. It's how a folder
gets its record (`Mkdir` takes none), and how an item copied before its
source's record was passed gets it.

App data is bookkeeping, not a change to the item: writing it moves
neither the modified time nor the changed time (ctime), as an access-time
update or checksums don't. A time or a mode in the same write does move
ctime.

**App objects.** A drive that's a `drive.AppObjects` (icbfs's is;
`Capabilities.AppObjects`) keeps a tool's objects beside the drive, not in
the tree and not about any one item: a mirror's table of the ids its app
keys use, say. `AppObject(ctx, name)`, `SetAppObject(ctx, name, data)`,
`DeleteAppObject(ctx, name)` and `ListAppObjects(ctx, prefix)`; names are
app keys (`<tool>.<name>`), objects are small (4 MiB at most), and
versioned, so a view of the past reads the one current then. Reading one
needs the right to read the drive's root, writing one the right to write
it. No mount shows them, and nothing over HTTP.

**Source time.** `drive.WithSourceTime(ctx, t)` says the changes made
under ctx happened at t at their source: a mirror copying a source's
history. `Write`, `Mkdir`, `Move`, `Delete`, `SetTimes`, `SetOrigin` and
`Restore` honour it on icbfs. What they write is dated t, so `Versions`,
the trash and views of the past show the source's history; a new item is
created at t, and a file's modified time is t unless `ModTime` says
otherwise.

- Changes come in any order, though oldest first costs least. One dated
  behind its item's current state goes into the past at its date and the
  present stays as it was: a file's current version is put back on top
  (its older versions list where they belong), and a folder's copies from
  that date on gain the change. A time in the future is refused
  (`ErrInvalid`).
- A file's name is in its folder from the time it was written there: a
  version dated earlier than that is listed by `Versions` and read by id,
  not found by path in a view of then. A mirror writes a file at its
  `Created` time, so its name is there from the start.
- The filesystem has to start before the source's history does:
  `Settings.Created` when creating it.
- Dating a change is part of it: a change whose dates can't be recorded
  (the store failing after it took the change's writes) is undone and
  returns the error, so applying it again is safe. From its first change
  under a source time on, a filesystem dates every version of its items,
  ordinary writes too.
- The HTTP API doesn't take a source time: it's for a mirror using the
  library.

**Corrections.** A past the source got wrong (a date, a version that
shouldn't be there) is put right by the caller, with a drive that's a
`drive.FolderHistorian` and a `drive.FileHistorian` (icbfs's is both):

```go
h, err := fh.FolderHistory(ctx, drive.ByPath("/Project"))  // every state, oldest first
err = fh.ReplaceFolderHistory(ctx, drive.ByPath("/Project"), states, drive.Condition{IfMatch: h.Version})

f, err := fh.FileHistory(ctx, drive.ByPath("/Project/brief.txt"))  // every version, newest first by date
err = fh.ReplaceFileHistory(ctx, drive.ByPath("/Project/brief.txt"), []drive.FileState{
    {At: t1, Version: f.Versions[1].ID},                 // kept: copied on the server, nothing downloaded
    {At: t2, Content: body, Info: drive.ItemInfo{…}},    // new
}, drive.Condition{IfMatch: f.Version})
```

- `FolderHistory` is the folder's state at each time it changed (`At`,
  and its entries' names, ids and types), and `Version`, its state now.
  `ReplaceFolderHistory` erases the folder's history (its blocks' and its
  attributes') and writes the states in its place, oldest first; the last
  is the folder now, which may differ from before. Each state's attributes
  are the folder's now with the state's time as its modified time (a
  folder's modified time is its entries'). Items aren't touched: an entry
  names an existing item by id.
- `FileHistory` is the file's versions and its current version's id.
  `ReplaceFileHistory` erases the file's history (its content's and its
  attributes') and writes the states in its place; a state keeps an
  existing version or gives new bytes, with its record then (`Info`; a
  zero `Modified` is `At`). Streams and the file's name are left as they
  are.
- Both fail with a `PreconditionError` if the item has changed since
  `Version`; the caller makes no other change to it while they run.

### Mirroring a source: `drive.Mirror`

A sync daemon keeps a drive matching a source of documents (another
document store, say). `drive.NewMirror(d, sourceDrive)` does the
bookkeeping, keyed by each item's origin (the source drive and the item's
id there), so the daemon only says what each source item is now:

```go
m := drive.NewMirror(d, "webdav:docs")
applied, err := m.Apply(ctx, drive.SourceItem{
    Origin: drive.Origin{ID: id, Version: "4.0", Path: srcPath},
    Path:   "/Reports/brief.docx", Type: drive.TypeFile, ModTime: modified, Created: created,
    Open:   func(ctx context.Context) (io.ReadCloser, error) { return download(ctx, id) },
})
removed, err := m.Remove(drive.WithPass(ctx, passID), id) // to the trash
item, trashed, found, err := m.Known(ctx, id)              // what the drive holds
n, err := m.Undo(ctx, passID)                              // a bad pass, undone
```

| the drive holds for the source item | `Apply` |
|---|---|
| nothing | writes it (`created`), its times and origin in the write |
| it, same version | nothing (`unchanged`); the content isn't downloaded |
| it, same version, another record (times, app keys, mode, attributes) | sets it (`retimed`): one write, no download, no new version |
| it, another version | a new version (`updated`) |
| it, at another path | moves it (`moved`), then writes only if the version differs |
| it, in the trash | restores it, history and all (`restored`), then as above |

`m.InSourceTime()` makes the mirror write the source's history in the
source's time (`WithSourceTime`): an item is created at its `Created` time
(else its `ModTime`), a new version written at its `ModTime`, a move made
at its `Changed` time, and `Remove` deletes at the context's source time,
which the daemon sets to when the source deleted it. The daemon applies a
source's changes oldest first.

`Apply` returns an `Applied`: the item, and the actions it took. A
`SourceItem` carries, besides its content, what the mirror records on the
item: `ModTime`, `Created`, `Changed`, `App`, `Mode`, `WindowsAttrs`. All
of it goes in the one metadata write of a create or a new version (on
icbfs a new file is its content and its `.metadata`, each written once), and
on the no-content path (`retimed`) in one `SetInfo`, only what differs;
an item is `unchanged` only when none of it differs, and then nothing is
written. That needs a drive that's an `InfoSetter`; on one that's only a
`TimeSetter` the times go through, and app data, mode and attributes are
`not_supported`. A directory is made with `Mkdir`, then given its record
and origin in one write.

- `Open` is called only when the bytes are needed: never for a move, a
  restore of the version held, an item already up to date, or a new
  file whose path is taken. A new file is written on the condition that
  its path is free (`IfNoneMatch`), which the drive checks before it
  reads the content; the mirror looks at the path only if it's taken.
- The trash is the undo. An item the source deletes and brings back with
  the same id (a recycle bin, a folder shared again, a listing that came
  back short) is restored, not copied again; and `Undo` restores
  everything one pass removed. A daemon needn't cap how much a pass may
  remove.
- A path taken by an item not copied from that source item is refused
  (`ErrNotMirrored`); a directory there is adopted, and so is an item at
  the path that carries the source item's origin but has no origin record.
- Two items claiming one source item (a copy made twice): the newest
  claim is the one the mirror works on.
- On icbfs, finding a source item is a lookup (`OriginFinder`: an origin
  record per item, and a pointer from each trashed item to its entry);
  any other backend is searched, a listing of the drive and its trash.
- Applying in parallel: on icbfs, items made or removed under one folder
  contend on that folder's block, since each is a compare-and-swap of it.
  Of several at once, one lands and the rest read the block again and
  retry (with a short random pause, up to 20 times), so siblings cost
  extra rounds but all land. Items under different folders don't contend
  at all. A daemon can run applies across folders as wide as it likes;
  under any one folder, a handful at once is the useful most, and the
  retries are what bounds it, not correctness.
  (`TestCreatesInParallel` shows both halves.)

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

An actor without a profile on the drive is refused with `no_access`, and
so is a profile of the other kind (a Windows profile on a primary-POSIX
drive) unless it's `ByMode`.

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

Whether an actor may write is whether it holds `drive.write`. The one
switch is `icbfsdrive.Config.ReadOnly` (`readOnly` on a standalone
drive): every change through that drive fails with `read_only`, whatever
the actor's permissions, for a filesystem something else writes (a
mirror's) and users may only read.

A program that owns a drive, a mirror daemon writing its own filesystem,
a sync engine, an administrative tool, acts as the system:
`drive.AsSystem(ctx, actor)` is a context with the `System` profile and
the actor's name for the records operations leave. Never for a user's
request, which gets the user's own profile.

A trashed item has no directory left to check against, so the drive's
root is checked instead: reading its versions needs read access there, and
purging it needs write access there.

## Versions

Each content write is a new version. Its `time` is when it was stored,
or, for one a mirror wrote in source time, when it came to be at the
source; `stored` is always when it was stored. Renames,
moves and permission changes don't make versions, so renaming a document
doesn't make an open editor see a conflict.

```json
{"id": "0000-01a2…", "versions": [
  {"time": "2026-10-12T15:04:05.214Z", "stored": "2026-10-12T15:04:05.214Z", "version": "3f5e…",
   "size": 4211, "current": true, "names": ["q3-signoff"]},
  {"time": "2026-10-12T14:51:40.002Z", "stored": "2026-10-12T14:51:40.002Z", "version": "a19d…", "size": 4102}
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

**Checksums.** Each version can have an MD5 and a SHA-256 (`checksums`,
hex), on the item, on each version, and from `Checksums` (`drive.Checksummer`
in Go, `GET …/checksums` over HTTP), which computes and keeps them for any
version that hasn't any.

- A write through the drive API computes both as its bytes go by. A mount
  writing a file in one piece gets them at its flush; a mount's part-edit
  of a big file (a multipart write) doesn't, until they're asked for, not
  to read gigabytes for a few bytes.
- `WriteOptions.Checksums` (`X-Drive-MD5`, `X-Drive-SHA256`) are what the
  caller knows them to be: a mismatch fails with `ErrChecksum` and writes
  nothing.
- They're kept in the item's `.metadata`, by the version's ETag. Recording
  them isn't a change to the item: its ctime, its versions and its past
  are as they were.

**As of a time.** `asOf=<time>` on `list`, `stat` and `content` shows the
drive read-only as it was then.

- A time before the filesystem existed answers 404.
- A time in the future, by the store's clock, answers 400. A time up to two
  seconds ahead is waited for.

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

**Browsing the past: `pkg/history`.** A program that moves through a
filesystem's history (a slider over time, say) opens it with
`history.Open(ctx, history.Config{Store, Filesystem, Root, CacheDir, TTL, TokenCommand})`
(a store configuration, not an open store; `CacheDir` and `TTL` default to
the user's cache directory and 30 days; `TokenCommand`, for an Azure
store, is a program giving tokens in place of the key, as the mount's
`-azure-token-command`, [mount.md](mount.md)) and asks, as the profile in
each call's context:

- `List(ctx, dir, at)`: a directory as it was at a moment (now, for the
  zero time);
- `Versions(ctx, item)`: a file's versions, by id even once it's in the
  trash;
- `Search(ctx, query, limit)`: names in the tree now and in the trash. Each
  `Hit` has the item's id, path, name, type, when it last changed (or was
  deleted), whether it's `Deleted` (and its `TrashID`), and `See`, a moment
  to see it at: just before a deleted one went, the zero time for one
  that's there now. `Results.Truncated` says the limit cut the hits short;
- `Span(ctx)`: from the filesystem's creation to now, by the store's
  clock.

It reads through the history cache as-of mounts share (by default in the
user's cache directory, kept 30 days), so a directory listed at some
moments is listed at them again with no store requests, in later runs
too. A change to the past (a purge, a mirror writing what happened
earlier) is noticed within 30 seconds (`CheckPast`), plus up to a second
for one made through another drive, which signals at most once a second;
`PastChanges` counts
them, for a caller keeping listings of its own. `IsNotFound` tells a
path or item that wasn't there at that moment, and `Close` closes the
cache.

## Opening drives and filesystems in Go

```go
store, err := pkgicbfs.Open(pkgicbfs.StoreConfig{Kind: "s3", S3: pkgicbfs.S3{...}})
d, err := icbfsdrive.New(ctx, icbfsdrive.Config{
    Store: store, Filesystem: "docs", Root: "/", ID: "docs", CacheDir: dir})
defer d.Close()
```

- `pkg/icbfs` opens a store (`Open`, or `OpenS3` and `OpenAzure`; an
  Azure container with a token credential the host holds rather than the
  account's key, `OpenAzureWithCredential`) and administers its
  filesystems: `CreateFilesystem`, `ListFilesystems`,
  `DescribeFilesystem` (with the bytes used), `Resize`, `Archive` and
  `Prune` (archived filesystems only). `DefaultSettings(name)` is a
  filesystem with `icbfs-cli fs create`'s defaults (primary-POSIX, 100 GiB
  declared, root mode 0755), to change what's needed and create.
  `WatchMinIO` is below, under change events.
- `pkg/clicmd` and `pkg/winfspcmd` are the `icbfs-cli` and
  `icbfs-winfsp` commands as packages, each a `Main(args, newStore)`,
  and `pkg/storeflags` the store flags they take: `storeflags.Default`
  is this module's, and a host with a store of its own, a credential it
  holds, say, implements `storeflags.Store` and builds the same commands
  around it. `pkg/tokencmd` is a `TokenCredential` backed by a program
  the mount runs, `-azure-token-command`'s (the protocol is in
  [mount.md](mount.md)).
- `icbfsdrive.Config` names the filesystem, the drive's root directory in
  it, and the drive's registry `ID` (recorded in the origin of copies made
  from it). `History` is a history cache to share with other views; without
  one the drive opens a temporary one in `CacheDir`, which `Close` removes.
  `ReadOnly` refuses every change (see Permissions). `TrustFor` is how
  long the drive uses a folder it has read without asking the store again:
  for a daemon that is its filesystem's only writer (a mirror's), a walk
  down a path it walked a moment ago then costs nothing per folder. At 0,
  the default, each folder on the way is checked, two conditional reads
  with no bodies when it hasn't changed. The drive's own changes are seen
  at once either way; changes made elsewhere (a mount, another drive),
  after at most `TrustFor`. `Filesystem` returns the drive's icbfs
  filesystem.
- `icbfsdrive.OpenSideStore(ctx, store, name)` opens a side store,
  creating its filesystem the first time.
- `pkg/icbfstest` is for tests of programs built on icbfs: `Store(t)` is
  a fresh bucket on a throwaway MinIO in Docker, and `Start(t)` also
  returns its store configuration, for code that opens a store itself.

## Endpoints

Drive paths are under `/api/v1/drives/{drive}`, where `{drive}` is the
host's drive ID.

- Items are named by `path=` (from the drive's root) or `id=`.
- Metadata is JSON, content is raw bytes, and times are RFC 3339.
- Paths are looked up case-insensitively on primary-Windows filesystems,
  and exactly on primary-POSIX ones.

### The drive

- `GET /api/v1/drives/{drive}` returns the drive's id, name, kind and
  capabilities, and the actor and its profile.

### Reading

- `GET …/list?path=` lists one directory.
  - Options: `depth`, `limit`, `cursor`, `sort`, and `asOf` or `version`.
  - Each entry has: name, path, ID, type (`file`, `dir`, `symlink`), size,
    contentType, the current version (time and ID), modified, changed,
    created, `checksums` (when computed), `hasDraft` and `draftsBy`.
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
- `GET …/checksums?path=` (or `id=`, and `version=`) returns a version's
  `{"md5", "sha256"}`, computing and keeping them if they weren't.
- `GET …/diff?id=&from=&to=` returns a unified diff of two versions of a
  text file.

### Writing

- `PUT …/content?path=` makes the body the file's content.
  - It takes `If-Match`, `If-None-Match`, or neither for an unconditional
    write; and `parents=1` to create missing directories.
  - `X-Drive-MD5` and `X-Drive-SHA256` (hex) are the content's checksums as
    the caller knows them: if what arrived doesn't match, nothing is
    written (400, `checksum_mismatch`).
  - The body can be up to the filesystem's max file size; anything larger
    answers 413.
- `POST …/mkdir` takes `{"path", "parents"}`.
- `POST …/move` takes `{"from"` or `"fromId", "to", "replace"}`. The
  item's ID doesn't change.
- `POST …/copy` takes `{"from"` or `"fromId", "version"` or `"asOf", "to",
  "toDrive"}`. It copies within the drive or to another drive, optionally
  from an old or named version, or as the source was at a time.
- `DELETE …/content?path=` moves the item to the trash. `recursive=1`
  deletes a non-empty directory as one trash batch. `If-Match` is honored.
- `POST …/restore-version` takes `{"id", "version"}`. That version's content
  becomes current again, as a new version.

### Trash

A delete from the API or from any mount records a trash entry when a file's
last name goes or a directory is removed. An entry holds the ID, name,
original path, when, by whom, the last version and size, and the item's
origin (where it had been copied from: `origin`, as on an item) if it had
one. Nothing expires: an entry stays until the item is purged.

- `GET …/trash?path=` lists deleted items, newest first, optionally only
  those from under a directory. A trashed item's versions and content can
  be read before restoring it (`versions?id=`, `content?id=&version=`).
- `originDrive=` keeps only entries whose item had been copied from that
  drive, and `originId=` only from that item there. A mirror finds this
  way what it trashed when its source brings an item back with the same
  id, restores it, then writes a new version only if the source's version
  differs.
- `POST …/trash/{entry}/restore` takes `{"to", "parents", "replace"}`.
  - Without `to`, the item goes back to its original path.
  - A taken name answers 409.
  - Restoring a directory restores its whole batch.
  - A restored item keeps its origin and every version.
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
- `GET …/permissions?id=` returns one item's security descriptor (SDDL,
  and the ancestor it's inherited from when it has none of its own), or its
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

Errors are JSON, `{"error", "code"}`; a 412 also has the current version
in `currentVersion`, as well as in the `x-drive-current-version` header.

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
| a malformed request | 400 | `invalid` |
| the content doesn't match `X-Drive-MD5` or `X-Drive-SHA256` | 400 | `checksum_mismatch` |
| past the max file size | 413 | `too_large` |
| write to an as-of view, a read-only drive or an archived filesystem; prune of one that isn't archived | 409 | `read_only` |
| write contention exhausted the retries | 503 | `busy` |
| anything else | 500 | `internal` |

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
  "buckets": {"main": {"kind": "s3", "s3": {"endpoint": "http://127.0.0.1:9000", "bucket": "icbfs",
                                           "accessKey": "…", "secretKey": "…"}},
              "archive": {"kind": "azure", "azure": {"account": "…", "container": "icbfs", "key": "…"}}},
  "drives": {"docs": {"name": "Documents", "bucket": "main", "filesystem": "docs", "root": "/"}},
  "sideStore": {"bucket": "main", "filesystem": "drafts"},
  "profiles": {"alice": {"windows": {"user": "S-1-5-21-…-1001"}}},
  "permissions": {"alice": ["drive.*", "bucket.read"]}
}
```

- Each bucket is a store configuration, S3 or Azure, as in
  [mount.md](mount.md).
- Requests carry `Authorization: Bearer <token>` and `X-Drive-Actor: <actor>`.
- An actor not listed in `permissions` has every permission.
- `driveProfiles` gives a drive its own profiles, and `bucketSideStores`
  gives a bucket its own side store. A drive with `"readOnly": true`
  refuses every change.
- `notify` feeds a bucket's drives' event streams with changes made
  elsewhere (`"minio"`).
- `cacheDir` holds the drives' temporary files, as-of reads' history
  caches among them (the system's temporary directory if it's empty).
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
