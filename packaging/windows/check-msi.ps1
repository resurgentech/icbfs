# Installs an icbfs MSI with every setting given, checks what it put in
# place (files, version resources, the registry key, the sign-in task),
# and uninstalls it, checking that everything went. The release
# workflow runs it on a GitHub runner; by hand, on a test VM, as an
# administrator, with WinFsp installed:
#
#   check-msi.ps1 -Msi C:\path\icbfs-0.1.1-x64.msi -Version 0.1.1 [-Tag v0.1.1] [-Signed] [-Arch x64|arm64]
#
# The commands' PE machine field must be this machine's own
# architecture (not what an emulated shell reports), and icbfs-cli
# -version must run and print. -Arch says what the MSI is built for
# (default: this machine's). An ARM64 MSI on an x64 machine can't be
# installed, so there the check is that Windows Installer refuses it
# (1633, ERROR_INSTALL_PLATFORM_UNSUPPORTED) and installs nothing, and
# that the two commands inside it (an administrative install, which
# runs nothing) are ARM64 PEs. An x64 MSI on an ARM64 machine is an
# error: it would install, emulated, and fail the PE check by design.
#
# -Tag is what the commands' -version should print (a build of the tag).
# -Signed checks that the MSI and the commands carry a signature; whether
# this machine trusts the certificate doesn't matter.
param(
  [Parameter(Mandatory)][string]$Msi,
  [Parameter(Mandatory)][string]$Version,
  [string]$Tag = '',
  [switch]$Signed,
  [string]$Arch = ''
)
$ErrorActionPreference = 'Stop'
$Msi = (Get-Item $Msi).FullName
$log = Join-Path $env:TEMP 'icbfs-check-msi.log'

function Check($what, $got, $want) {
  if ($got -ne $want) { throw "$($what): '$got', want '$want'" }
  "$($what): $got"
}
function Run-Msiexec($arguments) {
  Remove-Item $log -ErrorAction SilentlyContinue
  $p = Start-Process msiexec -ArgumentList "$arguments /qn /norestart /l*v `"$log`"" -Wait -PassThru
  if ($p.ExitCode -ne 0) {
    Get-Content $log | Select-Object -Last 60
    throw "msiexec $($arguments): exit $($p.ExitCode) (log: $log)"
  }
}

# Win32_Processor.Architecture is the machine's own: 9 x64, 12 ARM64.
# $env:PROCESSOR_ARCHITECTURE says what this process is, which under
# emulation isn't the machine.
function Get-NativeArch {
  switch ((Get-CimInstance Win32_Processor | Select-Object -First 1).Architecture) {
    9 { 'x64' }
    12 { 'arm64' }
    default { throw "unsupported processor architecture $_" }
  }
}
# The machine field of a PE: the two bytes after the "PE\0\0" the
# header offset at 0x3C points to.
function Get-PEArch($file) {
  $b = [IO.File]::ReadAllBytes($file)
  $pe = [BitConverter]::ToInt32($b, 0x3C)
  if ($b[$pe] -ne 0x50 -or $b[$pe + 1] -ne 0x45 -or $b[$pe + 2] -ne 0 -or $b[$pe + 3] -ne 0) { throw "$file isn't a PE file" }
  switch ([BitConverter]::ToUInt16($b, $pe + 4)) {
    0x8664 { 'x64' }
    0xAA64 { 'arm64' }
    default { '0x{0:X4}' -f [BitConverter]::ToUInt16($b, $pe + 4) }
  }
}

function Check-Signature($file) {
  $s = Get-AuthenticodeSignature $file
  if (-not $s.SignerCertificate) { throw "$(Split-Path -Leaf $file) isn't signed ($($s.Status))" }
  "$(Split-Path -Leaf $file): signed by $($s.SignerCertificate.Subject), $($s.Status)"
}

$native = Get-NativeArch
if (-not $Arch) { $Arch = $native }
"machine: $native, package: $Arch"

if ($Signed) {
  "--- signatures"
  Check-Signature $Msi
}

if ($Arch -ne $native) {
  if ($Arch -ne 'arm64' -or $native -ne 'x64') {
    throw "a $Arch package can't be checked on a $native machine: test an x64 package on x64 and an ARM64 one on ARM64"
  }
  if ((Test-Path HKLM:\SOFTWARE\resurgentech\icbfs) -or (Test-Path 'C:\Program Files\icbfs')) {
    throw 'icbfs is already installed here; uninstall it first, so that "installs nothing" means something'
  }
  "--- an ARM64 package is refused on x64"
  Remove-Item $log -ErrorAction SilentlyContinue
  $p = Start-Process msiexec -ArgumentList "/i `"$Msi`" /qn /norestart /l*v `"$log`"" -Wait -PassThru
  Check 'msiexec /i exit code' $p.ExitCode 1633
  if (Test-Path HKLM:\SOFTWARE\resurgentech\icbfs) { throw 'the refused package wrote the registry key' }
  if (Test-Path 'C:\Program Files\icbfs') { throw 'the refused package made the install folder' }
  if (Get-ScheduledTask -TaskName 'icbfs mount' -ErrorAction SilentlyContinue) { throw 'the refused package made the task' }
  "installed nothing"

  "--- its commands are ARM64"
  $dir = Join-Path $env:TEMP 'icbfs-check-msi-admin'
  Remove-Item $dir -Recurse -Force -ErrorAction SilentlyContinue
  try {
    Run-Msiexec "/a `"$Msi`" TARGETDIR=`"$dir`""
    foreach ($exe in 'icbfs-winfsp.exe', 'icbfs-cli.exe') {
      $f = Get-ChildItem $dir -Recurse -Filter $exe | Select-Object -First 1
      if (-not $f) { throw "$exe isn't in the package" }
      Check "$exe machine" (Get-PEArch $f.FullName) 'arm64'
    }
  } finally {
    Remove-Item $dir -Recurse -Force -ErrorAction SilentlyContinue
  }
  return
}

"--- install"
Run-Msiexec "/i `"$Msi`" ACCOUNT=example CONTAINER=docs FILESYSTEM=team DRIVELETTER=J"

"--- what it installed"
$key = Get-ItemProperty HKLM:\SOFTWARE\resurgentech\icbfs
Check 'InstallDir' $key.InstallDir 'C:\Program Files\icbfs\'
Check 'Version' $key.Version $Version
Check 'Account' $key.Account 'example'
Check 'Container' $key.Container 'docs'
Check 'Filesystem' $key.Filesystem 'team'
Check 'DriveLetter' $key.DriveLetter 'J'
foreach ($f in 'icbfs-winfsp.exe', 'icbfs-cli.exe', 'mount-task.ps1', 'LICENSE.txt', 'README.txt') {
  if (-not (Test-Path "$($key.InstallDir)$f")) { throw "$f isn't installed" }
}
foreach ($exe in 'icbfs-winfsp.exe', 'icbfs-cli.exe') {
  $info = (Get-Item "$($key.InstallDir)$exe").VersionInfo
  Check "$exe ProductName" $info.ProductName 'icbfs'
  Check "$exe ProductVersion" $info.ProductVersion $Version
  Check "$exe machine" (Get-PEArch "$($key.InstallDir)$exe") $native
  # Through a file: PowerShell doesn't capture a windowed program's
  # output (icbfs-winfsp), and -version attaches to no console here.
  $out = Join-Path $env:TEMP 'icbfs-version.txt'
  $run = Start-Process "$($key.InstallDir)$exe" -ArgumentList '-version' -RedirectStandardOutput $out -NoNewWindow -Wait -PassThru
  $v = "$(Get-Content $out -Raw)".Trim()
  if (-not $v) { throw "$exe -version printed nothing (exit $($run.ExitCode))" }
  if ($exe -eq 'icbfs-cli.exe' -and $run.ExitCode -ne 0) { throw "$exe -version exit $($run.ExitCode)" }
  if ($Tag) { Check "$exe -version" $v $Tag } else { "$($exe) -version: $v" }
  if ($Signed) { Check-Signature "$($key.InstallDir)$exe" }
}
$task = Get-ScheduledTask -TaskName 'icbfs mount'
Check 'task user' $task.Principal.GroupId 'Users'
Check 'task trigger' $task.Triggers[0].CimClass.CimClassName 'MSFT_TaskLogonTrigger'
Check 'task time limit' $task.Settings.ExecutionTimeLimit 'PT0S'
Check 'task program' $task.Actions[0].Execute 'C:\Program Files\icbfs\icbfs-winfsp.exe'
Check 'task arguments' $task.Actions[0].Arguments '-store azure -azure-account "example" -azure-container "docs" -fs "team" J:'

"--- repair without settings keeps them"
Run-Msiexec "/fa `"$Msi`""
$key = Get-ItemProperty HKLM:\SOFTWARE\resurgentech\icbfs
Check 'Account after repair' $key.Account 'example'
Check 'DriveLetter after repair' $key.DriveLetter 'J'
$task = Get-ScheduledTask -TaskName 'icbfs mount'
Check 'task arguments after repair' $task.Actions[0].Arguments '-store azure -azure-account "example" -azure-container "docs" -fs "team" J:'

"--- uninstall"
Run-Msiexec "/x `"$Msi`""
if (Test-Path HKLM:\SOFTWARE\resurgentech\icbfs) { throw 'the registry key is still there' }
if (Get-ScheduledTask -TaskName 'icbfs mount' -ErrorAction SilentlyContinue) { throw 'the task is still there' }
if (Test-Path 'C:\Program Files\icbfs') { throw 'the install folder is still there' }
"gone: the files, the task and the registry key"

"--- READONLY=1, kept by a repair without it"
Run-Msiexec "/i `"$Msi`" ACCOUNT=example READONLY=1"
$key = Get-ItemProperty HKLM:\SOFTWARE\resurgentech\icbfs
Check 'ReadOnly' $key.ReadOnly '1'
$ro = '-store azure -azure-account "example" -azure-container "icbfs" -fs "default" -read-only I:'
Check 'read-only task arguments' (Get-ScheduledTask -TaskName 'icbfs mount').Actions[0].Arguments $ro
Run-Msiexec "/fa `"$Msi`""
Check 'read-only task arguments after repair' (Get-ScheduledTask -TaskName 'icbfs mount').Actions[0].Arguments $ro
Run-Msiexec "/x `"$Msi`""
if (Get-ScheduledTask -TaskName 'icbfs mount' -ErrorAction SilentlyContinue) { throw 'the read-only task is still there' }
