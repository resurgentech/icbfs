param(
    [Parameter(Mandatory)] [string]$Endpoint,
    [Parameter(Mandatory)] [string]$Bucket,
    [Parameter(Mandatory)] [string]$AccessKey,
    [Parameter(Mandatory)] [string]$SecretKey
)
# icbfs-sync end to end on Windows, run by run-sync-test.sh: the real
# icbfs-sync.exe against MinIO, checked through a real icbfs mount.
#
# Pairs: "ntfs" (an NTFS folder: ids, ACLs), "dav" (an NTFS folder made
# to behave as a WebDAV drive does: no ids, no ACLs, whole seconds) and
# "seeded" (a drive filled first by robocopy through a mount, then
# synced: nothing should be uploaded again).
$ErrorActionPreference = "Stop"
$bin = "C:\icbfs-test"
$cli, $sync, $winfsp = "$bin\icbfs-cli.exe", "$bin\icbfs-sync.exe", "$bin\icbfs-winfsp.exe"
$t = "C:\sync-test"
if (Test-Path $t) { Remove-Item $t -Recurse -Force }
New-Item -ItemType Directory $t | Out-Null
$store = @("-endpoint", $Endpoint, "-bucket", $Bucket, "-access-key", $AccessKey, "-secret-key", $SecretKey)
$script:failed = 0
function Check($ok, $what) {
    if ($ok) { "PASS  $what" } else { "FAIL  $what"; $script:failed++ }
}
function Put($path, $text) {
    New-Item -ItemType Directory (Split-Path $path) -Force | Out-Null
    [IO.File]::WriteAllText($path, $text)
}
$log = "$t\data\icbfs-sync.log"
function LogSince($n) { (Get-Content $log)[$n..((Get-Content $log).Count)] -join "`n" }
function LogLines { if (Test-Path $log) { (Get-Content $log).Count } else { 0 } }
function Once {
    $p = Start-Process $sync -ArgumentList @("once", "-config", "$t\config.json", "-data", "$t\data") -Wait -PassThru -NoNewWindow
    return $p.ExitCode
}
function Mount-Icbfs($fs, $drive) {
    Start-Process $winfsp -WindowStyle Hidden -ArgumentList ($store + @("-fs", $fs, $drive)) | Out-Null
    for ($i = 0; $i -lt 60 -and -not (Test-Path "$drive\"); $i++) { Start-Sleep -Milliseconds 500 }
}
function Dismount-Icbfs { Get-Process icbfs-winfsp -ErrorAction SilentlyContinue | Stop-Process -Force; Start-Sleep 2 }

foreach ($fs in "ntfs-mirror", "dav-mirror", "seeded-mirror") {
    & $cli fs create @store -fs $fs -primary-windows *> "$t\create-$fs.log"
}

# The folders.
Put "$t\ntfs\Matter\brief.docx" "the brief"
Put "$t\ntfs\Matter\exhibit.pdf" "exhibit A"
Put "$t\ntfs\secret.txt" "for guests to read"
icacls "$t\ntfs\secret.txt" /grant "*S-1-5-32-546:(R)" | Out-Null
Put "$t\dav\A\Adams\will.pdf" "the will"
Put "$t\dav\A\Adams\deed.pdf" "the deed"
Put "$t\dav\marker.url" "[InternetShortcut]"
Put "$t\seed\B\Brown\memo.docx" "memo"
Put "$t\seed\B\Brown\letter.pdf" "letter"

# Seeding: robocopy the folder onto the drive through a mount first.
Mount-Icbfs "seeded-mirror" "Y:"
robocopy "$t\seed" "Y:\" /E /R:1 /W:1 /NP /NFL /NDL | Out-Null
Dismount-Icbfs

@"
{
  "store": {"kind": "s3", "s3": {"endpoint": "$Endpoint", "bucket": "$Bucket", "accessKey": "$AccessKey", "secretKey": "$SecretKey"}},
  "pairs": [
    {"name": "ntfs", "folder": "$($t -replace '\\','\\')\\ntfs", "filesystem": "ntfs-mirror", "root": "/", "mode": "one-way"},
    {"name": "dav", "folder": "$($t -replace '\\','\\')\\dav", "filesystem": "dav-mirror", "root": "/", "mode": "one-way",
     "ids": "off", "acls": "off", "timeResolution": "1s", "requirePresent": ["marker.url"]},
    {"name": "seeded", "folder": "$($t -replace '\\','\\')\\seed", "filesystem": "seeded-mirror", "root": "/", "mode": "one-way",
     "ids": "off", "acls": "off", "timeResolution": "1s"}
  ]
}
"@ | Set-Content "$t\config.json" -Encoding ascii

# 1. The first pass.
$n = LogLines
$code = Once
$out = LogSince $n
Check ($code -eq 0) "the first pass succeeds (exit $code)"
Check ($out -match "ntfs: .*ids true, ACLs true") "an NTFS folder is seen to have ids and ACLs"
Check ($out -match "dav: .*ids false, ACLs false, times to 1s") "the WebDAV-like folder has neither, and whole seconds"
Check (-not ($out -match "seeded: upload")) "the seeded drive is adopted, nothing uploaded again"

# 2. Through a mount: content, and the ACL carried.
Mount-Icbfs "ntfs-mirror" "X:"
Check ((Get-Content "X:\Matter\brief.docx" -Raw) -eq "the brief") "content arrives"
$acl = (icacls "X:\secret.txt") -join " "
Check ($acl -match "Guests") "the folder's ACL is on the drive's file ($acl)"
Dismount-Icbfs

# 3. Renames: by id on NTFS, by hash on the WebDAV-like folder.
Rename-Item "$t\ntfs\Matter\brief.docx" "brief (filed).docx"
Rename-Item "$t\dav\A\Adams\will.pdf" "Last Will.pdf"
$n = LogLines
$code = Once
$out = LogSince $n
Check ($code -eq 0) "the rename pass succeeds"
Check ($out -match "ntfs: .*1 renames") "a rename on NTFS is moved, not re-uploaded"
Check (($out -match "dav: .*1 renames") -and ($out -match "dav: move-remote A/Adams/will.pdf -> A/Adams/Last Will.pdf")) "a rename on the WebDAV-like folder is found by its hash"

# 4. An editor's save: write a temp file, move the original away,
# move the temp file into place, delete the original.
$m = "$t\ntfs\Matter"
Put "$m\~WRL0001.tmp" "exhibit A, revised"
Rename-Item "$m\exhibit.pdf" "~WRL0002.tmp"
Rename-Item "$m\~WRL0001.tmp" "exhibit.pdf"
Remove-Item "$m\~WRL0002.tmp"
$n = LogLines
$code = Once
$out = LogSince $n
Check (($out -match "ntfs: upload Matter/exhibit.pdf") -and -not ($out -match "remove-remote Matter/exhibit.pdf")) "an editor's save is a new version, not a delete and a new file"

# 5. A disconnected WebDAV-like folder is held, not emptied.
Rename-Item "$t\dav\marker.url" "marker.off"
$n = LogLines
$code = Once
$out = LogSince $n
Check ($out -match "dav: held: marker.url") "a missing marker holds the pass"
Rename-Item "$t\dav\marker.off" "marker.url"

# 6. Nothing to do.
$n = LogLines
$code = Once
$out = LogSince $n
Check (($code -eq 0) -and (($out | Select-String "0 of 0 done" -AllMatches).Matches.Count -eq 3)) "an unchanged pass does nothing"

"--- icbfs-sync log ---"
Get-Content $log | Select-Object -Last 40
if ($script:failed -gt 0) { exit 1 }
