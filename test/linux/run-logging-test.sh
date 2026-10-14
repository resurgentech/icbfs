#!/bin/bash
# The logging chain end to end on the Linux VM, with the real binaries:
# icbfs-mockserver on this host, icbfs-logd and an icbfs mount on the VM.
#
#   test/linux/run-logging-test.sh <vm-ip>
#
# The mock server runs in a container, like test/minio's MinIO, and is
# removed with it. The mount is given no store credentials at all — it
# must get them from the server through the daemon (-log-socket). A set
# of filesystem actions is then run through it from a shell, and the
# server's log must record exactly those actions. Run twice: logging
# synchronously (each operation waits for the server) and with
# -log-async. A mount of a filesystem the server doesn't allow must fail.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [ -z "${ICBFS_MINIO_VM_ENDPOINT:-}" ]; then
  exec "$HERE/../minio/with-minio.sh" "$HERE/run-logging-test.sh" "$@"
fi
source "$HERE/lib.sh"

IP="${1:?usage: run-logging-test.sh <vm-ip>}"
REPO="$(cd "$HERE/../.." && pwd)"
HOST_ADDR="${ICBFS_MINIO_VM_ENDPOINT#http://}"
HOST_ADDR="${HOST_ADDR%:*}"
SECRET="logtest-$RANDOM$RANDOM"
SOCK=/run/icbfs-logd.sock
MNT=/mnt/icbfs-log
mkdir -p "$WORK_DIR"
LOG_NAME="logging-events-$(date +%Y%m%d%H%M%S).jsonl"
SERVER_LOG="$WORK_DIR/$LOG_NAME"

deploy_icbfs "$IP"
echo "Building icbfs-mockserver..." >&2
(cd "$REPO" && CGO_ENABLED=0 go build -o "$WORK_DIR/icbfs-mockserver" ./cmd/icbfs-mockserver)

# In a container with a published port, like the throwaway MinIO: Docker
# routes published ports past the host firewall, which would otherwise
# drop the VM's connection to a plain host process. The static binary
# runs in the MinIO image already on hand, as this user, so the log it
# writes into the work directory stays ours.
SERVER_CONTAINER="icbfs-test-mockserver-$$"
docker run -d --rm --name "$SERVER_CONTAINER" --user "$(id -u):$(id -g)" \
  -v "$WORK_DIR:/work" -p 0.0.0.0::8090 --entrypoint /work/icbfs-mockserver "${ICBFS_MINIO_IMAGE:-minio/minio:latest}" \
  -addr :8090 -secret "$SECRET" -log "/work/$LOG_NAME" -allow "logged-sync,logged-async" \
  -endpoint "$ICBFS_MINIO_VM_ENDPOINT" -bucket "$ICBFS_MINIO_BUCKET" \
  -access-key "$ICBFS_MINIO_ACCESS_KEY" -secret-key "$ICBFS_MINIO_SECRET_KEY" >/dev/null
PORT=$(docker port "$SERVER_CONTAINER" 8090/tcp | awk -F: '/0.0.0.0/{print $NF; exit}')
cleanup() {
  ssh_vm "$IP" "sudo umount -l $MNT 2>/dev/null; sudo pkill -x icbfs; sudo pkill -x icbfs-logd; true" >/dev/null 2>&1 || true
  docker logs "$SERVER_CONTAINER" >>"$WORK_DIR/mockserver.log" 2>&1 || true
  docker rm -f "$SERVER_CONTAINER" >/dev/null 2>&1 || true
}
trap cleanup EXIT

ssh_vm "$IP" "sudo pkill -x icbfs-logd; sudo $VM_SRC/icbfs-logd -socket $SOCK -server ws://$HOST_ADDR:$PORT/ws/daemon -secret $SECRET > /tmp/icbfs-logd.log 2>&1 &
for i in \$(seq 1 20); do [ -S $SOCK ] && exit 0; sleep 0.25; done; echo 'icbfs-logd never came up'; cat /tmp/icbfs-logd.log; exit 1"

fail=0
# run_mode <fs name> [-log-async]: mount with only -log-socket, run the
# actions, unmount (which delivers anything async logging still holds).
run_mode() {
  local fs="$1"; shift
  ssh_vm "$IP" "bash -s" <<EOF
set -e
sudo mkdir -p $MNT
sudo $VM_SRC/icbfs mount -log-socket $SOCK $* -fs $fs -allow-other $MNT > /tmp/icbfs-log-mount.log 2>&1 &
for i in \$(seq 1 120); do findmnt -n -o FSTYPE $MNT | grep -q fuse && break; sleep 0.25; done
findmnt -n $MNT >/dev/null || { echo "mount failed:"; cat /tmp/icbfs-log-mount.log; cat /tmp/icbfs-logd.log; exit 1; }
sudo bash -c '
  mkdir $MNT/d
  echo hello > $MNT/d/a
  cat $MNT/d/a > /dev/null
  ls $MNT/d > /dev/null
  echo world >> $MNT/d/a
  mv $MNT/d/a $MNT/d/b
  rm $MNT/d/b
  rmdir $MNT/d
  ls $MNT > /dev/null
'
sudo umount $MNT
for i in \$(seq 1 60); do pgrep -x icbfs >/dev/null || break; sleep 0.25; done
EOF
}

create_fs logged-sync
create_fs logged-async
echo "Mounting with synchronous logging..." >&2
run_mode logged-sync
echo "Mounting with -log-async..." >&2
run_mode logged-async -log-async

python3 - "$SERVER_LOG" <<'PY' || fail=1
import json, sys
want = ["write /d/a 6", "read /d/a 0", "ls /d 0", "write /d/a 12",
        "delete /d/b 0", "delete /d 0", "ls / 0"]
got = {}
for line in open(sys.argv[1]):
    e = json.loads(line)
    got.setdefault(e["fs"], []).append(f'{e["op"]} {e["path"]} {e.get("bytes", 0)}')
ok = True
for fs in ("logged-sync", "logged-async"):
    if got.get(fs) == want:
        print(f"PASS  {fs}: the server logged exactly the {len(want)} actions")
    else:
        ok = False
        print(f"FAIL  {fs}: the server logged\n        " + "\n        ".join(got.get(fs, ["(nothing)"])))
sys.exit(0 if ok else 1)
PY

out=$(ssh_vm "$IP" "sudo timeout 60 $VM_SRC/icbfs mount -log-socket $SOCK -fs not-allowed $MNT 2>&1; echo exit=\$?" 2>/dev/null | tail -2)
if echo "$out" | grep -q "not allowed" && ! echo "$out" | grep -q "exit=0"; then
  echo "PASS  a filesystem the server doesn't allow can't mount"
else
  echo "FAIL  mount of a disallowed filesystem: $out"; fail=1
fi
echo "server log: $SERVER_LOG" >&2
exit $fail
