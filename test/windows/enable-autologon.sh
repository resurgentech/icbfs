#!/bin/bash
# Turns on auto-logon as Administrator on a VM made before
# autounattend.xml did it at every boot, then restarts it
# (enable-autologon.ps1). The password goes over as a file and is deleted
# there; it's never on a command line.
#
#   ./enable-autologon.sh [ip]     (default: the VM's address)
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

ip="${1:-$(vm_ip)}"
[ -n "$ip" ] || { echo "no address for $VM_NAME; is it running?" >&2; exit 1; }

scp_vm "$HERE/enable-autologon.ps1" "Administrator@$ip:C:/Windows/Temp/enable-autologon.ps1"
scp_vm "$PASSWORD_FILE" "Administrator@$ip:C:/Windows/Temp/icbfs-vm-password"
ssh_vm "$ip" "powershell -NoProfile -ExecutionPolicy Bypass -File C:\\Windows\\Temp\\enable-autologon.ps1 -PasswordFile C:\\Windows\\Temp\\icbfs-vm-password; Restart-Computer -Force"
