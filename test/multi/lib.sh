#!/bin/bash
# Shared config and helpers for the multi-machine tests. Not run directly.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"
# shellcheck disable=SC1091
source "$HERE/../home.sh"
WORK_DIR="$ICBFS_TEST_HOME/test/multi/work"
LINUX_WORK="$ICBFS_TEST_HOME/test/linux/work"
VM_PREFIX="icbfs-multi-"
MAIN_VM="icbfs-fuse-test" # test/linux's VM, the first machine

ssh_vm() {
  local ip="$1"; shift
  ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR \
    -o ConnectTimeout=15 "ubuntu@${ip}" "$@"
}

scp_to_vm() {
  scp -q -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR "$2" "ubuntu@$1:$3"
}

vm_ip() {
  virsh domifaddr "$1" 2>/dev/null | awk '/ipv4/{print $4}' | cut -d/ -f1
}

# machines prints the IPs of the machines the tests use: test/linux's VM,
# then every running icbfs-multi-N; with ICBFS_MULTI_ONLY set, only the
# icbfs-multi-N, so test/linux's VM is free for another run meanwhile.
machines() {
  [ -n "${ICBFS_MULTI_ONLY:-}" ] || vm_ip "$MAIN_VM"
  virsh list --name | grep "^${VM_PREFIX}" | sort -V | while read -r name; do
    vm_ip "$name"
  done
}
