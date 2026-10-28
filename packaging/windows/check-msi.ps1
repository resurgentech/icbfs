# Installs an icbfs MSI with every setting given, checks what it put in
# place (files, version resources, the registry key, the sign-in task),
# and uninstalls it, checking that everything went. The release
# workflow runs it on a GitHub runner; by hand, on a test VM, as an
# administrator, with WinFsp installed:
#
#   check-msi.ps1 -Msi C:\path\icbfs-0.1.1-x64.msi -Version 0.1.1 [-Tag v0.1.1]
#
# -Tag is what the commands' -version should print (a build of the tag).
param(
  [Parameter(Mandatory)][string]$Msi,
  [Parameter(Mandatory)][string]$Version,
  [string]$Tag = ''
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
  # Through a file: PowerShell doesn't capture a windowed program's
  # output (icbfs-winfsp), and -version attaches to no console here.
  $out = Join-Path $env:TEMP 'icbfs-version.txt'
  Start-Process "$($key.InstallDir)$exe" -ArgumentList '-version' -RedirectStandardOutput $out -NoNewWindow -Wait
  $v = (Get-Content $out -Raw).Trim()
  if ($Tag) { Check "$exe -version" $v $Tag } else { "$($exe) -version: $v" }
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
