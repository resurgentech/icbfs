#!/bin/bash
# Builds work/autounattend.iso from autounattend.xml, with this host's SSH
# key and the VM's Administrator password filled in. Windows Setup scans
# attached removable media for that filename by itself.
#
# The password is generated once, into work/vm-password (gitignored), and
# only Setup's automatic first logon uses it; the scripts log in with the
# SSH key.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

mkdir -p "$WORK_DIR"
if [ ! -s "$PASSWORD_FILE" ]; then
  # Windows' default policy wants three of: upper, lower, digit, symbol.
  (umask 077; printf 'Aa1-%s\n' "$(tr -dc 'A-Za-z0-9' </dev/urandom | head -c 20)" >"$PASSWORD_FILE")
fi
[ -r "$SSH_PUBLIC_KEY" ] || { echo "no SSH public key at $SSH_PUBLIC_KEY" >&2; exit 1; }

password="$(cat "$PASSWORD_FILE")"
key="$(cat "$SSH_PUBLIC_KEY")"
python3 - "$HERE/autounattend.xml" "$WORK_DIR/autounattend.xml" "$password" "$key" <<'PY'
import sys
from xml.sax.saxutils import escape
src, dst, password, key = sys.argv[1:]
text = open(src).read()
text = text.replace("@ADMIN_PASSWORD@", escape(password)).replace("@SSH_PUBLIC_KEY@", escape(key))
open(dst, "w").write(text)
PY
chmod 600 "$WORK_DIR/autounattend.xml"
xorriso -as mkisofs -V "AUTOUNATTEND" -J -R -o "$WORK_DIR/autounattend.iso" "$WORK_DIR/autounattend.xml"
echo "built $WORK_DIR/autounattend.iso"
