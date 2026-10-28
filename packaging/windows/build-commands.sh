#!/bin/bash
# Builds the Windows commands the package holds, from the checkout this
# runs in, for both architectures: their version resources (go-winres:
# product name icbfs, VERSION as X.Y.Z), then icbfs-winfsp as a windowed
# program and icbfs-cli, into bin/x64 and bin/arm64. Cross-compiles from
# any OS; from git, so -version says which build each is. package.sh and
# the release workflow both build the commands with this.
#
#   packaging/windows/build-commands.sh 0.1.1
set -euo pipefail
version="${1:-}"
[[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "build-commands: want a version, X.Y.Z" >&2; exit 2; }
command -v go-winres >/dev/null || { echo "go-winres isn't on PATH: go install github.com/tc-hib/go-winres@v0.3.3" >&2; exit 1; }
cd "$(dirname "${BASH_SOURCE[0]}")/../.."
for c in icbfs-winfsp icbfs-cli; do
  go-winres make --in "cmd/$c/winres/winres.json" --out "cmd/$c/rsrc" --arch amd64,arm64 --product-version "$version" --file-version "$version"
done
for arch in amd64:x64 arm64:arm64; do
  goarch="${arch%:*}"; msiarch="${arch#*:}"
  GOOS=windows GOARCH="$goarch" go build -trimpath -ldflags "-s -w -H=windowsgui" -o "bin/$msiarch/icbfs-winfsp.exe" ./cmd/icbfs-winfsp
  GOOS=windows GOARCH="$goarch" go build -trimpath -ldflags "-s -w" -o "bin/$msiarch/icbfs-cli.exe" ./cmd/icbfs-cli
done
