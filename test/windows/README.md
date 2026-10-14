# Windows test VM

A disposable KVM/libvirt VM (`icbfs-winfsp-test`, Windows Server 2025
evaluation) for the suites that need a real WinFsp mount. Each run copies
the working tree to `C:\icbfs-test` on the VM, builds it there with the
VM's own Go, and uses a throwaway MinIO on the host
(`test/minio/with-minio.sh`) that is removed when the run ends. See
[docs/testing.md](../../docs/testing.md) for what each suite covers, and
for the latest results.

## Setup

Requirements on the host: libvirt and `virt-install`, `sshpass`, `xorriso`
(for the answer-file ISO), and Docker (for MinIO).

The only manual step is downloading the ISO. The Windows Server 2025
evaluation ISO sits behind a registration form at the [Evaluation
Center](https://www.microsoft.com/en-us/evalcenter/evaluate-windows-server-2025).

```
./create-vm.sh /path/to/windows-server-2025-eval.iso   # unattended install
ip=$(./wait-for-ssh.sh)                                # prints the IP once SSH is up
./check-eval-expiry.sh "$ip"                           # when the evaluation runs out
ssh Administrator@"$ip"                                # password in lib.sh
./destroy-vm.sh
```

- `autounattend.xml` installs Windows with an MBR layout (this libvirt
  profile boots SeaBIOS) on emulated SATA and e1000e, so no drivers are
  needed. It logs on automatically once, to enable OpenSSH.
- `wait-for-ssh.sh` restarts the VM, up to six times, if it stops before
  SSH is up. libvirtd has been seen to SIGTERM the VM during install, for
  reasons not found.
- `work/` (gitignored) holds the ISOs and each run's logs.

### What the VM needs installed

Install these once, then take a `clean-base` snapshot
(`virsh snapshot-create-as icbfs-winfsp-test clean-base`):

- **Go**, on the machine `Path`, for the native builds.
- **WinFsp**: `msiexec /i winfsp-<version>.msi /qn /norestart`, from
  [WinFsp's releases](https://github.com/winfsp/winfsp/releases/latest).
  Add `C:\Program Files (x86)\WinFsp\bin` to the machine `Path` yourself,
  because the installer doesn't, and restart `sshd` so SSH sessions see it.
- **winfsp-tests**: `winfsp-tests-<version>.zip` from the same release,
  unzipped to `C:\Windows\Temp\winfsp-tests\`.

### The evaluation clock

The evaluation edition expires after 180 days. `check-eval-expiry.sh` asks
Windows (`slmgr`) when it will.

- When expiry is near, `slmgr /rearm` and a reboot reset the clock once.
  Then re-take the snapshot.
- After that, `destroy-vm.sh` and `create-vm.sh` again. The install is
  unattended.

The current `clean-base` snapshot expires on 2027-04-08, with one rearm
left.

## Runs

```
./run-winfsp-tests.sh                    # winfsp-tests --external, case-insensitive mount (~5 min)
./run-winfsp-tests.sh icbfs-cs           # the same, -case-insensitive=false
./run-winfsp-tests.sh cmemfs-ci          # WinFsp's own memfs, as a baseline
./run-winfsp-tests.sh icbfs-cs -- -Only querydir_test,stream_create_test
./run-winfsp-tests.sh --no-deploy ...    # skip the copy and rebuild
./run-sync-test.sh [--no-deploy]         # icbfs-sync's Go tests on NTFS, then end to end
```

**winfsp-tests** runs every test in `winfsp-tests --list` through
`run-winfsp-tests.ps1`.

- Each test gets a fresh filesystem and mount, runs with its working
  directory on the mount, uses `--external --resilient --no-abort`, and
  has a 180-second timeout.
- The run prints the passing count, and any test that failed, timed out or
  couldn't mount. It exits non-zero if there were any.
- The result is saved in `work/`.

**The sync test** (`run-sync-test.ps1`) runs the real `icbfs-sync.exe`
against MinIO, and checks the results through a real mount. It covers
three pairs:

- an NTFS folder;
- an NTFS folder forced to behave as a WebDAV drive does (no IDs, no ACLs,
  whole seconds);
- a drive seeded with robocopy through a mount.

## Mounting by hand

`eval "$(../minio/up.sh)"` starts a MinIO and prints its address as the
VM sees it (`ICBFS_MINIO_VM_ENDPOINT`), its bucket and its keys.
`../minio/down.sh` removes it.

- A drive letter mounted in one SSH session is visible only in that logon
  session. Mount, use, and unmount within one `ssh` command:
  `Start-Process icbfs-winfsp.exe -ArgumentList ...`, then the commands,
  then `Stop-Process`.
- Go's boolean flags need `-flag=value` in an `-ArgumentList`. A separate
  `"false"` is read as the mount point.
