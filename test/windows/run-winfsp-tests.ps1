param(
    [Parameter(Mandatory=$true)][ValidateSet("icbfs-ci","icbfs-cs","cmemfs-ci","cmemfs-cs")][string]$Target,
    [string]$Endpoint = "",
    [string]$Bucket = "",
    [string]$AccessKey = "",
    [string]$SecretKey = "",
    [string]$Exe = "C:\icbfs-test\icbfs-winfsp.exe",
    [string]$Cli = "C:\icbfs-test\icbfs-cli.exe",
    [string]$Extra = "",
    [string]$Only = "",
    [string]$Out = "C:\Windows\Temp\winfsp-tests-result.txt",
    # Test filesystems hold files up to 8 GiB, not the 1 TiB default:
    # testable, and big enough to need multipart uploads.
    [uint64]$MaxFileSize = 8GB
)
# VM side of run-winfsp-tests.sh — copied to the VM and run there.
#
# Runs every winfsp-tests test against $Target, one fresh mount per test
# (a fresh icbfs filesystem name each time, so no test sees another's
# leftovers), cwd on the mount, --external --resilient --no-abort, 180s
# timeout per test.
#
#   icbfs-ci   icbfs, case-insensitive (the primary-Windows default)
#   icbfs-cs   icbfs, -case-insensitive=false
#   cmemfs-ci  WinFsp's own C memfs as installed (launchctl memfs64), the
#              configuration WinFsp's CI runs it in — the baseline
#   cmemfs-cs  the same memfs, started case-sensitive
#
# The cmemfs targets need no storage; the icbfs ones need -Endpoint,
# -Bucket and the keys (run-winfsp-tests.sh passes a throwaway MinIO's).
$ErrorActionPreference = "Continue"
$drive = "M:"
$bin = (Get-ChildItem 'C:\Program Files (x86)\WinFsp\SxS' -Directory | Sort-Object Name | Select-Object -Last 1).FullName + "\bin"
$tests = "C:\Windows\Temp\winfsp-tests\winfsp-tests-x64.exe"
$names = & $tests --list | Where-Object { $_.Trim() -ne "" }
if ($Only -ne "") { $names = $Only -split "," }
if ($Target -like "icbfs-*" -and ($Endpoint -eq "" -or $Bucket -eq "")) {
    Write-Error "$Target needs -Endpoint and -Bucket"
    exit 2
}
Remove-Item $Out -ErrorAction SilentlyContinue

function Stop-Fs {
    Get-Process memfs-x64,icbfs-winfsp -ErrorAction SilentlyContinue | Stop-Process -Force
    & "$bin\launchctl-x64.exe" stop memfs64 testdsk *> $null
    Start-Sleep -Milliseconds 700
}

function Start-Fs([string]$t) {
    $log = @{ RedirectStandardError = "C:\Windows\Temp\fs-err.log"; RedirectStandardOutput = "C:\Windows\Temp\fs-out.log" }
    switch ($Target) {
        "cmemfs-ci" { & "$bin\launchctl-x64.exe" start memfs64 testdsk '""' $drive *> $null }
        "cmemfs-cs" { Start-Process "$bin\memfs-x64.exe" -ArgumentList @("-F","NTFS","-n","65536","-s","67108864","-m",$drive) -WindowStyle Hidden @log | Out-Null }
        default {
            $ci = if ($Target -eq "icbfs-ci") { "true" } else { "false" }
            $fs = "w-" + $t.Substring(0, [Math]::Min(30, $t.Length)) + "-" + (Get-Random)
            $store = @("-endpoint",$Endpoint,"-bucket",$Bucket,"-access-key",$AccessKey,"-secret-key",$SecretKey)
            # Mounts never create a filesystem. Created here, on Windows,
            # the root gets this account's default security descriptor.
            & $Cli fs create @store -fs $fs -primary-windows -max-file-size $MaxFileSize *> C:\Windows\Temp\fs-create.log
            Start-Process $Exe -WindowStyle Hidden @log -ArgumentList ($store + @("-fs",$fs,"-case-insensitive=$ci",$drive)) | Out-Null
        }
    }
    for ($i = 0; $i -lt 40 -and -not (Test-Path "$drive\"); $i++) { Start-Sleep -Milliseconds 250 }
}

foreach ($t in $names) {
    Stop-Fs
    Start-Fs $t
    if (-not (Test-Path "$drive\")) { Add-Content $Out "$t ... MOUNT-FAILED"; continue }
    Set-Location "$drive\"
    $targs = @("--external","--resilient","--no-abort")
    if ($Extra -ne "") { $targs += ($Extra -split " ") }
    $targs += $t
    $tp = Start-Process $tests -ArgumentList $targs -PassThru -NoNewWindow `
        -RedirectStandardOutput C:\Windows\Temp\t-out.txt -RedirectStandardError C:\Windows\Temp\t-err.txt
    if (-not $tp.WaitForExit(180000)) {
        Set-Location C:\
        Stop-Fs
        $tp | Stop-Process -Force -ErrorAction SilentlyContinue
        Add-Content $Out "$t ... TIMEOUT"
        continue
    }
    Get-Content C:\Windows\Temp\t-out.txt, C:\Windows\Temp\t-err.txt -ErrorAction SilentlyContinue |
        Where-Object { $_ -notmatch "COMPLETE" -and $_.Trim() -ne "" } | Add-Content $Out
    Set-Location C:\
}
Stop-Fs
Get-Content $Out
