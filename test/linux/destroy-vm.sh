#!/bin/bash
# Fully destroys the VM: domain, snapshot metadata, and disk. The base
# cloud image itself (work/*.img) is untouched - the VM's own disk is
# a separate qcow2 backed by it, so destroying the VM doesn't require
# re-downloading anything to recreate it. Mirrors
# test/windows/destroy-vm.sh.
set -uo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

virsh destroy "$VM_NAME" 2>/dev/null || true
virsh undefine "$VM_NAME" --snapshots-metadata 2>/dev/null || true
sudo rm -f "/var/lib/libvirt/images/${VM_NAME}.qcow2"
echo "VM removed."
