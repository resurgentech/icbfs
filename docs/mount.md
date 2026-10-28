# Mounting

A mount attaches to an existing filesystem. Create one first with
`icbfs-cli fs create` (see [cli.md](cli.md)), in a bucket with versioning
turned on.

Both mount commands take the store's connection the way `icbfs-cli` does:
a flag, or else its environment variable. There are no default keys; a
mount without them fails.

| flag | environment variable | default |
|---|---|---|
| `-store` | `ICBFS_STORE` | `s3` (AWS S3, MinIO); or `azure` (Azure Blob Storage) |

An S3-API store (`-store s3`), a bucket with versioning on:

| flag | environment variable | default |
|---|---|---|
| `-endpoint` | `ICBFS_ENDPOINT` | `http://127.0.0.1:9000` |
| `-bucket` | `ICBFS_BUCKET` | `icbfs` |
| `-region` | `ICBFS_REGION` | `us-east-1` (MinIO ignores it) |
| `-access-key` | `ICBFS_ACCESS_KEY` | none |
| `-secret-key` | `ICBFS_SECRET_KEY` | none |

An Azure Blob Storage container (`-store azure`), in an account with blob
versioning on:

| flag | environment variable | default |
|---|---|---|
| `-azure-account` | `ICBFS_AZURE_ACCOUNT` | none |
| `-azure-container` | `ICBFS_AZURE_CONTAINER` | `icbfs` |
| `-azure-key` | `ICBFS_AZURE_KEY` | none: the account's key |
| `-azure-endpoint` | `ICBFS_AZURE_ENDPOINT` | `https://ACCOUNT.blob.core.windows.net/` |
| `-azure-token-command` | `ICBFS_AZURE_TOKEN_COMMAND` | none: a program that gets a token for the account, instead of the key (below) |

Prefer the variables for the keys: a flag's value is visible to other
users in the process list.

### A token command instead of the key

An Azure store can be opened with tokens for a signed-in identity
rather than the account's key: `-azure-token-command` names a program,
and the mount asks it for a token. How the program gets one (a sign-in,
the machine's account broker, a managed identity, the `az` CLI) is its
own business, and so is its configuration: it inherits the mount's
environment, and takes no arguments. The identity's role on the
container (Storage Blob Data Reader, or Contributor) decides what the
mount may do, and icbfs's own ACLs still decide per file. A key and a
token command together is an error; with a token command, only the
account and the container are needed.

The mount starts the program once, on open, and keeps it running: it
sends one JSON line on the program's standard input per ask and reads
one line back on its standard output, keeps the token until five
minutes before it expires, and asks again then. The program's standard
error is the mount's. The first ask is interactive, the one moment a
sign-in may be shown; the rest are silent. A program that exits is
started once more; one that answers with an error, or doesn't start,
fails the open. The program exits when its standard input closes.

```
> {"version": 1, "scopes": ["https://storage.azure.com/.default"], "interactive": true}
< {"version": 1, "token": "...", "expiresOn": "2026-10-27T12:34:56Z", "account": "who@tenant"}
< {"version": 1, "error": "what went wrong", "code": "interaction_required"}
```

- `version` is the protocol's, 1, in every request and every reply. A
  reply with another version is an error that names both; a program
  that doesn't speak the request's version answers
  `unsupported_version`.
- `scopes` is what the token is for: Azure Storage's.
- `interactive` false means answer silently, or reply
  `interaction_required`; true lets the program show a sign-in.
- `expiresOn` is RFC 3339. `account` is whose identity the token is; the
  mount only logs it. The codes are `interaction_required`,
  `unsupported_version` and `failed`.

The history cache of a store opened this way is its own, not the key's
(the cache is one per store and identity). `pkg/tokencmd` is the
mount's side, usable from Go by anything that takes a
`TokenCredential`, and its tests drive a fake program (the test binary
itself, run with `ICBFS_FAKE_TOKEN_COMMAND` as a script of replies),
the example of one.

| flag | default | meaning |
|---|---|---|
| `-fs` | `default` | the filesystem to mount |
| `-as-of` | | mount read-only as the filesystem was at a past time or named version (below) |
| `-cache-dir` | a new temp directory | where open files and the read cache are kept |
| `-cache-size` | 512 MiB | the read cache's limit |

The cache directory needs room for every open file's scratch copy, in
addition to the read cache.

## Linux (FUSE)

Requirements: FUSE 3 (`fuse3`), and `user_allow_other` in `/etc/fuse.conf`
if you use `-allow-other`.

**The metadata cache** (`-meta-cache`, as NFS's attribute cache). The
kernel trusts names, including "no such name", and attributes for this
long before asking icbfs again, which would ask the store: without it,
every path walk asks for every directory in the path, each a round trip
to the store. This mount's own changes show at once; another machine's
rename, create, delete or `chmod` can take up to the timeout to show here.
What doesn't wait:

- An open checks the store: the file's newest content, and that the name
  it was opened by still leads to it (if not, the open follows the name as
  it is now).
- Locks: taking one brings an open file up to what the last holder wrote,
  and releasing one stores what was written under it.
- An `O_EXCL` create is always decided by the store.

`-meta-cache 0` trusts nothing: slower, with every change elsewhere
visible at once.

```
export ICBFS_ENDPOINT=http://minio:9000 ICBFS_ACCESS_KEY=… ICBFS_SECRET_KEY=…
icbfs mount -fs docs /mnt/docs
```

Unmount with `fusermount3 -u /mnt/docs`, or press Ctrl+C in the
command.

### With mount(8) and fstab

The `icbfs` binary is also a mount helper. Link it as
`/sbin/mount.fuse.icbfs`, and `mount -t fuse.icbfs NAME DIR` mounts the
filesystem NAME, the same as `icbfs mount -fs NAME DIR`, in the
background; `umount DIR` unmounts it.

```
sudo ln -s /usr/local/bin/icbfs /sbin/mount.fuse.icbfs
sudo mount -t fuse.icbfs docs /mnt/docs -o allow_other
# /etc/fstab
docs  /mnt/docs  fuse.icbfs  allow_other,_netdev  0  0
```

- The store connection comes from the `ICBFS_*` variables, or failing
  those from `/etc/icbfs.env` (`ICBFS_ENDPOINT=...` lines; keep it mode
  600).
- Options: `ro`, `allow_other`, `locking`, `nolocking`, `notify`, `debug`,
  `relatime`, `strictatime`, `noatime`, `nodiratime`, `dev`, `suid`,
  `noexec`, `cache_dir=DIR`, `cache_size=BYTES`, `meta_cache=DURATION` and
  `as_of=TIME`. Generic options that mean nothing here (`rw`, `defaults`,
  `auto`, `noauto`, `user`, `nouser`, `nodev`, `nosuid`, `exec`, `atime`,
  `norelatime`, `diratime`, `async`, `_netdev`, `nofail`,
  `default_permissions`) are accepted and ignored; anything else is
  refused.
- `mount -o remount,ro` (or `rw`, `noexec`, `noatime` and the other
  generic flags) changes a mount's flags in the kernel; icbfs keeps
  running. Its own access-time recording follows the options it was
  mounted with.
- The mount shows its filesystem's name as its source and `fuse.icbfs` as
  its type (`findmnt`, `/proc/mounts`). The mount's log goes to
  `/tmp/icbfs-mount-*.log`.

Linux-only flags:

| flag | meaning |
|---|---|
| `-allow-other` | let users other than the one mounting use the mount. Without it, the kernel refuses every other user, whatever the file modes say. |
| `-locking=false` | refuse `flock`/`fcntl` (ENOSYS). By default they're locks shared with every other mount (see [architecture.md](architecture.md#locking)), which costs a read of the file's `.lock` object per flush. |
| `-meta-cache D` | how long the kernel trusts names and attributes (above): `5s` by default, a day for `-as-of`, `0` for not at all |
| `-notify` | MinIO change notifications: a lock wait retries as soon as the lock changes |
| `-read-only` | mount read-only |
| `-dev`, `-suid` | honour device nodes, and setuid and setgid bits on executables (as root). Off by default, as on every FUSE mount: anyone who can write the filesystem, from any machine, could plant a device node or a setuid binary. |
| `-noexec` | don't allow executing files from the mount |
| `-atime MODE` | when reads record access times: `relatime` (the default, as Linux's: when the recorded one is no newer than the last change, or a day old), `strict` (every read) or `noatime` (never) |
| `-nodiratime` | record no access times for directories |
| `-debug` | log every FUSE request |

The mount uses `default_permissions`: the kernel checks mode bits, and on
a primary-POSIX filesystem POSIX ACLs too (`setfacl`, `getfacl`), against
the caller. Extended attributes work in the `user.`, `trusted.` and
`security.` namespaces (`setfattr`, `getfattr`); see
[architecture.md](architecture.md#metadata).

## Windows (WinFsp)

### Install WinFsp first

`icbfs-winfsp` needs [WinFsp](https://github.com/winfsp/winfsp) installed
on the machine. WinFsp is a kernel driver plus a user-mode DLL. It is not
bundled. Without it, a mount fails with a DLL-not-found error.

Install it as an administrator, in either of two ways:

- run `winfsp-<version>.msi` from [WinFsp's latest
  release](https://github.com/winfsp/winfsp/releases/latest), silently
  with `msiexec /i winfsp.msi /qn /norestart` if you like; or
- `choco install winfsp`.

The installer does not add `C:\Program Files (x86)\WinFsp\bin` to `Path`.
`icbfs-winfsp` finds the DLL through the registry without it, but WinFsp's
own tools (`winfsp-tests`, `launchctl`) need it. If you add it, restart any
service that should see the change, such as `sshd`.

WinFsp supports Windows 7 through 11 on x86, x64 and ARM64. icbfs is
tested on Windows Server 2025 and Windows 11, x64.

### Mount

```
set ICBFS_ENDPOINT=http://minio:9000
set ICBFS_ACCESS_KEY=…
set ICBFS_SECRET_KEY=…
icbfs-winfsp -fs docs X:
```

The mount point is a drive letter (`X:`). Stop the command (Ctrl+C) to
unmount.

| flag | default | meaning |
|---|---|---|
| `-case-insensitive` | `true` | look names up case-insensitively, as NTFS does. Names are always stored as given. |
| `-atime MODE` | `hourly` | when reads and folder listings record access times: `hourly` (when the recorded one is over an hour old, as NTFS does when it keeps access times at all), `relatime`, `strict` (every read and listing: a store write each) or `noatime` (never, as most Windows machines since 1803, which turn access times off on volumes over 128 GB). `-nodiratime` leaves folders' alone. |
| `-meta-cache D` | `5s` (for `-as-of`, file and folder details without limit, names a day) | how long names and file and folder details are trusted, as on Linux (above): another machine's renames, creates, deletes and attribute changes can take this long to show in a listing or a file's properties; an open still reads its name and content from the store. `0` trusts nothing. |

WinFsp keeps no names of its own: every open hands the driver a whole
path. So the driver keeps a small name cache (at most 32 MB; at the
default timeout, about what was touched in the last second), fed by
directory listings, which also stat their entries 16 at a time. Opening
what a listing just showed, or probing for a name it didn't (Explorer's
`desktop.ini`), then needs no request.

Boolean flags take `-flag=false`. A bare `-flag false` reads `false` as the
mount point.

A drive letter mounted from one logon session is visible only in that
session. A mount started over SSH can't be seen from the desktop, or from
another SSH session.

### What works on Windows

- Security descriptors, stored with full fidelity on primary-Windows
  filesystems and enforced by Windows' own access check. On primary-POSIX
  filesystems, a DACL is derived from the mode bits.
- ReadOnly, Hidden, System and Archive attributes.
- Alternate data streams.
- Share modes and delete-on-close.
- Symlinks, shown as reparse points.
- Hard links made on Linux open as ordinary files. Windows can't create
  them: WinFsp has no hard-link callback.
- The four file times, each settable, creation time included.

### Copying onto a Windows mount

Copies keep what they would onto NTFS (tested: see
[testing.md](testing.md)), but:

- **Use robocopy for ACLs**: `robocopy SRC X:\DST /E /COPY:DATSO` (or
  `/SEC`). `xcopy /O` doesn't work: WinFsp's own security computation at
  create drops the inherited entries of the ACL a copy supplies, so the
  new item gets an empty DACL, and xcopy is then shut out of it.
  `robocopy /COPY:DAT` and `Copy-Item` keep data, attributes and times.
- **Hard links** can't be made on the mount (`mklink /H` fails). robocopy
  copies a source's hard links as separate files, as it does onto NTFS.

Match `-atime` to the machine's own volumes, so a copy's access times
come out as they would on NTFS: `fsutil behavior query
disablelastaccess` reports whether NTFS records them, and if it says
they're disabled, mount with `-atime noatime`.

Byte-range locks (`LockFileEx`) stay local to the Windows machine. WinFsp
handles them in its kernel driver, so they don't reach icbfs. Change
notifications and the logging daemon are Linux-only.

## Mounting the past

`-as-of` mounts a filesystem read-only as it was at an earlier time:

```
icbfs mount -as-of 2026-10-12 … /mnt/yesterday           # as that day ended, local time
icbfs mount -as-of 2026-10-12T15:04:05 … /mnt/then       # local time
icbfs-winfsp -as-of 2026-10-12T22:04:05Z … Y:            # RFC 3339
icbfs mount -as-of q3-signoff … /mnt/q3                  # a named version
```

- The mount fails if the filesystem didn't exist at that time, or if the
  time is in the future.
- An as-of mount reads the store's history through a cache kept between
  mounts, one per store and credential: `-history-cache-dir` (by
  default `icbfs/history` in the user's cache directory, e.g.
  `~/.cache` or `%LOCALAPPDATA%`), and `-history-ttl` for how long it
  keeps what it fetched (`720h`, 30 days, by default). After a purge, a
  mount drops what the cache kept of that filesystem when it starts. Other programs that read the
  same filesystem's history (`pkg/history`) share the cache, so a mount
  starts with what they've fetched.
- If the shared cache can't be opened, the mount logs why and uses a
  temporary one, deleted at unmount. A mount run as root refuses to make
  one in another user's directory (as `sudo -E` would, in their
  `~/.cache`), since that user couldn't open it afterwards.
- An as-of mount takes no locks, and `-notify` doesn't apply to it.

Named versions are made with `icbfs-cli names create` (see
[cli.md](cli.md)). How past views work, and what they cost, is in
[architecture.md](architecture.md#past-views-as-of-mounts-and-named-versions).
