# Windows test VMs

Disposable KVM/libvirt VMs for the suites that need a real WinFsp mount:

- `icbfs-winfsp-test`: Windows Server 2025 evaluation, with no desktop
  (Server Core), the default.
- `icbfs-win11-test`: Windows 11 Enterprise LTSC 2024 evaluation, with a
  desktop and Explorer, for anything with a window. Every script here
  takes it with `ICBFS_WIN_VM=icbfs-win11-test`.

Each run copies the working tree to `C:\icbfs-test` on the VM, builds it there with the
VM's own Go, and uses a throwaway MinIO on the host
(`test/minio/with-minio.sh`) that is removed when the run ends. See
[docs/testing.md](../../docs/testing.md) for what each suite covers, and
for the latest results.

## Setup

Requirements on the host: libvirt and `virt-install`, `xorriso` (for the
answer-file ISO), Docker (for MinIO), and an SSH key at
`~/.ssh/id_ed25519.pub` (or `$ICBFS_VM_SSH_KEY`).

The only manual step is downloading the ISO.

- **Windows Server 2025:** behind a registration form at the [Evaluation
  Center](https://www.microsoft.com/en-us/evalcenter/evaluate-windows-server-2025).
- **Windows 11 Enterprise LTSC 2024:** a direct link, with no form
  (5.1 GB):
  `curl -L -o win11-ltsc-2024-eval-en-us.iso 'https://go.microsoft.com/fwlink/?linkid=2289029&clcid=0x409&culture=en-us&country=us'`.

`create-vm.sh` tells the two apart by the ISO's name (`CLIENT`, as in
Microsoft's own file names, means Windows 11).

```
./create-vm.sh /path/to/windows-eval.iso   # unattended install
ip=$(./wait-for-ssh.sh)                    # prints the IP once SSH is up
./setup-vm.sh "$ip"                        # Go, WinFsp, winfsp-tests
./check-eval-expiry.sh "$ip"               # when the evaluation runs out
ssh Administrator@"$ip"                    # logs in with your SSH key
./destroy-vm.sh
```

For the Windows 11 VM, put `ICBFS_WIN_VM=icbfs-win11-test` before each.

- `autounattend.xml` installs Windows with an MBR layout (this libvirt
  profile boots SeaBIOS) on emulated SATA and e1000e, so no drivers are
  needed. It logs on automatically once, to enable OpenSSH and install your
  SSH key for Administrator.
- `build-answer-iso.sh` fills your key and a generated Administrator
  password into a copy of `autounattend.xml` under `work/`. The password is
  kept in `work/vm-password` (gitignored); the scripts never use it.
- `wait-for-ssh.sh` restarts the VM, up to six times, if it stops before
  SSH is up. libvirtd has been seen to SIGTERM the VM during install, for
  reasons not found.
- `work/` (gitignored) holds the ISOs and each run's logs.

### What the VM needs installed

`setup-vm.sh` installs these (`setup-vm.ps1`; the versions are at the top
of `setup-vm.sh`). Then shut the VM down and take a `clean-base` snapshot
(`virsh snapshot-create-as <vm> clean-base`). With Administrator logged
on (auto-logon), `virsh shutdown` is ignored; shut it down from inside:
`ssh Administrator@<ip> Stop-Computer -Force`.

- **Go**, on the machine `Path`, for the native builds.
- **WinFsp**, from [WinFsp's
  releases](https://github.com/winfsp/winfsp/releases/latest), with
  `C:\Program Files (x86)\WinFsp\bin` added to the machine `Path`, which
  its installer doesn't do.
- **winfsp-tests**, from the same release, unzipped to
  `C:\Windows\Temp\winfsp-tests\`.
- **The .NET SDK** and **llvm-mingw** (a C compiler, for cgo), on the
  machine `Path`, for building native Windows programs on icbfs.
- **Defender exclusions** for the Go module cache, the build cache and
  `C:\icbfs-test`. Defender takes a generated file in `modernc.org/sqlite`
  (`sqlite_g_*.go`) for malware (`TrojanDropper:VBS/Mikey.A!MSR`) and
  blocks the build, so the script also allows that one signature. Go
  remembers a blocked read in its build cache: after a block, run
  `go clean -cache` as well.

### Seeing the Windows 11 desktop

A program started over SSH runs in a session with no desktop, and a GUI
one fails there. So `autounattend.xml` logs Administrator on at the
console at every boot. For a VM made before it did, run
`./enable-autologon.sh` once; the password goes over as a file, never on
a command line.

```
./run-in-desktop.sh 'C:\path\to\app.exe' [shot.png] [seconds]
```

starts a program on that desktop (with a scheduled task that runs only
while Administrator is logged on), waits, and saves a screenshot of the
console (`virsh screenshot`), by default to `work/desktop.png`. A fourth
argument is the program's arguments. A script that drives a window (by UI
Automation, say) runs this way too, through PowerShell with
`-WindowStyle Hidden`, or its console window takes the clicks.

The VM's clock is the host's local time read as UTC (libvirt's default
for Windows), hours off unless the runners have set it (`sync_vm_clock`
in `lib.sh`).

To use the desktop yourself, connect with Remmina (or any RDP client) to
the VM's address (`virsh domifaddr icbfs-win11-test`) as `Administrator`,
with the password in `work/vm-password`. Remote Desktop takes over the
console session: while you're connected, the console shows the lock
screen. `virt-manager` shows the console as well, but slowly.

Windows 11 Setup's TPM 2.0 and Secure Boot checks are skipped
(`LabConfig` in `autounattend.xml`), so this VM boots as the Server one
does: SeaBIOS, MBR, no TPM.

### The evaluation clock

`check-eval-expiry.sh` asks Windows (`slmgr`) when the evaluation runs
out: 180 days for Windows Server, 90 for Windows 11 Enterprise LTSC.

- When expiry is near, `slmgr /rearm` and a reboot reset the clock once.
  Then re-take the snapshot.
- After that, `destroy-vm.sh` and `create-vm.sh` again. The install is
  unattended.

## Runs

```
./run-winfsp-tests.sh                    # winfsp-tests --external, case-insensitive mount (~5 min)
./run-winfsp-tests.sh icbfs-cs           # the same, -case-insensitive=false
./run-winfsp-tests.sh cmemfs-ci          # WinFsp's own memfs, as a baseline
./run-winfsp-tests.sh icbfs-cs -- -Only querydir_test,stream_create_test
./run-winfsp-tests.sh --no-deploy ...    # skip the copy and rebuild
./run-sync-test.sh [--no-deploy]         # icbfs-sync's Go tests on NTFS, then end to end
./run-consistency.sh                     # two mounts as two machines (after a deploy)
./run-bench.sh [--no-deploy]             # timings of what Explorer and Office do
../metadata/run-windows.sh               # metadata copies (Windows 11 VM; see test/metadata)
```

Both runners first set the VM's clock to the host's: a snapshot saved
running brings back the clock it was saved with, and tests that compare
file times with the store's would fail.

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
