# The Windows copy tests, run as Administrator on the Windows test VM
# (run-windows.sh puts it there): each tool copies the test tree
# (tree.json, built fresh before every copy) onto NTFS and onto a fresh
# icbfs mount, and metatree compares the two copies. A difference is
# something icbfs keeps differently from NTFS.
param(
    [Parameter(Mandatory)][string]$Src,       # the deployed working tree
    [Parameter(Mandatory)][string]$EnvFile,   # the store's flags and keys, KEY=VALUE lines
    [string]$Out = "C:\Windows\Temp\metadata-results",
    [string]$Drive = "M:"
)
$ErrorActionPreference = "Continue"
$metatree = "$Src\metatree.exe"
$tree = "$Src\test\metadata\tree.json"
$work = "C:\metadata-test"
Remove-Item $Out, $work -Recurse -Force -ErrorAction SilentlyContinue
New-Item -ItemType Directory $Out, $work | Out-Null

# It runs on the logged-on desktop, not in an SSH session, so what it
# says goes to summary.txt too, and its exit code to exit.txt, for
# run-windows.sh to wait for.
function Say([string]$s) { $s; Add-Content -Encoding UTF8 "$Out\summary.txt" $s }
function Finish([int]$rc) {
    Get-Process icbfs-winfsp -ErrorAction SilentlyContinue | Stop-Process -Force
    Set-Content "$Out\exit.txt" $rc
    exit $rc
}

# The store: ICBFS_STORE_ARGS (flags, no secrets) and the ICBFS_* keys,
# which icbfs-cli and the mount read from the environment.
foreach ($line in Get-Content $EnvFile) {
    $k, $v = $line -split "=", 2
    if ($k) { Set-Item "env:$k" $v }
}
Remove-Item $EnvFile -Force
$store = @($env:ICBFS_STORE_ARGS -split " " | Where-Object { $_ })
$fs = "metadata-" + (Get-Date -Format yyyyMMddHHmmss)
& "$Src\icbfs-cli.exe" fs create @store -fs $fs -primary-windows 2>&1 | Set-Content -Encoding UTF8 "$Out\fs-create.log"
# Access times recorded as NTFS records them here: Windows 11 leaves last
# access updates off by default ("Disabled" in fsutil's answer), and
# icbfs's default records them hourly, as NTFS does with them on.
$atime = "hourly"
if ((fsutil behavior query disablelastaccess) -match "Disabled") { $atime = "noatime" }
Say "NTFS last access updates: $((fsutil behavior query disablelastaccess) -join ' '); icbfs mounted with -atime $atime"
Start-Process "$Src\icbfs-winfsp.exe" -WindowStyle Hidden -RedirectStandardError "$Out\mount.log" -RedirectStandardOutput "$Out\mount.out" `
    -ArgumentList ($store + @("-fs", $fs, "-atime", $atime, $Drive)) | Out-Null
for ($i = 0; $i -lt 60 -and -not (Test-Path "$Drive\"); $i++) { Start-Sleep -Milliseconds 500 }
if (-not (Test-Path "$Drive\")) { Say "FAIL  the icbfs mount never came up: see mount.log"; Finish 2 }

function Fresh-Source {
    Remove-Item "$work\src" -Recurse -Force -ErrorAction SilentlyContinue
    & $metatree build $tree "$work\src"
    if ($LASTEXITCODE -ne 0) { Say "FAIL  building the tree"; Finish 2 }
}

# Each tool copies $s (a tree) into $d (a new folder), keeping all it can,
# and answers 0 for success.
function Robocopy-Rc { if ($LASTEXITCODE -lt 8) { 0 } else { $LASTEXITCODE } }
$tools = [ordered]@{
    "robocopy_DAT"   = { param($s, $d) robocopy $s $d /E /SL /COPY:DAT /DCOPY:DAT /R:0 /W:0 /NP /NFL /NDL /NJH; Robocopy-Rc }
    "robocopy_DATSO" = { param($s, $d) robocopy $s $d /E /SL /COPY:DATSO /DCOPY:DAT /B /R:0 /W:0 /NP /NFL /NDL /NJH; Robocopy-Rc }
    "xcopy"          = { param($s, $d) xcopy "$s" "$d" /E /I /H /K /O /X /B /Y /Q; $LASTEXITCODE }
    "copy_item"      = { param($s, $d) try { Copy-Item "$s\*" $d -Recurse -Force -ErrorAction Stop; 0 } catch { Write-Output $_; 1 } }
    # Explorer's copy (the shell's CopyHere) isn't here: scripted, it
    # never finished, on the desktop or off it. It copies with
    # CopyFileEx, as Copy-Item does.
    "wim"            = { param($s, $d)
        # DISM's image: everything NTFS keeps, captured and applied.
        $img = "$work\tree.wim"
        Remove-Item $img -ErrorAction SilentlyContinue
        dism /Capture-Image /ImageFile:$img /CaptureDir:$s /Name:tree /Quiet | Out-Null
        if ($LASTEXITCODE -ne 0) { return $LASTEXITCODE }
        dism /Apply-Image /ImageFile:$img /Index:1 /ApplyDir:$d /Quiet
        $LASTEXITCODE }
}

Fresh-Source
& $metatree check $tree "$work\src" 2>&1 | Set-Content -Encoding UTF8 "$Out\source-check.txt"
if ($LASTEXITCODE -ne 0) { Say "FAIL  the tree on NTFS isn't its description: see source-check.txt"; Finish 2 }

$fail = 0
Say ("{0,-16} {1,-12} {2}" -f "tool", "vs NTFS", "kept of the description (on icbfs)")
foreach ($t in $tools.Keys) {
    $since = (Get-Date).ToUniversalTime().AddMinutes(-1).ToString("yyyy-MM-ddTHH:mm:ssZ")
    $ref, $dst = "$work\ref-$t", "$Drive\$t"
    New-Item -ItemType Directory $ref, $dst -Force | Out-Null
    # The same ACL on both: what a copy without its source's ACLs
    # inherits is then comparable.
    $acl = Get-Acl $ref
    $acl.SetSecurityDescriptorSddlForm("O:BAG:SYD:PAI(A;OICI;FA;;;BA)(A;OICI;FA;;;SY)(A;OICI;0x1200a9;;;BU)")
    Set-Acl $ref $acl
    Set-Acl $dst $acl
    Fresh-Source
    $refRc = (& $tools[$t] "$work\src" $ref 2>&1 | Tee-Object "$Out\$t.ref.log" | Select-Object -Last 1)
    Fresh-Source # its access times untouched by the first copy
    $icbfsRc = (& $tools[$t] "$work\src" $dst 2>&1 | Tee-Object "$Out\$t.icbfs.log" | Select-Object -Last 1)
    # Each copy read once: reading moves access times. PowerShell's >
    # writes UTF-16, so metatree writes its own files.
    & $metatree dump $ref "$Out\$t.ref.json" 2>&1 | Set-Content -Encoding UTF8 "$Out\$t.ref-dump.log"
    & $metatree dump $dst "$Out\$t.icbfs.json" 2>&1 | Set-Content -Encoding UTF8 "$Out\$t.icbfs-dump.log"
    # metatree answers 0 (the same), 1 (differences) or 2 (couldn't compare).
    # Hard links: WinFsp has no way for a filesystem to make one (the
    # request is refused before icbfs sees it), so they aren't compared.
    $diff = @(& $metatree diff -since $since -skip links "$Out\$t.ref.json" "$Out\$t.icbfs.json" 2>$null)
    $diffRc = $LASTEXITCODE
    $diff | Set-Content -Encoding UTF8 "$Out\$t.diff.txt"
    $check = @(& $metatree check $tree "$Out\$t.icbfs.json" 2>$null)
    $checkRc = $LASTEXITCODE
    $check | Set-Content -Encoding UTF8 "$Out\$t.check.txt"
    if ($diffRc -gt 1 -or $checkRc -gt 1) {
        Say ("{0,-16} couldn't compare: see {0}.*-dump.log" -f $t)
        $fail = 1
        continue
    }
    $n = @($diff | Where-Object { $_ }).Count
    $kept = @($check | Where-Object { $_ }).Count
    $status = "same"
    if ($n -gt 0 -or "$refRc" -ne "$icbfsRc") { $status = "$n differ"; $fail = 1 }
    if ("$refRc" -ne "$icbfsRc") { $status += " (exit $refRc on NTFS, $icbfsRc on icbfs)" }
    Say ("{0,-16} {1,-12} {2}" -f $t, $status, "$kept fields not kept (the tool's own choice if NTFS matches)")
}
Finish $fail
