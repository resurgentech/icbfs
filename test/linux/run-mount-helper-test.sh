#!/bin/bash
# "mount -t fuse.icbfs" on the Linux VM, through the mount helper
# (/sbin/mount.fuse.icbfs, a link to the icbfs binary): mount by name with
# the store connection from /etc/icbfs.env, see the name as the mount's
# source, write, unmount, and mount read-only.
#
#   test/linux/run-mount-helper-test.sh <vm-ip>
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [ -z "${ICBFS_MINIO_VM_ENDPOINT:-}" ]; then
  exec "$HERE/../minio/with-minio.sh" "$HERE/run-mount-helper-test.sh" "$@"
fi
source "$HERE/lib.sh"

IP="${1:?usage: run-mount-helper-test.sh <vm-ip>}"
FS="helper-$(date +%Y%m%d%H%M%S)"
MNT=/mnt/icbfs-helper

deploy_icbfs "$IP"
create_fs "$FS"

fail=0
check() { if eval "$2"; then echo "PASS  $1"; else echo "FAIL  $1"; fail=1; fi; }
vm() { ssh_vm "$IP" "$@" 2>/dev/null; }

cleanup() { vm "sudo umount $MNT 2>/dev/null; sudo rm -f /sbin/mount.fuse.icbfs /etc/icbfs.env" || true; }
trap cleanup EXIT

vm "sudo ln -sf $VM_SRC/icbfs /sbin/mount.fuse.icbfs && sudo mkdir -p $MNT && printf 'ICBFS_ENDPOINT=%s\nICBFS_BUCKET=%s\nICBFS_ACCESS_KEY=%s\nICBFS_SECRET_KEY=%s\n' $ICBFS_MINIO_VM_ENDPOINT $ICBFS_MINIO_BUCKET $ICBFS_MINIO_ACCESS_KEY $ICBFS_MINIO_SECRET_KEY | sudo tee /etc/icbfs.env >/dev/null && sudo chmod 600 /etc/icbfs.env"

check "mount -t fuse.icbfs mounts by name" "vm 'sudo mount -t fuse.icbfs $FS $MNT'"
check "the mount's source is the filesystem's name" "vm \"findmnt -n -o SOURCE,FSTYPE $MNT\" | grep -q \"^$FS *fuse.icbfs\""
check "it's writable" "vm 'echo hello | sudo tee $MNT/a.txt >/dev/null && sudo cat $MNT/a.txt' | grep -q hello"
check "umount unmounts it, and icbfs exits" "vm 'sudo umount $MNT && sleep 1 && ! findmnt $MNT >/dev/null && ! pgrep -f \"[i]cbfs mount -fs $FS\" >/dev/null'"
check "mount -o ro mounts read-only" "vm 'sudo mount -t fuse.icbfs -o ro $FS $MNT && sudo cat $MNT/a.txt' | grep -q hello"
check "and refuses writes there" "! vm 'echo x | sudo tee $MNT/b.txt' >/dev/null 2>&1"
check "mount -o remount,rw makes it writable" "vm 'sudo mount -o remount,rw $MNT && echo x | sudo tee $MNT/b.txt >/dev/null'"
check "mount -o remount,ro makes it read-only again" "vm 'sudo mount -o remount,ro $MNT' && ! vm 'echo y | sudo tee $MNT/c.txt' >/dev/null 2>&1"
vm "sudo umount $MNT" || true
check "an unknown filesystem fails to mount, with the reason" "vm 'sudo mount -t fuse.icbfs no-such-fs $MNT 2>&1; true' | grep -q 'no-such-fs'"
check "an unknown option is refused" "! vm 'sudo mount -t fuse.icbfs -o frobnicate $FS $MNT' >/dev/null 2>&1"
exit $fail
