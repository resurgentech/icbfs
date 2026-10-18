#!/bin/bash
# Connectathon's NFS test suite (cthon04: basic, general, special and lock
# tests) against an icbfs FUSE mount on the test VM. Takes the VM's IP (from
# wait-for-ssh.sh) as $1.
#
#   test/linux/run-cthon.sh <vm-ip>
#
# CTHON_TESTS picks the groups (-a, the default, is all; -b basic, -g
# general, -s special, -l lock), CTHON_PASSES how many times to run them
# (1), and CTHON_MOUNT_ARGS adds icbfs mount flags ("-meta-cache 0"). The first run clones cthon04 (pinned to CTHON_REV) on the VM and
# builds it. Storage is a throwaway MinIO; ICBFS_TEST_STORE=s3 or azure runs
# on the real bucket or container instead (test/store.sh). The log lands in
# work/cthon-<time>.log; the run fails unless every group completes.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$HERE/lib.sh"
if store_needs_minio; then
  exec "$HERE/../minio/with-minio.sh" "$HERE/run-cthon.sh" "$@"
fi
store_setup

IP="${1:?usage: run-cthon.sh <vm-ip>}"
CTHON_REV=86a4501
CTHON_DIR=/home/ubuntu/cthon04
TESTS="${CTHON_TESTS:--a}"
PASSES="${CTHON_PASSES:-1}"
FS_NAME="cthon-$(date +%Y%m%d%H%M%S)"
MNT=/mnt/icbfs-cthon
LOG="$WORK_DIR/cthon-$(date +%Y%m%d%H%M%S).log"

echo "Building cthon04 ($CTHON_REV) on the VM if needed..." >&2
ssh_vm "$IP" "set -e
  if [ ! -x $CTHON_DIR/basic/test1 ] || [ \"\$(git -C $CTHON_DIR rev-parse --short=7 HEAD 2>/dev/null)\" != $CTHON_REV ]; then
    command -v pkg-config >/dev/null && pkg-config --exists libtirpc || sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq build-essential pkg-config libtirpc-dev >/dev/null
    [ -d $CTHON_DIR/.git ] || git clone -q git://git.linux-nfs.org/projects/steved/cthon04.git $CTHON_DIR
    git -C $CTHON_DIR checkout -q $CTHON_REV
    make -s -C $CTHON_DIR >/dev/null
  fi"

deploy_icbfs "$IP"
echo "Mounting ${FS_NAME} at ${MNT}..." >&2
trap 'ssh_vm "$IP" "sudo umount $MNT 2>/dev/null; sudo pkill -x icbfs; rm -f /tmp/icbfs-mount-*.env; true" || true' EXIT
create_fs "$FS_NAME"
# shellcheck disable=SC2086 # CTHON_MOUNT_ARGS is words
mount_icbfs "$IP" "$MNT" "$FS_NAME" ${CTHON_MOUNT_ARGS:-}

echo "Running cthon04 $TESTS, $PASSES pass(es) (log: $LOG)..." >&2
set +e
ssh_vm "$IP" "sudo bash -c 'cd $CTHON_DIR && ./runtests -N $PASSES $TESTS -t $MNT/cthon'" > "$LOG" 2>&1
status=$?
set -e
grep -E "^(Starting|All tests completed|.*(failed|FAILED|error))" "$LOG" | tail -20 >&2
if [ "$status" -ne 0 ] || ! grep -q "All tests completed" "$LOG"; then
  echo "cthon04: FAILED (exit $status; see $LOG)" >&2
  exit 1
fi
echo "cthon04: all tests completed" >&2
