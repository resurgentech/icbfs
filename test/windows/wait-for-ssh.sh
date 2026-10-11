#!/bin/bash
# Waits for the VM to get a DHCP lease and then for sshd to answer on
# port 22, auto-restarting the VM (up to 6x) if it ever gets shut off
# along the way - see README.md's "unexplained libvirtd SIGTERM" section
# for why that safety net exists. Prints the VM's IP to stdout on success.
set -uo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

max_restarts=6
restarts=0
elapsed=0
timeout="${1:-1700}"
ip=""

wait_for_running() {
  while true; do
    state=$(virsh domstate "$VM_NAME" 2>/dev/null)
    [ "$state" = "running" ] && return 0
    if [ "$state" = "shut off" ]; then
      restarts=$((restarts+1))
      if [ $restarts -gt $max_restarts ]; then
        echo "giving up: shut off again and hit max restarts (${max_restarts}) at elapsed ${elapsed}s" >&2
        exit 3
      fi
      echo "VM shut off (elapsed ${elapsed}s) - restarting (attempt ${restarts}/${max_restarts})" >&2
      virsh start "$VM_NAME" >/dev/null 2>&1
      sleep 5
      ip=""
      continue
    fi
    sleep 5
    elapsed=$((elapsed+5))
  done
}

wait_for_running
while [ -z "$ip" ]; do
  wait_for_running
  ip=$(vm_ip)
  [ -n "$ip" ] && break
  sleep 10; elapsed=$((elapsed+10))
  [ $elapsed -ge "$timeout" ] && { echo "TIMEOUT waiting for DHCP lease after ${elapsed}s" >&2; exit 1; }
done

while true; do
  wait_for_running
  if nc -z -w2 "$ip" 22 2>/dev/null; then
    echo "$ip"
    exit 0
  fi
  sleep 10; elapsed=$((elapsed+10))
  [ $elapsed -ge "$timeout" ] && { echo "TIMEOUT waiting for sshd after ${elapsed}s, last known ip=$ip" >&2; exit 1; }
done
