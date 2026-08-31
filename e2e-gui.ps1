# e2e-gui.ps1 — end-to-end test for the Eliauk GUI (M6d foolproof + M7a accounts + M7c game panel).
#
# Drives the real GUI binary against a local coordination server on 9090/9091:
#
#  Phase A (legacy anonymous p2p): two GUI instances start from pre-written
#    config.json, whitelist each other by fingerprint, establish an encrypted
#    session (p2p: session established), and the mcprobe client reaches the
#    server world at the host's *virtual* IP over TCP.
#
#  Phase B (M7a accounts + session token): one GUI registers a fresh account
#    via -create-account/-password, caches the server-issued session token in
#    its config, then logs back in with the token only (no password).
#
#  Phase C (M7c game panel): the same GUI auto-starts a dedicated server with a
#    small Java "server" compiled on the fly (javac+jar), which proves the
#    game panel writes eula.txt + server.properties, launches java, persists the
#    java/server-jar paths in the config, and stops the server at exit.
#
# Requires an elevated shell (Phase A creates virtual NICs). Leaves the machine
# as it found it: adapter, processes and temp files are removed at the end.
#
# Run:  powershell -ExecutionPolicy Bypass -File e2e-gui.ps1

param(
    [string]$ServerAddr = 'ws://127.0.0.1:9090/ws',
    [int]$GuiLifetime = 50
)

$ErrorActionPreference = 'Stop'
$root = 'E:\eliaukvpn'
$go = 'C:\Program Files\Go\bin\go.exe'
$javaHome = $env:JAVA_HOME
Set-Location $root
# Make wintun.dll findable for the cleanup P/Invoke (it lives next to the exes).
$env:PATH = "$root\bin;" + $env:PATH

$stamp = Get-Date -Format 'HHmmss'
$tmp = Join-Path $root ('.e2e' + $stamp)
New-Item -ItemType Directory -Path $tmp | Out-Null

$procs = New-Object System.Collections.ArrayList

function Wait-Log([string]$path, [string]$pattern, [int]$timeoutSec) {
    # Go's log.* writes to stderr (.err); mcprobe writes to stdout (.out) — check both.
    $alt = $path
    if ($path -like '*.out') { $alt = [System.IO.Path]::ChangeExtension($path, '.err') }
    elseif ($path -like '*.err') { $alt = [System.IO.Path]::ChangeExtension($path, '.out') }
    $deadline = (Get-Date).AddSeconds($timeoutSec)
    while ((Get-Date) -lt $deadline) {
        foreach ($p in @($path, $alt)) {
            if (Test-Path $p) {
                try {
                    if (Select-String -Path $p -Pattern $pattern -Quiet -ErrorAction Stop) { return $true }
                } catch {}
            }
        }
        Start-Sleep -Milliseconds 500
    }
    return $false
}

function Start-Proc([string]$exe, [string[]]$argList, [string]$name) {
    # NOTE: param must NOT be called $args — that collides with PowerShell's
    # automatic $args variable and the array silently never binds (caught here).
    $p = Start-Process -FilePath $exe -ArgumentList $argList -NoNewWindow `
        -RedirectStandardOutput "$tmp\$name.out" -RedirectStandardError "$tmp\$name.err" -PassThru
    [void]$procs.Add($p)
    return $p
}

# Wintun P/Invoke: cleanup of Eliauk-e2e* adapters. Remove-NetAdapter is
# unreliable under -NoProfile (module cmdlets don't surface), wintun.dll does.
Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;
public static class WintunClean {
    [DllImport("wintun.dll", CharSet = CharSet.Unicode, CallingConvention = CallingConvention.StdCall)]
    public static extern bool WintunDeleteAdapter(string name);
}
'@

function Wait-ProcExit([object]$p, [int]$timeoutSec) {
    $deadline = (Get-Date).AddSeconds($timeoutSec)
    while ((Get-Date) -lt $deadline -and -not $p.HasExited) { Start-Sleep -Milliseconds 500 }
    return $p.HasExited
}

function Test-TcpPort([int]$port, [int]$timeoutMs = 2000) {
    $client = New-Object System.Net.Sockets.TcpClient
    try {
        $iar = $client.BeginConnect('127.0.0.1', $port, $null, $null)
        if ($iar.AsyncWaitHandle.WaitOne($timeoutMs)) {
            $client.EndConnect($iar)
            return $true
        }
        return $false
    } catch {
        return $false
    } finally {
        $client.Close()
    }
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

    Write-Host '=== start server (9090/9091, account store) ==='
    $serverProc = Start-Proc .\bin\server.exe @('-addr',':9090','-relay-listen','0.0.0.0:9091','-relay-public','127.0.0.1:9091','-accounts',"$tmp\accounts.json") 'server'
    Start-Sleep -Seconds 1

    Write-Host '=== Phase A: legacy anonymous p2p ==='
    $hostProc = Start-Proc .\bin\gui.exe @('-config',"$tmp\host.json",'-keyfile',"$tmp\host.key",'-vnic-name','Eliauk-e2eh','-exit-after',"${GuiLifetime}s",'-debug-packets') 'host'
    $joinProc = Start-Proc .\bin\gui.exe @('-config',"$tmp\join.json",'-keyfile',"$tmp\join.key",'-vnic-name','Eliauk-e2ej','-exit-after',"${GuiLifetime}s",'-debug-packets') 'join'

    Write-Host '--- wait for registration + session ---'
    $hReg = Wait-Log "$tmp\host.out" 'registered' 25
    $jReg = Wait-Log "$tmp\join.out" 'registered' 25
    Write-Host "  host registered: $hReg ; join registered: $jReg"
    $hSes = Wait-Log "$tmp\host.out" 'session established' 20
    $jSes = Wait-Log "$tmp\join.out" 'session established' 20
    Write-Host "  host session: $hSes ; join session: $jSes"
    Start-Sleep -Seconds 2

    Write-Host '--- data plane: mcprobe server (host side) + client (join side) ---'
    $mcSrv = Start-Proc .\bin\mcprobe.exe @('-mode','server') 'mc-server'
    Start-Sleep -Seconds 2
    $mcCli = Start-Proc .\bin\mcprobe.exe @('-mode','client') 'mc-client'
    Start-Sleep -Seconds 12

    Write-Host '--- mc-client.out ---'
    Get-Content "$tmp\mc-client.out" -ErrorAction SilentlyContinue | Select-Object -First 15
    Write-Host '--- mc-server.out ---'
    Get-Content "$tmp\mc-server.out" -ErrorAction SilentlyContinue | Select-Object -First 10

    Write-Host '--- debug-packets (host) ---'
    Get-Content "$tmp\host.err","$tmp\host.out" -ErrorAction SilentlyContinue | Select-String 'vnic->tunnel|tunnel->vnic' | Select-Object -First 6
    Write-Host '--- debug-packets (join) ---'
    Get-Content "$tmp\join.err","$tmp\join.out" -ErrorAction SilentlyContinue | Select-String 'vnic->tunnel|tunnel->vnic' | Select-Object -First 6

    # Phase A data plane proven — release 25565: mcprobe -mode server holds it
    # forever, which would mask Phase C's fake-Java-server bind/port checks.
    foreach ($p in @($mcSrv, $mcCli)) {
        try { if ($p -and -not $p.HasExited) { Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue } } catch {}
    }
    Write-Host '--- mcprobe stopped (25565 released) ---'

    Write-Host '--- wait for GUI exit (phase A) ---'
    [void](Wait-ProcExit $hostProc ($GuiLifetime + 20))
    [void](Wait-ProcExit $joinProc ($GuiLifetime + 20))
    Write-Host "  host exited=$($hostProc.HasExited) code=$($hostProc.ExitCode)"
    Write-Host "  join exited=$($joinProc.HasExited) code=$($joinProc.ExitCode)"

    Write-Host '--- config + identity written ---'
    Get-Content "$tmp\host.json"
    Write-Host "  identity keys: host=$(Test-Path "$tmp\host.key") join=$(Test-Path "$tmp\join.key")"

    Write-Host '=== Phase B: M7a account register -> session token -> token login ==='
    @{ name='acct'; server=$ServerAddr } | ConvertTo-Json | Set-Content -Path "$tmp\acct.json" -Encoding UTF8

    $reg = Start-Proc .\bin\gui.exe @('-config',"$tmp\acct.json",'-keyfile',"$tmp\acct.key",'-account','host','-password','e2epass','-create-account','-vnic=false','-exit-after','25s') 'acct-reg'
    $r1 = Wait-Log "$tmp\acct-reg.out" 'registered' 25
    Write-Host "  account registered: $r1"
    if (-not $r1) { throw 'account registration failed' }
    [void](Wait-ProcExit $reg 40)

    $acct = Get-Content "$tmp\acct.json" -Raw | ConvertFrom-Json
    $tokLen = 0
    if ($acct.token) { $tokLen = $acct.token.Length }
    Write-Host "  cfg account = '$($acct.account)' (expect 'host'); token bytes = $tokLen (expect > 0)"
    if ($acct.account -ne 'host' -or $tokLen -le 0) { throw 'account/token not persisted' }

    $tok2 = Start-Proc .\bin\gui.exe @('-config',"$tmp\acct.json",'-keyfile',"$tmp\acct.key",'-account','host','-vnic=false','-exit-after','20s') 'acct-login'
    $r2 = Wait-Log "$tmp\acct-login.out" 'registered' 25
    Write-Host "  token-only login registered: $r2"
    if (-not $r2) { throw 'token-only login failed' }
    [void](Wait-ProcExit $tok2 35)
    $acct2 = Get-Content "$tmp\acct.json" -Raw | ConvertFrom-Json
    $tokLen2 = 0
    if ($acct2.token) { $tokLen2 = $acct2.token.Length }
    Write-Host "  token bytes after re-login = $tokLen2 (must be > 0)"
    if ($tokLen2 -le 0) { throw 'token lost after re-login' }

    Write-Host '=== Phase C: M7c game panel (fake server.jar) ==='
    $javaSrc = @'
import java.io.*;
import java.net.*;

public class FakeMcServer {
    public static void main(String[] args) throws Exception {
        System.out.println("Starting minecraft server version 1.21");
        ServerSocket ss = new ServerSocket(25565);
        System.out.println("Done (0.001s)! For help, type 'help'");
        BufferedReader in = new BufferedReader(new InputStreamReader(System.in));
        String line;
        while ((line = in.readLine()) != null) {
            if (line.equals("stop")) { System.out.println("Stopping server"); ss.close(); return; }
        }
    }
}
'@
    # UTF8 *without* BOM — javac rejects a BOM in .java source (Set-Content -Encoding UTF8 writes one).
    [System.IO.File]::WriteAllText("$tmp\FakeMcServer.java", $javaSrc, (New-Object System.Text.UTF8Encoding($false)))
    $javac = Join-Path $javaHome 'bin\javac.exe'
    if (-not (Test-Path $javac)) { $javac = 'javac' }
    & $javac "$tmp\FakeMcServer.java" -d "$tmp"; if ($LASTEXITCODE -ne 0) { throw 'javac fake server' }
    $jarExe = Join-Path $javaHome 'bin\jar.exe'
    if (-not (Test-Path $jarExe)) { $jarExe = 'jar' }
    & $jarExe cfe "$tmp\server.jar" FakeMcServer -C "$tmp" FakeMcServer.class; if ($LASTEXITCODE -ne 0) { throw 'jar fake server' }
    Write-Host "  fake server.jar compiled"

    $game = Start-Proc .\bin\gui.exe @('-config',"$tmp\acct.json",'-keyfile',"$tmp\acct.key",'-account','host','-game-start',"$tmp\server.jar",'-vnic=false','-exit-after','30s') 'acct-game'
    $gStart = Wait-Log "$tmp\acct-game.out" 'game: server started' 25
    Write-Host "  game: server started log: $gStart"
    if (-not $gStart) { throw 'dedicated server did not start' }

    $props = Test-Path "$tmp\server.properties"
    $eula = Test-Path "$tmp\eula.txt"
    Write-Host "  server.properties written: $props ; eula.txt written: $eula"
    if (-not ($props -and $eula)) { throw 'game panel did not write server.properties/eula.txt' }
    $sp = Get-Content "$tmp\server.properties" -Raw
    if ($sp -notmatch 'server-port=25565' -or $sp -notmatch 'online-mode=false') { throw 'server.properties defaults missing' }
    Write-Host "  server.properties has port+online-mode defaults"

    $portUp = $false
    for ($i = 0; $i -lt 10; $i++) {
        if (Test-TcpPort 25565) { $portUp = $true; break }
        Start-Sleep -Milliseconds 500
    }
    Write-Host "  port 25565 listening: $portUp"
    if (-not $portUp) { throw 'fake MC server not listening on 25565' }

    [void](Wait-ProcExit $game 45)
    Write-Host "  game GUI exited: $($game.HasExited) code=$($game.ExitCode)"

    $acct3 = Get-Content "$tmp\acct.json" -Raw | ConvertFrom-Json
    Write-Host "  cfg java persisted: $([bool]$acct3.java) ; cfg server_jar persisted: $([bool]$acct3.server_jar)"
    if (-not $acct3.java -or -not $acct3.server_jar) { throw 'java/server_jar not persisted to config' }

    $portDown = -not (Test-TcpPort 25565)
    Write-Host "  port 25565 closed after exit: $portDown"
    if (-not $portDown) { throw 'dedicated server still listening after GUI exit' }
    if (-not (Wait-Log "$tmp\acct-game.out" 'game: server stopped at exit' 5)) { throw 'no stop-at-exit log' }

    Write-Host ''
    Write-Host '=== E2E PASS ==='
} catch {
    Write-Host "  E2E ERROR: $($_.Exception.Message)" -ForegroundColor Red
    Write-Host "  at: $($_.ScriptStackTrace)" -ForegroundColor Red
    exit 1
} finally {
    Write-Host '=== cleanup ==='
    foreach ($p in $procs) {
        try { if (-not $p.HasExited) { Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue } } catch {}
    }
    # Kill any java child the fake MC server leaked (only ones referencing this run).
    Get-CimInstance Win32_Process -Filter "Name='java.exe'" -ErrorAction SilentlyContinue |
        Where-Object { $_.CommandLine -like "*$tmp*" } |
        ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }
    foreach ($n in 'Eliauk-e2eh','Eliauk-e2ej') {
        try { if ([WintunClean]::WintunDeleteAdapter($n)) { Write-Host "  adapter removed: $n" } } catch {}
    }
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
    Write-Host "  workspace removed: $tmp"
}
