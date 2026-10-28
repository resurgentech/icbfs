#!/bin/bash
# Builds icbfs's Windows package into dist/: icbfs-<version>-x64.msi and
# icbfs-<version>-arm64.msi, signed. The commands are built here, from
# git (cross-compiled, so they say which build they are), with their
# version resources; the signing and the MSIs are done on the Windows
# VM (build.ps1), with the key sent over SSH's stdin, never on a disk.
#
#   packaging/windows/package.sh            HEAD's tag, vX.Y.Z, built from a clean checkout of it
#   packaging/windows/package.sh X.Y.Z      this working tree, as X.Y.Z (a build to try, not to ship)
#
# The key, by ICBFS_CODESIGN:
#   bao         (default) from OpenBao: bao login first; BAO_ADDR, and the
#               KV mount and path in ICBFS_CODESIGN_BAO_MOUNT (kv) and
#               ICBFS_CODESIGN_BAO_PATH, fields pfx (PKCS#12, base64) and password
#   pfx         ICBFS_CODESIGN_PFX names a PFX file, ICBFS_CODESIGN_PASSWORD its password
#   none        unsigned
#
# Needs go-winres (go install github.com/tc-hib/go-winres@v0.3.3) here,
# and the .NET 10 SDK on the VM. test/windows/lib.sh does the SSH; the
# VM is ICBFS_WIN_VM, icbfs-winfsp-test by default.
set -euo pipefail
SELF="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SELF/../.." && pwd)"
# shellcheck disable=SC1091
source "$ROOT/test/windows/lib.sh"

codesign_key() {
  case "${ICBFS_CODESIGN:-bao}" in
    none) ;;
    pfx)
      [ -n "${ICBFS_CODESIGN_PFX:-}" ] || { echo "ICBFS_CODESIGN=pfx: ICBFS_CODESIGN_PFX names the file" >&2; return 1; }
      base64 -w0 "$ICBFS_CODESIGN_PFX"
      printf '\n%s\n' "${ICBFS_CODESIGN_PASSWORD:-}" ;;
    bao)
      local mount="${ICBFS_CODESIGN_BAO_MOUNT:-kv}" path="${ICBFS_CODESIGN_BAO_PATH:-}" pfx password
      [ -n "$path" ] || { echo "ICBFS_CODESIGN_BAO_PATH: where the key is in OpenBao (or ICBFS_CODESIGN=none for an unsigned build)" >&2; return 1; }
      pfx="$(bao kv get -mount="$mount" -field=pfx "$path")" \
        || { echo "no signing key from OpenBao (${BAO_ADDR:-BAO_ADDR unset}, $mount/$path): bao login, or ICBFS_CODESIGN=none" >&2; return 1; }
      password="$(bao kv get -mount="$mount" -field=password "$path")"
      printf '%s\n%s\n' "$pfx" "$password" ;;
    *) echo "ICBFS_CODESIGN: bao, pfx or none" >&2; return 1 ;;
  esac
}

command -v go-winres >/dev/null || { echo "go-winres isn't on PATH: go install github.com/tc-hib/go-winres@v0.3.3" >&2; exit 1; }
ip="$(vm_ip || true)" # with the VM down, virsh fails and set -e would end this before the message
[ -n "$ip" ] || { echo "no address for $VM_NAME; is it running?" >&2; exit 1; }

# The key first, so a missing one stops this before the build.
key="$(codesign_key)"

# What to build: a clean checkout of HEAD's tag, or this tree as told.
src="$ROOT"
cleanup() { :; }
if [ $# -gt 0 ]; then
  version="$1"
  [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "package: $version: want X.Y.Z" >&2; exit 1; }
  echo "building this working tree as $version (not a release: its commands say which commit they are)" >&2
else
  tag="$(cd "$ROOT" && git describe --tags --exact-match HEAD 2>/dev/null || true)"
  [[ "$tag" =~ ^v([0-9]+\.[0-9]+\.[0-9]+)$ ]] || { echo "package: HEAD has no vX.Y.Z tag; a version to build the working tree as, or just release first" >&2; exit 1; }
  version="${BASH_REMATCH[1]}"
  src="$(mktemp -d)"
  cleanup() { (cd "$ROOT" && git worktree remove --force "$src") 2>/dev/null || true; }
  trap cleanup EXIT
  (cd "$ROOT" && git worktree add -q --detach "$src" "$tag")
  echo "building $tag from a clean checkout" >&2
fi

# The commands, with their version resources, both architectures.
(
  cd "$src"
  for c in icbfs-winfsp icbfs-cli; do
    go-winres make --in "cmd/$c/winres/winres.json" --out "cmd/$c/rsrc" --arch amd64,arm64 --product-version "$version" --file-version "$version"
  done
  for arch in amd64:x64 arm64:arm64; do
    goarch="${arch%:*}"; msiarch="${arch#*:}"
    GOOS=windows GOARCH="$goarch" go build -trimpath -ldflags "-s -w -H=windowsgui" -o "bin/$msiarch/icbfs-winfsp.exe" ./cmd/icbfs-winfsp
    GOOS=windows GOARCH="$goarch" go build -trimpath -ldflags "-s -w" -o "bin/$msiarch/icbfs-cli.exe" ./cmd/icbfs-cli
  done
)

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
