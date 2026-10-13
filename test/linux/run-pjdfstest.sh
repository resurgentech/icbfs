#!/bin/bash
# Runs pjdfstest against a real icbfs FUSE mount on the certification
# VM — the durable, re-runnable version of the manual steps used to
# find the real bugs recorded in ROADMAP.md/TESTING.md. Takes the VM's
# IP (from wait-for-ssh.sh) as $1.
#
# Mounts a *fresh* filesystem name each run (date-based) so a prior
# run's state, or a prior run's crash, never leaks into this one —
# exactly the gap that let the first real run's "No plan found in TAP
# output" cascade (the mount had already died from an earlier panic)
# look like a wall of unrelated failures instead of one root cause.
#
# Mounted under /mnt (not /home/ubuntu/mnt) with -allow-other: a real
# finding from running this suite's own -u/-g simulated-user tests
# (e.g. open/06.t) — /home/ubuntu is mode 0750 by default, which
# blocks any non-ubuntu-group uid from even traversing into a mount
# underneath it, and FUSE's own kernel-side allow_other restriction
# separately blocks every uid but the mount's own regardless of file
# permissions. Both looked at first like a real permission-enforcement
# bug in icbfs; neither is. See README.md's writeup.
#
# Storage is a throwaway MinIO (test/minio/with-minio.sh) that exists
# only for this run: started here, removed when the script exits however
# it exits. Nothing is left running between runs.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [ -z "${ICBFS_MINIO_VM_ENDPOINT:-}" ]; then
  exec "$HERE/../minio/with-minio.sh" "$HERE/run-pjdfstest.sh" "$@"
fi
source "$HERE/lib.sh"

IP="${1:?usage: run-pjdfstest.sh <vm-ip>}"
ENDPOINT="$ICBFS_MINIO_VM_ENDPOINT"
BUCKET="$ICBFS_MINIO_BUCKET"
FS_NAME="pjdfstest-$(date +%Y%m%d%H%M%S)"
MNT=/mnt/icbfs-test

REPO="$(cd "$HERE/../.." && pwd)"
SRC=/home/ubuntu/icbfs-test
mkdir -p "$WORK_DIR"

# Always test this working tree (tracked plus untracked-but-not-ignored
# files), freshly copied: a stale checkout on the VM once built an
# icbfs-cli that rejected -allow-other, and the run went on to "pass"
# against the VM's own disk.
echo "Deploying working tree to ${SRC} and building icbfs-cli..." >&2
tarball="$WORK_DIR/icbfs-src.tar"
(cd "$REPO" && git ls-files -co --exclude-standard -z | tar --null -T - -cf "$tarball")
scp_to_vm "$IP" "$tarball" /tmp/icbfs-src.tar >/dev/null
ssh_vm "$IP" "rm -rf $SRC && mkdir -p $SRC && tar -xf /tmp/icbfs-src.tar -C $SRC && cd $SRC && go build -o icbfs-cli ./cmd/icbfs"

echo "Mounting ${FS_NAME} at ${MNT}..." >&2
# Unmount before the MinIO underneath goes away, even on failure.
trap 'ssh_vm "$IP" "sudo umount -l $MNT 2>/dev/null; sudo pkill -x icbfs-cli; true" || true' EXIT
ssh_vm "$IP" "sudo mkdir -p $MNT; sudo chmod 0755 /mnt; sudo $SRC/icbfs-cli mount -endpoint $ENDPOINT -bucket $BUCKET -fs $FS_NAME -access-key $ICBFS_MINIO_ACCESS_KEY -secret-key $ICBFS_MINIO_SECRET_KEY -uid 0 -gid 0 -allow-other $MNT > /tmp/icbfs-mount-${FS_NAME}.log 2>&1 &"
# Never run the suite against whatever is under $MNT without the mount:
# that's the VM's own disk, and it passes everything.
if ! ssh_vm "$IP" "for i in \$(seq 1 30); do findmnt -n -o FSTYPE $MNT | grep -q fuse && exit 0; sleep 0.5; done; exit 1"; then
  echo "icbfs mount at $MNT never came up — mount log:" >&2
  ssh_vm "$IP" "cat /tmp/icbfs-mount-${FS_NAME}.log" >&2
  exit 1
fi

echo "Running pjdfstest (needs root; this can take several minutes against real network storage)..." >&2
set +e
ssh_vm "$IP" "sudo bash -c 'cd $MNT && prove -r /home/ubuntu/pjdfstest/tests'" > "$WORK_DIR/pjdfstest-${FS_NAME}.log" 2>&1
set -e

echo "" >&2
echo "=== pjdfstest summary (full log: $WORK_DIR/pjdfstest-${FS_NAME}.log) ===" >&2
sed -n '/^Test Summary Report/,$p' "$WORK_DIR/pjdfstest-${FS_NAME}.log"
echo "" >&2
# Pass/fail is judged against the accepted differences in
# pjdfstest-known.txt, not prove's own exit code.
"$HERE/pjdfstest-check.sh" "$WORK_DIR/pjdfstest-${FS_NAME}.log"
