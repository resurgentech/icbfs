#!/bin/bash
# Shared config/helpers for the scripts in this directory. Not run directly.
set -uo pipefail

VM_NAME="icbfs-winfsp-test"
VM_PASSWORD="IcbfsP0c!Test2026"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORK_DIR="$HERE/work"

ssh_vm() {
  local ip="$1"; shift
  sshpass -p "$VM_PASSWORD" ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    -o ConnectTimeout=15 "Administrator@${ip}" "$@"
}

vm_ip() {
  virsh domifaddr "$VM_NAME" 2>/dev/null | awk '/ipv4/{print $4}' | cut -d/ -f1
}
