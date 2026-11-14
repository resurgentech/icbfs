param($Endpoint,$Bucket,$AccessKey,$SecretKey,
  [string]$Exe="C:\icbfs-test\icbfs-winfsp.exe",
  [string]$Cli="C:\icbfs-test\icbfs-cli.exe",
  [string]$Fixture="C:\icbfs-test\namesfixture.exe",
  [string]$Out="C:\icbfs-test\names-result.txt")
# VM side of run-names-test.sh: what a WinFsp mount does today with names
# Windows can't hold, on a primary-POSIX filesystem (the fixture put them
# there through the core), and on a primary-Windows one (the control).
# It asserts nothing about the outcomes: it prints a table, one row per
# name and kind, and ends with "RESULT done" if it ran to the end.
# ASCII only: the names are built with [char] codes.
$ErrorActionPreference = "Continue"
$store = @("-endpoint",$Endpoint,"-bucket",$Bucket,"-access-key",$AccessKey,"-secret-key",$SecretKey)
$drive = "M:"
Remove-Item $Out -ErrorAction SilentlyContinue

Add-Type -TypeDefinition @'
using System; using System.Runtime.InteropServices; using System.ComponentModel; using System.Text;
using Microsoft.Win32.SafeHandles;
public static class W {
  [DllImport("kernel32.dll", CharSet=CharSet.Unicode, SetLastError=true)] static extern SafeFileHandle CreateFileW(string n, uint a, uint s, IntPtr sa, uint d, uint f, IntPtr t);
  [DllImport("kernel32.dll", SetLastError=true)] static extern bool ReadFile(SafeFileHandle h, byte[] b, uint n, out uint r, IntPtr o);
  [DllImport("kernel32.dll", CharSet=CharSet.Unicode, SetLastError=true)] static extern bool MoveFileExW(string a, string b, uint f);
  [DllImport("kernel32.dll", CharSet=CharSet.Unicode, SetLastError=true)] static extern bool DeleteFileW(string a);
  [DllImport("kernel32.dll", CharSet=CharSet.Unicode, SetLastError=true)] static extern bool RemoveDirectoryW(string a);
  [DllImport("kernel32.dll", CharSet=CharSet.Unicode, SetLastError=true)] static extern bool CreateDirectoryW(string a, IntPtr sa);
  [StructLayout(LayoutKind.Sequential)] struct US { public ushort Length; public ushort Max; public IntPtr Buf; }
  [StructLayout(LayoutKind.Sequential)] struct OA { public int Length; public IntPtr Root; public IntPtr Name; public uint Attr; public IntPtr SD; public IntPtr SQOS; }
  [StructLayout(LayoutKind.Sequential)] struct IOSB { public IntPtr Status; public IntPtr Info; }
  [DllImport("ntdll.dll")] static extern int NtCreateFile(out SafeFileHandle h, uint access, ref OA oa, out IOSB iosb, IntPtr alloc, uint attrs, uint share, uint disp, uint opts, IntPtr ea, uint ealen);

  static string Err(int e) { return "W" + e + " " + new Win32Exception(e).Message; }
  static string Status(int st) {
    string n;
    switch ((uint)st) {
      case 0xC0000034: n = "OBJECT_NAME_NOT_FOUND"; break;
      case 0xC0000033: n = "OBJECT_NAME_INVALID"; break;
      case 0xC000003A: n = "OBJECT_PATH_NOT_FOUND"; break;
      case 0xC0000035: n = "OBJECT_NAME_COLLISION"; break;
      case 0xC0000022: n = "ACCESS_DENIED"; break;
      case 0xC000003B: n = "OBJECT_PATH_SYNTAX_BAD"; break;
      case 0xC0000103: n = "NOT_A_DIRECTORY"; break;
      case 0xC00000BA: n = "FILE_IS_A_DIRECTORY"; break;
      case 0xC000000D: n = "INVALID_PARAMETER"; break;
      case 0xC0000101: n = "DIRECTORY_NOT_EMPTY"; break;
      case 0xC0000043: n = "SHARING_VIOLATION"; break;
      case 0xC0000106: n = "NAME_TOO_LONG"; break;
      case 0xC0000008: n = "INVALID_HANDLE"; break;
      default: n = ""; break;
    }
    return "S" + ((uint)st).ToString("X8") + (n == "" ? "" : " " + n);
  }
  static string Read(SafeFileHandle h, bool dir) {
    if (dir) return "OK dir";
    byte[] b = new byte[64]; uint r;
    if (!ReadFile(h, b, 64, out r, IntPtr.Zero)) return "OPENED, read " + Err(Marshal.GetLastWin32Error());
    return "OK read '" + Encoding.UTF8.GetString(b, 0, (int)r) + "'";
  }
  // Open an existing file or folder by a Win32 path.
  public static string Open(string path, bool dir) {
    SafeFileHandle h = CreateFileW(path, 0x80000000u, 7, IntPtr.Zero, 3, dir ? 0x02000000u : 0x80u, IntPtr.Zero);
    if (h.IsInvalid) return Err(Marshal.GetLastWin32Error());
    using (h) { return Read(h, dir); }
  }
  // Create a new file or folder by a Win32 path.
  public static string Create(string path, bool dir) {
    if (dir) return CreateDirectoryW(path, IntPtr.Zero) ? "OK" : Err(Marshal.GetLastWin32Error());
    SafeFileHandle h = CreateFileW(path, 0x40000000u, 7, IntPtr.Zero, 1, 0x80u, IntPtr.Zero);
    if (h.IsInvalid) return Err(Marshal.GetLastWin32Error());
    h.Dispose(); return "OK";
  }
  // NtCreateFile of an NT path (\??\M:\...): no Win32 normalization.
  public static string Nt(string path, bool dir, bool create) {
    IntPtr buf = Marshal.StringToHGlobalUni(path);
    IntPtr pus = Marshal.AllocHGlobal(Marshal.SizeOf(typeof(US)));
    US us = new US(); us.Length = (ushort)(path.Length * 2); us.Max = (ushort)(us.Length + 2); us.Buf = buf;
    Marshal.StructureToPtr(us, pus, false);
    OA oa = new OA(); oa.Length = Marshal.SizeOf(typeof(OA)); oa.Name = pus; oa.Attr = 0x40;
    SafeFileHandle h; IOSB io;
    uint access = create ? 0x40100000u : 0x80100000u;
    uint opts = 0x20u | (dir ? 1u : 0x40u);
    int st = NtCreateFile(out h, access, ref oa, out io, IntPtr.Zero, dir ? 0x10u : 0x80u, 7, create ? 2u : 1u, opts, IntPtr.Zero, 0);
    Marshal.FreeHGlobal(pus); Marshal.FreeHGlobal(buf);
    if (st < 0) return Status(st);
    using (h) { return create ? "OK created" : Read(h, dir); }
  }
  public static string Move(string a, string b) { return MoveFileExW(a, b, 0) ? "OK" : Err(Marshal.GetLastWin32Error()); }
  public static string Delete(string p, bool dir) {
    bool ok = dir ? RemoveDirectoryW(p) : DeleteFileW(p);
    return ok ? "OK" : Err(Marshal.GetLastWin32Error());
  }
}
'@

# One row per case: id, stored name. Kept in step with namesfixture/main.go.
$names = @(
  @('plain','plain'), @('lt','a<b'), @('gt','a>b'), @('colon','a:b'), @('quote','a"b'),
  @('pipe','a|b'), @('qmark','a?b'), @('star','a*b'), @('backslash','a\b'),
  @('tab',"a$([char]9)b"), @('ctl01',"a$([char]1)b"), @('trailing-dot','a.'), @('trailing-space','a '),
  @('CON','CON'), @('NUL.txt','NUL.txt'), @('com1','com1'), @('LPT1-sup',"LPT$([char]0xB9)"),
  @('holds-F03A',"a$([char]0xF03A)b"), @('colons255',(':' * 255)))

function Esc([string]$s) {
  $sb = New-Object Text.StringBuilder
  foreach ($c in $s.ToCharArray()) {
    $n = [int]$c
    if ($n -lt 32 -or $n -gt 126 -or $c -eq '|') { [void]$sb.AppendFormat('\u{0:X4}', $n) } else { [void]$sb.Append($c) }
  }
  $r = $sb.ToString()
  if ($r.Length -gt 40) { $r = $r.Substring(0,12) + "...(" + $s.Length + " chars)" }
  return $r
}
function Cell([string]$s) { return (($s -replace '\r?\n',' ') -replace '\|','\u007C') }
function Say($line) { Add-Content -Path $Out -Value $line -Encoding UTF8; Write-Output $line }

function Stop-Fs {
  Get-Process icbfs-winfsp -ErrorAction SilentlyContinue | Stop-Process -Force
  Start-Sleep -Milliseconds 900
}
function Mount-Fs([string]$fs) {
  Remove-Item C:\Windows\Temp\names-err.log,C:\Windows\Temp\names-out.log -ErrorAction SilentlyContinue
  Start-Process $Exe -WindowStyle Hidden -RedirectStandardError C:\Windows\Temp\names-err.log -RedirectStandardOutput C:\Windows\Temp\names-out.log `
    -ArgumentList ($store + @("-fs",$fs,$drive))
  for ($i = 0; $i -lt 60 -and -not (Test-Path "$drive\"); $i++) { Start-Sleep -Milliseconds 250 }
  return (Test-Path "$drive\")
}

# --- listings -------------------------------------------------------------
function List-Gci([string]$dir) {
  try { return ,@(Get-ChildItem -LiteralPath $dir -Force -ErrorAction Stop | ForEach-Object { $_.Name }) } catch { return ,@("ERROR: " + $_.Exception.Message) }
}
function List-Cmd([string]$dir) {
  $f = "C:\Windows\Temp\names-dir.txt"
  Remove-Item $f -ErrorAction SilentlyContinue
  cmd /u /c "dir /b $dir > $f 2>&1" | Out-Null
  if (-not (Test-Path $f)) { return ,@("ERROR: no output") }
  $t = [IO.File]::ReadAllText($f, [Text.Encoding]::Unicode)
  return ,@($t -split "\r\n" | Where-Object { $_ -ne '' })
}
$script:shell = $null
function List-Shell([string]$dir) {
  try {
    if (-not $script:shell) { $script:shell = New-Object -ComObject Shell.Application }
    $ns = $script:shell.NameSpace($dir)
    if ($ns -eq $null) { return ,@("ERROR: NameSpace returned null") }
    $r = @()
    foreach ($it in $ns.Items()) { $p = $it.Path; $r += (Split-Path -Leaf $p) }
    return ,$r
  } catch { return ,@("ERROR: " + $_.Exception.Message) }
}
function Shell-Parse([string]$dir, [string]$name) {
  try {
    if (-not $script:shell) { $script:shell = New-Object -ComObject Shell.Application }
    $ns = $script:shell.NameSpace($dir)
    if ($ns -eq $null) { return "NameSpace null" }
    $it = $ns.ParseName($name)
    if ($it -eq $null) { return "null" }
    return "found size=" + $it.Size
  } catch { return "ERROR " + $_.Exception.Message }
}
function Listed($list, [string]$name) { return ($list -ccontains $name) }
function YN($b) { if ($b) { "yes" } else { "NO" } }
# A listing that is itself an error ("ERROR: ...") lists nothing.
function ListedYN($list, [string]$name) { if ($list.Count -ge 1 -and $list[0] -like 'ERROR*') { "ERR" } else { YN (Listed $list $name) } }
function GetItem([string]$p) { try { $i = Get-Item -LiteralPath $p -Force -ErrorAction Stop; return "found" } catch { return "ERR " + $_.Exception.Message } }
function Extras($list, $expected) {
  return @($list | Where-Object { -not ($expected -ccontains $_) })
}

# --- the primary-POSIX mount ------------------------------------------------
Stop-Fs
$fs = "names-" + (Get-Random)
& $Cli fs create @store -fs $fs *> C:\Windows\Temp\names-create.log
& $Fixture @store -fs $fs
Say "# primary-POSIX filesystem $fs, mounted case-insensitively (the default) on $drive"
if (-not (Mount-Fs $fs)) { Say "MOUNT-FAILED"; Get-Content C:\Windows\Temp\names-err.log; Say "RESULT fail=1"; exit 1 }
Say "## fsutil fsinfo volumeinfo $drive"
fsutil fsinfo volumeinfo $drive | ForEach-Object { Say $_ }

$allNames = @($names | ForEach-Object { $_[1] })
$dirs = @{ file = "$drive\A"; folder = "$drive\A\F" }
$expectedIn = @{ file = ($allNames + @("F")); folder = $allNames }
$lists = @{}
foreach ($k in "file","folder") {
  $d = $dirs[$k]
  $lists["$k-gci"] = List-Gci $d
  $lists["$k-cmd"] = List-Cmd $d
  $lists["$k-shell"] = List-Shell $d
  foreach ($m in "gci","cmd","shell") {
    $x = Extras $lists["$k-$m"] $expectedIn[$k]
    Say ("LISTING $k $m in $d : " + $lists["$k-$m"].Count + " entries; not a stored name: " + (($x | ForEach-Object { if ($_ -like 'ERROR*') { Cell $_ } else { Esc $_ } }) -join ', '))
    Say ("  entries: " + (($lists["$k-$m"] | ForEach-Object { if ($_ -like 'ERROR*') { Cell $_ } else { Esc $_ } }) -join ', '))
  }
}

# Device names, other ways in, before the table below renames anything.
Say ""
Say "## Device names: PowerShell, cmd and the Shell on the tree A files"
foreach ($id in "CON","NUL.txt","com1","LPT1-sup") {
  $nm = ($names | Where-Object { $_[0] -eq $id })[1]
  $p = "$drive\A\$nm"
  $tp = try { Test-Path -LiteralPath $p } catch { "ERROR " + $_.Exception.Message }
  $gc = try { (Get-Content -LiteralPath $p -ErrorAction Stop -Raw) } catch { "ERROR " + $_.Exception.Message }
  $psi = New-Object Diagnostics.ProcessStartInfo "cmd.exe", ('/c type "' + $p + '"')
  $psi.RedirectStandardOutput = $true; $psi.RedirectStandardError = $true; $psi.RedirectStandardInput = $true; $psi.UseShellExecute = $false
  $pr = [Diagnostics.Process]::Start($psi); $pr.StandardInput.Close()
  if ($pr.WaitForExit(10000)) { $ty = ($pr.StandardOutput.ReadToEnd() + " " + $pr.StandardError.ReadToEnd()).Trim() + " (exit " + $pr.ExitCode + ")" } else { $pr.Kill(); $ty = "TIMED OUT, killed" }
  $sp2 = Shell-Parse "$drive\A" $nm
  # The item Explorer's own listing gives for it: its path, size and whether that path opens.
  $via = "no item in the Shell listing"
  try {
    foreach ($it in $script:shell.NameSpace("$drive\A").Items()) {
      if ((Split-Path -Leaf $it.Path) -ceq $nm) { $via = "item path=" + (Esc $it.Path) + " size=" + $it.Size + " open=" + [W]::Open($it.Path, $false) }
    }
  } catch { $via = "ERROR " + $_.Exception.Message }
  Say ("DEVICE " + $id + " (" + (Esc $nm) + "): Test-Path=" + $tp + " | Get-Content=" + (Cell "$gc") + " | cmd type=" + (Cell $ty) + " | Shell ParseName=" + (Cell $sp2) + " | Shell Items() " + (Cell $via))
}

Say ""
Say "## Table (tree A: list, open, rename; tree B: delete)"
Say "| kind | id | name | Get-ChildItem | Get-Item | cmd dir | Shell list | Shell ParseName | open plain | open \\?\ | open NT \??\ | rename plain | rename \\?\ | rename result seen | delete plain | delete \\?\ | delete result seen |"
Say "|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|"
foreach ($k in "file","folder") {
  $isDir = ($k -eq "folder")
  $dA = $dirs[$k]
  $dB = $dA -replace '^M:\\A','M:\B'
  foreach ($n in $names) {
    $id = $n[0]; $nm = $n[1]
    $gci = ListedYN $lists["$k-gci"] $nm; $cmdl = ListedYN $lists["$k-cmd"] $nm; $shl = ListedYN $lists["$k-shell"] $nm
    $gi = GetItem "$dA\$nm"
    $sp = Shell-Parse $dA $nm
    $p = "$dA\$nm"
    $oPlain = [W]::Open($p, $isDir)
    $oExt = [W]::Open("\\?\$p", $isDir)
    $oNt = [W]::Nt("\??\$p", $isDir, $false)
    # rename (tree A) to a plain name, plain path first, then \\?\.
    $new = "r-$id"
    $rPlain = [W]::Move($p, "$dA\$new")
    $rExt = "(not tried)"
    if ($rPlain -ne "OK") { $rExt = [W]::Move("\\?\$p", "\\?\$dA\$new") }
    $after = List-Cmd $dA
    $seen = "new=" + (YN (Listed $after $new)) + " old gone=" + (YN (-not (Listed $after $nm)))
    # delete (tree B).
    $q = "$dB\$nm"
    $dPlain = [W]::Delete($q, $isDir)
    $dExt = "(not tried)"
    if ($dPlain -ne "OK") { $dExt = [W]::Delete("\\?\$q", $isDir) }
    $afterB = List-Cmd $dB
    $seenB = "old gone=" + (YN (-not (Listed $afterB $nm)))
    Say ("| $k | $id | " + (Esc $nm) + " | $gci | " + (Cell $gi) + " | $cmdl | $shl | " + (Cell $sp) + " | " + (Cell $oPlain) + " | " + (Cell $oExt) + " | " + (Cell $oNt) + " | " + (Cell $rPlain) + " | " + (Cell $rExt) + " | $seen | " + (Cell $dPlain) + " | " + (Cell $dExt) + " | $seenB |")
  }
}

Stop-Fs

# --- the primary-Windows control ---------------------------------------------
Say ""
$fs2 = "names-pw-" + (Get-Random)
& $Cli fs create @store -fs $fs2 -primary-windows *> C:\Windows\Temp\names-create2.log
Say "# primary-Windows filesystem $fs2 on ${drive}: the same names made three ways (P1 plain path, P2 \\?\ path, P3 NT path \??\)"
if (-not (Mount-Fs $fs2)) { Say "MOUNT-FAILED (primary-Windows)"; Get-Content C:\Windows\Temp\names-err.log; Say "RESULT fail=1"; exit 1 }
Say "## fsutil fsinfo volumeinfo $drive (primary-Windows)"
fsutil fsinfo volumeinfo $drive | ForEach-Object { Say $_ }
foreach ($t in "P1","P2","P3") { New-Item -ItemType Directory "$drive\$t" | Out-Null; New-Item -ItemType Directory "$drive\$t\F" | Out-Null }
Say "| kind | id | name | P1 plain create | P2 \\?\ create | P3 NT create |"
Say "|---|---|---|---|---|---|"
foreach ($k in "file","folder") {
  $isDir = ($k -eq "folder")
  foreach ($n in $names) {
    $id = $n[0]; $nm = $n[1]
    $sub = if ($isDir) { "\F" } else { "" }
    $c1 = [W]::Create("$drive\P1$sub\$nm", $isDir)
    $c2 = [W]::Create("\\?\$drive\P2$sub\$nm", $isDir)
    $c3 = [W]::Nt("\??\$drive\P3$sub\$nm", $isDir, $true)
    Say ("| $k | $id | " + (Esc $nm) + " | " + (Cell $c1) + " | " + (Cell $c2) + " | " + (Cell $c3) + " |")
  }
}
foreach ($t in "P1","P2","P3") {
  Say ("PRIMARY-WINDOWS entries that did get made in $t : " + ((List-Gci "$drive\$t" | ForEach-Object { Esc $_ }) -join ', ') + " ; in $t\F : " + ((List-Gci "$drive\$t\F" | ForEach-Object { Esc $_ }) -join ', '))
}
Stop-Fs
Say "RESULT done"
