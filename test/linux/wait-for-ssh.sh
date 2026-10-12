#!/bin/bash
# Waits for the VM to get a DHCP lease and then for sshd to answer on
# port 22. Prints the VM's IP to stdout on success. Mirrors
# test/windows/wait-for-ssh.sh's shape, minus that script's own
# auto-restart loop — the periodic libvirtd SIGTERM issue documented
# in test/windows/README.md was never observed against this VM, so
# that workaround isn't duplicated here unless it turns out to be
# needed too.
set -uo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

timeout="${1:-600}"
elapsed=0
ip=""

while [ -z "$ip" ]; do
  state=$(virsh domstate "$VM_NAME" 2>/dev/null)
  if [ "$state" != "running" ]; then
    sleep 5; elapsed=$((elapsed+5))
    [ $elapsed -ge "$timeout" ] && { echo "TIMEOUT: VM not running after ${elapsed}s (state=$state)" >&2; exit 1; }
    continue
  fi
  ip=$(vm_ip)
  [ -n "$ip" ] && break
  sleep 5; elapsed=$((elapsed+5))
  [ $elapsed -ge "$timeout" ] && { echo "TIMEOUT waiting for DHCP lease after ${elapsed}s" >&2; exit 1; }
done

while true; do
  if nc -z -w2 "$ip" 22 2>/dev/null; then
    echo "$ip"
    exit 0
  fi
  sleep 5; elapsed=$((elapsed+5))
  [ $elapsed -ge "$timeout" ] && { echo "TIMEOUT waiting for sshd after ${elapsed}s, last known ip=$ip" >&2; exit 1; }
done
