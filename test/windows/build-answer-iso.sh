#!/bin/bash
# Builds work/autounattend.iso from autounattend.xml. Windows Setup scans
# attached removable media for this filename automatically - no
# --location/PXE needed.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

mkdir -p "$WORK_DIR"
xorriso -as mkisofs -V "AUTOUNATTEND" -J -R -o "$WORK_DIR/autounattend.iso" "$HERE/autounattend.xml"
echo "built $WORK_DIR/autounattend.iso"
