#!/bin/bash
# Large-file check on the Linux VM — OPT-IN, not part of the regular
# runs: it moves ~20 GB between the VM and MinIO and takes about ten
# minutes. Run it when a change touches the content write path (OpenFile
# flush, multipart uploads, truncate, the max-file-size limit), not as
# routine:
#
#   test/linux/run-largefile-test.sh <vm-ip>
#
# Against a fresh filesystem with the test runners' 8 GiB limit (8 MiB
# parts) on a throwaway MinIO, it checks that:
#   - a 6 GiB file (past S3's 5 GiB single-PUT limit, so multipart)
#     writes and reads back with a matching hash;
#   - a one-byte edit in the middle lands, and leaves every other byte
#     alone (the flush copies the untouched parts server-side);
#   - growing past 8 GiB, by write or truncate, is EFBIG and changes
#     nothing.
# Needs ~13 GB free on the VM for the scratch file and read-back.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$HERE/lib.sh"
if store_needs_minio; then
  exec "$HERE/../minio/with-minio.sh" "$HERE/run-largefile-test.sh" "$@"
fi
store_setup

IP="${1:?usage: run-largefile-test.sh <vm-ip>}"
FS_NAME="largefile-$(date +%Y%m%d%H%M%S)"
MNT=/mnt/icbfs-largefile

deploy_icbfs "$IP"
trap 'ssh_vm "$IP" "sudo umount -l $MNT 2>/dev/null; sudo pkill -x icbfs; rm -f /tmp/icbfs-mount-*.env; true" || true' EXIT
create_fs "$FS_NAME" -max-file-size $((8 << 30))
mount_icbfs "$IP" "$MNT" "$FS_NAME"

echo "Running the large-file check (about ten minutes)..." >&2
ssh_vm "$IP" "bash -s" <<EOF
set -u
MNT=$MNT
fail=0
pass() { echo "PASS  \$*"; }
bad()  { echo "FAIL  \$*"; fail=1; }
dropcaches() { sync; echo 3 | sudo tee /proc/sys/vm/drop_caches >/dev/null; }

# 6 GiB of deterministic pseudo-random bytes, regenerated to check.
gen() { openssl enc -aes-128-ctr -pass pass:icbfs -nosalt -pbkdf2 < <(head -c \$((6 << 30)) /dev/zero) 2>/dev/null; }
OFF=3000000000

t0=\$(date +%s)
want=\$(gen | sudo tee \$MNT/f | sha256sum | cut -d' ' -f1)
t1=\$(date +%s)
size=\$(stat -c %s \$MNT/f)
[ "\$size" = \$((6 << 30)) ] && pass "write 6 GiB (\$((t1 - t0))s)" || bad "write 6 GiB: size \$size"
dropcaches
got=\$(sudo sha256sum \$MNT/f | cut -d' ' -f1)
[ "\$got" = "\$want" ] && pass "read back 6 GiB: hash matches" || bad "read back 6 GiB: hash differs"

t0=\$(date +%s)
printf X | sudo dd of=\$MNT/f bs=1 seek=\$OFF conv=notrunc status=none
t1=\$(date +%s)
dropcaches
b=\$(sudo dd if=\$MNT/f bs=1 skip=\$OFF count=1 status=none)
[ "\$b" = X ] && pass "one-byte edit at 3 GB (\$((t1 - t0))s, including the full download on open)" || bad "one-byte edit: byte is '\$b'"
pre=\$(sudo head -c \$OFF \$MNT/f | sha256sum | cut -d' ' -f1)
post=\$(sudo tail -c +\$((OFF + 2)) \$MNT/f | sha256sum | cut -d' ' -f1)
wpre=\$(gen | head -c \$OFF | sha256sum | cut -d' ' -f1)
wpost=\$(gen | tail -c +\$((OFF + 2)) | sha256sum | cut -d' ' -f1)
[ "\$pre" = "\$wpre" ] && [ "\$post" = "\$wpost" ] && pass "bytes around the edit unchanged" || bad "bytes around the edit changed"

out=\$(sudo truncate -s \$(( (8 << 30) + 1 )) \$MNT/f 2>&1)
echo "\$out" | grep -q "File too large" && pass "truncate past 8 GiB: File too large" || bad "truncate past 8 GiB: \$out"
out=\$(printf Y | sudo dd of=\$MNT/f bs=1 seek=\$(( 8 << 30 )) conv=notrunc status=none 2>&1)
echo "\$out" | grep -q "File too large" && pass "write past 8 GiB: File too large" || bad "write past 8 GiB: \$out"
size=\$(stat -c %s \$MNT/f)
[ "\$size" = \$((6 << 30)) ] && pass "size unchanged after refused growth" || bad "size after refused growth: \$size"

sudo rm -f \$MNT/f
exit \$fail
EOF
