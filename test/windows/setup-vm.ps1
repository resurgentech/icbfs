# Installs what the test runs need on a freshly created Windows VM: Go,
# WinFsp (with its bin directory on the machine Path) and winfsp-tests,
# and the .NET SDK and llvm-mingw for native apps built on icbfs.
# Run by setup-vm.sh, over SSH, as Administrator. Safe to run again.
param(
  [Parameter(Mandatory)][string]$GoVersion,     # e.g. 1.27.3
  [Parameter(Mandatory)][string]$WinFspVersion, # the release tag, e.g. 2.1
  [Parameter(Mandatory)][string]$WinFspBuild,   # e.g. 2.1.25156
  [Parameter(Mandatory)][string]$DotnetVersion, # the SDK, e.g. 10.0.402
  [Parameter(Mandatory)][string]$LlvmMingwVersion # the release tag, e.g. 20260922
)
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue' # Invoke-WebRequest is very slow with it on
$tmp = 'C:\Windows\Temp'

function Install-Msi($url, $file) {
  Invoke-WebRequest $url -OutFile "$tmp\$file"
  $p = Start-Process msiexec -ArgumentList "/i `"$tmp\$file`" /qn /norestart" -Wait -PassThru
  if ($p.ExitCode -ne 0 -and $p.ExitCode -ne 3010) { throw "${file}: msiexec exit $($p.ExitCode)" }
}

# Defender takes a generated file of modernc.org/sqlite for malware
# (sqlite_g_*.go) and blocks the build. Keep it out of the module cache,
# the build cache and the working tree the runners copy in, but on
# everywhere else.
foreach ($dir in "$env:USERPROFILE\go", "$env:LOCALAPPDATA\go-build", 'C:\icbfs-test') {
  Add-MpPreference -ExclusionPath $dir
}
# And allow that one signature (TrojanDropper:VBS/Mikey.A!MSR), in case
# it's hit before the exclusions apply. Go remembers a blocked read in
# its build cache, so after a block: go clean -cache.
Add-MpPreference -ThreatIDDefaultAction_Ids 2147750906 -ThreatIDDefaultAction_Actions Allow

Install-Msi "https://go.dev/dl/go$GoVersion.windows-amd64.msi" 'go.msi'

$release = "https://github.com/winfsp/winfsp/releases/download/v$WinFspVersion"
Install-Msi "$release/winfsp-$WinFspBuild.msi" 'winfsp.msi'
Invoke-WebRequest "$release/winfsp-tests-$WinFspBuild.zip" -OutFile "$tmp\winfsp-tests.zip"
Expand-Archive "$tmp\winfsp-tests.zip" -DestinationPath "$tmp\winfsp-tests" -Force

# For native apps built on icbfs: the .NET SDK, which builds a WinUI
# front end, and a MinGW C compiler for cgo, which builds a Go core as a
# DLL the front end loads.
Invoke-WebRequest https://dot.net/v1/dotnet-install.ps1 -OutFile "$tmp\dotnet-install.ps1"
& "$tmp\dotnet-install.ps1" -Version $DotnetVersion -InstallDir 'C:\Program Files\dotnet' | Out-Null
$mingw = "llvm-mingw-$LlvmMingwVersion-ucrt-x86_64"
if (-not (Test-Path "C:\$mingw")) {
  Invoke-WebRequest "https://github.com/mstorsjo/llvm-mingw/releases/download/$LlvmMingwVersion/$mingw.zip" -OutFile "$tmp\llvm-mingw.zip"
  Expand-Archive "$tmp\llvm-mingw.zip" -DestinationPath 'C:\' -Force
}

# WinFsp's installer doesn't put its bin directory on the Path, nor do
# the other two theirs.
$path = [Environment]::GetEnvironmentVariable('Path', 'Machine')
foreach ($bin in 'C:\Program Files (x86)\WinFsp\bin', 'C:\Program Files\dotnet', "C:\$mingw\bin") {
  if ($path -notlike "*$bin*") { $path = "$path;$bin" }
}
[Environment]::SetEnvironmentVariable('Path', $path, 'Machine')
'installed'
