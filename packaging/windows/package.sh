#!/bin/bash
# Builds icbfs's Windows package here, into dist/: icbfs-<version>-x64.msi
# and icbfs-<version>-arm64.msi. The commands are built here
# (build-commands.sh); the MSIs on the Windows test VM (build.ps1). This
# is for trying the package out: a release's MSIs come from the release
# workflow (.github/workflows/release.yml), built on GitHub's runners
# and signed by SignPath. This build is unsigned, unless a key is given
# to sign it with: ICBFS_CODESIGN_PFX names a PFX file
# (ICBFS_CODESIGN_PASSWORD its password), or is "-" for the PFX in
# base64 and its password, a line each, on standard input. The key goes
# to the VM over SSH's stdin, never onto its disk.
#
#   packaging/windows/package.sh            HEAD's tag, vX.Y.Z, built from a clean checkout of it
#   packaging/windows/package.sh X.Y.Z      this working tree, as X.Y.Z
#
# Needs go-winres (go install github.com/tc-hib/go-winres@v0.3.3) here,
# and the .NET 10 SDK on the VM. test/windows/lib.sh does the SSH; the
# VM is ICBFS_WIN_VM, icbfs-winfsp-test by default.
set -euo pipefail
SELF="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SELF/../.." && pwd)"
# shellcheck disable=SC1091
source "$ROOT/test/windows/lib.sh"

ip="$(vm_ip || true)" # with the VM down, virsh fails and set -e would end this before the message
[ -n "$ip" ] || { echo "no address for $VM_NAME; is it running?" >&2; exit 1; }

key=""
case "${ICBFS_CODESIGN_PFX:-}" in
  "") ;;
  -) key="$(cat)"
     [ -n "$key" ] || { echo "ICBFS_CODESIGN_PFX=-: the key (the PFX in base64, then its password) on standard input" >&2; exit 1; } ;;
  *) key="$(base64 -w0 "$ICBFS_CODESIGN_PFX"; printf '\n%s' "${ICBFS_CODESIGN_PASSWORD:-}")" ;;
esac

# What to build: a clean checkout of HEAD's tag, or this tree as told.
src="$ROOT"
cleanup() { :; }
if [ $# -gt 0 ]; then
  version="$1"
  [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "package: $version: want X.Y.Z" >&2; exit 1; }
  echo "building this working tree as $version (its commands say which commit they are)" >&2
else
  tag="$(cd "$ROOT" && git describe --tags --exact-match HEAD 2>/dev/null || true)"
  [[ "$tag" =~ ^v([0-9]+\.[0-9]+\.[0-9]+)$ ]] || { echo "package: HEAD has no vX.Y.Z tag; give a version to build the working tree as" >&2; exit 1; }
  version="${BASH_REMATCH[1]}"
  src="$(mktemp -d)"
  cleanup() { (cd "$ROOT" && git worktree remove --force "$src") 2>/dev/null || true; }
  trap cleanup EXIT
  (cd "$ROOT" && git worktree add -q --detach "$src" "$tag")
  echo "building $tag from a clean checkout" >&2
fi

"$src/packaging/windows/build-commands.sh" "$version"

# To the VM: the commands and the package's files; the MSIs back.
dir='C:\icbfs-pkg'
mkdir -p "$WORK_DIR" "$ROOT/dist"
tar="$WORK_DIR/icbfs-pkg.tar"
(cd "$src" && tar -cf "$tar" bin/x64 bin/arm64 LICENSE packaging/windows)
sync_vm_clock "$ip"
scp_vm "$tar" "Administrator@$ip:C:/Windows/Temp/icbfs-pkg.tar"
ssh_vm "$ip" "if (Test-Path $dir) { Remove-Item $dir -Recurse -Force }; New-Item -ItemType Directory $dir | Out-Null; tar -xf C:\\Windows\\Temp\\icbfs-pkg.tar -C $dir"
printf '%s\n' "$key" | ssh_vm "$ip" "powershell -NoProfile -ExecutionPolicy Bypass -File $dir\\packaging\\windows\\build.ps1 -Version $version" 2>&1 \
  | grep -v -i 'warning\|post-quantum\|store now\|upgraded'
for arch in x64 arm64; do
  scp_vm "Administrator@$ip:C:/icbfs-pkg/dist/icbfs-$version-$arch.msi" "$ROOT/dist/"
done
ssh_vm "$ip" "Remove-Item $dir -Recurse -Force"
ls -l "$ROOT"/dist/icbfs-"$version"-*.msi
