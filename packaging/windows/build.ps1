# Builds icbfs's MSIs on Windows from the commands package.sh built and
# brought here: signs the commands, builds an MSI per architecture (WiX,
# pinned in .config\dotnet-tools.json beside this), and signs those.
# package.sh runs it on the build VM; it runs anywhere with the .NET 10
# SDK, from a folder holding
#
#   bin\x64\icbfs-winfsp.exe, bin\x64\icbfs-cli.exe, bin\arm64\...
#   packaging\windows\        this file, icbfs.wxs, mount-task.ps1, README.txt, .config\
#   LICENSE
#
# The signing key comes on stdin, two lines: the PFX in base64, then its
# password. With nothing on stdin, the package is unsigned.
#
#   powershell -File packaging\windows\build.ps1 -Version 0.1.1 < key
param(
  [Parameter(Mandatory)][string]$Version,
  [string]$Out = ""
)
$ErrorActionPreference = 'Stop'
$root = Resolve-Path "$PSScriptRoot\..\.."
if (-not $Out) { $Out = "$root\dist" }
# The SDK's own folder, when an SSH session's PATH hasn't got it.
if (-not (Get-Command dotnet -ErrorAction SilentlyContinue) -and (Test-Path "$env:ProgramFiles\dotnet\dotnet.exe")) {
  $env:PATH = "$env:ProgramFiles\dotnet;$env:PATH"
}

$cert = $null
$pfx = if ([Console]::IsInputRedirected) { [Console]::In.ReadLine() } else { $null }
if ($pfx) {
  $password = [Console]::In.ReadLine()
  $cert = [Security.Cryptography.X509Certificates.X509Certificate2]::new([Convert]::FromBase64String($pfx), $password)
  if (-not $cert.HasPrivateKey) { throw "the signing key has no private key" }
  "signing as $($cert.Subject), $($cert.Thumbprint)"
} else {
  "no signing key on stdin: the package is unsigned"
}

function Sign([string[]]$files) {
  if (-not $cert) { return }
  foreach ($f in $files) {
    $s = Set-AuthenticodeSignature -FilePath $f -Certificate $cert -HashAlgorithm SHA256 -TimestampServer 'http://timestamp.digicert.com'
    # A self-signed key's signature reads UnknownError on a machine that
    # doesn't trust it yet; what matters is that the file is signed.
    if (-not $s.SignerCertificate) { throw "signing ${f}: $($s.StatusMessage)" }
  }
}

function Run([scriptblock]$b, [string]$what) {
  & $b
  if ($LASTEXITCODE -ne 0) { throw "${what}: exit $LASTEXITCODE" }
}

try {
  New-Item -ItemType Directory -Force $Out | Out-Null
  Push-Location $PSScriptRoot
  try {
    Run { dotnet tool restore } 'dotnet tool restore'
    $wixVersion = (Get-Content .config\dotnet-tools.json | ConvertFrom-Json).tools.wix.version
    Run { dotnet wix extension add -acceptEula wix7 "WixToolset.Util.wixext/$wixVersion" } 'wix extension add'
    foreach ($arch in 'x64', 'arm64') {
      $bin = "$root\bin\$arch"
      Sign @("$bin\icbfs-winfsp.exe", "$bin\icbfs-cli.exe")
      $msi = "$Out\icbfs-$Version-$arch.msi"
      # WiX v6 and later are under the Open Source Maintenance Fee: free
      # until the organization's revenue passes $10,000 a year, then a
      # sponsorship of wixtoolset on GitHub (docs.firegiant.com/wix/osmf).
      Run { dotnet wix build -acceptEula wix7 icbfs.wxs -arch $arch -d "Version=$Version" -d "BinDir=$bin" -ext "WixToolset.Util.wixext/$wixVersion" -o $msi } "wix build $arch"
      Sign @($msi)
      Remove-Item "$Out\icbfs-$Version-$arch.wixpdb" -ErrorAction SilentlyContinue
      "built $msi"
    }
  } finally {
    Pop-Location
  }
} finally {
  if ($cert) { $cert.Dispose() }
}
