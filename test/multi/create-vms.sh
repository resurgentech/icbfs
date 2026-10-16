#!/bin/bash
# Creates the small Linux VMs for the multi-machine tests (run-multi.sh):
# icbfs-multi-1 .. icbfs-multi-N, each 1 vCPU and 1 GiB, from the same
# Ubuntu cloud image as test/linux's VM, with only FUSE installed (the
# runner copies prebuilt binaries in).
#
#   test/multi/create-vms.sh [N]   (default 2)
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
N="${1:-2}"

CLOUD_IMAGE="$LINUX_WORK/ubuntu-24.04-server-cloudimg-amd64.img"
if [ ! -f "$CLOUD_IMAGE" ]; then
  mkdir -p "$LINUX_WORK"
  echo "Downloading Ubuntu 24.04 cloud image..." >&2
  curl -fsSL -o "$CLOUD_IMAGE" \
    https://cloud-images.ubuntu.com/releases/24.04/release/ubuntu-24.04-server-cloudimg-amd64.img
fi

# qemu runs as libvirt-qemu, which can't reach anything under $HOME: the
# image and disks go in libvirt's pool (as test/linux/create-vm.sh does).
IMAGES=/var/lib/libvirt/images
BASE="$IMAGES/icbfs-multi-base.img"
if ! sudo test -f "$BASE"; then
  sudo cp --reflink=auto "$CLOUD_IMAGE" "$BASE"
  sudo chown libvirt-qemu:libvirt-qemu "$BASE"
  sudo chmod 644 "$BASE"
fi

mkdir -p "$WORK_DIR"
USERDATA="$WORK_DIR/cloud-init.generated.yaml"
sed "s|REPLACED_BY_CREATE_VMS_SH|$(cat "$HOME/.ssh/id_ed25519.pub")|" "$HERE/cloud-init.yaml" > "$USERDATA"

for i in $(seq 1 "$N"); do
  name="${VM_PREFIX}$i"
  if virsh dominfo "$name" >/dev/null 2>&1; then
    echo "$name exists" >&2
    continue
  fi
  disk="$IMAGES/$name.qcow2"
  sudo qemu-img create -q -f qcow2 -F qcow2 -b "$BASE" "$disk" 8G
  sudo chown libvirt-qemu:libvirt-qemu "$disk"
  virt-install \
    --name "$name" \
    --memory 1024 --vcpus 1 \
    --disk path="$disk",bus=virtio \
    --import --os-variant ubuntu24.04 \
    --network network=default \
    --cloud-init user-data="$USERDATA" \
    --graphics none \
    --noautoconsole
done

for i in $(seq 1 "$N"); do
  name="${VM_PREFIX}$i"
  echo "Waiting for $name..." >&2
  for _ in $(seq 1 120); do
    ip=$(vm_ip "$name")
    if [ -n "$ip" ] && ssh_vm "$ip" "cloud-init status --wait >/dev/null 2>&1; command -v fusermount3" >/dev/null 2>&1; then
      echo "$name: $ip" >&2
      break
    fi
    sleep 5
  done
done
