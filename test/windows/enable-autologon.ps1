# Logs Administrator on at the console at every boot, so there's a desktop
# for GUI programs (started into it with a scheduled task) and for virsh
# screenshot. Run by enable-autologon.sh, which copies the password here as
# a file, so it's never on a command line. Deletes that file.
param([Parameter(Mandatory)][string]$PasswordFile)
$ErrorActionPreference = 'Stop'
try {
  $k = 'HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Winlogon'
  Set-ItemProperty $k AutoAdminLogon '1'
  Set-ItemProperty $k DefaultUserName 'Administrator'
  Set-ItemProperty $k DefaultDomainName $env:COMPUTERNAME
  Set-ItemProperty $k DefaultPassword (Get-Content $PasswordFile -Raw).Trim()
  # With a count, Windows turns auto-logon off once it runs out.
  Remove-ItemProperty $k AutoLogonCount -ErrorAction SilentlyContinue
} finally {
  Remove-Item $PasswordFile -Force
}
'auto-logon on'
