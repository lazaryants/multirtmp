<#
.SYNOPSIS
    Release build: -ldflags="-s -w" (strip symbols/debug info) + version and
    git commit hash baked in via -X, for -version and GET /status.

.PARAMETER Version
    Version string to embed. Defaults to `git describe --tags --always`
    (falls back to "dev" if git is unavailable — e.g. no .git directory).

.EXAMPLE
    .\build.ps1
    .\build.ps1 -Version v1.2.0
#>
param(
    [string]$Version = ""
)

$ErrorActionPreference = "Stop"

if (-not $Version) {
    try {
        $Version = (git describe --tags --always 2>$null).Trim()
    } catch {
        $Version = ""
    }
    if (-not $Version) {
        $Version = "dev"
    }
}

$Commit = "unknown"
try {
    $Commit = (git rev-parse --short HEAD 2>$null).Trim()
    if (-not $Commit) { $Commit = "unknown" }
} catch {}

$pkg = "multirtmp/internal/buildinfo"
$ldflags = "-s -w -X $pkg.Version=$Version -X $pkg.Commit=$Commit"

New-Item -ItemType Directory -Force -Path bin | Out-Null

Write-Host "Building multirtmp $Version ($Commit)..."
go build -ldflags="$ldflags" -o bin\multirtmp.exe .\cmd\multirtmp
if ($LASTEXITCODE -ne 0) {
    throw "go build failed with exit code $LASTEXITCODE"
}

Write-Host "OK: bin\multirtmp.exe"
& bin\multirtmp.exe -version
