#!/bin/bash
# Checks a package package.sh built, on the Windows VM (check-msi.ps1
# there): the install with every setting, what it put in place, a
# repair, the uninstall; and, when ICBFS_CODESIGN_PFX is set as it was
# for the build, that the commands and the MSI are signed.
#
#   packaging/windows/package-test.sh            HEAD's tag's package, dist/icbfs-X.Y.Z-x64.msi
#   packaging/windows/package-test.sh X.Y.Z      that version's
set -euo pipefail
SELF="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SELF/../.." && pwd)"
# shellcheck disable=SC1091
source "$ROOT/test/windows/lib.sh"

tag=""
if [ $# -gt 0 ]; then
  version="$1"
else
  tag="$(cd "$ROOT" && git describe --tags --exact-match HEAD 2>/dev/null || true)"
  [[ "$tag" =~ ^v([0-9]+\.[0-9]+\.[0-9]+)$ ]] || { echo "package-test: HEAD has no vX.Y.Z tag; which version?" >&2; exit 1; }
  version="${BASH_REMATCH[1]}"
fi
msi="icbfs-$version-x64.msi"
[ -f "$ROOT/dist/$msi" ] || { echo "no dist/$msi: just package first" >&2; exit 1; }
ip="$(vm_ip || true)" # with the VM down, virsh fails and set -e would end this before the message
[ -n "$ip" ] || { echo "no address for $VM_NAME; is it running?" >&2; exit 1; }

# PowerShell over SSH drops an empty argument, so -Tag only with one.
extra=""
[ -z "$tag" ] || extra+=" -Tag $tag"
[ -z "${ICBFS_CODESIGN_PFX:-}" ] || extra+=" -Signed"
scp_vm "$ROOT/dist/$msi" "$SELF/check-msi.ps1" "Administrator@$ip:C:/Windows/Temp/"
ssh_vm "$ip" "powershell -NoProfile -ExecutionPolicy Bypass -File C:\\Windows\\Temp\\check-msi.ps1 -Msi C:\\Windows\\Temp\\$msi -Version $version$extra" 2>&1 \
  | grep -v -i 'warning\|post-quantum\|store now\|upgraded'
