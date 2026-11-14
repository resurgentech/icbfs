param($Endpoint,$Bucket,$AccessKey,$SecretKey,
  [string]$Exe="C:\icbfs-test\icbfs-winfsp.exe",
  [string]$Cli="C:\icbfs-test\icbfs-cli.exe",
  [string]$Fixture="C:\icbfs-test\namesfixture.exe",
  [string]$Out="C:\icbfs-test\names-result.txt")
# VM side of run-names-test.sh (T164): what a WinFsp mount of a primary-POSIX
# filesystem does with names Windows can't hold. The fixture put the names
# there through the core, stored exactly as given (`a:b`, `a.`, tab...). The
# mount must show each as its substitute (Cygwin's rule: the character plus
# 0xF000, a trailing space or dot as U+F020 or U+F02E, a stored substitute
# escaped with U+F000), and accept the substitute back. Every check prints
# PASS or FAIL; the script ends with "RESULT done" only if all passed, else
# "RESULT fail=N". Device names (CON, NUL.txt, com1, LPT-superscript-1) are
# left as they are: T163 found they open. A primary-Windows mount is the
# control: it must not translate. ASCII only: names are built with [char].
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

  // A path of 240 characters or more goes by \\?\ (MAX_PATH): the 255-unit
  // names are then opened by that path, not the plain one.
  static string L(string p) { return (p.Length >= 240 && !p.StartsWith(@"\\?\") && !p.StartsWith(@"\??\")) ? @"\\?\" + p : p; }
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
    SafeFileHandle h = CreateFileW(L(path), 0x80000000u, 7, IntPtr.Zero, 3, dir ? 0x02000000u : 0x80u, IntPtr.Zero);
    if (h.IsInvalid) return Err(Marshal.GetLastWin32Error());
    using (h) { return Read(h, dir); }
  }
  // Create a new file or folder by a Win32 path.
  public static string Create(string path, bool dir) {
    if (dir) return CreateDirectoryW(L(path), IntPtr.Zero) ? "OK" : Err(Marshal.GetLastWin32Error());
    SafeFileHandle h = CreateFileW(L(path), 0x40000000u, 7, IntPtr.Zero, 1, 0x80u, IntPtr.Zero);
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
  [DllImport("kernel32.dll", CharSet=CharSet.Unicode, SetLastError=true)] static extern uint GetFinalPathNameByHandleW(SafeFileHandle h, StringBuilder b, uint n, uint f);
  [DllImport("kernel32.dll", CharSet=CharSet.Unicode, SetLastError=true)] static extern byte CreateSymbolicLinkW(string l, string t, uint f);
  // The name the mount reports for an opened path (the normalized name).
  public static string Final(string path) {
    SafeFileHandle h = CreateFileW(L(path), 0x80000000u, 7, IntPtr.Zero, 3, 0x02000080u, IntPtr.Zero);
    if (h.IsInvalid) return "ERR " + Err(Marshal.GetLastWin32Error());
    using (h) { StringBuilder sb = new StringBuilder(1024); uint n = GetFinalPathNameByHandleW(h, sb, 1024, 4); return n == 0 ? "ERR " + Err(Marshal.GetLastWin32Error()) : sb.ToString(); }
  }
  public static string Symlink(string link, string target) { return CreateSymbolicLinkW(link, target, 2) != 0 ? "OK" : Err(Marshal.GetLastWin32Error()); }
  public static string Move(string a, string b) { return MoveFileExW(L(a), L(b), 0) ? "OK" : Err(Marshal.GetLastWin32Error()); }
  public static string Delete(string p, bool dir) {
    bool ok = dir ? RemoveDirectoryW(L(p)) : DeleteFileW(L(p));
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
  @('holds-F03A',"a$([char]0xF03A)b"), @('colons255',(':' * 255)), @('case','Mix:Case'))

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

$script:fail = 0
function Check([bool]$ok, [string]$what, [string]$detail = "") {
  if ($ok) { Say ("PASS " + $what) } else { $script:fail++; Say ("FAIL " + $what + $(if ($detail) { " :: " + $detail } else { "" })) }
}

# The mapping, written out again here (not shared with the Go code): the
# shown form of a stored name.
$forbidden = [char[]]'<>:"|?*\'
function ShownName([string]$s) {
  $sb = New-Object Text.StringBuilder
  $cs = $s.ToCharArray()
  for ($i = 0; $i -lt $cs.Length; $i++) {
    $c = $cs[$i]; $n = [int]$c
    $inSet = (($n -ge 0xF000 -and $n -le 0xF020) -or (@(0xF022,0xF02A,0xF02E,0xF03A,0xF03C,0xF03E,0xF03F,0xF05C,0xF07C) -contains $n))
    if ($inSet) { [void]$sb.Append([char]0xF000); [void]$sb.Append($c) }
    elseif (($n -ge 1 -and $n -le 31) -or ($forbidden -contains $c)) { [void]$sb.Append([char]($n + 0xF000)) }
    elseif ($i -eq $cs.Length - 1 -and $c -eq ' ') { [void]$sb.Append([char]0xF020) }
    elseif ($i -eq $cs.Length - 1 -and $c -eq '.') { [void]$sb.Append([char]0xF02E) }
    else { [void]$sb.Append($c) }
  }
  return $sb.ToString()
}
function Hex([string]$s) { return ([BitConverter]::ToString([Text.Encoding]::UTF8.GetBytes($s))).Replace('-','').ToLower() }
# What is stored under a directory, through the core: "hexname|kind|hextarget".
function Stored([string]$fsName, [string]$dir) {
  $o = & $Fixture @store -fs $fsName -list $dir 2>&1
  $r = @()
  foreach ($l in @($o)) {
    $p = ([string]$l) -split "`t"
    if ($p.Count -ge 3) { $r += ($p[0] + "|" + $p[1] + "|" + $p[2]) } else { $r += ("ERROR " + $l) }
  }
  return ,$r
}
function HasStored($list, [string]$name, [string]$kind) { return ($list -contains ((Hex $name) + "|" + $kind + "|")) }
function SameSet($a, $b) { return ((@($a | Sort-Object) -join ",") -ceq (@($b | Sort-Object) -join ",")) }

# --- the primary-POSIX mount ------------------------------------------------
Stop-Fs
$fs = "names-" + (Get-Random)
& $Cli fs create @store -fs $fs *> C:\Windows\Temp\names-create.log
& $Fixture @store -fs $fs
Say "# primary-POSIX filesystem $fs, mounted case-insensitively (the default) on $drive"
if (-not (Mount-Fs $fs)) { Say "MOUNT-FAILED"; Get-Content C:\Windows\Temp\names-err.log; Say "RESULT fail=1"; exit 1 }

$shown = @{}
foreach ($n in $names) { $shown[$n[0]] = ShownName $n[1] }
$shownAll = @($names | ForEach-Object { $shown[$_[0]] })
$dirs = @{ file = "$drive\A"; folder = "$drive\A\F" }

# 1. Listing: exactly the shown names, by cmd and by the Shell; none raw.
Say ""
Say "## listing"
foreach ($k in "file","folder") {
  $d = $dirs[$k]
  $expect = if ($k -eq "file") { $shownAll + @("F","lnk") } else { $shownAll }
  $cmdl = List-Cmd $d
  $extra = @($cmdl | Where-Object { -not ($expect -ccontains $_) })
  $missing = @($expect | Where-Object { -not ($cmdl -ccontains $_) })
  Check (($extra.Count -eq 0) -and ($missing.Count -eq 0)) "$k listing by cmd dir is exactly the shown names" ("extra: " + (($extra | ForEach-Object { Esc $_ }) -join ', ') + " missing: " + (($missing | ForEach-Object { Esc $_ }) -join ', '))
  $shl = List-Shell $d
  foreach ($n in $names) { Check (Listed $shl $shown[$n[0]]) ("$k " + $n[0] + " is in the Shell listing") ("shell entries: " + (($shl | ForEach-Object { Esc $_ }) -join ', ')) }
}
Check ($shown['colon'] -cne $shown['holds-F03A']) "a:b and the name holding a literal U+F03A show as two different names"

# 2. Open and read by the shown name; the name an open reports; case.
Say ""
Say "## open"
foreach ($k in "file","folder") {
  $isDir = ($k -eq "folder")
  foreach ($n in $names) {
    $id = $n[0]; $p = $dirs[$k] + "\" + $shown[$id]
    $want = if ($isDir) { "OK dir" } else { "OK read 'file A $id'" }
    $r = [W]::Open($p, $isDir)
    Check ($r -ceq $want) "$k $id opens and reads by its shown name" $r
    $r2 = [W]::Open("\\?\$p", $isDir)
    Check ($r2 -ceq $want) "$k $id opens by its shown name, \\?\ path" $r2
    $r3 = [W]::Nt("\??\$p", $isDir, $false)
    Check ($r3 -ceq $want) "$k $id opens by its shown name, NT path" $r3
  }
}
$lower = "$drive\A\" + $shown['case'].ToLower()
$exact = "$drive\A\" + $shown['case']
Check (([W]::Open($lower, $false)) -ceq "OK read 'file A case'") "a folded (lower-case) open of a translated name finds the same file"
Check (([W]::Open($exact, $false)) -ceq "OK read 'file A case'") "an exact-case open of the translated name finds it"
$fin = [W]::Final($lower)
Check ($fin.EndsWith("\A\" + $shown['case'], [StringComparison]::Ordinal)) "the name an open reports is the shown name in its stored case" (Esc $fin)
$fin = [W]::Final("$drive\A\" + $shown['colon'])
Check ($fin.EndsWith("\A\" + $shown['colon'], [StringComparison]::Ordinal)) "the name an open of a:b reports is the shown name" (Esc $fin)
# A bad marker (U+F000 then anything outside the set) is refused.
$bad = "$drive\A\bad" + [char]0xF000 + "x"
$rb = [W]::Open($bad, $false)
Check ($rb -like 'W123*') "an open of a bad U+F000 sequence is refused" $rb
# A symlink stored with a raw colon target is read, and followed, by its shown target.
$rl = [W]::Open("$drive\A\lnk", $false)
Check ($rl -ceq "OK read 'file A colon'") "a symlink to a:b opens through its translated target" $rl

# 3. Rename: to a plain name, then to another forbidden one (stored raw).
Say ""
Say "## rename"
foreach ($k in "file","folder") {
  $isDir = ($k -eq "folder"); $d = $dirs[$k]
  foreach ($n in $names) {
    $id = $n[0]; $old = $d + "\" + $shown[$id]
    $plain = "r-$id"
    $r = [W]::Move($old, "$d\$plain")
    Check ($r -ceq "OK") "$k $id renames to a plain name" $r
    $after = List-Cmd $d
    Check (($after -ccontains $plain) -and -not ($after -ccontains $shown[$id])) "$k $id : the new name is listed and the old one is gone"
    $tgtShown = "x" + $id + [char]0xF03A + "y"
    $r = [W]::Move("$d\$plain", "$d\$tgtShown")
    Check ($r -ceq "OK") "$k $id renames to another forbidden name" $r
    $want = if ($isDir) { "OK dir" } else { "OK read 'file A $id'" }
    $ro = [W]::Open("$d\$tgtShown", $isDir)
    Check ($ro -ceq $want) "$k $id opens by the new shown name" $ro
  }
}
$expA = @($names | ForEach-Object { Hex ("x" + $_[0] + ":y") })
$stA = Stored $fs "/A"
$gotA = @($stA | Where-Object { $_ -like '*|file|' } | ForEach-Object { ($_ -split '\|')[0] })
Check (SameSet $gotA $expA) "A: every renamed file is stored with the raw colon" ($stA -join ';')
$stAF = Stored $fs "/A/F"
$gotAF = @($stAF | ForEach-Object { ($_ -split '\|')[0] })
Check (SameSet $gotAF $expA) "A/F: every renamed folder is stored with the raw colon" ($stAF -join ';')

# 4. Delete, by the shown name, in tree B.
Say ""
Say "## delete"
foreach ($k in "file","folder") {
  $isDir = ($k -eq "folder")
  $dB = ($dirs[$k] -replace '^M:\\A','M:\B')
  foreach ($n in $names) {
    $id = $n[0]; $q = $dB + "\" + $shown[$id]
    $r = [W]::Delete($q, $isDir)
    Check ($r -ceq "OK") "$k $id deletes by its shown name" $r
    $left = @(List-Cmd $dB | Where-Object { $_ -ceq $shown[$id] })
    Check ($left.Count -eq 0) "$k $id is gone from the listing"
  }
}
$stB = Stored $fs "/B"
Check (SameSet @($stB | ForEach-Object { ($_ -split '\|')[0] }) @((Hex "F"))) "B: every file is deleted from the store" ($stB -join ';')
$stBF = Stored $fs "/B/F"
Check ($stBF.Count -eq 0) "B/F: every folder is deleted from the store" ($stBF -join ';')

# 5. Created in Windows with a substitute: stored with the raw character.
Say ""
Say "## create"
New-Item -ItemType Directory "$drive\C" | Out-Null
$cn = @(
  @(("a" + [char]0xF03A + "b"), "a:b", "file"),
  @(("d" + [char]0xF03F + "e"), "d?e", "dir"),
  @(("t" + [char]0xF02E), "t.", "file"),
  @(("s" + [char]0xF020), "s ", "file"),
  @(("q" + [char]0xF000 + [char]0xF03A + "z"), ("q" + [char]0xF03A + "z"), "file"),
  @(("w" + [char]0xF05C + "v"), 'w\v', "file"),
  @(("c" + [char]0xF009 + "d"), ("c" + [char]9 + "d"), "file"))
foreach ($c in $cn) {
  $r = [W]::Create("$drive\C\" + $c[0], ($c[2] -eq "dir"))
  Check ($r -ceq "OK") ("create " + $c[2] + " " + (Esc $c[1]) + " by its shown name") $r
}
$r = [W]::Symlink("$drive\C\sl", ("t" + [char]0xF03A + "u"))
Check ($r -ceq "OK") "create a symlink whose target holds U+F03A" $r
$stC = Stored $fs "/C"
foreach ($c in $cn) { Check (HasStored $stC $c[1] $c[2]) ("stored as " + (Esc $c[1]) + " (" + $c[2] + ")") ($stC -join ';') }
$wantLink = (Hex "sl") + "|symlink|" + (Hex "t:u")
Check ($stC -contains $wantLink) "the symlink target is stored with the raw colon" ($stC -join ';')
$lc = List-Cmd "$drive\C"
Check (($lc -ccontains ("a" + [char]0xF03A + "b")) -and ($lc -ccontains ("q" + [char]0xF000 + [char]0xF03A + "z"))) "a created a:b and the literal-U+F03A name list as two names"
$bad = "$drive\C\bad" + [char]0xF000
$r = [W]::Create($bad, $false)
Check ($r -like 'W123*') "a create with a bad U+F000 sequence is refused" $r
$r = [W]::Create("$drive\C\" + ([string][char]0xF03A * 256), $false)
Check ($r -notlike 'OK*') "a 256-unit name is refused" $r
$r = [W]::Create("$drive\C\" + ([string][char]0xF03A * 255), $false)
Check ($r -ceq "OK") "a 255-unit name full of U+F03A is created" $r
Check (HasStored (Stored $fs "/C") (':' * 255) "file") "it is stored as 255 colons"
$rd = [W]::Delete("$drive\C\t" + [char]0xF02E, $false)
Check ($rd -ceq "OK") "a trailing-dot name deletes by its shown name" $rd
Check (-not (HasStored (Stored $fs "/C") "t." "file")) "and is gone from the store"

Stop-Fs

# --- the primary-Windows control: no translation -----------------------------
Say ""
Say "## primary-Windows control"
$fs2 = "names-pw-" + (Get-Random)
& $Cli fs create @store -fs $fs2 -primary-windows *> C:\Windows\Temp\names-create2.log
if (-not (Mount-Fs $fs2)) { Say "MOUNT-FAILED (primary-Windows)"; Get-Content C:\Windows\Temp\names-err.log; Say "RESULT fail=1"; exit 1 }
$pw = @(("a" + [char]0xF03A + "b"), ("m" + [char]0xF000 + [char]0xF03A + "n"))
foreach ($nm in $pw) {
  $r = [W]::Create("$drive\$nm", $false)
  Check ($r -ceq "OK") ("primary-Windows: create " + (Esc $nm)) $r
}
$lp = List-Cmd "$drive\"
foreach ($nm in $pw) { Check ($lp -ccontains $nm) ("primary-Windows: " + (Esc $nm) + " is listed as typed, not translated") }
$stP = Stored $fs2 "/"
foreach ($nm in $pw) { Check (HasStored $stP $nm "file") ("primary-Windows: " + (Esc $nm) + " is stored as typed") ($stP -join ';') }
$r = [W]::Create("\\?\$drive\bad<name", $false)
Check ($r -notlike 'OK*') "primary-Windows: a raw < name is still refused" $r
Stop-Fs

if ($script:fail -eq 0) { Say "RESULT done" } else { Say ("RESULT fail=" + $script:fail); exit 1 }
