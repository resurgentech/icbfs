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
