#!/bin/bash
# Fully destroys the VM: domain, snapshot metadata, disk, and staged
# install media. Use when the eval clock has expired (see
# check-eval-expiry.sh) or the VM is otherwise unrecoverable - recreating
# from scratch via create-vm.sh is fully unattended, so this isn't a big
# deal when it's actually needed.
set -uo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

virsh destroy "$VM_NAME" 2>/dev/null || true
virsh undefine "$VM_NAME" --snapshots-metadata 2>/dev/null || true
sudo rm -f "/var/lib/libvirt/images/${VM_NAME}.qcow2" \
           "/var/lib/libvirt/images/${VM_NAME}-install.iso" \
           "/var/lib/libvirt/images/${VM_NAME}-autounattend.iso"
echo "VM and staged media removed."
