# Starts a program on the logged-on console desktop (auto-logon), not in
# this SSH session, which has no desktop: a scheduled task set to run only
# while Administrator is logged on runs in that session. Needs no password.
param(
  [Parameter(Mandatory)][string]$Exe,
  [string]$Arguments = ''
)
$ErrorActionPreference = 'Stop'
$name = 'icbfs-run-in-desktop'
$action = if ($Arguments) { New-ScheduledTaskAction -Execute $Exe -Argument $Arguments } else { New-ScheduledTaskAction -Execute $Exe }
$principal = New-ScheduledTaskPrincipal -UserId Administrator -LogonType Interactive -RunLevel Highest
$settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit 0 -AllowStartIfOnBatteries
Register-ScheduledTask $name -Action $action -Principal $principal -Settings $settings -Force | Out-Null
Start-ScheduledTask $name
Start-Sleep 2
Unregister-ScheduledTask $name -Confirm:$false # leaves the program running
