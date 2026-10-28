# The icbfs installer's sign-in task: registers (or removes) the
# scheduled task that mounts the drive when a user signs in. The
# installer runs this as the machine, from the install folder;
# docs/mount.md, "The Windows package", says what the task does.
#
#   mount-task.ps1 -Register -Exe 'C:\Program Files\icbfs\icbfs-winfsp.exe' -Account ACCOUNT [-Container icbfs] [-Filesystem default] [-DriveLetter I]
#   mount-task.ps1 -Unregister
#
# The task runs in the signing-in user's own session (the drive is
# theirs, and a token command may have to sign them in), as any member
# of Users, without a time limit, and starts nothing at install: the
# first mount is at the next sign-in. The mount's credential is the
# user's own (ICBFS_AZURE_KEY in their environment, or the machine's
# ICBFS_AZURE_TOKEN_COMMAND); this file holds none.
param(
  [switch]$Register,
  [switch]$Unregister,
  [string]$Exe,
  [string]$Account,
  [string]$Container = 'icbfs',
  [string]$Filesystem = 'default',
  [string]$DriveLetter = 'I'
)
$ErrorActionPreference = 'Stop'
$name = 'icbfs mount'

if ($Unregister) {
  Unregister-ScheduledTask -TaskName $name -Confirm:$false -ErrorAction SilentlyContinue
  exit 0
}
if (-not $Register) {
  Write-Error 'mount-task.ps1: -Register or -Unregister'
  exit 2
}
if (-not $Exe -or -not $Account) {
  Write-Error 'mount-task.ps1 -Register: -Exe and -Account are needed'
  exit 2
}
$letter = $DriveLetter.TrimEnd(':')
if ($letter -notmatch '^[A-Za-z]$') {
  Write-Error "mount-task.ps1: drive letter '$DriveLetter': one letter"
  exit 2
}

$arguments = "-store azure -azure-account `"$Account`" -azure-container `"$Container`" -fs `"$Filesystem`" ${letter}:"
$action = New-ScheduledTaskAction -Execute $Exe -Argument $arguments
$trigger = New-ScheduledTaskTrigger -AtLogOn
$principal = New-ScheduledTaskPrincipal -GroupId 'BUILTIN\Users' -RunLevel Limited
# No time limit (the mount lasts the session), on battery too, and one
# instance per signing-in user.
$settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit ([TimeSpan]::Zero) `
  -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -MultipleInstances Parallel -StartWhenAvailable
Register-ScheduledTask -TaskName $name -Action $action -Trigger $trigger -Principal $principal -Settings $settings `
  -Description "Mounts the icbfs filesystem '$Filesystem' (account $Account, container $Container) as drive ${letter}: at sign-in. Installed by icbfs; removed with it." `
  -Force | Out-Null
