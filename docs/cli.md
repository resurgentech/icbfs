# icbfs-cli

`icbfs-cli` does what isn't a filesystem operation: creating and managing
filesystems, naming versions, and the trash.

```
icbfs-cli fs create   -fs NAME [-primary-windows] [-size N] [-max-file-size N] [-dir-block-entries N] [root options]
icbfs-cli fs list     [-json]
icbfs-cli fs describe -fs NAME [-json]
icbfs-cli fs check    -fs NAME [-repair] [-json]
icbfs-cli fs dedupe   -fs NAME [-path PATH] [-min-size N] [-dump FILE|-] [-current] [-dry-run] [-history] [-json]
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
| `-azure-token-command` | `ICBFS_AZURE_TOKEN_COMMAND` | none: a program that gets a token, instead of the key (see [mount.md](mount.md)) |

A DRIVE is `-fs NAME`, optionally with `-root DIR` for a directory in
the filesystem. The `fs` commands always name a filesystem with `-fs`.

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
  counts objects nothing reaches, which some operations leave legitimately,
  and the retired holders purges left (an item stubs still name). For
  stubs it reports `ref-target-missing`, `ref-to-ref`, `ref-entry-dead`,
  `ref-entry-pending` and `ref-holder-dead`. `-repair` drops dead `.refs`
  entries, finalizes or drops pending ones, and erases dead holders; it
  then exits non-zero only for what it couldn't repair. Run it on a quiet
  filesystem: one changing while it's walked can show problems that
  aren't.
- `fs dedupe` finds file content versions with identical bytes, in the
  filesystem or under `-path`, and replaces copies with stubs (versions
  that name another version instead of holding bytes). Without
  `-dry-run`, each current version that is a copy of its group's
  canonical one is replaced: the file reads the same bytes, and its
  history has the same versions at the same dates, the current one now a
  stub; the copy's bytes are freed. It prints how many versions (and
  bytes) it replaced, and each one it skipped, with the reason:
  `referrers` (stubs already name that version), `changed` (a writer
  changed the file during the run), `stub` (it already is one) or
  `target` (the canonical copy can't be a stub's target). A run that was
  killed is finished by the next one ("finished N left by an earlier
  run"); a run with nothing left to do writes nothing. With `-history`,
  past versions are replaced too: each affected file's history is
  rewritten, the same versions at the same dates with its duplicates now
  stubs (of identical consecutive versions, the newer becomes a stub at
  the older). Stubs that name a version of a file being rewritten are
  first re-pointed to the copy that stays ("re-pointed N stubs"), and
  stubs naming a purged file's retired holder are re-pointed to a live
  copy of its content, if there is one. A file whose stubs elsewhere
  name a version that stays is skipped (`referrers`), as is one open on
  this machine (`open`).
  - It lists the filesystem's objects once and reads each file's
    `.metadata` once. Versions are grouped by SHA-256 and size, live files
    only (the trash is not scanned). A version with no recorded checksums
    (one a mount wrote) has them computed, which reads the version once,
    and recorded in the file's `.metadata`, so the next scan reads none;
    recording leaves the file's dates and history as they were.
  - A version smaller than `-min-size` (default 2 MiB) is not looked at;
    the report counts them. Hard-linked files count once.
  - In each group the canonical copy, the one that would stay, is the
    oldest version by date, unless another copy already has stubs naming
    it: that one stays.
  - `-dry-run` prints, for current versions only and for all versions: the
    groups, the duplicate versions, the bytes they would free, the
    versions already stubs and those below the minimum. `-current` scans
    current versions only (cheaper: histories are not read) and leaves out
    the all-versions numbers. `-history` is the run that rewrites
    histories; it can't be combined with `-current`. A writing run prints the same numbers, from its scan
    before the writes, then what it wrote.
  - `-dump FILE` (`-` for standard output, the report then on standard
    error) writes a JSON line for each version scanned, files in path
    order, a file's versions newest first by date: `path`, `itemId`,
    `versionId`, `etag`, `date`, `size` (the content's real size),
    `md5` and `sha256` each with `md5_computed` / `sha256_computed`
    (false: recorded before this run), and for a stub `ref` (`key`,
    `version`). With a writing run, the dump is of the scan before the
    writes.
  - Through the daemon, the file is written on your machine: the server
    sends the versions back.
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

Permanent deletion of a single item is the drive library's `Purge`
(see [api.md](api.md#permanent-delete)).
