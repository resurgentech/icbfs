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
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

IP="${1:?usage: run-pjdfstest.sh <vm-ip> [minio-endpoint]}"
ENDPOINT="${2:-http://192.168.122.1:9000}"
FS_NAME="pjdfstest-$(date +%Y%m%d%H%M%S)"
MNT=/mnt/icbfs-test

echo "Building icbfs-cli on the VM..." >&2
ssh_vm "$IP" "cd ~/icbfs && go build -o icbfs-cli ./cmd/icbfs"

echo "Mounting ${FS_NAME} at ${MNT}..." >&2
ssh_vm "$IP" "sudo mkdir -p $MNT; sudo chmod 0755 /mnt; sudo /home/ubuntu/icbfs/icbfs-cli mount -endpoint $ENDPOINT -bucket icbfs-winfsp-manual -fs $FS_NAME -access-key minioadmin -secret-key minioadmin -uid 0 -gid 0 -allow-other $MNT > /tmp/icbfs-mount-${FS_NAME}.log 2>&1 &
sleep 3"

echo "Running pjdfstest (needs root; this can take several minutes against real network storage)..." >&2
set +e
ssh_vm "$IP" "sudo bash -c 'cd $MNT && prove -r ~/pjdfstest/tests'" > "$WORK_DIR/pjdfstest-${FS_NAME}.log" 2>&1
RET=$?
set -e

echo "Unmounting..." >&2
ssh_vm "$IP" "sudo umount -l $MNT 2>/dev/null || true"

echo "" >&2
echo "=== pjdfstest summary (full log: $WORK_DIR/pjdfstest-${FS_NAME}.log) ===" >&2
tail -30 "$WORK_DIR/pjdfstest-${FS_NAME}.log"
echo "exit code: $RET" >&2
exit $RET
