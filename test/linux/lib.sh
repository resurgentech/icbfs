#!/bin/bash
# Shared config/helpers for the scripts in this directory. Not run
# directly. Mirrors test/windows/lib.sh's own shape.
set -uo pipefail

VM_NAME="icbfs-fuse-test"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORK_DIR="$HERE/work"

ssh_vm() {
  local ip="$1"; shift
  ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    -o ConnectTimeout=15 "ubuntu@${ip}" "$@"
}

scp_to_vm() {
  local ip="$1"; local src="$2"; local dst="$3"
  scp -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null "$src" "ubuntu@${ip}:${dst}"
}

scp_from_vm() {
  local ip="$1"; local src="$2"; local dst="$3"
  scp -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null "ubuntu@${ip}:${src}" "$dst"
}

vm_ip() {
  virsh domifaddr "$VM_NAME" 2>/dev/null | awk '/ipv4/{print $4}' | cut -d/ -f1
}

# VM_SRC is where deploy_icbfs puts the working tree on the VM; the
# icbfs-cli built from it is $VM_SRC/icbfs-cli.
VM_SRC=/home/ubuntu/icbfs-test

# deploy_icbfs <ip> copies this working tree (tracked plus
# untracked-but-not-ignored files) to the VM fresh and builds icbfs-cli
# and icbfs-logd there. Always fresh: a stale checkout on the VM once built an icbfs-cli
# that rejected -allow-other, and pjdfstest went on to "pass" against the
# VM's own disk.
deploy_icbfs() {
  local ip="$1" repo tarball
  repo="$(cd "$HERE/../.." && pwd)"
  mkdir -p "$WORK_DIR"
  tarball="$WORK_DIR/icbfs-src.tar"
  echo "Deploying working tree to ${VM_SRC} and building icbfs-cli..." >&2
  (cd "$repo" && git ls-files -co --exclude-standard -z | tar --null -T - -cf "$tarball") || return 1
  scp_to_vm "$ip" "$tarball" /tmp/icbfs-src.tar >/dev/null || return 1
  ssh_vm "$ip" "rm -rf $VM_SRC && mkdir -p $VM_SRC && tar -xf /tmp/icbfs-src.tar -C $VM_SRC && cd $VM_SRC && go build -o icbfs-cli ./cmd/icbfs && go build -o icbfs-logd ./cmd/icbfs-logd"
}

# mount_icbfs <ip> <mountpoint> <fs name> [mount flags...] mounts that
# filesystem on the throwaway MinIO (test/minio's ICBFS_MINIO_*
# variables) in the background on the VM, as root with -allow-other, and
# waits until it really is a FUSE mount — never let a test run against
# whatever is under the mountpoint without it (the VM's own disk, which
# passes everything). On failure it prints the mount log and returns 1.
# Unmount with: ssh_vm <ip> "sudo umount -l <mountpoint>; sudo pkill -x icbfs-cli"
mount_icbfs() {
  local ip="$1" mnt="$2" fs="$3"
  shift 3
  ssh_vm "$ip" "sudo mkdir -p $mnt; sudo chmod 0755 /mnt; sudo $VM_SRC/icbfs-cli mount -endpoint $ICBFS_MINIO_VM_ENDPOINT -bucket $ICBFS_MINIO_BUCKET -fs $fs -access-key $ICBFS_MINIO_ACCESS_KEY -secret-key $ICBFS_MINIO_SECRET_KEY $* -uid 0 -gid 0 -allow-other $mnt > /tmp/icbfs-mount-${fs}.log 2>&1 &"
  if ! ssh_vm "$ip" "for i in \$(seq 1 30); do findmnt -n -o FSTYPE $mnt | grep -q fuse && exit 0; sleep 0.5; done; exit 1"; then
    echo "icbfs mount at $mnt never came up — mount log:" >&2
    ssh_vm "$ip" "cat /tmp/icbfs-mount-${fs}.log" >&2
    return 1
  fi
}
