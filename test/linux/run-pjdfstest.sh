#!/bin/bash
# Runs pjdfstest against a real icbfs FUSE mount on the test VM. Takes
# the VM's IP (from wait-for-ssh.sh) as $1.
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
# it exits. Nothing is left running between runs. ICBFS_TEST_STORE=s3 or
# azure runs on the real bucket or container instead (test/store.sh).
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$HERE/lib.sh"
if store_needs_minio; then
  exec "$HERE/../minio/with-minio.sh" "$HERE/run-pjdfstest.sh" "$@"
fi
store_setup

IP="${1:?usage: run-pjdfstest.sh <vm-ip>}"
FS_NAME="pjdfstest-$(date +%Y%m%d%H%M%S)"
# Test filesystems hold files up to 8 GiB, not the 1 TiB default:
# testable, and big enough to need multipart uploads.
MAX_FILE_SIZE=$((8 << 30))
MNT=/mnt/icbfs-test

# pjdfstest itself, built on the VM the first time (pinned).
PJDFSTEST_REV=85a8aea
echo "Building pjdfstest ($PJDFSTEST_REV) on the VM if needed..." >&2
ssh_vm "$IP" "set -e
  if [ ! -x /home/ubuntu/pjdfstest/pjdfstest ]; then
    command -v autoreconf >/dev/null || sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq autoconf automake build-essential >/dev/null
    [ -d /home/ubuntu/pjdfstest/.git ] || git clone -q https://github.com/pjd/pjdfstest.git /home/ubuntu/pjdfstest
    cd /home/ubuntu/pjdfstest && git checkout -q $PJDFSTEST_REV && autoreconf -ifs >/dev/null 2>&1 && ./configure >/dev/null && make -s pjdfstest >/dev/null
  fi"

deploy_icbfs "$IP"

echo "Mounting ${FS_NAME} at ${MNT}..." >&2
# Unmount before the MinIO underneath goes away, even on failure.
trap 'ssh_vm "$IP" "sudo umount -l $MNT 2>/dev/null; sudo pkill -x icbfs; rm -f /tmp/icbfs-mount-*.env; true" || true' EXIT
create_fs "$FS_NAME" -max-file-size "$MAX_FILE_SIZE"
mount_icbfs "$IP" "$MNT" "$FS_NAME"

echo "Running pjdfstest (needs root; this can take several minutes against real network storage)..." >&2
set +e
# $PJDFSTEST_TESTS narrows the run (e.g. "link/00.t chmod"), repeated
# $PJDFSTEST_REPEAT times (default once), to chase a flaky test.
tests=/home/ubuntu/pjdfstest/tests
if [ -n "${PJDFSTEST_TESTS:-}" ]; then
  tests=$(for t in $PJDFSTEST_TESTS; do printf '/home/ubuntu/pjdfstest/tests/%s ' "$t"; done)
fi
for _ in $(seq "${PJDFSTEST_REPEAT:-1}"); do
  ssh_vm "$IP" "sudo bash -c 'cd $MNT && prove -r $tests'"
done > "$WORK_DIR/pjdfstest-${FS_NAME}.log" 2>&1
set -e

echo "" >&2
echo "=== pjdfstest summary (full log: $WORK_DIR/pjdfstest-${FS_NAME}.log) ===" >&2
sed -n '/^Test Summary Report/,$p' "$WORK_DIR/pjdfstest-${FS_NAME}.log"
echo "" >&2
# Pass/fail is judged against the accepted differences in
# pjdfstest-known.txt, not prove's own exit code.
"$HERE/pjdfstest-check.sh" "$WORK_DIR/pjdfstest-${FS_NAME}.log"
