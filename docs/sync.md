# icbfs-sync

`icbfs-sync` keeps folders on a Windows machine in step with icbfs drives.
Each **pair** is a folder (a local path or a mapped network drive) and a
drive (an icbfs filesystem, or a directory in one):

- **one-way:** the folder is the truth, and the drive mirrors it.
- **two-way:** changes go both ways. When both sides changed a file, the
  folder's change wins.

Nothing is lost on the drive. An overwrite is a new version, and a delete
goes to the drive's trash, so the drive's history and trash hold
everything the folder ever had.

It works from **listings, not change events**. Each pass lists the folder
(and, when due, the drive), compares both with what it recorded at the
last pass, and acts on the differences. That makes it work on folders that
can't report changes, such as a WebDAV drive, and lets it catch up after
any amount of time away.

It runs **in the user's session, not as a service.** A mapped network
drive belongs to the user's logon and exists only while they're logged in,
so a service couldn't see it.

## Setting up

```
icbfs-sync init          # write a settings file to start from
notepad "%APPDATA%\icbfs-sync\config.json"
icbfs-sync set-secret    # store the secret key encrypted for this account
icbfs-sync once          # one pass of every pair, from a command prompt
icbfs-sync install       # start the tray at login
icbfs-sync               # start the tray now
```

`icbfs-sync paths` prints where the settings, state and log are kept.

The drive's filesystem must already exist (`icbfs-cli fs create`; see
[cli.md](cli.md)). For a WebDAV folder, create it primary-Windows, so the
drive holds Windows names and permissions.

### Starting from a copy you already have

If a copy of the folder is already at hand, seed the drive from it before
the first pass instead of having icbfs-sync upload everything:

1. Mount the filesystem (`icbfs-winfsp ... Y:`; see [mount.md](mount.md)).
2. Copy onto it with robocopy, which keeps file times:
   `robocopy C:\copy Y:\ /E /COPY:DAT /DCOPY:T` (add `/SEC` to keep ACLs
   from an NTFS copy).
3. Unmount, and run the first pass.

On the first pass, a file whose path, size and modification time match on
both sides is **adopted**: it is recorded as in step without being copied.
Only what differs is transferred. Times are compared to the folder's time
resolution, so a WebDAV folder's whole seconds match a copy's finer times.

## Settings

The settings file is JSON: one `store`, and a list of `pairs`.

```json
{
  "store": {
    "endpoint": "https://s3.example.com",
    "region": "us-east-1",
    "bucket": "documents",
    "accessKey": "ACCESS-KEY",
    "secretKeyProtected": "…written by icbfs-sync set-secret…"
  },
  "pairs": [
    {
      "name": "documents",
      "folder": "Z:\\",
      "filesystem": "documents",
      "root": "/",
      "mode": "one-way",
      "interval": "5m",
      "remoteEvery": "1h",
      "maxRemovals": 200,
      "requirePresent": ["marker.url"],
      "ignore": ["*.url"],
      "ids": "auto",
      "acls": "auto"
    }
  ]
}
```

**store**

| setting | meaning |
|---|---|
| `endpoint`, `region`, `bucket` | the S3-API store the drives are in |
| `accessKey` | the access key |
| `secretKey` | the secret key in the clear, or leave it out and run `icbfs-sync set-secret` |
| `secretKeyProtected` | the secret key encrypted for this Windows account (DPAPI), written by `set-secret` |

**pairs**

| setting | default | meaning |
|---|---|---|
| `name` | required | the pair's name, in the tray and the log |
| `folder` | required | the folder: `C:\Data`, or a mapped drive, `Z:\` |
| `filesystem` | required | the icbfs filesystem |
| `root` | `/` | the directory in the filesystem the pair keeps in step |
| `mode` | required | `one-way` or `two-way` |
| `interval` | `5m` | time between passes |
| `remoteEvery` | `1h` | one-way only: how often the drive is listed too (see below) |
| `maxRemovals` | none | a pass that would remove more files than this waits for confirmation |
| `requirePresent` | none | paths that must be in the folder for a pass to go ahead |
| `ignore` | none | name patterns skipped, added to the built-in list |
| `ids` | `auto` | use the folder's stable file IDs: `auto` (ask the volume), `on`, `off` |
| `acls` | `auto` | copy the folder's ACLs to the drive: `auto`, `on`, `off` |
| `timeResolution` | the volume's | how finely the folder keeps times, such as `1s` |
| `caseSensitive` | `false` | tell names apart by case. Windows doesn't. |
| `paused` | `false` | start paused |

**`ids`, `acls` and `timeResolution` with `auto`** ask the volume
(`GetVolumeInformation`).

- NTFS and ReFS have stable file IDs, real ACLs and 100 ns times.
- Any other volume, including a WebDAV drive, is taken to have none of
  those, with times kept to the second.
- Set these explicitly if a volume reports more than it really keeps.

**Ignored names.** Built in:

- `~$*`, `~*.tmp`, `*.tmp` and `.~lock.*`: editors' lock and temporary
  files;
- `Thumbs.db`, `desktop.ini` and `.DS_Store`;
- `$RECYCLE.BIN` and `System Volume Information`.

Patterns are matched case-insensitively against each part of a path. A
matching directory is skipped with everything in it. Ignored files are not
copied in either direction, and are never removed from the other side.

## How a pass decides

Each pair keeps a **base**: for every path, what both sides held after the
last pass. The base records:

- size and time;
- the folder's file ID;
- the drive's item ID and version;
- the content hash, when one was computed;
- the ACL's hash.

A pass works in this order:

1. **Checks** that the folder is really there (see Safety).
2. **Lists** the folder and, when due, the drive.
   - In two-way mode the drive is listed every pass.
   - In one-way mode it is listed every `remoteEvery`, after any error, and
     on demand. In between, the drive is taken to be as the last pass left
     it, which saves listing a large drive every few minutes.
3. **Finds renames** first, so that a moved file is moved on the drive,
   keeping its history, instead of being uploaded again:
   - by the folder's file IDs, where the volume has them;
   - by the drive's item IDs (two-way: a rename made on the drive is made
     in the folder);
   - by a directory's contents: a directory gone and one new, holding
     exactly the same files with the same sizes and times;
   - by spelling: the same name with different case.
4. **Compares** each path's folder side, drive side and base:

   | folder | drive | one-way | two-way |
   |---|---|---|---|
   | new | — | upload | upload |
   | — | new | remove from the drive | download |
   | changed | unchanged | upload | upload |
   | unchanged | changed | upload (the drive's change stays in its history) | download |
   | changed | changed | upload | upload (the drive's change stays in its history) |
   | removed | unchanged | remove from the drive | remove from the drive |
   | removed | changed | remove from the drive | download (the change wins) |
   | unchanged | removed | upload again | remove from the folder |
   | changed | removed | upload again | upload (the change wins) |
   | new | new, same size and time | adopt | adopt |

5. **Checks** the plan against `maxRemovals`.
6. **Carries out** the plan in order: moves, new directories (shallowest
   first), content, then removals (files, then directories, deepest
   first). It records the new base as it goes.

**Renames without file IDs.** A WebDAV folder has no stable IDs, so a
rename there looks like one file gone and a new one. When a new file is the
same size as a file that's gone:

1. The new file is read once, into a spool file, and hashed (SHA-256).
2. It is compared with the gone file's hash. That hash comes from the base,
   or from the drive's stored hash (`user.sync.sha256`), or failing both,
   from reading the drive's copy.
3. If they match, the drive's file is **moved** instead of a new one being
   uploaded.

A rename that is noticed across two passes, so that the old file was
already removed from the drive, is matched the same way against the
drive's trash, and **restored from the trash** under its new name.

Files are hashed only when a size match makes a rename possible, not on
every pass.

**Editors' saves.** Many editors save by writing a temporary file, moving
the original aside, moving the temporary file into place, and deleting the
original. The temporary names are ignored, so what the pass sees is the
same path with new content: a new version on the drive, not a delete and a
new file.

**ACLs.** Where both sides keep real ACLs, a file's or directory's ACL is
copied to the drive when it changes, as SDDL.

## Safety

A pass is **held**, and changes nothing, when:

- **A `requirePresent` path is missing.** This is a marker file that shows
  a network drive is really connected. The check is made on the raw
  listing, before names are ignored, so a marker can be ignored (not
  copied) and still guard the pass.
- **The folder lists as empty**, but the base says it had files. A
  disconnected network drive often looks like an empty folder.
- **It would remove more than `maxRemovals` files** (from either side).
  The tray shows *Confirm removals* on the pair, and
  `icbfs-sync once -confirm` lets them go ahead from a command prompt.

Even a removal that goes ahead only moves the file to the drive's trash.

## The tray

Running `icbfs-sync` with no command starts the tray app.

- The **icon's tooltip** shows the overall state: up to date, syncing,
  paused, or needs attention.
- The **top of the menu** shows the current upload and download rates.
- **Each pair's submenu** shows:
  - its state, and what its last pass did;
  - its traffic;
  - *Sync now*, *Pause*/*Resume*, and *Confirm removals* (when a pass is
    held);
  - *Open folder*;
  - the ten most recent actions.
- **Global items:** *Sync all now*, *Open log*, *Edit settings*, *Restart*
  (reloads the settings), and *Quit*.

`icbfs-sync install` adds the tray to the current user's startup programs
(`HKCU\Software\Microsoft\Windows\CurrentVersion\Run`), and
`icbfs-sync uninstall` removes it. Build it with `-ldflags -H=windowsgui`,
so that starting it doesn't open a console window. The command-line
commands still write to the console they were started from.

## Commands

```
icbfs-sync                 the tray (Windows)
icbfs-sync tray            the same
icbfs-sync run             sync until stopped, logging to the console
icbfs-sync once [-pair NAME] [-confirm]
                           one pass of each pair (or one), then exit
icbfs-sync init            write a settings file to start from
icbfs-sync set-secret      store the secret key encrypted for this account
icbfs-sync install         start the tray at login
icbfs-sync uninstall
icbfs-sync paths           where the settings, state and log are
```

Every command takes `-config FILE` and `-data DIR` to use other locations.

## Where things are

| what | where |
|---|---|
| settings | `%APPDATA%\icbfs-sync\config.json` |
| log | `%LOCALAPPDATA%\icbfs-sync\icbfs-sync.log` (rotated at 10 MiB; the previous log is kept) |
| each pair's base | `%LOCALAPPDATA%\icbfs-sync\<pair>\state.db` (SQLite) |
| each pair's spool | `%LOCALAPPDATA%\icbfs-sync\<pair>\spool` (files being hashed or uploaded) |
| each pair's cache | `%LOCALAPPDATA%\icbfs-sync\<pair>\cache` |

Deleting a pair's `state.db` makes the next pass a first pass. Files that
match on both sides are adopted, and the rest are compared as new. In
one-way mode, that means files only on the drive are removed from it (to
its trash).

On the drive, the changes are recorded as made by
`icbfs-sync:<host>/<user>`.

## Troubleshooting

- **"held: marker.url isn't in the folder"** or **"the folder lists as
  empty"**: the folder isn't connected. Reconnect the network drive; the
  next pass carries on.
- **"N files would be removed (more than M)"**: check that the removals are
  intended, then use *Confirm removals* or `icbfs-sync once -confirm`.
- **Everything is uploaded again after a rename on a WebDAV drive**: the
  file's size changed as well, so there was nothing to match it by. The
  old content is still in the drive's trash.
- **A change made on the drive doesn't show up in one-way mode**: this is
  expected. One-way mode overwrites the drive from the folder, and the
  drive's change stays in its history. Use *Sync now* to list the drive at
  once.

## Tested, and not

- **Go tests** for the planner, the engine, both sides and the command
  pass, including under the race detector. The folder side's Windows tests
  run on NTFS.
- **End to end on Windows Server 2025** (`test/windows/run-sync-test.sh`):
  the real `icbfs-sync.exe` against MinIO, checked through a real mount.
  Three pairs: an NTFS folder; an NTFS folder made to behave as a WebDAV
  drive does (no IDs, no ACLs, whole seconds); and a drive seeded by
  robocopy. All 12 checks pass. See [testing.md](testing.md).
- **Not yet tested:**
  - a real WebDAV redirector drive (the test machine has no WebDAV
    client);
  - the tray's menus, by hand;
  - folders of hundreds of thousands of files.
