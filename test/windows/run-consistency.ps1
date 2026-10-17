param($Endpoint,$Bucket,$AccessKey,$SecretKey)
# VM side of run-consistency.sh: two mounts of one filesystem on the VM,
# M: and N:, stand in for two machines. What N: changes, an open on M:
# sees at once (close-to-open); M:'s listings and cached names see it
# within the -meta-cache timeout. Prints PASS/FAIL lines and RESULT fail=N.
$ErrorActionPreference="Stop"
$store=@("-endpoint",$Endpoint,"-bucket",$Bucket,"-access-key",$AccessKey,"-secret-key",$SecretKey)
Get-Process icbfs-winfsp -ErrorAction SilentlyContinue | Stop-Process -Force; Start-Sleep -Milliseconds 700
$fs="xm-"+(Get-Random)
& C:\icbfs-test\icbfs-cli.exe fs create @store -fs $fs -primary-windows *> $null
foreach ($d in "M:","N:") { Start-Process C:\icbfs-test\icbfs-winfsp.exe -WindowStyle Hidden -RedirectStandardError "C:\Windows\Temp\xm-$($d[0]).log" -RedirectStandardOutput "C:\Windows\Temp\xm-$($d[0])o.log" -ArgumentList ($store+@("-fs",$fs,"-meta-cache","1s",$d)) }
for ($i=0;$i -lt 40 -and -not ((Test-Path "M:\") -and (Test-Path "N:\"));$i++){Start-Sleep -Milliseconds 250}
$fail=0
function check($what,$ok){ if ($ok) {"PASS $what"} else {"FAIL $what"; $script:fail=1} }
function within($what,[scriptblock]$cond){ $sw=[Diagnostics.Stopwatch]::StartNew(); while(-not (& $cond)){ if($sw.ElapsedMilliseconds -gt 3000){ check "$what (within 3s)" $false; return }; Start-Sleep -Milliseconds 50 }; check "$what (after $($sw.ElapsedMilliseconds) ms)" $true }

[IO.File]::WriteAllText("N:\doc.txt","version one")
check "M: reads N:'s new file" ([IO.File]::ReadAllText("M:\doc.txt") -eq "version one")
$null=Get-ChildItem M:\ ; $null=Test-Path M:\doc.txt   # warm M:'s cache
[IO.File]::WriteAllText("N:\doc.txt","version two, longer than before")
check "an open on M: reads N:'s newer content at once" ([IO.File]::ReadAllText("M:\doc.txt") -eq "version two, longer than before")
Rename-Item N:\doc.txt renamed.txt
$ok=$true; try { [IO.File]::ReadAllText("M:\doc.txt") | Out-Null; $ok=$false } catch {}
check "an open on M: of a name renamed on N: fails at once" $ok
within "M: lists the rename" { $n=(Get-ChildItem M:\).Name; ($n -contains "renamed.txt") -and -not ($n -contains "doc.txt") }
$null=Test-Path M:\made-later.txt
[IO.File]::WriteAllText("N:\made-later.txt","x")
within "M: sees a file N: created (once cached as missing)" { Test-Path M:\made-later.txt }
Remove-Item N:\renamed.txt
$ok=$true; try { [IO.File]::ReadAllText("M:\renamed.txt") | Out-Null; $ok=$false } catch {}
check "an open on M: of a file deleted on N: fails at once" $ok
within "M: lists the delete" { -not ((Get-ChildItem M:\).Name -contains "renamed.txt") }
Get-Process icbfs-winfsp -ErrorAction SilentlyContinue | Stop-Process -Force
"RESULT fail=$fail"
