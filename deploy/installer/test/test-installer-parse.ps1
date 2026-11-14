<#
.SYNOPSIS
    Checks the installer's parsing code (deploy\installer\parse.iss) without
    running the installer: compiles parse-test.iss and runs it. It installs
    nothing.

.PARAMETER LivePort
    Port of a running multirtmp API to ask /status (optional).

.PARAMETER ExpectLive
    Whether a source is connected to that multirtmp right now.
#>
param(
    [string]$LivePort = "",
    [switch]$ExpectLive
)

$ErrorActionPreference = "Stop"
$here = $PSScriptRoot
$root = Resolve-Path (Join-Path $here "..\..\..")
$work = Join-Path ([IO.Path]::GetTempPath()) ("multirtmp-parse-test-" + [guid]::NewGuid().ToString("N").Substring(0, 8))
New-Item -ItemType Directory -Force -Path $work | Out-Null

try {
    # The config "install" really writes (config.WriteInstallConfig).
    $installCfg = Join-Path $work "install-config.json"
    $env:MULTIRTMP_WRITE_INSTALL_CONFIG = $installCfg
    try {
        go test -count=1 -run '^TestWriteInstallConfigForInstallerTest$' "$root\internal\config" | Out-Null
        if ($LASTEXITCODE -ne 0) { throw "could not generate the install config" }
    } finally { Remove-Item Env:MULTIRTMP_WRITE_INSTALL_CONFIG }

    $iscc = $null
    foreach ($key in 'HKCU:\SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\Inno Setup 6_is1',
                     'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\Inno Setup 6_is1',
                     'HKLM:\SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall\Inno Setup 6_is1') {
        if (Test-Path $key) {
            $c = Join-Path (Get-ItemProperty $key).InstallLocation "ISCC.exe"
            if (Test-Path $c) { $iscc = $c; break }
        }
    }
    if (-not $iscc) { throw "Inno Setup 6 (ISCC.exe) not found" }

    & $iscc /Q "/O$work" (Join-Path $here "parse-test.iss")
    if ($LASTEXITCODE -ne 0) { throw "ISCC failed with exit code $LASTEXITCODE" }

    $result = Join-Path $work "result.txt"
    $args = @('/VERYSILENT', '/SUPPRESSMSGBOXES', "/RESULT=$result",
              "/TESTDATA=$root\internal\config\testdata", "/INSTALLCFG=$installCfg")
    if ($LivePort) { $args += "/LIVEPORT=$LivePort"; $args += "/EXPECTLIVE=$([int][bool]$ExpectLive)" }
    $p = Start-Process (Join-Path $work "parse-test.exe") -ArgumentList $args -Wait -PassThru

    Get-Content $result
    if ($p.ExitCode -ne 0) { throw "installer parse test FAILED (exit code $($p.ExitCode))" }
    Write-Host "installer parse test: OK"
} finally {
    Remove-Item $work -Recurse -Force -ErrorAction SilentlyContinue
}
