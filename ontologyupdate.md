# Ontology update: drives, documents and the drive API

For host's software ontology (`ontology/software/knowledge/`). These
are the owner's decisions of 2026-10-13, made while designing the drive API
on top of icbfs (see icbfs's `apiproposal.md` for the full design). Each
section names the entity or interface it changes, what the ontology says
now, and what it should say.

## Drive (`domain/drive.md`)

**Adds.**

- A drive is one of many kinds, over one contract: icbfs is the first
  backend; SharePoint libraries, network file shares and Git repositories
  follow. The contract is a Go interface covering the superset of what drives
  can do; each drive declares its **capabilities** (stable ids, versions, the
  tree as of a time, named versions, trash, permanent delete, conditional
  writes, change events, a permissions model, diff), and an operation its
  backend can't do is refused as not supported, so the app shows only what
  works.
- **Host owns the drive registry**: id, name, kind, backend, root (a whole
  icbfs filesystem or a directory in one), writable policy, owning org, and
  the role assignments on it. The library serves whatever drive host hands
  it and keeps no state about drives.
- **Storage under icbfs drives.** Infrastructure provides buckets (versioning
  on) and their credentials; host **registers buckets**. icbfs filesystems
  (icbfs's own storage unit, which mounts attach to) are created in registered
  buckets through the drive API, from the admin panel, or with `icbfs-cli`;
  every setting is given at creation, and only the declared size changes later.
  Mounts never create a filesystem. One filesystem can back several drives
  (the whole tree, subtrees).
- **Copying between drives** is a read from one and a write to the other,
  whatever their kinds; the copy records its origin (drive, id, version) where
  the target can keep it. This settles "a document copied from one drive to
  another is a new document on the target", with the origin link the owner
  asked for.

**Naming.** "Drive" is the API's and the product's word; icbfs's engine keeps
"filesystem" for its storage unit.

## Document (`domain/document.md`)

**Changes.**

- **Versions are times, not numbers.** Replace "a version number per file"
  with: each version is shown as the time it was written (store-assigned), and
  carries an id the API uses to fetch it or to save conditionally. Two saves
  within a second on AWS share a time; the ids tell them apart.
- **Drafts are kept by host, in a store.** Replace "the editor's autosave
  is the editor's own ... reaches the drive only on save" with: the editor's
  autosave writes a **draft**, per user and document, kept by host in its
  side store (below), never in RAM and never as a version; saving publishes
  the draft, only if the document is still at the version the draft started
  from — otherwise the user sees the conflict and merges or overwrites.

**Adds.**

- **Named versions**: a name for a time, on a drive (its subtree) or a whole
  filesystem — a snapshot that copies nothing. Created for now or for a past
  time; created and deleted, never moved. A document, a folder or a whole
  drive can be opened at a named version as at any time.
- **Trash**: every delete is recorded and kept forever; a deleted document or
  folder can be restored anywhere, its history intact. There is no emptying
  the trash.
- **Permanent delete**: one item at a time (a document, or a folder with
  everything under it), for removal requests. It removes the content and
  metadata, every version, the trash entry and everyone's drafts; the name may
  remain in old versions of its folder; the audit log remains.

## Drive API (`application/drive-api.md`)

**Changes.**

- "Specs to come from the owner": the design is icbfs's `apiproposal.md`; the
  spec will be an OpenAPI file, spec-first like host's.
- What the contract carries, now settled: list, stat, read (by range, at a
  time, at a version, at a named version), write (conditional on a version),
  mkdir, move, copy (within or between drives), delete to trash, restore,
  permanent delete, versions, named versions, diff, change events, the
  drive's capabilities, the actor's effective rights on an item, and the
  identities a drive's permissions mention.
- It's a library first: a Go interface the Interface server's code calls
  directly, with HTTP handlers over it that host-server mounts.

## Permissions (`application/authorization.md`, `domain/role.md`)

**Adds.**

- Two layers on a drive. First host's check: the operation's permission
  (kind-dot-action, proposed `drive.read`, `drive.write`, `drive.delete`,
  `drive.restore`, `drive.purge`, `drive.name`, `drive.permissions.read`,
  `storage.read`, `storage.admin`) against the actor's role assignments.
  Then the file's own permissions, checked as the actor's **profile**.
- **Profile**: an identity the drive's storage knows — a Windows account (SID
  and group SIDs) on a primary-Windows filesystem, a POSIX uid and gids on a
  primary-POSIX one. Host maps actors (users and agents) to profiles per
  drive. The file's own check then follows its storage's model: Windows ACLs,
  or POSIX modes.
- Files without an ACL of their own inherit from their nearest ancestor that
  has one.
- The drive API can list every identity a drive's permissions mention (a long
  scan), to help an administrator map actors to profiles; names for Windows
  SIDs come from the directory, not from the drive.
- Permanent delete is a permission (`drive.purge`) held through roles, not an
  administrator's prerogative.

## Review (`domain/review.md`)

**Adds.** Review information — comments, feedback, the base version a review
started from, the provenance of versions an agent writes — lives in host's
**side store**: one icbfs-backed store for host's own data about documents,
whatever drive the document is on. Drafts live there too.

## Daemon and machines (`domain/machine.md`, the daemon's part)

**Adds.**

- A mount gets its store credentials from the server through the daemon, and
  can't mount without them (credentials given directly remain possible, for
  use outside host). Every read, write, delete and listing on a Linux
  mount is logged through the daemon; the Windows mount doesn't log, there
  being no daemon on Windows (out of scope).
- **`icbfs-cli`**: side commands that aren't filesystem operations (creating
  and deleting named versions now; more later) go through the daemon to
  host, which checks the permission and does them; the actor is the agent
  on the machine. No store credentials on the machine.
- A machine no longer owns a document store: drives exist on their own, and a
  machine is granted permission to mount one.

## Presence (new, or in the web app's part)

Who has a document open, and whether editing or viewing, is host's: it
knows its users and has a live channel to them. The drive API doesn't track it.
