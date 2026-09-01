# Build the release executable eliaukvpn.exe (windowless, self-contained apart
# from wintun.dll + the system WebView2 runtime).
#
# Usage:
#   powershell -File build.ps1            # builds v1.0.0
#   powershell -File build.ps1 1.2.3      # stamps main.version = 1.2.3

param([string]$Version = "1.0.0")

$ErrorActionPreference = 'Stop'
$go = 'C:\Program Files\Go\bin\go.exe'
if (-not (Test-Path $go)) { $go = (Get-Command go -ErrorAction Stop).Source }

$ldflags = "-H windowsgui -X main.version=$Version"
Write-Host "building eliaukvpn.exe (v$Version) ..."
& $go build -trimpath -ldflags $ldflags -o eliaukvpn.exe ./cmd/gui
if ($LASTEXITCODE -ne 0) { throw "build failed (exit $LASTEXITCODE)" }

# Ship the Wintun DLL beside the exe so the virtual NIC works on first run.
$dll = 'wintun.dll'
if (-not (Test-Path $dll) -and (Test-Path "bin\$dll")) {
    Copy-Item "bin\$dll" $dll -Force
    Write-Host "copied $dll"
}

Write-Host "done: $((Resolve-Path .\eliaukvpn.exe).Path)"
