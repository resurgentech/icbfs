#!/bin/bash
# Installs Go, WinFsp and winfsp-tests on a VM create-vm.sh made
# (setup-vm.ps1), restarts sshd so new SSH sessions see the new Path, and
# checks each is there. Then take the clean-base snapshot (README.md).
#
#   ./setup-vm.sh [ip]     (default: the VM's address)
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

GO_VERSION=1.27.3
WINFSP_VERSION=2.1
WINFSP_BUILD=2.1.25156
DOTNET_VERSION=10.0.402
LLVM_MINGW_VERSION=20260922

ip="${1:-$(vm_ip)}"
[ -n "$ip" ] || { echo "no address for $VM_NAME; is it running?" >&2; exit 1; }

scp_vm "$HERE/setup-vm.ps1" "Administrator@$ip:C:/Windows/Temp/setup-vm.ps1"
ssh_vm "$ip" "powershell -NoProfile -ExecutionPolicy Bypass -File C:\\Windows\\Temp\\setup-vm.ps1 -GoVersion $GO_VERSION -WinFspVersion $WINFSP_VERSION -WinFspBuild $WINFSP_BUILD -DotnetVersion $DOTNET_VERSION -LlvmMingwVersion $LLVM_MINGW_VERSION"
ssh_vm "$ip" "Restart-Service sshd" || true # drops this session; the next one sees the Path
sleep 3
ssh_vm "$ip" "go version; dotnet --version; gcc --version | Select-Object -First 1;(Get-Command launchctl-x64.exe).Source; (Get-ChildItem C:\\Windows\\Temp\\winfsp-tests -Recurse -Filter winfsp-tests-x64.exe).FullName"
