# Linux/FUSE test environment

Mirrors `test/windows/README.md`'s pattern: a disposable KVM/libvirt VM,
built unattended, used to run real, independently-trusted upstream test
suites against icbfs's own driver — not a reference filesystem, and not
on the dev machine, per `TESTING.md`'s whole reasoning for why this
needs to run somewhere disposable with real root.

## Layout

- `cloud-init.yaml` — unattended first-boot setup (packages, Go,
  Docker) for the Ubuntu 24.04 cloud image.
- `create-vm.sh` — downloads the cloud image once, stages it into
  libvirt's own images pool, and calls `virt-install` with that
  cloud-init file.
- `wait-for-ssh.sh` — polls until the VM is up and SSH-reachable.
- `destroy-vm.sh` — tears the VM down, keeping the downloaded cloud
  image and staged base disk so recreating it doesn't need
  re-downloading.
- `run-pjdfstest.sh` — the durable, re-runnable version of this doc's
  manual steps: builds `icbfs-cli`, mounts a fresh filesystem, runs
  pjdfstest, unmounts, reports a summary.
- `lib.sh` — shared SSH/SCP helpers (key-based auth, `ubuntu@<ip>`, no
  password — unlike the Windows VM's password-based `sshpass`, since
  cloud-init injects an SSH key directly).

## Normal workflow

```
./create-vm.sh          # first time only, or after destroy-vm.sh
./wait-for-ssh.sh        # prints the VM's IP once SSH is up
./run-pjdfstest.sh <ip>  # build, mount, run pjdfstest, unmount, summarize
./destroy-vm.sh          # when done
```

Root is free inside this VM — it's disposable, so `sudo` directly
rather than building a privilege-elevation path, same reasoning
`test/windows/README.md` gives.

## One real gotcha building this, kept on record

Passing a full custom `user-data` file *and* `virt-install`'s own
separate `--cloud-init clouduser-ssh-key=` option at the same time
silently drops the SSH key injection — SSH came up, but key auth was
rejected outright. Fixed by setting `ssh_authorized_keys` directly
inside `cloud-init.yaml`, with `create-vm.sh` `sed`-substituting the
real `~/.ssh/id_ed25519.pub` contents into a generated copy before
passing *only* that file to `virt-install`.

A `libtap-harness-perl` package was initially listed for pjdfstest's
`prove` harness — it doesn't exist as a separate package on Ubuntu
24.04. Removed: `prove`/`Test::Harness` already ship with the base
`perl` package (confirmed via `which prove`).

## pjdfstest: verified end to end, three real crash bugs found and fixed

[pjdfstest](https://github.com/pjd/pjdfstest) is the POSIX syscall
conformance suite `TESTING.md` standardizes on. Built on the VM via
`autoreconf -ifs && ./configure && make pjdfstest`, run via `prove -r`
against a real icbfs FUSE mount as root (many of its tests specifically
assert behavior that only differs under root vs. non-root).

Running the complete suite — not a hand-picked subset — surfaced three
real bugs in `internal/icbfs`, all from the same root cause: this
implementation has no sparse-region support at all, so any resize or
write that extends a file always means actually materializing that
many real bytes as one in-memory `[]byte`, unlike a real filesystem's
sparse "hole."

1. **Negative `truncate()`/`ftruncate()` size** — `make([]byte,
   negative)` panics with `"makeslice: len out of range"`, taking the
   whole process down over one malformed call. pjdfstest's own
   `truncate`/`ftruncate` groups exercise exactly this.
2. **Absurdly large size** — pjdfstest's `truncate/12.t`/`ftruncate/12.t`
   specifically probe `truncate(path, 999999999999999)` (~909 TB) and
   accept either `EFBIG` or `EINVAL` as correct; this implementation
   hit the exact same `makeslice` panic instead.
3. **The hardest to find: a write at a huge offset.** pjdfstest's
   `open` category probes the 32-bit `off_t` boundary with a 1-byte
   write at offset `2147483649` (2³¹+1). `OpenFile.applyLocked`'s
   `make([]byte, end)` tried to materialize the ~2 GB logical gap as
   one real allocation — on this VM's 4 GB of RAM, already running the
   rest of the suite, that got the **entire `icbfs-cli` process
   OOM-killed by the Linux kernel outright** (confirmed via
   `dmesg`/`journalctl`: no Go panic trace at all, since `SIGKILL`
   can't be caught). This only reproduced running the *complete* suite
   — an isolated run of just the nearby test categories did not
   trigger it, and a simple 500-cycle create/write/read/delete
   reproduction loop showed flat memory use throughout, ruling out a
   simple per-operation leak. Pinpointing the exact trigger needed
   temporary `runtime.MemStats` logging (caught a single 5-second
   window jumping from 3 MB to 2 GB of heap, with object count
   *dropping* — the signature of one huge allocation, not a leak)
   followed by targeted logging at the two candidate allocation
   sites, which caught the exact call: a write, not a truncate, at
   offset 2³¹+1.

**Fixed** with a new `maxSetAttrSize` constant (`internal/icbfs/filesystem.go`)
bounding both `SetAttr`'s resize path and `OpenFile.applyLocked`'s
write-extension path. An initial value of `1<<40` (1 TiB) — reasoning
only about Go's own runtime allocation limits — was wrong, caught
first by a failing regression test locally, then confirmed wrong again
by a repeat OOM on the real VM, since the real trigger is ~2 GB, far
under a 1 TiB bound. Corrected to `1<<31` (2 GiB, the exact 32-bit
`off_t` boundary pjdfstest itself tests for) — deliberately far more
conservative than "the largest value the runtime allows," and the
value that actually closes the real-world gap. Both paths now return
the new `ErrInvalidArgument` sentinel (mapped to `EINVAL` on the FUSE
side, `STATUS_INVALID_PARAMETER` on the WinFsp side) instead of
crashing. All three are covered by regression tests in
`internal/icbfs/filesystem_test.go` and `openfile_test.go`, each
verified discriminating by temporarily removing its guard and
reproducing the real failure first.

**Verified fixed for real**: a complete pjdfstest run (238 files, 8,792
assertions) finished with the mount alive throughout — the first time
that happened, confirmed via a liveness-touch check immediately after
`prove` exited.

## A real finding that wasn't an icbfs bug: `allow_other` and mount-path traversal

pjdfstest's `open/06.t` (and several other files using its `-u`/`-g`
simulated-caller flags) initially looked like a genuine
permission-enforcement bug: create a subdirectory, `chown` it to a
non-root uid, `chmod 0755`, then try to create a file inside it *as
that same uid* — and get `EACCES`, even though `stat` confirmed the
directory's owner and mode were both recorded correctly.

Reproduced independently of pjdfstest entirely (`sudo -u \#65534 touch
...`), which ruled out a pjdfstest/`setfsuid` quirk and confirmed it
was real. The actual cause was two compounding, unrelated test-
environment gaps, neither one icbfs's own code:

1. **`/home/ubuntu` is mode `0750`** (Ubuntu's own default) — any uid
   outside the `ubuntu` group gets `EACCES` just *traversing into* a
   mount sitting under it, before the request ever reaches FUSE at
   all.
2. **The mount had no `allow_other` option.** This is a hard,
   unconditional FUSE kernel restriction (`fuse_allow_current_process`),
   completely independent of `default_permissions` or any file's own
   mode: without it, every caller except the mounting user (and root)
   is rejected outright, regardless of what the permission bits say.

Confirmed by fixing both at once (traversable mount path, `allow_other`
added) and re-running: `sudo -u #65534 touch` on the same directory
then succeeded. Re-running the *full* pjdfstest suite with both fixes
dropped `open/06.t` from 78 failing subtests to 47, and the total
failing-assertion count across the whole suite from 5,050 to 3,721 —
real improvement across many other files that also use `-u`/`-g`, not
just that one.

**Fixed, as an opt-in flag, not a changed default**: added `-allow-other`
to `cmd/icbfs mount` (off by default, same opt-in pattern as
`--locking`/`--notify`) — multi-user access to a FUSE mount has real
security implications, so this project doesn't silently turn it on;
the qualification harness (`run-pjdfstest.sh`) now passes it
explicitly and mounts under `/mnt` instead of `/home/ubuntu/mnt`.

One narrower, genuinely still-open finding remains in the same area:
`open/06.t` still has 47 failing subtests (a different range than
before the fix — subtests 65–111, not the earlier 3–36 range), not yet
root-caused.

## What's left failing, and why

A complete run is `Files=238, Tests=8792`; 168 files now pass
completely clean. Of the 70 that don't:

- **`mkfifo`/`mknod` (13 files)** — these syscalls only make sense for
  device/special files, which `ARCHITECTURE.md` explicitly puts out of
  scope. Not a gap.
- **Most other categories' remaining failures (~55 files: `rename`,
  `open`, `link`, `chmod`, `chown`, `mkdir`, `rmdir`, `unlink`,
  `symlink`, `utimensat`)** — these test files loop over
  `regular/dir/fifo/block/char/socket/symlink`; spot-checking several
  directly (`rename/09.t`, `chown/00.t`, `link/00.t`, `unlink/11.t`)
  confirmed only the 4 unsupported device types fail in each. Not an
  exhaustive per-assertion audit, but a confirmed pattern.
- **`open/06.t`** — the one genuinely open finding above.

## Environment specifics

- VM: `icbfs-fuse-test`, Ubuntu 24.04, user `ubuntu` (SSH key auth,
  passwordless), `sudo` for root.
- Storage is a throwaway MinIO that `run-pjdfstest.sh` starts on the
  host for the run, through `test/minio/with-minio.sh`. It listens on a
  random port at the host's libvirt bridge address
  (`192.168.122.1:<port>`) and has a fresh versioned bucket. The
  script removes it, data included, when it exits however it exits;
  nothing stays running between runs. See `TESTING.md`'s "Object
  storage for the VM runs".
- A `clean-base` snapshot exists (`virsh snapshot-create-as
  icbfs-fuse-test clean-base`) from after cloud-init finished but
  *before* pjdfstest/fuse-xfstests were cloned/built — reverting to it
  means re-cloning/re-building those, not just re-mounting.

## Not yet done: `fuse-xfstests`/`fsx`

[rfjakob/fuse-xfstests](https://github.com/rfjakob/fuse-xfstests) is
hard-wired specifically for gocryptfs/loopback's own mount conventions
(confirmed by reading its `check-gocryptfs` wrapper) — it's not a
generic FUSE plug-in point, so integrating icbfs properly needs a
dedicated `check-icbfs` wrapper + hooks into `common/rc`, scoped as
separate future work rather than forced through as a shortcut here.
Its bundled `fsx` fuzzer builds standalone (`cd ltp && make fsx`)
without needing the full tree's `make`, which fails on an unrelated
tool (`fsstress`, removed XFS ioctl constants on modern Ubuntu) —
`fsx` itself was never run against icbfs in this pass.
