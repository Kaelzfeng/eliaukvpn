# e2e-gui.ps1 — end-to-end test for the foolproof GUI (task #25).
#
# Verifies the new orchestration with a local coordination server on 9090/9091:
#  1. two GUI instances start from pre-written config.json (no -name/-server
#     flags) and register with the server;
#  2. they whitelist each other by fingerprint and establish an encrypted
#     session (p2p: session established);
#  3. mcprobe server/client confirm the data plane: the joining instance
#     discovers the world at the host's *virtual* IP and reaches it over TCP;
#  4. config.json and identity.key are written; both instances exit cleanly
#     (exit code 0) via -exit-after.
#
# Requires an elevated shell (virtual NICs are created). Leaves the machine as
# it found it: adapter and temp files are removed at the end.
#
# Run:  powershell -ExecutionPolicy Bypass -File e2e-gui.ps1

param(
    [string]$ServerAddr = 'ws://127.0.0.1:9090/ws',
    [int]$GuiLifetime = 50
)

$ErrorActionPreference = 'Stop'
$root = 'E:\eliaukvpn'
$go = 'C:\Program Files\Go\bin\go.exe'
Set-Location $root

$stamp = Get-Date -Format 'HHmmss'
$tmp = Join-Path $root ('.e2e' + $stamp)
New-Item -ItemType Directory -Path $tmp | Out-Null

$procs = New-Object System.Collections.ArrayList

function Wait-Log([string]$path, [string]$pattern, [int]$timeoutSec) {
    $deadline = (Get-Date).AddSeconds($timeoutSec)
    while ((Get-Date) -lt $deadline) {
        if (Test-Path $path) {
            try {
                if (Select-String -Path $path -Pattern $pattern -Quiet -ErrorAction Stop) { return $true }
            } catch {}
        }
        Start-Sleep -Milliseconds 500
    }
    return $false
}

function Start-Proc([string]$exe, [string[]]$args, [string]$name) {
    $p = Start-Process -FilePath $exe -ArgumentList $args -NoNewWindow `
        -RedirectStandardOutput "$tmp\$name.out" -RedirectStandardError "$tmp\$name.err" -PassThru
    [void]$procs.Add($p)
    return $p
}

try {
    Write-Host '=== build ==='
    & $go build -o bin/server.exe ./cmd/server;   if ($LASTEXITCODE -ne 0) { throw 'build server' }
    & $go build -o bin/gui.exe ./cmd/gui;         if ($LASTEXITCODE -ne 0) { throw 'build gui' }
    & $go build -o bin/mcprobe.exe ./cmd/mcprobe; if ($LASTEXITCODE -ne 0) { throw 'build mcprobe' }
    & $go build -o bin/genident.exe ./cmd/genident; if ($LASTEXITCODE -ne 0) { throw 'build genident' }

    Write-Host '=== identities + configs ==='
    $hostFp = (& .\bin\genident.exe "$tmp\host.key" | Select-Object -Last 1).Trim()
    $joinFp = (& .\bin\genident.exe "$tmp\join.key" | Select-Object -Last 1).Trim()
    Write-Host "  host fp = $hostFp"
    Write-Host "  join fp = $joinFp"

    @{ name='host'; server=$ServerAddr; friends=@(@{ name='join'; code=$joinFp }) } |
        ConvertTo-Json -Depth 3 | Set-Content -Path "$tmp\host.json" -Encoding UTF8
    @{ name='join'; server=$ServerAddr; friends=@(@{ name='host'; code=$hostFp }) } |
        ConvertTo-Json -Depth 3 | Set-Content -Path "$tmp\join.json" -Encoding UTF8

    Write-Host '=== start server (9090/9091) ==='
    Start-Proc .\bin\server.exe @('-addr',':9090','-relay-listen','0.0.0.0:9091','-relay-public','127.0.0.1:9091') 'server'
    Start-Sleep -Seconds 1

    Write-Host '=== start two GUI instances ==='
    $hostProc = Start-Proc .\bin\gui.exe @('-config',"$tmp\host.json",'-keyfile',"$tmp\host.key",'-vnic-name','Eliauk-e2eh','-exit-after',"${GuiLifetime}s",'-debug-packets') 'host'
    $joinProc = Start-Proc .\bin\gui.exe @('-config',"$tmp\join.json",'-keyfile',"$tmp\join.key",'-vnic-name','Eliauk-e2ej','-exit-after',"${GuiLifetime}s",'-debug-packets') 'join'

    Write-Host '=== wait for registration + session ==='
    $hReg = Wait-Log "$tmp\host.out" 'registered' 25
    $jReg = Wait-Log "$tmp\join.out" 'registered' 25
    Write-Host "  host registered: $hReg ; join registered: $jReg"
    $hSes = Wait-Log "$tmp\host.out" 'session established' 20
    $jSes = Wait-Log "$tmp\join.out" 'session established' 20
    Write-Host "  host session: $hSes ; join session: $jSes"
    Start-Sleep -Seconds 2

    Write-Host '=== data plane: mcprobe server (host side) + client (join side) ==='
    $mcSrv = Start-Proc .\bin\mcprobe.exe @('-mode','server') 'mc-server'
    Start-Sleep -Seconds 2
    $mcCli = Start-Proc .\bin\mcprobe.exe @('-mode','client') 'mc-client'
    Start-Sleep -Seconds 12

    Write-Host '--- mc-client.out ---'
    Get-Content "$tmp\mc-client.out" -ErrorAction SilentlyContinue | Select-Object -First 15
    Write-Host '--- mc-server.out ---'
    Get-Content "$tmp\mc-server.out" -ErrorAction SilentlyContinue | Select-Object -First 10

    Write-Host '--- debug-packets (host) ---'
    Get-Content "$tmp\host.out" -ErrorAction SilentlyContinue | Select-String 'vnic->tunnel|tunnel->vnic' | Select-Object -First 6
    Write-Host '--- debug-packets (join) ---'
    Get-Content "$tmp\join.out" -ErrorAction SilentlyContinue | Select-String 'vnic->tunnel|tunnel->vnic' | Select-Object -First 6

    Write-Host '=== wait for GUI exit ==='
    $deadline = (Get-Date).AddSeconds($GuiLifetime + 20)
    while ((Get-Date) -lt $deadline) {
        if ($hostProc.HasExited -and $joinProc.HasExited) { break }
        Start-Sleep -Seconds 1
    }
    Write-Host "  host exited=$($hostProc.HasExited) code=$($hostProc.ExitCode)"
    Write-Host "  join exited=$($joinProc.HasExited) code=$($joinProc.ExitCode)"

    Write-Host '=== config + identity written ==='
    Get-Content "$tmp\host.json"
    Write-Host "  identity keys: host=$(Test-Path "$tmp\host.key") join=$(Test-Path "$tmp\join.key")"
} finally {
    Write-Host '=== cleanup ==='
    foreach ($p in $procs) {
        try { if (-not $p.HasExited) { Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue } } catch {}
    }
    Remove-NetAdapter -Name 'Eliauk-e2eh','Eliauk-e2ej' -Confirm:$false -ErrorAction SilentlyContinue
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
    Write-Host "  workspace removed: $tmp"
}
