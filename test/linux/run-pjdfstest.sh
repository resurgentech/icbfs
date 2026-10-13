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
FS_NAME="pjdfstest-$(date +%Y%m%d%H%M%S)"
# Test filesystems hold files up to 8 GiB, not the 1 TiB default:
# testable, and big enough to need multipart uploads.
MAX_FILE_SIZE=$((8 << 30))
MNT=/mnt/icbfs-test

deploy_icbfs "$IP"

echo "Mounting ${FS_NAME} at ${MNT}..." >&2
# Unmount before the MinIO underneath goes away, even on failure.
trap 'ssh_vm "$IP" "sudo umount -l $MNT 2>/dev/null; sudo pkill -x icbfs-cli; true" || true' EXIT
mount_icbfs "$IP" "$MNT" "$FS_NAME" -max-file-size "$MAX_FILE_SIZE"

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
