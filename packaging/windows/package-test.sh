#!/bin/bash
# Checks a package package.sh built, on the Windows VM (check-msi.ps1
# there): the install with every setting, what it put in place, a
# repair, the uninstall; and, when ICBFS_CODESIGN_PFX is set as it was
# for the build, that the commands and the MSI are signed.
#
#   packaging/windows/package-test.sh                HEAD's tag's package, dist/icbfs-X.Y.Z-x64.msi
#   packaging/windows/package-test.sh X.Y.Z          that version's
#   packaging/windows/package-test.sh [X.Y.Z] arm64  the ARM64 package
#
# The arch is x64 by default; a lone x64 or arm64 is the arch, with
# HEAD's tag's version. It is held against the machine's own, asked of
# the VM over SSH. The same: the full check. An ARM64 package on an x64
# machine: that Windows Installer refuses it (1633), that it installs
# nothing, and that its two commands are ARM64 PEs. An x64 package on
# an ARM64 machine is an error (it would run emulated): test the x64
# package on an x64 machine.
set -euo pipefail
SELF="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SELF/../.." && pwd)"
# shellcheck disable=SC1091
source "$ROOT/test/windows/lib.sh"

usage="usage: package-test.sh [X.Y.Z] [x64|arm64]"
version=""
arch=x64
case $# in
  0) ;;
  1) if [[ "$1" =~ ^(x64|arm64)$ ]]; then arch="$1"; else version="$1"; fi ;;
  2) version="$1"; arch="$2" ;;
  *) echo "$usage" >&2; exit 1 ;;
esac
[[ "$arch" =~ ^(x64|arm64)$ ]] || { echo "$usage" >&2; exit 1; }

tag=""
if [ -z "$version" ]; then
  tag="$(cd "$ROOT" && git describe --tags --exact-match HEAD 2>/dev/null || true)"
  [[ "$tag" =~ ^v([0-9]+\.[0-9]+\.[0-9]+)$ ]] || { echo "package-test: HEAD has no vX.Y.Z tag; which version?" >&2; exit 1; }
  version="${BASH_REMATCH[1]}"
fi
msi="icbfs-$version-$arch.msi"
[ -f "$ROOT/dist/$msi" ] || { echo "no dist/$msi: just package first" >&2; exit 1; }
ip="$(vm_ip || true)" # with the VM down, virsh fails and set -e would end this before the message
[ -n "$ip" ] || { echo "no address for $VM_NAME; is it running?" >&2; exit 1; }

# The machine's own architecture (Win32_Processor: 9 x64, 12 ARM64), not
# what an emulated shell reports.
host="$(ssh_vm "$ip" "(Get-CimInstance Win32_Processor | Select-Object -First 1).Architecture" 2>/dev/null | tr -d '\r' | tail -1)"
case "$host" in
  9) host=x64 ;;
  12) host=arm64 ;;
  *) echo "package-test: can't tell $VM_NAME's architecture (got '$host')" >&2; exit 1 ;;
esac
if [ "$arch" = x64 ] && [ "$host" = arm64 ]; then
  echo "package-test: $VM_NAME is ARM64 and would run the x64 package emulated; test that on an x64 machine" >&2
  exit 1
fi
echo "package-test: $VM_NAME is $host, package $arch"

# PowerShell over SSH drops an empty argument, so -Tag only with one.
extra=""
[ -z "$tag" ] || extra+=" -Tag $tag"
[ -z "${ICBFS_CODESIGN_PFX:-}" ] || extra+=" -Signed"
scp_vm "$ROOT/dist/$msi" "$SELF/check-msi.ps1" "Administrator@$ip:C:/Windows/Temp/"
ssh_vm "$ip" "powershell -NoProfile -ExecutionPolicy Bypass -File C:\\Windows\\Temp\\check-msi.ps1 -Msi C:\\Windows\\Temp\\$msi -Version $version -Arch $arch$extra" 2>&1 \
  | grep -v -i 'warning\|post-quantum\|store now\|upgraded'
