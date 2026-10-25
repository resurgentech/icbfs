# Linux test VM

A disposable KVM/libvirt VM (`icbfs-fuse-test`, Ubuntu 24.04) for the test
suites that need a real FUSE mount and root. Each run deploys the working
tree to the VM, builds it there, and uses a throwaway MinIO on the host
(`test/minio/with-minio.sh`) that is removed when the run ends. See
[docs/testing.md](../../docs/testing.md) for what each suite covers, and
for the latest results.

## Setup

Requirements on the host: libvirt and `virt-install`, Docker (for MinIO),
and an SSH key at `~/.ssh/id_ed25519.pub`.

```
./create-vm.sh         # downloads the cloud image once; unattended install via cloud-init
./wait-for-ssh.sh      # prints the VM's IP once SSH is up
./destroy-vm.sh        # removes the VM, keeping the downloaded image
```

- `create-vm.sh` stages the image into `/var/lib/libvirt/images`, because
  qemu can't read from `$HOME`. It writes the SSH key into a generated
  copy of `cloud-init.yaml`.
- Log in as `ubuntu`, with key auth and passwordless `sudo`.
- `work/` (gitignored) holds downloads and each run's logs.

## Runs

```
./run-pjdfstest.sh <ip>          # pjdfstest against a fresh mount, as root (~7 min)
./run-xfstests.sh <ip> [args]    # xfstests, -g quick by default (most of a day)
./run-cthon.sh <ip>              # Connectathon: basic, general, special and lock tests
./run-mount-helper-test.sh <ip>  # mount -t fuse.icbfs through the helper
./run-logging-test.sh <ip>       # mount → icbfs-logd → icbfs-mockserver (~1 min)
./run-largefile-test.sh <ip>     # 6 GiB file; opt-in (~10 min, ~20 GB of traffic)
../metadata/run-linux.sh <ip>    # metadata copies (see test/metadata)
```

`ICBFS_TEST_STORE=s3` or `azure` runs pjdfstest or Connectathon on the
real bucket or container instead of MinIO (`test/store.sh`).

**pjdfstest** builds pjdfstest on the VM, mounts a fresh filesystem under
`/mnt` with `-allow-other`, and runs the whole suite with `prove -r`.
`pjdfstest-check.sh` then judges the log:

- assertions listed, with reasons, in `pjdfstest-known.txt` are accepted;
- anything else fails the run: an unlisted failure, a test file that
  crashed, or a run that didn't finish;
- listed assertions that have started passing are reported, so the list
  doesn't go stale.

Re-check a saved log with `./pjdfstest-check.sh work/<log>`.

**The logging test** runs `icbfs-mockserver` in a container on the host,
and `icbfs-logd` plus a mount on the VM.

- The mount gets its store credentials only from the server.
- A shell then makes a directory, and writes, reads, lists, appends to,
  renames and removes a file. The server's log must hold exactly the
  seven events those produce.
- It runs with synchronous logging, then with `-log-async`.
- It checks that a drive the server doesn't know can't mount, and that a
  read-only drive on a directory mounts just that directory.
- It runs `icbfs-cli` through the daemon, with no credentials.

**The large-file test** is not part of the regular runs. Run it after
changing the content write path (flush, multipart uploads, truncate, the
max file size). On an 8 GiB-limit filesystem it checks:

- a 6 GiB file reads back intact;
- a one-byte edit in the middle leaves every other byte alone;
- growing the file past 8 GiB fails with `EFBIG`.

It needs about 13 GB free on the VM.

## Notes

- The mount must be under a path every test uid can traverse.
  `/home/ubuntu` is `0750`, so pjdfstest mounts under `/mnt`.
- The mount also needs `-allow-other`. Without it, FUSE refuses every uid
  but the mounting one, whatever the modes say.
- A `clean-base` snapshot (`virsh snapshot-create-as icbfs-fuse-test
  clean-base`) taken right after cloud-init finishes is a quick way back
  to a fresh VM. After a revert, pjdfstest is rebuilt on the next run.
