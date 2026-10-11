#!/bin/bash
# Stages install media into libvirt's images pool (qemu needs search
# permission on the whole path, which $HOME doesn't grant - see README.md)
# and creates the VM. Pass the path to a Windows Server 2025 eval ISO
# (download it by hand once - see README.md, Microsoft gates it behind a
# registration form that isn't worth scripting around) and, optionally, a
# path to a virtio-win driver ISO (not required - this VM uses emulated
# SATA/e1000e so it boots with zero extra drivers, see README.md).
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

INSTALL_ISO="${1:?usage: create-vm.sh <path-to-windows-server-eval.iso>}"

"$HERE/build-answer-iso.sh"

IMAGES=/var/lib/libvirt/images
sudo cp --reflink=auto "$INSTALL_ISO" "$IMAGES/${VM_NAME}-install.iso"
sudo cp --reflink=auto "$WORK_DIR/autounattend.iso" "$IMAGES/${VM_NAME}-autounattend.iso"
sudo chown libvirt-qemu:libvirt-qemu "$IMAGES/${VM_NAME}-install.iso" "$IMAGES/${VM_NAME}-autounattend.iso"
sudo chmod 644 "$IMAGES/${VM_NAME}-install.iso" "$IMAGES/${VM_NAME}-autounattend.iso"

virt-install \
  --name "$VM_NAME" \
  --memory 8192 --vcpus 4 \
  --disk size=60,bus=sata \
  --cdrom "$IMAGES/${VM_NAME}-install.iso" \
  --disk path="$IMAGES/${VM_NAME}-autounattend.iso",device=cdrom \
  --os-variant win2k25 \
  --network network=default \
  --graphics vnc \
  --noautoconsole
