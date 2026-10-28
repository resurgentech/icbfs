#!/bin/bash
# Shared config/helpers for the scripts in this directory. Not run
# directly. Mirrors test/windows/lib.sh's own shape.
set -uo pipefail

# ICBFS_TEST_VM picks another test VM (create-vm.sh makes it), so two runs
# can go at once on two VMs.
VM_NAME="${ICBFS_TEST_VM:-icbfs-fuse-test}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC1091
source "$HERE/../home.sh"
WORK_DIR="$ICBFS_TEST_HOME/test/linux/work"
# shellcheck disable=SC1091
source "$HERE/../store.sh"

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
# mount command built from it is $VM_SRC/icbfs, the CLI $VM_SRC/icbfs-cli.
VM_SRC=/home/ubuntu/icbfs-test

# create_fs <fs name> [icbfs-cli fs create flags...] creates a filesystem
# on the test store (test/store.sh) from this host (mounts never create one), its
# root owned by root (the tests run as root on the VM).
create_fs() {
  local fs="$1" repo
  shift
  repo="$(cd "$HERE/../.." && pwd)"
  mkdir -p "$WORK_DIR"
  if [ ! -x "$WORK_DIR/icbfs-cli" ] || [ -n "$(find "$repo/cmd" "$repo/internal" -newer "$WORK_DIR/icbfs-cli" -name '*.go' -print -quit)" ]; then
    (cd "$repo" && go build -o "$WORK_DIR/icbfs-cli" ./cmd/icbfs-cli) || return 1
  fi
  [ -n "${STORE_ARGS:-}" ] || store_setup || return 1
  # shellcheck disable=SC2086 # STORE_ARGS is words
  "$WORK_DIR/icbfs-cli" fs create $STORE_ARGS -fs "$fs" -root-uid 0 -root-gid 0 "$@" >/dev/null
}

# deploy_icbfs <ip> copies this working tree (tracked plus
# untracked-but-not-ignored files) to the VM fresh and builds the icbfs
# mount command and icbfs-cli there. Always fresh: a stale checkout on
# the VM once built a mount command that rejected -allow-other, and
# pjdfstest went on to "pass" against the VM's own disk.
deploy_icbfs() {
  local ip="$1" repo tarball
  repo="$(cd "$HERE/../.." && pwd)"
  mkdir -p "$WORK_DIR"
  tarball="$WORK_DIR/icbfs-src-$ip.tar" # one per VM: two runs can deploy at once
  echo "Deploying working tree to ${VM_SRC} and building icbfs and icbfs-cli..." >&2
  (cd "$repo" && git ls-files -co --exclude-standard -z | tar --null -T - -cf "$tarball") || return 1
  scp_to_vm "$ip" "$tarball" /tmp/icbfs-src.tar >/dev/null || return 1
  ssh_vm "$ip" "rm -rf $VM_SRC && mkdir -p $VM_SRC && tar -xf /tmp/icbfs-src.tar -C $VM_SRC && cd $VM_SRC && go build -o icbfs ./cmd/icbfs && go build -o icbfs-cli ./cmd/icbfs-cli"
}

# mount_icbfs <ip> <mountpoint> <fs name> [mount flags...] mounts that
# (existing — see create_fs) filesystem on the test store (test/store.sh)
# in the background on the VM, as root with -allow-other, and
# waits until it really is a FUSE mount — never let a test run against
# whatever is under the mountpoint without it (the VM's own disk, which
# passes everything). On failure it prints the mount log and returns 1.
# Unmount with: ssh_vm <ip> "sudo umount -l <mountpoint>; sudo pkill -x icbfs"
mount_icbfs() {
  local ip="$1" mnt="$2" fs="$3"
  shift 3
  [ -n "${STORE_VM_ARGS:-}" ] || store_setup || return 1
  # The keys reach the VM on ssh's stdin, into a file only its owner reads.
  printf '%s\n' "$STORE_ENV" | ssh_vm "$ip" "umask 077; cat > /tmp/icbfs-mount-${fs}.env"
  ssh_vm "$ip" "sudo mkdir -p $mnt; sudo chmod 0755 /mnt; sudo sh -c 'set -a; . /tmp/icbfs-mount-${fs}.env; set +a; exec $VM_SRC/icbfs mount $STORE_VM_ARGS -fs $fs $* -allow-other $mnt' < /dev/null > /tmp/icbfs-mount-${fs}.log 2>&1 &"
  if ! ssh_vm "$ip" "for i in \$(seq 1 30); do findmnt -n -o FSTYPE $mnt | grep -q fuse && exit 0; sleep 0.5; done; exit 1"; then
    echo "icbfs mount at $mnt never came up — mount log:" >&2
    ssh_vm "$ip" "cat /tmp/icbfs-mount-${fs}.log" >&2
    return 1
  fi
}
