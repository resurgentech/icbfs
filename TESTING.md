# Testing icbfs: the big-picture certification plan

> **Note to other agents: ignore this file for now.** This is a plan, not
> wired into any build target, CI job, or `go test` path yet. Nothing here
> needs implementing unless Jared explicitly asks for it. Don't let this
> doc redirect work you're already doing elsewhere in the repo.

This is a plan for a layer of testing icbfs doesn't have yet: proof, using
real, independently-written, widely-trusted test suites against a real
mount, that the filesystem is actually correct — not just that our own Go
test code is internally consistent with itself, and not a hand-rolled
reimplementation of tests that already exist and are already trusted by
the filesystems community.

## What already exists (don't rebuild this)

- **`internal/block/block_test.go`, `internal/objstore/s3store_test.go`** —
  unit tests on the B-tree block encoding and the S3 store's CAS/versioning
  primitives in isolation.
- **`internal/icbfs/filesystem_test.go`** — directory-split consistency and
  a concurrent-link CAS-race regression test, against the `icbfs.Filesystem`
  API directly (no FUSE involved).
- **`internal/fuseserver/mount_test.go`** — the first real end-to-end tier:
  a real MinIO container, a real versioned bucket, and a real kernel FUSE
  mount (`fs.Mount`, not a fake), driven through Go's `os` package
  (`os.WriteFile`, `os.Symlink`, `os.Link`, ...). This already proves the
  mount works when the *client* is `os/syscall` called in-process from the
  same test binary.

That last point is exactly the gap this document is about — and the right
fix for that gap is not to write more scenarios by hand, it's to point
proven upstream suites at the mount.

## The three upstream suites this plan standardizes on

Don't reinvent POSIX/Windows filesystem conformance testing — it already
exists, is already trusted, and is already what real filesystems (ZFS,
btrfs, gocryptfs, s3fs-fuse, WinFsp itself) get certified against.

### POSIX/FUSE semantics: [pjdfstest](https://github.com/pjd/pjdfstest)

A perl/C test suite, originally from FreeBSD, that exercises POSIX file
syscalls (`chmod`, `chown`, `link`, `unlink`, `rename`, `symlink`,
`truncate`, `mkdir`, `utimes`, ...) and asserts exact, spec-correct
behavior — including errno-level edge cases most hand-written tests never
think to check (e.g. `rename()` onto an existing non-empty directory must
fail with `ENOTEMPTY`, not silently succeed or return the wrong errno).
Other FUSE-backed object-store filesystems use it for exactly this reason
(e.g. it's been raised directly against s3fs-fuse). This replaces nearly
everything in the old hand-rolled catalog about permissions, links,
symlinks, rename, and timestamps — pjdfstest already covers that ground
far more rigorously than scenario-by-scenario scripting would.

- Needs: autoconf ≥2.69, automake ≥1.15, a C compiler, perl + the TAP-Harness
  perl package, and **root** (many of its tests specifically assert
  behavior that only differs under root vs. non-root, e.g. permission
  bypass).
- Run as: build it, then run its harness against the live mountpoint
  (`prove` over its generated test files, pointed at a directory under the
  mount).

### Stress and crash-consistency: fuse-xfstests + fsx

[xfstests](https://github.com/mix-mirror/xfstests) is the kernel
filesystem world's gold standard, built for block-device filesystems
(ext4, xfs, btrfs) but there's a known patch set that adapts it to run
against a FUSE mount instead:
[rfjakob/fuse-xfstests](https://github.com/rfjakob/fuse-xfstests), already
used in production by gocryptfs for the same reason we'd use it. The
`generic/` test group (filesystem-agnostic, not xfs-specific) is the
relevant subset. Bundled alongside it is **`fsx`** — a randomized
read/write/truncate/mmap fuzzer that hammers a single file with mutating
operations and checks for corruption. `fsx` is a far better version of
"two processes racing writes, kill one mid-operation" than anything
hand-scripted: it's specifically designed to shake out the class of bug
icbfs's CAS-retry write path is most exposed to.

- Needs: a patched xfstests checkout, root, and — like pjdfstest — real
  tolerance for things going wrong: a bad FUSE implementation can wedge
  the mount or hang a kernel thread under this suite. That's the direct
  reason this needs to run somewhere disposable (see below), not on a dev
  machine.

### Windows/WinFsp conformance:
[winfsp-tests](https://github.com/winfsp/winfsp/wiki/WinFsp-Testing)

This is WinFsp's *own* official conformance suite — the thing the WinFsp
project itself uses to validate any WinFsp-based filesystem, with a
built-in `--external` mode specifically for pointing it at a third-party
driver (like icbfs's eventual WinFsp layer) instead of WinFsp's own
reference/passthrough filesystem. It verifies non-WinFsp-specific behavior
against real NTFS as ground truth. Prebuilt binaries ship directly on
WinFsp's GitHub releases (`winfsp-tests-<version>.zip`) — no need to
install Visual Studio just to obtain the test runner.

- **Blocked, not actionable yet**: the WinFsp access layer doesn't exist
  in this codebase (confirmed design intent in `ARCHITECTURE.md`, not yet
  built per `ROADMAP.md`). This section is here so the plan exists before
  the driver does, not because there's anything to run today.

## Supplementary layer: real client ergonomics

The three suites above certify *POSIX/Windows filesystem semantics*. They
don't tell you whether `scp`, `rsync`, `git`, or `sqlite3` — real,
independently-written clients with their own buffering and I/O patterns —
actually have a good day against this specific mount. That's a smaller,
different, and still-worth-having layer, kept intentionally thin since the
suites above now cover the semantics ground this used to try to cover by
hand:

- **`git clone`/`commit`** into the mount, then re-clone and `diff -r` —
  stresses many small files, exec-bit mode preservation, and (once it
  exists) rename-into-place, in one realistic workload.
- **`sqlite3`** — create a database on the mount, insert several thousand
  rows across multiple transactions, `.dump` and diff against the same
  operations run on tmpfs. Stresses random-offset writes and fsync/WAL
  behavior specifically.
- **`scp`/`rsync -e ssh` round-trip** of a real directory tree, through a
  disposable per-run `sshd` (ephemeral host key, loopback port, no system
  `sshd` dependency) — proves the mount behaves as a normal `scp`/`sftp`
  target, which is the concrete thing that prompted this whole plan.
- **`tar -xzf` of a real third-party tarball** directly into the mount,
  `diff -r` against extracting the same tarball to tmpfs.

Each of these: verify via an independent checksum/`diff -r`, never just
"the client exited 0" — a client can exit successfully while the
filesystem silently mangled bytes underneath it.

## A real gap found while drafting this doc

`internal/fuseserver/node.go` has no `Rename` method. That's not yet
called out in `MISSING_FEATURES.md`/`ROADMAP.md`, but it's a real blocker
for a chunk of this plan: pjdfstest has a whole `rename/` test group,
`rsync`'s default behavior is "write a temp file, then `rename()` it into
place," and `git`'s object/index writes lean on the same pattern. Those
will fail for that reason alone until `Rename` exists — not a finding
about the suites or client tools. Worth its own line in
`MISSING_FEATURES.md` separately; not fixing it here, just flagging it so
it doesn't look like a surprise failure later.

## Where these actually run

Both pjdfstest and fuse-xfstests want root and can genuinely wedge a FUSE
mount or hang a kernel thread if icbfs has a bad enough bug. That's not a
reason to avoid running them — it's the reason to run them somewhere
disposable instead of the dev machine.

### Linux: a disposable KVM VM — this part is simple and actionable now

This is exactly the "give it root and go to town" instinct, and KVM
already being available makes it cheap:

1. **Boot a cloud image with `virt-install` + cloud-init** — no
   interactive OS install needed, same experience as spinning up a cloud
   VM. Ubuntu/Debian publish ready-made cloud images; cloud-init's
   `NoCloud` datasource injects your SSH key and runs setup commands on
   first boot:
   ```
   virt-install \
     --name icbfs-fuse-test --memory 4096 --vcpus 2 \
     --disk size=20 --import --os-variant ubuntu24.04 \
     --network network=default \
     --cloud-init user-data=cloud-init.yaml
   ```
   `cloud-init.yaml` sets `ssh_authorized_keys` to your public key and
   `packages:`/`runcmd:` to install `fuse3`, `build-essential`,
   `autoconf`, `automake`, `perl`, `git`, `golang`, and a local MinIO
   binary (or just run MinIO in a container inside the VM via Docker — it
   has its own kernel, this is fine).
2. **SSH in exactly like any other box** — `virsh domifaddr
   icbfs-fuse-test` for its IP on the libvirt NAT bridge, then `ssh
   ubuntu@<ip>`. No new workflow to learn here.
3. **Root inside the VM is free** — it's disposable, so just `sudo -i` (or
   run everything as root directly) rather than building any special
   privilege-elevation path. This is the whole point of using a VM instead
   of the dev machine for tests that explicitly want root.
4. **Snapshot the clean, set-up state, then revert instead of cleaning
   up**: `virsh snapshot-create-as icbfs-fuse-test clean-base` once
   dependencies are installed. After a run — especially an xfstests/fsx
   run that might genuinely wedge the mount — `virsh snapshot-revert
   icbfs-fuse-test clean-base` is faster and more trustworthy than trying
   to manually clean a guest that may be in a weird kernel-level state.
5. **Get the code in and run it**: `rsync -az --exclude .git ./
   ubuntu@<ip>:~/icbfs/`, then over that same SSH session: `go build -o
   ~/icbfs-bin ./cmd/icbfs`, mount it, run pjdfstest's harness and
   `fuse-xfstests`' `./check -g generic` (or targeted groups) and `fsx`
   against the mountpoint, and `scp`/`rsync` results back out.

No need for Vagrant/Packer/Terraform at this scale — it's one VM, and
`virt-install` + a shell script around steps 2–5 is the whole
orchestration layer. Worth reaching for something heavier only if this
grows into multiple parallel VMs or a CI fleet.

### Windows: verified end to end on this machine — SSH access confirmed working, not just planned

This stopped being a plan and became a running, SSH-reachable VM
(`icbfs-winfsp-test`, under this host's KVM/libvirt), built entirely in
`winvm-poc/` (gitignored scratch dir). Every claim below was actually run,
not taken on faith:

- `virt-install --os-variant win2k25` was accepted by this host's
  libvirt/osinfo-db, correctly recognized `win2k25`, and auto-applied the
  Windows-appropriate Hyper-V enlightenments and `cpu
  mode="host-passthrough"` on its own.
- The `virtio-win` driver ISO downloads directly, no gate — 877MB pulled
  straight from `fedorapeople.org`, verified as a real ISO 9660
  filesystem.
- **The one real manual step**: the Windows Server 2025 eval *ISO* itself
  is not a direct download — Microsoft gates it behind a genuine HTML
  `<form>` registration (name/email/company) on
  `info.microsoft.com`, not a curl-able redirect like `virtio-win`.
  Jared registered and downloaded it by hand once
  (`~/Downloads/...SERVER_EVAL_x64FRE_en-us.iso`, reflink-copied into
  `winvm-poc/`, sha256 verified identical). That's the only step in this
  entire flow that needs a human.
- **A fully unattended install actually ran**: `wiminfo` against the real
  `install.wim` confirmed image index 1 is `SERVERSTANDARDCORE`; a
  hand-built `autounattend.xml` (BIOS/MBR partitioning, since this host's
  `win2k25` domain profile defaults to SeaBIOS, not UEFI — confirmed by
  the absence of a `<loader>` tag in the generated domain XML) was burned
  onto a small ISO and attached as a second CD-ROM. `virsh screenshot`
  confirmed Setup skipped every interactive wizard screen (language,
  edition, EULA, disk partitioning) and went straight to "Installing
  Windows Server... 7% complete."
- **A real bug found running this for real, not a hypothetical**: the VM
  installed, then sat at a `logonui.exe` "press Ctrl+Alt+Del to unlock"
  screen indefinitely. Root cause: the original answer file put the
  OpenSSH-enablement commands under `FirstLogonCommands` in the
  `oobeSystem` pass, which only fire on an actual interactive logon —
  without an `<AutoLogon>` block, nothing ever logs in to trigger them, so
  the VM just waits at the login screen forever.
- **First fix attempt was wrong, and failed loudly — also worth keeping
  on record.** Moved the commands to `RunSynchronousCommand` in the
  `specialize` pass instead (runs as SYSTEM during setup, no logon
  needed). That traded one bug for a worse one: `specialize` runs very
  early, before networking is reliably up, and **any** non-zero exit from
  a `RunSynchronousCommand` there aborts Setup outright with "The computer
  restarted unexpectedly... Click OK to restart the installation" — hit
  that for real, on a fresh install, confirmed via `virsh screenshot`.
  `FirstLogonCommands` doesn't have that failure mode (a failing command
  there doesn't nuke the whole install). **Correct fix**: stay with
  `FirstLogonCommands` in `oobeSystem` (already proven working once,
  manually) and just add the missing `<AutoLogon>` block so a human never
  has to trigger it — plus `-ErrorAction SilentlyContinue` and a forced
  `; exit 0` on every command, belt-and-suspenders against this exact
  class of failure recurring.
- **A second, still-unexplained problem, mitigated rather than solved.**
  Independent of the answer-file bugs above, this VM's `qemu-system-x86_64`
  process was killed mid-install by a direct `SIGTERM` from `libvirtd`
  itself (confirmed in `/var/log/libvirt/qemu/icbfs-winfsp-test.log`:
  `terminating on signal 15 from pid 1067 (/usr/bin/libvirtd)`) — three
  separate times, at different elapsed durations each time (not a fixed
  timer), with `libvirtd` never having restarted. Ruled out: kernel
  OOM-kill, `systemd-oomd` (active, zero log entries), `libvirt-guests`
  (disabled), host suspend (this is a desktop, no battery, no suspend
  events in `logind`), hook scripts (`/etc/libvirt/hooks/` doesn't exist),
  wrapper binaries around `virsh`/`qemu-system-x86_64`, and any extra
  watchdog process in `ps aux`. **Root cause not found.** Mitigated, per
  Jared's direction, by making the wait-for-SSH watch auto-restart the VM
  (`virsh start`) whenever it finds the domain shut off instead of giving
  up — capped at 6 retries. This is a real open question about the host,
  not about icbfs or this plan, and is worth root-causing separately if it
  keeps happening.
- **Fully confirmed hands-off, with the real fix in place**: destroyed and
  recreated the VM from scratch with the corrected answer file. It hit
  the `libvirtd`-SIGTERM issue once more (auto-restarted by the watch, no
  human involved), then reached SSH with **zero manual steps** — no
  login, no intervention:
  ```
  $ ssh Administrator@<winvm-ip>
  PS C:\Users\Administrator> whoami
  win-b455ggnmnur\administrator
  PS C:\Users\Administrator> Get-Service sshd | Format-List Status,StartType
  Status    : Running
  StartType : Automatic
  PS C:\Users\Administrator> Get-WindowsCapability -Online -Name OpenSSH.Server~~~~0.0.1.0 | Format-List Online
  Online : True
  ```
  Landed in a real PowerShell prompt (the `DefaultShell` registry command
  took effect). This is the actual, confirmed answer to "is the new flow
  hands-off": yes, modulo the still-unexplained `libvirtd` kill, which the
  watch now absorbs automatically rather than requiring a person to notice
  and restart it.
- **Snapshotted the working state**: `virsh snapshot-create-as
  icbfs-winfsp-test clean-base` taken right after SSH was confirmed, then
  the VM was shut down to free its 8GB — revert to the snapshot instead of
  re-running the whole install dance.

The reusable path, now verified rather than theoretical:

1. **Get the Windows ISO** — the one manual step, per above.
2. **Create the VM** (BIOS/MBR, matching this host's default `win2k25`
   firmware profile — no UEFI/virtio driver injection needed since the
   emulated SATA disk and e1000e NIC both work out of the box):
   ```
   virt-install --name icbfs-winfsp-test --memory 8192 --vcpus 4 \
     --disk size=60,bus=sata \
     --cdrom windows-server-2025-eval.iso \
     --disk path=autounattend.iso,device=cdrom \
     --os-variant win2k25 --network network=default \
     --graphics vnc --noautoconsole
   ```
   (`autounattend.iso` is built with `xorriso -as mkisofs -V
   AUTOUNATTEND -J -R -o autounattend.iso unattend-src/`, carrying the
   `autounattend.xml` described above — Windows Setup scans attached
   media for that filename automatically, no `--location`/PXE needed.)
3. **Wait, and auto-restart if `libvirtd` kills it** — poll `virsh
   domstate`/`domifaddr` for a DHCP lease, then port 22 (`nc -z <ip> 22`);
   if `domstate` ever reports `shut off` before SSH is reachable, `virsh
   start` the domain again and keep waiting. With the `AutoLogon` fix,
   nothing manual is needed once the VM is actually running.
4. **Snapshot** once SSH is confirmed, same as above.
5. **Running winfsp-tests** (still the actual end goal, still blocked):
   once SSH works, `scp`/`Invoke-WebRequest` the prebuilt
   `winfsp-tests-<version>.zip` from WinFsp's GitHub releases in — no
   Visual Studio install needed just to get the test runner — and invoke
   it with `--external` pointed at icbfs's WinFsp mount.

Everything through step 4 is now verified on this machine, hands-off
confirmed end to end. Step 5 is the only piece still blocked — on the
WinFsp driver existing in this codebase, which it doesn't yet.

### The eval clock: checking whether `clean-base` is still usable before reusing it

The eval ISO is time-bombed (180 days), so the `clean-base` snapshot above
won't be good forever. Don't track this by computing "180 days from
creation" ourselves — Windows' own licensing service (`slmgr`) already
knows the real expiration, including any rearms already used, and is the
authoritative source. Query it directly:

```
ssh Administrator@<winvm-ip> "cscript //nologo C:\Windows\System32\slmgr.vbs /xpr"
```

Confirmed live on this VM (2026-10-10): `Windows(R), ServerStandardEval
edition: Timebased activation will expire 4/8/2027 12:08:21 AM` — i.e.
`clean-base`, as of now, is good until **2027-04-08**. `slmgr /dlv` gives
the longer form, including `Remaining Windows rearm count: 1` — one
`slmgr /rearm` + reboot is available to reset the 180-day clock a second
time (total ~360 days of life) before a from-scratch reinstall is
mandatory.

The practical check before reusing the snapshot for anything: boot it,
run the `slmgr /xpr` one-liner above, and compare its date to today.
- **Comfortably in the future**: just use it.
- **Close to expiring, rearm not yet used**: `slmgr /rearm` + reboot once,
  then re-snapshot `clean-base` with the new expiration noted.
- **Expired, or rearm already used**: don't fight it — delete the VM
  (`virsh destroy` + `virsh undefine --snapshots-metadata` + remove the
  qcow2/staged ISOs, as done earlier this session) and recreate from
  scratch via the `virt-install`/`autounattend.xml` flow above. That path
  is now fully verified and hands-off, so a from-scratch rebuild isn't a
  big deal when it's actually needed.
at all, which it doesn't yet.

## Automation shape

- Both suites, plus the supplementary client-ergonomics layer, run inside
  their respective disposable VM, not on the dev machine and not in
  default CI.
- A thin orchestration script per OS: revert snapshot → boot → push source
  in (`rsync`/`scp`) → SSH in and run the suite(s) → pull results back out
  → optionally revert again. No heavier tooling needed at one-VM scale.
- Gate the whole tier behind something like `make certify-linux` /
  `make certify-windows`, kept separate from `make test`/default CI.

## Explicitly out of scope for this document

- **The REST API and Views.** Both are deferred, undesigned features per
  `ARCHITECTURE.md` — nothing to certify yet.
- **Performance/load testing.** This plan is about correctness under real
  suites and real clients, not throughput or latency benchmarking. A
  load-test tier is a different document.
