# Testing icbfs: the big-picture certification plan

> **Note to other agents:** this is a plan, not wired into any build
> target, CI job, or `go test` path yet — don't let it redirect work
> you're already doing elsewhere in the repo unless it's actually
> relevant. It's no longer purely speculative, though: Jared has asked
> to move forward with `ROADMAP.md`'s Part F (the WinFsp driver) as of
> 2026-10-10, and the Windows VM this doc describes is real, verified,
> and ready — see Part F's own updated intro in `ROADMAP.md` for the
> current plan and open questions before touching any of this.

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

### Windows: verified end to end, documentation and scripts live in `test/windows/`

Fully verified on this host, not just planned — real KVM/libvirt VM, fully
unattended Windows Server 2025 install, confirmed SSH access with zero
manual steps, a reusable snapshot, and a documented check for whether the
eval clock still has life left before reusing vs. recreating it. Two real
bugs were found and fixed running this for real (an answer-file mistake
that aborted Setup outright, and a login-screen hang), plus one
still-unexplained host issue (periodic `libvirtd` SIGTERM) that's
mitigated with an auto-restart loop rather than solved.

See **[`test/windows/README.md`](test/windows/README.md)** for the full
story and **`test/windows/*.sh`** for the actual runnable scripts
(`create-vm.sh`, `wait-for-ssh.sh`, `check-eval-expiry.sh`,
`destroy-vm.sh`). WinFsp itself and `winfsp-tests` are now installed and
verified working on this VM too (107/107 of `winfsp-tests`' own default
suite passing against its embedded reference filesystem) — see
`test/windows/README.md`'s own section on this. The only piece still
blocked is running `winfsp-tests --external` against *icbfs's own*
WinFsp driver, which doesn't exist in this codebase yet
(`ROADMAP.md`'s Part F, now in progress).

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
