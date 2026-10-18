#!/bin/bash
# Multi-machine tests: every machine (test/linux's VM, and the
# icbfs-multi-N VMs from create-vms.sh) mounts one filesystem on a
# throwaway MinIO, locking on as by default, and they run each workload at
# the same time. Each workload's result is checked from every machine, and
# at the end, with everything unmounted, icbfs-cli fs check must find
# nothing wrong.
#
#   test/multi/run-multi.sh [workload ...]   (default: all)
#
# Workloads: smoke, storm, exclusive, counter, append, rename, fsstress,
# cthon.
#
# The store is a throwaway MinIO; with ICBFS_MULTI_STORE=s3 the real S3
# bucket .env.aws names (test/aws), with the AWS CLI's credentials; with
# ICBFS_MULTI_STORE=azure the Azure container .env.azure names (test/azure),
# with the account key from the az CLI.
# MULTI_MACHINES limits how many machines take part, and ICBFS_MULTI_ONLY
# leaves test/linux's VM out (test/multi/lib.sh); STORM, EXCL, COUNT,
# APPEND, RENAMES and FSSTRESS_OPS set the workloads' sizes (per machine).
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$HERE/lib.sh"
# STORE_ARGS are the store's flags as this host reaches it, STORE_VM_ARGS
# as the VMs do. Its secrets are exported here as the ICBFS_* variables
# icbfs-cli reads, and STORE_ENV is the same lines for the VMs: never on a
# command line (ps would show them).
if [ "${ICBFS_MULTI_STORE:-minio}" = azure ]; then
  set -a
  # shellcheck disable=SC1091
  source "$REPO/.env.azure"
  set +a
  STORE_ARGS="-store azure -azure-account $ICBFS_TEST_AZURE_ACCOUNT -azure-container $ICBFS_TEST_AZURE_CONTAINER"
  STORE_VM_ARGS="$STORE_ARGS"
  ICBFS_AZURE_KEY=$(az storage account keys list --subscription "$ICBFS_TEST_AZURE_SUBSCRIPTION" \
    -g "$ICBFS_TEST_AZURE_RESOURCE_GROUP" -n "$ICBFS_TEST_AZURE_ACCOUNT" --query '[0].value' -o tsv)
  export ICBFS_AZURE_KEY
  STORE_ENV="ICBFS_AZURE_KEY=$ICBFS_AZURE_KEY"
else
  if [ "${ICBFS_MULTI_STORE:-minio}" = s3 ]; then
    set -a
    # shellcheck disable=SC1091
    source "$REPO/.env.aws"
    set +a
    eval "$(aws configure export-credentials --format env)"
    region="${ICBFS_TEST_S3_REGION:-us-east-1}"
    STORE_ARGS="-store s3 -endpoint https://s3.$region.amazonaws.com -bucket $ICBFS_TEST_S3_BUCKET -region $region"
    STORE_VM_ARGS="$STORE_ARGS"
    ICBFS_ACCESS_KEY="$AWS_ACCESS_KEY_ID" ICBFS_SECRET_KEY="$AWS_SECRET_ACCESS_KEY"
  else
    if [ -z "${ICBFS_MINIO_VM_ENDPOINT:-}" ]; then
      exec "$HERE/../minio/with-minio.sh" "$HERE/run-multi.sh" "$@"
    fi
    STORE_ARGS="-store s3 -endpoint $ICBFS_MINIO_ENDPOINT -bucket $ICBFS_MINIO_BUCKET"
    STORE_VM_ARGS="-store s3 -endpoint $ICBFS_MINIO_VM_ENDPOINT -bucket $ICBFS_MINIO_BUCKET"
    ICBFS_ACCESS_KEY="$ICBFS_MINIO_ACCESS_KEY" ICBFS_SECRET_KEY="$ICBFS_MINIO_SECRET_KEY"
  fi
  export ICBFS_ACCESS_KEY ICBFS_SECRET_KEY
  STORE_ENV=$(printf 'ICBFS_ACCESS_KEY=%s\nICBFS_SECRET_KEY=%s' "$ICBFS_ACCESS_KEY" "$ICBFS_SECRET_KEY")
fi
STORM="${STORM:-300}" EXCL="${EXCL:-100}" COUNT="${COUNT:-40}" APPEND="${APPEND:-100}" RENAMES="${RENAMES:-50}" FSSTRESS_OPS="${FSSTRESS_OPS:-2000}"

WORKLOADS=("$@")
[ ${#WORKLOADS[@]} -gt 0 ] || WORKLOADS=(smoke storm exclusive counter append rename fsstress cthon)

mapfile -t IPS < <(machines | grep . | head -n "${MULTI_MACHINES:-99}")
N=${#IPS[@]}
if [ "$N" -lt 2 ]; then
  echo "need at least two machines: test/linux's VM and test/multi/create-vms.sh's" >&2
  exit 1
fi
STAMP=$(date +%Y%m%d%H%M%S)
FS="multi-$STAMP"
MNT=/mnt/multi
BIN=/home/ubuntu/icbfs-multi
OUT="$WORK_DIR/run-$STAMP"
mkdir -p "$OUT"
echo "machines: ${IPS[*]}" >&2

# Static binaries from this host, so the small VMs need no Go.
(cd "$REPO" && CGO_ENABLED=0 go build -o "$WORK_DIR/icbfs" ./cmd/icbfs && go build -o "$WORK_DIR/icbfs-cli" ./cmd/icbfs-cli)
cli() {
  # shellcheck disable=SC2086 # STORE_ARGS is words
  "$WORK_DIR/icbfs-cli" "$@" $STORE_ARGS
}
cli fs create -fs "$FS" -root-uid 0 -root-gid 0 >/dev/null

# fsstress from test/linux's VM, where run-xfstests.sh builds xfstests,
# whether or not that VM takes part (ICBFS_MULTI_ONLY).
MAIN_IP=$(vm_ip "$MAIN_VM")
FSSTRESS=""
if [ -n "$MAIN_IP" ] && ssh_vm "$MAIN_IP" "test -x /home/ubuntu/xfstests-dev/ltp/fsstress"; then
  scp -q -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR \
    "ubuntu@${MAIN_IP}:/home/ubuntu/xfstests-dev/ltp/fsstress" "$WORK_DIR/fsstress"
  FSSTRESS="$WORK_DIR/fsstress"
fi

# Connectathon (cthon04), as test/linux/run-cthon.sh builds it on that VM.
CTHON=""
if [ -n "$MAIN_IP" ] && ssh_vm "$MAIN_IP" "test -x /home/ubuntu/cthon04/basic/test1"; then
  ssh_vm "$MAIN_IP" "tar -C /home/ubuntu -cf - cthon04" > "$WORK_DIR/cthon04.tar"
  CTHON="$WORK_DIR/cthon04.tar"
fi

unmount_all() {
  for ip in "${IPS[@]}"; do
    ssh_vm "$ip" "sudo umount $MNT 2>/dev/null || sudo umount -l $MNT 2>/dev/null; sudo pkill -f '[i]cbfs-multi/icbfs mount' 2>/dev/null; rm -f /tmp/icbfs-multi.env; true" || true
  done
}
trap unmount_all EXIT

for ip in "${IPS[@]}"; do
  ssh_vm "$ip" "mkdir -p $BIN" >/dev/null
  scp_to_vm "$ip" "$WORK_DIR/icbfs" "$BIN/icbfs"
  if [ -n "$FSSTRESS" ]; then
    scp_to_vm "$ip" "$FSSTRESS" "$BIN/fsstress"
    ssh_vm "$ip" "ldd $BIN/fsstress | grep -q 'not found' && sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq libaio1t64 liburing2 >/dev/null; true"
  fi
  if [ -n "$CTHON" ]; then
    scp_to_vm "$ip" "$CTHON" "$BIN/cthon04.tar"
    # Its general tests compile programs on the filesystem under test, so
    # every machine needs a compiler, make, and libtirpc's headers.
    ssh_vm "$ip" "tar -C $BIN -xf $BIN/cthon04.tar; command -v make >/dev/null && command -v pkg-config >/dev/null && pkg-config --exists libtirpc || sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq build-essential pkg-config libtirpc-dev >/dev/null; true"
  fi
  # The smoke workload's xattr and ACL tools (on VMs made before
  # cloud-init installed them).
  ssh_vm "$ip" "command -v setfattr >/dev/null && command -v setfacl >/dev/null || sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq attr acl >/dev/null; true"
  ssh_vm "$ip" "sudo mkdir -p $MNT"
  # The keys reach the VM on ssh's stdin, into a file only its owner reads,
  # never on a command line (ps would show them).
  printf '%s\n' "$STORE_ENV" |
    ssh_vm "$ip" "umask 077; cat > /tmp/icbfs-multi.env"
  # Detached, with no stdin or output left to the ssh session, so ssh returns.
  ssh_vm "$ip" "sudo sh -c 'set -a; . /tmp/icbfs-multi.env; set +a; exec setsid $BIN/icbfs mount \
    $STORE_VM_ARGS -fs $FS -allow-other $MNT' \
    < /dev/null > /tmp/icbfs-multi.log 2>&1 &"
done
for ip in "${IPS[@]}"; do
  if ! ssh_vm "$ip" "for i in \$(seq 1 60); do findmnt -n -o FSTYPE $MNT | grep -q fuse && exit 0; sleep 0.5; done; exit 1"; then
    echo "the mount on $ip never came up:" >&2
    ssh_vm "$ip" "cat /tmp/icbfs-multi.log" >&2
    exit 1
  fi
done

fail=0
check() { if eval "$2"; then echo "PASS  $1"; else echo "FAIL  $1"; fail=1; fi; }

# everywhere SCRIPT runs SCRIPT as root on every machine at once, with $i
# (1..N) and $N set, and fails if any of them did. Each machine's output
# is in $OUT/<workload>-<i>.log.
everywhere() {
  local name="$1" script="$2" pids=() i=0 status=0
  for ip in "${IPS[@]}"; do
    i=$((i + 1))
    ssh_vm "$ip" "sudo env i=$i N=$N MNT=$MNT BIN=$BIN bash -c $(printf %q "$script")" > "$OUT/$name-$i.log" 2>&1 &
    pids+=($!)
  done
  for p in "${pids[@]}"; do wait "$p" || status=1; done
  return $status
}

# on IP SCRIPT runs SCRIPT as root on one machine, after dropping the
# kernel's cached names and attributes: the mounts trust them for a while
# (-meta-cache), and a check reading another machine's results must see the
# store's answer, not this machine's cache.
on() { ssh_vm "$1" "sudo sh -c 'echo 2 > /proc/sys/vm/drop_caches'; sudo env N=$N MNT=$MNT bash -c $(printf %q "$2")"; }

# same_everywhere CMD: CMD prints the same thing on every machine.
same_everywhere() {
  local first="" out
  for ip in "${IPS[@]}"; do
    out=$(on "$ip" "$1" | md5sum)
    [ -z "$first" ] && first="$out"
    [ "$out" = "$first" ] || return 1
  done
}

for w in "${WORKLOADS[@]}"; do
  case "$w" in
  smoke)
    # One machine, the everyday operations; then another machine reads
    # what it wrote, after the close.
    check "smoke: create, write, read, rename, link, symlink, chmod, xattr, ACL, flock, a 20 MB file" "on ${IPS[0]} '
      set -e; cd \$MNT; mkdir -p smoke; cd smoke
      echo hello > a; [ \"\$(cat a)\" = hello ]
      mv a b; [ ! -e a ]; [ \"\$(cat b)\" = hello ]
      ln b c; [ \$(stat -c %h b) = 2 ]
      ln -s b d; [ \"\$(readlink d)\" = b ]; [ \"\$(cat d)\" = hello ]
      chmod 640 b; [ \$(stat -c %a b) = 640 ]
      setfattr -n user.note -v yes b; [ \"\$(getfattr --only-values -n user.note b)\" = yes ]
      setfacl -m u:1234:r b; getfacl -c b | grep -q user:1234:r
      flock -x b true
      head -c 20000000 /dev/urandom > big; sha256sum big > big.sum
      rm c; [ \$(stat -c %h b) = 1 ]
    '"
    if [ "$N" -ge 2 ]; then
      check "smoke: another machine reads the 20 MB file intact" "on ${IPS[1]} 'cd \$MNT/smoke && sha256sum -c big.sum >/dev/null'"
    fi
    ;;
  storm)
    # Every machine creates its own names in one directory at once:
    # every name lands, and every machine lists them all.
    on "${IPS[0]}" "mkdir -p \$MNT/storm"
    check "storm: $N machines create $STORM names each in one directory" \
      "everywhere storm 'for k in \$(seq 1 $STORM); do : > \$MNT/storm/m\$i-\$k || exit 1; done'"
    check "storm: all $((STORM * N)) are there" "[ \$(on ${IPS[0]} 'ls \$MNT/storm | wc -l') -eq $((STORM * N)) ]"
    check "storm: every machine lists the same names" "same_everywhere 'ls \$MNT/storm'"
    ;;
  exclusive)
    # Every machine creates the same names with O_EXCL: each is created
    # by exactly one.
    on "${IPS[0]}" "mkdir -p \$MNT/excl"
    # (The shell's own noclobber error goes to the subshell's stderr.)
    everywhere exclusive "set -o noclobber; for k in \$(seq 1 $EXCL); do (: > \$MNT/excl/f\$k) 2>/dev/null && echo \$k; done; true" || true
    check "exclusive: each of $EXCL names created by exactly one of $N machines" \
      "[ \$(cat $OUT/exclusive-*.log | grep -xE '[0-9]+' | sort -n | uniq | wc -l) -eq $EXCL ] && [ \$(cat $OUT/exclusive-*.log | grep -cxE '[0-9]+') -eq $EXCL ]"
    ;;
  counter)
    # Every machine increments one counter under flock: no increment is
    # lost (locks, and close-to-open, between machines).
    on "${IPS[0]}" "echo 0 > \$MNT/counter && : > \$MNT/counter.lock"
    check "counter: $N machines increment it $COUNT times each under flock" \
      "everywhere counter 'for k in \$(seq 1 $COUNT); do flock -x \$MNT/counter.lock -c \"v=\\\$(cat \$MNT/counter); echo \\\$((v + 1)) > \$MNT/counter\" || exit 1; done'"
    check "counter: it reads $((COUNT * N)) everywhere" \
      "same_everywhere 'cat \$MNT/counter' && [ \$(on ${IPS[0]} 'cat \$MNT/counter') -eq $((COUNT * N)) ]"
    ;;
  append)
    # Every machine appends lines to one log under flock: none is lost or
    # torn.
    on "${IPS[0]}" ": > \$MNT/log"
    check "append: $N machines append $APPEND lines each under flock" \
      "everywhere append 'for k in \$(seq 1 $APPEND); do flock -x \$MNT/log -c \"echo m\$i-line-\$k >> \$MNT/log\" || exit 1; done'"
    check "append: $((APPEND * N)) whole, distinct lines" \
      "[ \$(on ${IPS[0]} 'grep -cE \"^m[0-9]+-line-[0-9]+\\\$\" \$MNT/log') -eq $((APPEND * N)) ] && [ \$(on ${IPS[0]} 'sort -u \$MNT/log | wc -l') -eq $((APPEND * N)) ]"
    ;;
  rename)
    # Every machine moves its own files between two shared directories,
    # back and forth: nothing is lost or duplicated.
    on "${IPS[0]}" "mkdir -p \$MNT/ra \$MNT/rb"
    everywhere rename-setup "for k in \$(seq 1 $RENAMES); do : > \$MNT/ra/r\$i-\$k; done"
    check "rename: $N machines move $RENAMES files each between two directories, 4 times" \
      "everywhere rename 'for round in 1 2 3 4; do for k in \$(seq 1 $RENAMES); do if [ \$((round % 2)) = 1 ]; then mv \$MNT/ra/r\$i-\$k \$MNT/rb/ || exit 1; else mv \$MNT/rb/r\$i-\$k \$MNT/ra/ || exit 1; fi; done; done'"
    check "rename: all $((RENAMES * N)) are back, once each" \
      "[ \$(on ${IPS[0]} 'ls \$MNT/ra | wc -l') -eq $((RENAMES * N)) ] && [ \$(on ${IPS[0]} 'ls \$MNT/rb | wc -l') -eq 0 ]"
    ;;
  fsstress)
    if [ -z "$FSSTRESS" ]; then
      echo "SKIP  fsstress (build xfstests on test/linux's VM: just xfstests)"
      continue
    fi
    # fsstress on every machine at once, each in a directory of its own,
    # which shares the filesystem's root and blocks with the others.
    # fsstress exits 0 even when its workers fail or are killed, so what
    # it says counts too: its errors go to stderr, the machine's log.
    check "fsstress: $N machines, $FSSTRESS_OPS operations each" \
      "everywhere fsstress '\$BIN/fsstress -d \$MNT/stress-\$i -n $FSSTRESS_OPS -p 2 -s \$i >/dev/null' && ! grep -q . \"\$OUT\"/fsstress-*.log"
    ;;
  cthon)
    if [ -z "$CTHON" ]; then
      echo "SKIP  cthon (build it on test/linux's VM: test/linux/run-cthon.sh)"
      continue
    fi
    # Connectathon's basic, general, special and lock tests on every
    # machine at once, each in a directory of its own on the one
    # filesystem. runtests says "All tests completed" only if every
    # group passed.
    check "cthon: $N machines, all groups" \
      "everywhere cthon 'cd \$BIN/cthon04 && ./runtests -a -t \$MNT/cthon-\$i' && [ \$(grep -l 'All tests completed' \"\$OUT\"/cthon-*.log | wc -l) -eq $N ]"
    ;;
  *)
    echo "unknown workload $w" >&2
    exit 1
    ;;
  esac
done

unmount_all
trap - EXIT
cli fs check -fs "$FS" -json > "$OUT/check.json" || true
check "fs check finds nothing wrong" "grep -q '\"problems\":null' $OUT/check.json"
grep -o '"directories":[0-9]*,"files":[0-9]*,"blocks":[0-9]*' "$OUT/check.json" >&2 || true
grep -o '"unreachable":[0-9]*' "$OUT/check.json" >&2 || true
echo "logs: $OUT" >&2
exit $fail
