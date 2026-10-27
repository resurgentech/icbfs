# Starts a program on the logged-on console desktop (auto-logon), not in
# this SSH session, which has no desktop: a scheduled task set to run only
# while the user is logged on runs in that session. Needs no password.
# -User names another logged-on user than Administrator.
param(
  [Parameter(Mandatory)][string]$Exe,
  [string]$Arguments = '',
  [string]$User = 'Administrator'
)
$ErrorActionPreference = 'Stop'
$name = 'icbfs-run-in-desktop'
$action = if ($Arguments) { New-ScheduledTaskAction -Execute $Exe -Argument $Arguments } else { New-ScheduledTaskAction -Execute $Exe }
$principal = New-ScheduledTaskPrincipal -UserId $User -LogonType Interactive -RunLevel Highest
$settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit 0 -AllowStartIfOnBatteries
Register-ScheduledTask $name -Action $action -Principal $principal -Settings $settings -Force | Out-Null
Start-ScheduledTask $name
Start-Sleep 2
Unregister-ScheduledTask $name -Confirm:$false # leaves the program running
