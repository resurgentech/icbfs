#!/bin/bash
# Removes the multi-machine test VMs (icbfs-multi-N): domains and disks.
# test/linux's VM and the cloud image are untouched.
set -uo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

virsh list --all --name | grep "^${VM_PREFIX}" | while read -r name; do
  virsh destroy "$name" 2>/dev/null || true
  virsh undefine "$name" 2>/dev/null || true
  sudo rm -f "/var/lib/libvirt/images/${name}.qcow2"
  echo "$name removed." >&2
done
