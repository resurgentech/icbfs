#!/bin/bash
# xfstests (fstests) against icbfs's FUSE mount on the Linux VM, through
# the mount helper: FSTYP=fuse, FUSE_SUBTYP=.icbfs, with a fresh test and
# scratch filesystem on a throwaway MinIO.
#
#   test/linux/run-xfstests.sh <vm-ip> [check arguments]   (default: -g quick)
#
# The first run installs xfstests' build dependencies and builds xfstests
# (pinned to XFSTESTS_REV) on the VM. Results are copied to
# work/xfstests-<time>/; failures not listed in xfstests-known.txt fail the
# run.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [ -z "${ICBFS_MINIO_VM_ENDPOINT:-}" ]; then
  exec "$HERE/../minio/with-minio.sh" "$HERE/run-xfstests.sh" "$@"
fi
source "$HERE/lib.sh"

IP="${1:?usage: run-xfstests.sh <vm-ip> [check arguments]}"
shift
CHECK_ARGS=("$@")
[ ${#CHECK_ARGS[@]} -gt 0 ] || CHECK_ARGS=(-g quick)

XFSTESTS_REV=22348afe338c0f6d540c0d7ef0db749eaa51b218 # master, 2026-09-20
XFSTESTS=/home/ubuntu/xfstests-dev
STAMP=$(date +%Y%m%d%H%M%S)
TEST_FS="xfs-test-$STAMP"
SCRATCH_FS="xfs-scratch-$STAMP"
TEST_DIR=/mnt/xfs-test
SCRATCH_MNT=/mnt/xfs-scratch
OUT="$WORK_DIR/xfstests-$STAMP"
vm() { ssh_vm "$IP" "$@"; }

deploy_icbfs "$IP"
create_fs "$TEST_FS" -max-file-size $((8 << 30))
create_fs "$SCRATCH_FS" -max-file-size $((8 << 30))

echo "Preparing xfstests on the VM..." >&2
vm "set -e
if [ ! -f ~/.xfstests-deps ]; then
  sudo apt-get update -qq
  sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq acl attr automake bc dbench dump e2fsprogs fio gawk \
    gcc git indent libacl1-dev libaio-dev libcap-dev libgdbm-dev libtool libtool-bin liburing-dev libuuid1 make \
    psmisc python3 quota sed uuid-dev uuid-runtime xfsprogs xfslibs-dev sqlite3 libgdbm-compat-dev >/dev/null
  touch ~/.xfstests-deps
fi
if [ \"\$(git -C $XFSTESTS rev-parse HEAD 2>/dev/null)\" != $XFSTESTS_REV ] || [ ! -x $XFSTESTS/ltp/fsx ]; then
  rm -rf $XFSTESTS
  git clone -q https://git.kernel.org/pub/scm/fs/xfs/xfstests-dev.git $XFSTESTS
  git -C $XFSTESTS checkout -q $XFSTESTS_REV
  make -C $XFSTESTS -j\$(nproc) >/dev/null
fi
# xfstests compares a mount's type with FSTYP; map fuse.icbfs to fuse, as
# it already maps fuse.glusterfs and fuse.ceph-fuse.
grep -q fuse.icbfs $XFSTESTS/common/rc || sed -i \"s|-e 's/fuse.ceph-fuse/ceph-fuse/'|-e 's/fuse.ceph-fuse/ceph-fuse/' -e 's/fuse.icbfs/fuse/'|\" $XFSTESTS/common/rc
grep -q fuse.icbfs $XFSTESTS/common/rc
id fsgqa >/dev/null 2>&1 || sudo useradd -m fsgqa
id 123456-fsgqa >/dev/null 2>&1 || sudo useradd 123456-fsgqa
id fsgqa2 >/dev/null 2>&1 || sudo useradd fsgqa2
getent group fsgqa >/dev/null || sudo groupadd fsgqa
sudo ln -sf $VM_SRC/icbfs /sbin/mount.fuse.icbfs
printf 'ICBFS_ENDPOINT=%s\nICBFS_BUCKET=%s\nICBFS_ACCESS_KEY=%s\nICBFS_SECRET_KEY=%s\n' \
  $ICBFS_MINIO_VM_ENDPOINT $ICBFS_MINIO_BUCKET $ICBFS_MINIO_ACCESS_KEY $ICBFS_MINIO_SECRET_KEY | sudo tee /etc/icbfs.env >/dev/null
sudo chmod 600 /etc/icbfs.env
sudo mkdir -p $TEST_DIR $SCRATCH_MNT
cat > $XFSTESTS/local.config <<EOF
export FSTYP=fuse
export FUSE_SUBTYP=.icbfs
export TEST_DEV=$TEST_FS
export TEST_DIR=$TEST_DIR
export SCRATCH_DEV=$SCRATCH_FS
export SCRATCH_MNT=$SCRATCH_MNT
export TEST_FS_MOUNT_OPTS='-o allow_other'
export MOUNT_OPTIONS='-o allow_other'
EOF
sudo rm -rf $XFSTESTS/results"

cleanup() {
  vm "sudo umount $TEST_DIR $SCRATCH_MNT 2>/dev/null; sudo rm -f /etc/icbfs.env" >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "Running ./check ${CHECK_ARGS[*]} (this takes a while against an object store)..." >&2
status=0
vm "cd $XFSTESTS && sudo timeout 8h ./check ${CHECK_ARGS[*]}" >"$WORK_DIR/xfstests-$STAMP.log" 2>&1 || status=$?

mkdir -p "$OUT"
vm "sudo tar -C $XFSTESTS -czf /tmp/xfstests-results.tgz results" >/dev/null 2>&1 &&
  scp_from_vm "$IP" /tmp/xfstests-results.tgz "$OUT/results.tgz" >/dev/null 2>&1 &&
  tar -xzf "$OUT/results.tgz" -C "$OUT"
mv "$WORK_DIR/xfstests-$STAMP.log" "$OUT/check.log"

"$HERE/xfstests-check.sh" "$OUT/check.log" || status=1
echo "results: $OUT" >&2
exit $status
