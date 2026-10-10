#!/bin/bash
# Queries Windows' own licensing service for the real eval expiration
# (including any rearms already used) rather than computing "180 days
# since creation" ourselves. See README.md's "eval clock" section.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

IP="${1:-$(vm_ip)}"
if [ -z "$IP" ]; then
  echo "no IP given and VM has no DHCP lease right now (is it running?)" >&2
  exit 1
fi

ssh_vm "$IP" 'cscript //nologo C:\Windows\System32\slmgr.vbs /xpr'
