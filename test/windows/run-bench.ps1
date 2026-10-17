param(
    [Parameter(Mandatory=$true)][string]$Endpoint,
    [Parameter(Mandatory=$true)][string]$Bucket,
    [string]$AccessKey = "",
    [string]$SecretKey = "",
    [string]$Exe = "C:\icbfs-test\icbfs-winfsp.exe",
    [string]$Cli = "C:\icbfs-test\icbfs-cli.exe",
    # Each comma-separated value is one mount's -meta-cache; each gets its
    # own fresh filesystem and runs every step.
    [string]$MetaCaches = "0,1s",
    [int]$Files = 1000,
    [int]$Reads = 200,
    [string]$Out = "C:\Windows\Temp\bench-result.txt"
)
# VM side of run-bench.sh: what Explorer and Office do to a mount, timed.
# One line per step: "<meta-cache> <step> <milliseconds>", then the mount
# process's peak working set.
$ErrorActionPreference = "Stop"
$drive = "M:"
Remove-Item $Out -ErrorAction SilentlyContinue

function Stop-Fs {
    Get-Process icbfs-winfsp -ErrorAction SilentlyContinue | Stop-Process -Force
    Start-Sleep -Milliseconds 700
}

function Step([string]$cache, [string]$name, [scriptblock]$body) {
    $sw = [Diagnostics.Stopwatch]::StartNew()
    try {
        & $body | Out-Null
        $sw.Stop()
        $line = "{0} {1} {2}" -f $cache, $name, [int]$sw.Elapsed.TotalMilliseconds
    } catch {
        $line = "{0} {1} ERROR {2}" -f $cache, $name, $_.Exception.Message
    }
    Add-Content $Out $line
    Write-Output $line
}

$store = @("-endpoint",$Endpoint,"-bucket",$Bucket,"-access-key",$AccessKey,"-secret-key",$SecretKey)
foreach ($cache in $MetaCaches -split ",") {
    Stop-Fs
    $fs = "bench-" + (Get-Random)
    & $Cli fs create @store -fs $fs -primary-windows *> C:\Windows\Temp\bench-create.log
    $p = Start-Process $Exe -PassThru -WindowStyle Hidden -RedirectStandardError C:\Windows\Temp\bench-err.log -RedirectStandardOutput C:\Windows\Temp\bench-out.log `
        -ArgumentList ($store + @("-fs",$fs,"-meta-cache",$cache,$drive))
    for ($i = 0; $i -lt 40 -and -not (Test-Path "$drive\"); $i++) { Start-Sleep -Milliseconds 250 }
    if (-not (Test-Path "$drive\")) { Add-Content $Out "$cache MOUNT-FAILED"; continue }

    $dir = "$drive\folder"
    New-Item -ItemType Directory $dir | Out-Null
    Step $cache "create-$Files-files" { for ($i = 0; $i -lt $Files; $i++) { [IO.File]::WriteAllText("$dir\file$i.txt", "content $i") } }
    Step $cache "list-folder-first" { Get-ChildItem $dir | Measure-Object }
    Step $cache "list-folder-again" { Get-ChildItem $dir | Measure-Object }
    Step $cache "details-of-every-file" { Get-ChildItem $dir | ForEach-Object { $_.Length + $_.LastWriteTime.Ticks } }
    Step $cache "open-read-$Reads-files" { for ($i = 0; $i -lt $Reads; $i++) { [IO.File]::ReadAllText("$dir\file$i.txt") } }
    Step $cache "open-read-$Reads-again" { for ($i = 0; $i -lt $Reads; $i++) { [IO.File]::ReadAllText("$dir\file$i.txt") } }

    $deep = "$drive\a\b\c\d\e\f"
    New-Item -ItemType Directory $deep -Force | Out-Null
    [IO.File]::WriteAllText("$deep\deep.txt", "deep")
    Step $cache "open-read-deep-100x" { for ($i = 0; $i -lt 100; $i++) { [IO.File]::ReadAllText("$deep\deep.txt") } }
    Step $cache "probe-missing-names-200x" { for ($i = 0; $i -lt 200; $i++) { Test-Path "$dir\desktop.ini" } }

    # Office's save: write a temporary file, replace the document with it.
    [IO.File]::WriteAllText("$drive\doc.docx", "v0")
    Step $cache "office-save-50x" {
        for ($i = 0; $i -lt 50; $i++) {
            [IO.File]::WriteAllText("$drive\~WRL$i.tmp", "version $i")
            [IO.File]::Replace("$drive\~WRL$i.tmp", "$drive\doc.docx", [NullString]::Value)
        }
    }
    $src = "C:\Windows\Temp\bench-src"
    if (-not (Test-Path $src)) {
        New-Item -ItemType Directory $src | Out-Null
        for ($i = 0; $i -lt 100; $i++) { [IO.File]::WriteAllBytes("$src\f$i.bin", (New-Object byte[] 65536)) }
    }
    Step $cache "copy-in-100-files" { Copy-Item $src "$drive\copied" -Recurse }

    $p.Refresh()
    $mb = [int]($p.PeakWorkingSet64 / 1MB)
    Add-Content $Out ("{0} peak-working-set-mb {1}" -f $cache, $mb)
    Write-Output ("{0} peak-working-set-mb {1}" -f $cache, $mb)
}
Stop-Fs
