#!/bin/bash
# Starts a program on the VM's logged-on desktop (run-in-desktop.ps1), waits
# and saves a screenshot of the console. Needs auto-logon (autounattend.xml,
# or enable-autologon.sh for an older VM).
#
#   ./run-in-desktop.sh 'C:\path\to\app.exe' [screenshot.png] [seconds]
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

exe="$1"
shot="${2:-$WORK_DIR/desktop.png}"
wait="${3:-10}"
ip="$(vm_ip)"
[ -n "$ip" ] || { echo "no address for $VM_NAME; is it running?" >&2; exit 1; }

scp_vm "$HERE/run-in-desktop.ps1" "Administrator@$ip:C:/Windows/Temp/run-in-desktop.ps1"
ssh_vm "$ip" "powershell -NoProfile -ExecutionPolicy Bypass -File C:\\Windows\\Temp\\run-in-desktop.ps1 -Exe '$exe'"
sleep "$wait"
virsh screenshot "$VM_NAME" "$shot" >/dev/null
echo "$shot"
