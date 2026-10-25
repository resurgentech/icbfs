# icbfs-cli

`icbfs-cli` does what isn't a filesystem operation: creating and managing
filesystems, naming versions, and the trash.

```
icbfs-cli fs create   -fs NAME [-primary-windows] [-size N] [-max-file-size N] [-dir-block-entries N] [root options]
icbfs-cli fs list     [-json]
icbfs-cli fs describe -fs NAME [-json]
icbfs-cli fs check    -fs NAME [-json]
icbfs-cli fs resize   -fs NAME -size N
icbfs-cli fs archive  -fs NAME
icbfs-cli fs prune    -fs NAME -yes
icbfs-cli names create DRIVE -name NAME [-at TIME] [-filesystem-wide]
icbfs-cli names list   DRIVE [-json]
icbfs-cli names delete DRIVE -name NAME
icbfs-cli trash list    DRIVE [-path PATH] [-limit N] [-json]
icbfs-cli trash restore DRIVE -id ENTRY [-to PATH [-parents]] [-replace]
```

`icbfs-cli <group> <command> -h` lists a command's flags. `-json` gives
JSON output where it's shown.

## Connecting

`icbfs-cli` connects in one of two ways.

**Directly**, with the store's connection, the same flags the mounts take
(see [mount.md](mount.md)):

| flag | environment variable | default |
|---|---|---|
| `-store` | `ICBFS_STORE` | `s3`; or `azure` |
| `-endpoint` | `ICBFS_ENDPOINT` | `http://127.0.0.1:9000` |
| `-bucket` | `ICBFS_BUCKET` | `icbfs` |
| `-region` | `ICBFS_REGION` | `us-east-1` |
| `-access-key` | `ICBFS_ACCESS_KEY` | |
| `-secret-key` | `ICBFS_SECRET_KEY` | |
| `-azure-account` | `ICBFS_AZURE_ACCOUNT` | |
| `-azure-container` | `ICBFS_AZURE_CONTAINER` | `icbfs` |
| `-azure-key` | `ICBFS_AZURE_KEY` | |
| `-azure-endpoint` | `ICBFS_AZURE_ENDPOINT` | `https://ACCOUNT.blob.core.windows.net/` |

A DRIVE is then `-fs NAME`, optionally with `-root DIR` for a directory
in the filesystem.

**Through the daemon**, with `-daemon SOCKET` (or `ICBFS_DAEMON`). The
command goes to the server, which records who asked (`host/uid:N`, from
the daemon), may refuse it, and runs it; no store credentials are needed
on the machine. A DRIVE is then
`-drive ID`, the server's ID for it. See [daemon.md](daemon.md).

The `fs` commands always name a filesystem with `-fs`, either way.

## Filesystems

```
icbfs-cli fs create -fs docs                                   # primary-POSIX, root owned by you
icbfs-cli fs create -fs share -primary-windows -root-owner S-1-5-21-…-1001
```

These settings are fixed at creation:

- `-primary-windows`: Windows names, case-insensitive directory order, and
  Windows ACLs as the authority. The default is primary-POSIX.
- `-max-file-size`: the largest file the filesystem holds, in bytes.
  Default 1 TiB.
- `-dir-block-entries`: how many entries a directory block holds before it
  splits, 4 to 4096. More favours lookups and listings, fewer the cost of
  each change. Default 128.

This setting can change later with `fs resize`:

- `-size`: the declared capacity that `df` shows. Default 100 GiB.

The root folder's ownership:

- **Primary-POSIX:** `-root-uid`, `-root-gid` (default: yours) and
  `-root-mode` (default `0755`).
- **Primary-Windows:**
  - `-root-owner SID` gives full control to that SID, SYSTEM and
    Administrators;
  - or `-root-sddl` gives a full security descriptor;
  - run on Windows with neither, the root takes the creating account's
    permissions.

The other `fs` commands:

- `fs list` lists the bucket's filesystems.
- `fs describe` shows one filesystem's settings, with its used space.
- `fs check` walks a filesystem and reports inconsistencies, changing
  nothing, and exits non-zero if it found any: directory blocks that don't
  decode or are out of order, names leading to missing objects, link
  counts that don't match the names, directories with two names. It also
  counts objects nothing reaches, which some operations leave legitimately.
  Run it on a quiet filesystem: one changing while it's walked can show
  problems that aren't.
- `fs archive` makes a filesystem read-only. It takes effect at the next
  mount.
- `fs prune -yes` erases every version of everything in an **archived**
  filesystem and frees its slot. It can't be undone.

## Named versions

A named version is a name for a moment: a snapshot that copies nothing.

```
icbfs-cli names create -fs docs -name q3-signoff                   # now
icbfs-cli names create -fs docs -name before-import -at 2026-10-01T09:00
icbfs-cli names list   -fs docs
icbfs-cli names delete -fs docs -name q3-signoff
```

- `-at` takes a date, a local date-time, or RFC 3339, as `-as-of` does.
- A name made on a directory drive (`-root /team`) belongs to that drive.
  `-filesystem-wide` makes it belong to the whole filesystem.
- Names are created and deleted, never moved.

Mount a named version with `-as-of NAME` (see [mount.md](mount.md)).

## Trash

Every delete from a mount or the API that removes a file's last name, or a
directory, is recorded in the filesystem's trash, and kept until the item
is purged.

```
icbfs-cli trash list    -fs docs [-path /reports] [-limit 20]
icbfs-cli trash restore -fs docs -id ENTRY
icbfs-cli trash restore -fs docs -id ENTRY -to /elsewhere/name -parents
```

- A restored item comes back with its whole history.
- Without `-to`, it goes back where it was.
- A taken name is refused, unless `-replace` is given; that moves what's
  there now to the trash first.

Permanent deletion of a single item is done through the drive API's purge
(see [api.md](api.md#permanent-delete)).
