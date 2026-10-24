#!/bin/bash
# The Linux copy tests on the Linux test VM, as root, against a throwaway
# MinIO (or ICBFS_TEST_STORE): cp, rsync and tar copy the test tree onto
# tmpfs and onto a fresh icbfs mount, and the copies are compared. A few
# minutes. See README.md.
#
#   test/metadata/run-linux.sh <vm-ip>
#
# Results land in test/metadata/work/linux-<time>/: per tool the
# differences from tmpfs (<tool>.diff.txt), what the copy kept of the
# description (<tool>.check.txt) and the tool's output.
set -euo pipefail
META="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$META/../linux/lib.sh" # sets its own HERE
if store_needs_minio; then
  exec "$META/../minio/with-minio.sh" "$META/run-linux.sh" "$@"
fi
store_setup

IP="${1:?usage: run-linux.sh <vm-ip>}"
FS_NAME="metadata-$(date +%Y%m%d%H%M%S)"
MNT=/mnt/icbfs-metadata
RESULTS="$META/work/linux-$(date +%Y%m%d%H%M%S)"
mkdir -p "$RESULTS"

deploy_icbfs "$IP"
ssh_vm "$IP" "cd $VM_SRC && go build -o metatree ./test/metadata/metatree"
trap 'ssh_vm "$IP" "sudo umount -l $MNT 2>/dev/null; sudo pkill -x icbfs; rm -f /tmp/icbfs-mount-*.env; true" || true' EXIT
create_fs "$FS_NAME"
mount_icbfs "$IP" "$MNT" "$FS_NAME"

set +e
ssh_vm "$IP" "sudo bash $VM_SRC/test/metadata/copy-tests-linux.sh $VM_SRC/metatree $VM_SRC/test/metadata/tree.json $MNT /tmp/metadata-results"
rc=$?
set -e
ssh_vm "$IP" "sudo tar -C /tmp/metadata-results -cf - . && sudo rm -rf /tmp/metadata-results" | tar -C "$RESULTS" -xf -
echo "results: $RESULTS" >&2
exit $rc
