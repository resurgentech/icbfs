# Metadata copy tests

Do the ordinary ways of copying files onto icbfs keep what they keep onto
a local disk? Each tool copies the same tree twice, once onto a local disk
(tmpfs on Linux, NTFS on Windows) and once onto an icbfs mount, and the two
copies are compared field by field. Any difference is icbfs's: the local
disk is the reference for what that tool keeps, so a tool that drops
something by design (a plain Explorer copy's new creation time) is no
failure, and one icbfs keeps that the disk wouldn't is.

- `tree.json` describes the tree: the hard cases for times (decades old,
  before 1970, after 2038, 100ns fractions, a creation time after the
  modified time), POSIX modes (setuid, setgid, sticky, none), owners with no
  names, extended attributes (text, binary, a 60 KB value, `trusted.`),
  POSIX access and default ACLs, Windows attributes, owners and DACLs
  (protected and inherited), alternate data streams, symlinks (to a file,
  a folder, nowhere), hard links, empty and 9 MiB files, deep folders and
  awkward names.
- `metatree` builds it with the system's own calls and compares trees:
  `build`, `check` (against the description), `dump` and `diff` (two
  trees). See its doc comment.
- `run-linux.sh <vm-ip>` runs the Linux tools on the Linux test VM as root:
  `cp -a`, `cp -rp`, `rsync -aAXH`, `rsync -aAXHU` and `tar` with ACLs and
  extended attributes.
- `run-windows.sh` runs the Windows tools on the Windows 11 test VM as
  Administrator: robocopy (`/COPY:DAT /DCOPY:DAT`, and `/COPY:DATSO /B`),
  `xcopy /E /H /K /O /X /B`, `Copy-Item` and a DISM image captured and
  applied, and first the Go tests that need NTFS and WinFsp. It runs on the
  VM's logged-on desktop. Explorer's copy isn't among them: scripted (the
  shell's `CopyHere`), it never finished.

Both use a throwaway MinIO and a fresh filesystem, take a few minutes, and
leave per tool, in `work/<platform>-<time>/`, the differences from the
local disk (`<tool>.diff.txt`), what the copy kept of the description
(`<tool>.check.txt`: the tool's own choices, when the local disk's copy
lost them too) and the tool's output.

Hard links aren't compared on Windows: WinFsp has no way for a filesystem
to make one ("The request is not supported", before icbfs sees it).
xcopy `/O` fails on icbfs: given an ACL of inherited entries at create
(what `/O` copies), WinFsp computes the new item's descriptor without
them, an empty DACL, and xcopy is then shut out of what it made. robocopy
`/COPY:DATSO` (or `/SEC`) copies the same ACLs whole.

The Windows mount's `-atime` follows the VM's NTFS: `noatime` when
`fsutil behavior query disablelastaccess` says last-access updates are
disabled (Windows 11's default), `hourly` otherwise, so both copies record
access times alike.

What can't be compared, by design: status-change times (no tool can set
them), and on Linux creation times, which nothing can set; there a copy's
creation time is the moment of the copy on both disks. Creation times come
onto icbfs from Windows tools, `icbfs-sync` and the drive API (whose own
tests cover them: `pkg/drive/icbfsdrive/times_test.go`,
`internal/syncer/created_test.go`).
