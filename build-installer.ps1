<#
.SYNOPSIS
    Builds the installer: dist\multirtmp-setup-<version>.exe.

.DESCRIPTION
    1. Builds multirtmp.exe with build.ps1 (same version as the installer).
    2. Takes mediamtx.exe and its LICENSE from -MediaMTXDir (mediamtx.exe is
       not in the repository) and checks its version: deploy\mediamtx\
       mediamtx.yml is made for one exact MediaMTX version, and MediaMTX
       refuses to start with settings it does not know.
    3. Runs Inno Setup's ISCC on deploy\installer\multirtmp.iss.

.PARAMETER Version
    Defaults to `git describe --tags --always`, as in build.ps1.

.PARAMETER MediaMTXDir
    Folder with mediamtx.exe and LICENSE. Default: C:\mediamtx.

.PARAMETER MediaMTXVersion
    The MediaMTX version deploy\mediamtx\mediamtx.yml is made for.

.EXAMPLE
    .\build-installer.ps1
    .\build-installer.ps1 -MediaMTXDir D:\tools\mediamtx
#>
param(
    [string]$Version = "",
    [string]$MediaMTXDir = "C:\mediamtx",
    [string]$MediaMTXVersion = "v1.21.3"
)

$ErrorActionPreference = "Stop"
$root = $PSScriptRoot

if (-not $Version) {
    try { $Version = (git -C $root describe --tags --always 2>$null).Trim() } catch { $Version = "" }
    if (-not $Version) { $Version = "dev" }
}

# Installer file name: v1.2.0 -> 1.2.0. Windows file version must be numeric:
# v1.2.0 -> 1.2.0.0, v1.2.0-3-gabc123 -> 1.2.0.3, anything else -> 0.0.0.0.
$fileVersion = $Version -replace '^v', ''
$numericVersion = "0.0.0.0"
if ($Version -match '^v?(\d+)\.(\d+)\.(\d+)(?:-(\d+)-g[0-9a-f]+)?$') {
    $build = if ($Matches[4]) { $Matches[4] } else { "0" }
    $numericVersion = "$($Matches[1]).$($Matches[2]).$($Matches[3]).$build"
}

# MediaMTX
$mtxExe = Join-Path $MediaMTXDir "mediamtx.exe"
$mtxLicense = Join-Path $MediaMTXDir "LICENSE"
foreach ($f in $mtxExe, $mtxLicense) {
    if (-not (Test-Path $f)) { throw "Not found: $f (set -MediaMTXDir)" }
}
$mtxActual = (& $mtxExe --version 2>&1 | Out-String).Trim()
if ($mtxActual -ne $MediaMTXVersion) {
    throw "mediamtx.exe in $MediaMTXDir is '$mtxActual', but deploy\mediamtx\mediamtx.yml is made for $MediaMTXVersion. Use the matching mediamtx.exe, or update mediamtx.yml and -MediaMTXVersion together."
}

# Inno Setup is often installed per-user (no admin), so look it up in the
# registry rather than in Program Files.
$iscc = $null
foreach ($key in 'HKCU:\SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\Inno Setup 6_is1',
                 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\Inno Setup 6_is1',
                 'HKLM:\SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall\Inno Setup 6_is1') {
    if (Test-Path $key) {
        $candidate = Join-Path (Get-ItemProperty $key).InstallLocation "ISCC.exe"
        if (Test-Path $candidate) { $iscc = $candidate; break }
    }
}
if (-not $iscc) { throw "Inno Setup 6 (ISCC.exe) not found" }

# multirtmp.exe
Push-Location $root
try { & (Join-Path $root "build.ps1") -Version $Version } finally { Pop-Location }

$outDir = Join-Path $root "dist"
New-Item -ItemType Directory -Force -Path $outDir | Out-Null

Write-Host "Building installer $Version (file version $numericVersion), MediaMTX $mtxActual..."
& $iscc /Q `
    "/DAppVersion=$Version" `
    "/DFileVersion=$fileVersion" `
    "/DNumericVersion=$numericVersion" `
    "/DSourceRoot=$root" `
    "/DMediaMTXDir=$MediaMTXDir" `
    "/DOutputDir=$outDir" `
    (Join-Path $root "deploy\installer\multirtmp.iss")
if ($LASTEXITCODE -ne 0) { throw "ISCC failed with exit code $LASTEXITCODE" }

$setup = Join-Path $outDir "multirtmp-setup-$fileVersion.exe"
Write-Host "OK: $setup"
