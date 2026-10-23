#!/bin/bash
# Stages install media into libvirt's images pool (qemu needs search
# permission on the whole path, which $HOME doesn't grant - see README.md)
# and creates the VM. Pass the path to a Windows evaluation ISO (see
# README.md for where to get one): Windows Server 2025 for the default
# VM, or Windows 11 Enterprise LTSC 2024 with ICBFS_WIN_VM=icbfs-win11-test.
# The VM uses emulated SATA/e1000e, so it boots with no extra drivers.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

INSTALL_ISO="${1:?usage: create-vm.sh <path-to-windows-eval.iso>}"
# The libosinfo name virt-install tunes the VM for, from the ISO's name.
case "$(basename "$INSTALL_ISO")" in
*CLIENT*) OS_VARIANT=win11 ;;
*) OS_VARIANT=win2k25 ;;
esac

"$HERE/build-answer-iso.sh"

IMAGES=/var/lib/libvirt/images
sudo cp --reflink=auto "$INSTALL_ISO" "$IMAGES/${VM_NAME}-install.iso"
sudo cp --reflink=auto "$WORK_DIR/autounattend.iso" "$IMAGES/${VM_NAME}-autounattend.iso"
sudo chown libvirt-qemu:libvirt-qemu "$IMAGES/${VM_NAME}-install.iso" "$IMAGES/${VM_NAME}-autounattend.iso"
sudo chmod 644 "$IMAGES/${VM_NAME}-install.iso" "$IMAGES/${VM_NAME}-autounattend.iso"

# 80 GB: Windows 11 Setup refuses a disk under 64 GB.
virt-install \
  --name "$VM_NAME" \
  --memory 8192 --vcpus 4 \
  --disk size=80,bus=sata \
  --cdrom "$IMAGES/${VM_NAME}-install.iso" \
  --disk path="$IMAGES/${VM_NAME}-autounattend.iso",device=cdrom \
  --os-variant "$OS_VARIANT" \
  --network network=default \
  --graphics vnc \
  --noautoconsole
