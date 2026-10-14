#!/bin/bash
# Shared config/helpers for the scripts in this directory. Not run directly.
set -uo pipefail

VM_NAME="icbfs-winfsp-test"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORK_DIR="$HERE/work"
# The key the VM lets in as Administrator (written in by autounattend.xml),
# and the Administrator password build-answer-iso.sh generates (gitignored).
SSH_PUBLIC_KEY="${ICBFS_VM_SSH_KEY:-$HOME/.ssh/id_ed25519.pub}"
PASSWORD_FILE="$WORK_DIR/vm-password"

SSH_OPTS=(-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o BatchMode=yes -o ConnectTimeout=15)

ssh_vm() {
  local ip="$1"; shift
  ssh "${SSH_OPTS[@]}" "Administrator@${ip}" "$@"
}

scp_vm() {
  scp -q "${SSH_OPTS[@]}" "$@"
}

vm_ip() {
  virsh domifaddr "$VM_NAME" 2>/dev/null | awk '/ipv4/{print $4}' | cut -d/ -f1
}
