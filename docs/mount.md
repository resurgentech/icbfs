# Mounting

A mount attaches to an existing filesystem. Create one first with
`icbfs-cli fs create` (see [cli.md](cli.md)), in a bucket with versioning
turned on.

Both mount commands take the store's connection as flags:

| flag | default | meaning |
|---|---|---|
| `-endpoint` | `http://127.0.0.1:9000` | S3-API endpoint |
| `-bucket` | `icbfs` | bucket (versioning must be on) |
| `-region` | `us-east-1` | region (MinIO ignores it) |
| `-access-key`, `-secret-key` | `minioadmin` | credentials |
| `-fs` | `default` | the filesystem to mount |
| `-as-of` | | mount read-only as the filesystem was at a past time or named version (below) |
| `-cache-dir` | a new temp directory | where open files and the read cache are kept |
| `-cache-size` | 512 MiB | the read cache's limit |

The cache directory needs room for every open file's scratch copy, in
addition to the read cache.

## Linux (FUSE)

Requirements: FUSE 3 (`fuse3`), and `user_allow_other` in `/etc/fuse.conf`
if you use `-allow-other`.

```
icbfs mount -endpoint http://minio:9000 -bucket icbfs -access-key … -secret-key … -fs docs /mnt/docs
```

Unmount with `fusermount3 -u /mnt/docs`, or press Ctrl+C in the
command.

Linux-only flags:

| flag | meaning |
|---|---|
| `-allow-other` | let users other than the one mounting use the mount. Without it, the kernel refuses every other user, whatever the file modes say. |
| `-locking` | `flock`/`fcntl` locks shared with other mounts (see [architecture.md](architecture.md#locking)) |
| `-notify` | MinIO change notifications: with `-locking`, a lock wait retries as soon as the lock changes |
| `-log-socket SOCK -drive ID` | mount a drive through the logging daemon (see [daemon.md](daemon.md)) |
| `-log-async` | with `-log-socket`: queue events instead of waiting for the server |
| `-debug` | log every FUSE request |

The mount uses `default_permissions`: the kernel checks mode bits against
the caller.

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
tested on Windows Server 2025, x64.

### Mount

```
icbfs-winfsp -endpoint http://minio:9000 -bucket icbfs -access-key … -secret-key … -fs docs X:
```

The mount point is a drive letter (`X:`). Stop the command (Ctrl+C) to
unmount.

| flag | default | meaning |
|---|---|---|
| `-case-insensitive` | `true` | look names up case-insensitively, as NTFS does. Names are always stored as given. |

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
- The four file times, each settable.

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
- An as-of mount keeps its version cache in a fresh directory under
  `-cache-dir`, and deletes it at unmount.
- `-locking` and `-notify` don't apply to an as-of mount.

Named versions are made with `icbfs-cli names create` (see
[cli.md](cli.md)). How past views work, and what they cost, is in
[architecture.md](architecture.md#past-views-as-of-mounts-and-named-versions).
