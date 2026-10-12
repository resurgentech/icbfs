#!/bin/bash
# Creates the disposable Linux certification VM from the Ubuntu cloud
# image, fully unattended via cloud-init (no interactive install at
# all, unlike the Windows VM's ISO+answer-file dance — cloud images
# don't need one). Mirrors test/windows/create-vm.sh's staging-into-
# libvirt's-images-pool pattern (qemu needs search permission on the
# whole path, which $HOME doesn't grant).
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

CLOUD_IMAGE="$WORK_DIR/ubuntu-24.04-server-cloudimg-amd64.img"
if [ ! -f "$CLOUD_IMAGE" ]; then
  echo "Downloading Ubuntu 24.04 cloud image..." >&2
  curl -fsSL -o "$CLOUD_IMAGE" \
    https://cloud-images.ubuntu.com/releases/24.04/release/ubuntu-24.04-server-cloudimg-amd64.img
fi

IMAGES=/var/lib/libvirt/images
DISK="$IMAGES/${VM_NAME}.qcow2"
BASE="$IMAGES/${VM_NAME}-base.img"

# qemu runs as libvirt-qemu, which has no search permission anywhere
# under $HOME — same reason test/windows/create-vm.sh stages its own
# install media into libvirt's images pool first rather than
# referencing it from $HOME directly (confirmed the hard way: qcow2
# creation succeeds, but the VM then fails at first boot with
# "Cannot access backing file ... Permission denied").
sudo cp --reflink=auto "$CLOUD_IMAGE" "$BASE"
sudo chown libvirt-qemu:libvirt-qemu "$BASE"
sudo chmod 644 "$BASE"

# Copy-on-write local disk backed by the staged cloud image, grown to
# a real working size — the cloud image itself is a thin, minimal root.
sudo qemu-img create -f qcow2 -F qcow2 -b "$BASE" "$DISK" 20G
sudo chown libvirt-qemu:libvirt-qemu "$DISK"
sudo chmod 644 "$DISK"

PUBKEY="$(cat "$HOME/.ssh/id_ed25519.pub")"
USERDATA="$WORK_DIR/cloud-init.generated.yaml"
sed "s|REPLACED_BY_CREATE_VM_SH|${PUBKEY}|" "$HERE/cloud-init.yaml" > "$USERDATA"

virt-install \
  --name "$VM_NAME" \
  --memory 4096 --vcpus 2 \
  --disk path="$DISK",bus=virtio \
  --import --os-variant ubuntu24.04 \
  --network network=default \
  --cloud-init user-data="$USERDATA" \
  --graphics none \
  --noautoconsole
