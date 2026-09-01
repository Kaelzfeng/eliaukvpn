# e2e-update.ps1 — end-to-end test for the Eliauk self-update loop.
#
# Serves a fake "v9.9.9" release (a copy of the GUI built with a stamped
# version) over a local HTTP server, runs the real GUI with the -update-check
# automation hook, and verifies the whole chain:
#
#   1. manifest fetched + Ed25519 signature verified (updatesign round-trip)
#   2. new exe downloaded and SHA-256 verified
#   3. on exit, the detached install script swaps the running exe and relaunches
#   4. the swapped exe now reports v9.9.9
#
# Leaves the machine as it found it. Run:
#   powershell -ExecutionPolicy Bypass -File e2e-update.ps1

$ErrorActionPreference = 'Stop'
$root = 'E:\eliaukvpn'
$go = 'C:\Program Files\Go\bin\go.exe'
Set-Location $root

$stamp = Get-Date -Format 'HHmmss'
$tmp = Join-Path $root ('.upd' + $stamp)
New-Item -ItemType Directory -Path $tmp | Out-Null
$feedDir = Join-Path $tmp 'feed'
New-Item -ItemType Directory -Path $feedDir | Out-Null

$port = 9092
$procs = New-Object System.Collections.ArrayList

function Wait-Log([string]$path, [string]$pattern, [int]$timeoutSec) {
    $alt = [System.IO.Path]::ChangeExtension($path, '.err')
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

function Start-FileServer([string]$dir, [int]$portNum) {
    # Minimal HTTP file server on a raw TCP listener (HttpListener needs URL
    # ACL reservations; TcpListener works un-elevated). The listener must be
    # created INSIDE the job — a live socket cannot be serialized into a
    # Start-Job process.
    $job = Start-Job -ScriptBlock {
        param($d, $portNum)
        $listener = New-Object System.Net.Sockets.TcpListener([System.Net.IPAddress]::Loopback, $portNum)
        $listener.Start()
        while ($true) {
            $client = $null
            try { $client = $listener.AcceptTcpClient() } catch { break }
            $stream = $client.GetStream()
            try {
                $reader = New-Object System.IO.StreamReader($stream)
                $line = $reader.ReadLine()
                if ($line -match '^GET (\S+)') {
                    $rel = [System.Uri]::UnescapeDataString($matches[1].TrimStart('/'))
                    $path = [System.IO.Path]::Combine($d, $rel)
                    if ([System.IO.File]::Exists($path)) {
                        $bytes = [System.IO.File]::ReadAllBytes($path)
                        $head = "HTTP/1.1 200 OK`r`nContent-Length: $($bytes.Length)`r`nConnection: close`r`n`r`n"
                        $hbytes = [System.Text.Encoding]::ASCII.GetBytes($head)
                        $stream.Write($hbytes, 0, $hbytes.Length)
                        $stream.Write($bytes, 0, $bytes.Length)
                    } else {
                        $head = "HTTP/1.1 404 Not Found`r`nConnection: close`r`n`r`n"
                        $hbytes = [System.Text.Encoding]::ASCII.GetBytes($head)
                        $stream.Write($hbytes, 0, $hbytes.Length)
                    }
                }
            } catch {}
            try { $client.Close() } catch {}
        }
    } -ArgumentList $dir, $portNum
    return $job
}

function Start-Proc([string]$exe, [string[]]$argList, [string]$name) {
    $p = Start-Process -FilePath $exe -ArgumentList $argList -NoNewWindow `
        -RedirectStandardOutput "$tmp\$name.out" -RedirectStandardError "$tmp\$name.err" -PassThru
    [void]$procs.Add($p)
    return $p
}

try {
    Write-Host '=== build ==='
    & $go build -o bin/gui.exe ./cmd/gui;      if ($LASTEXITCODE -ne 0) { throw 'build gui' }
    & $go build -o bin/updatesign.exe ./cmd/updatesign; if ($LASTEXITCODE -ne 0) { throw 'build updatesign' }

    Write-Host '=== stage a fake v9.9.9 release ==='
    # "new" build = the GUI stamped with a much higher version.
    & $go build -ldflags '-X main.version=9.9.9' -o (Join-Path $feedDir 'eliauk-new.exe') ./cmd/gui
    if ($LASTEXITCODE -ne 0) { throw 'build new exe' }
    $sum = (Get-FileHash -Algorithm SHA256 (Join-Path $feedDir 'eliauk-new.exe')).Hash.ToLower()

    $manifest = @{
        version  = '9.9.9'
        url      = "http://127.0.0.1:$port/eliauk-new.exe"
        sha256   = $sum
        notes    = 'e2e fake release'
    }
    $manifest | ConvertTo-Json | Set-Content (Join-Path $tmp 'update.json') -Encoding ascii
    # Sign it with the release key baked into the binary (tests the full verify path).
    & .\bin\updatesign.exe -priv (Join-Path $root 'signing\release.priv') -in (Join-Path $tmp 'update.json') -out (Join-Path $feedDir 'update.json')
    if ($LASTEXITCODE -ne 0) { throw 'sign manifest' }

    # The app under test is a copy of the current GUI, so the swap never touches
    # bin/gui.exe itself.
    Copy-Item bin\gui.exe (Join-Path $tmp 'upd-app.exe')

    Write-Host '=== serve feed + run GUI with -update-check ==='
    $srvJob = Start-FileServer $feedDir $port
    Start-Sleep -Milliseconds 800

    $app = Start-Proc (Join-Path $tmp 'upd-app.exe') @(
        '-config', (Join-Path $tmp 'empty.json'),
        '-vnic=false',
        '-update-check', "http://127.0.0.1:$port/update.json",
        '-exit-after', '30s'
    ) 'upd-app'

    if (-not (Wait-Log "$tmp\upd-app.out" 'update: staged v9.9.9' 25)) {
        Write-Host '--- upd-app.err tail ---'
        if (Test-Path "$tmp\upd-app.err") { Get-Content "$tmp\upd-app.err" -Tail 20 }
        throw 'no staged-update log'
    }
    Write-Host "  staged download OK: update: staged v9.9.9"

    # The app quits (-exit-after); at exit it spawns the detached install bat.
    $deadline = (Get-Date).AddSeconds(40)
    while ((Get-Date) -lt $deadline -and -not $app.HasExited) { Start-Sleep -Milliseconds 500 }
    if (-not $app.HasExited) { throw 'GUI did not exit' }
    Write-Host "  GUI exited: code=$($app.ExitCode)"

    # Give the bat ~1s wait + copy time, then the swap is done and the new exe
    # has relaunched itself (kill that instance — it is just a test artifact).
    Start-Sleep -Seconds 3
    Get-Process upd-app -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue

    # The swapped exe must now report v9.9.9.
    $ver = (& (Join-Path $tmp 'upd-app.exe') -version 2>&1 | Select-Object -Last 1)
    Write-Host "  swapped exe reports: $ver"
    if ("$ver".Trim() -ne '9.9.9') { throw "expected swapped version 9.9.9, got $ver" }

    # The downloaded temp exe must have been deleted by the install bat.
    $leftover = Get-ChildItem $env:TEMP -Filter 'eliauk-update-*.exe' -ErrorAction SilentlyContinue
    if ($leftover) { Write-Host "  warning: leftover download: $($leftover.Name)" }
    else { Write-Host '  temp download cleaned up: True' }

    Write-Host ''
    Write-Host '=== E2E-UPDATE PASS ==='
} catch {
    Write-Host "  E2E ERROR: $($_.Exception.Message)" -ForegroundColor Red
    Write-Host "  at: $($_.ScriptStackTrace)" -ForegroundColor Red
    exit 1
} finally {
    Write-Host '=== cleanup ==='
    Get-Process upd-app -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
    Get-Process eliauk-new -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
    foreach ($p in $procs) {
        try { if (-not $p.HasExited) { Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue } } catch {}
    }
    if ($srvJob) { Stop-Job $srvJob -ErrorAction SilentlyContinue; Remove-Job $srvJob -Force -ErrorAction SilentlyContinue }
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
    Write-Host "  workspace removed: $tmp"
}
