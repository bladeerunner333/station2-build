param(
    [int]$Port = 8765
)
$ErrorActionPreference = 'Stop'
$Root = Split-Path -Parent $MyInvocation.MyCommand.Path

# --- locate adb.exe ---
$AdbCandidates = @(
    (Join-Path $Root 'adb.exe'),
    'C:\platform-tools\adb.exe'
)
$AdbPath = $null
foreach ($c in $AdbCandidates) {
    if (Test-Path $c) { $AdbPath = $c; break }
}
if (-not $AdbPath) {
    $found = Get-Command 'adb.exe' -ErrorAction SilentlyContinue
    if ($found) { $AdbPath = $found.Source }
}
if (-not $AdbPath) {
    Write-Host "ERROR: adb.exe not found. Put adb.exe next to server.ps1 or in C:\platform-tools\" -ForegroundColor Red
    Read-Host "Press Enter to close"
    exit 1
}
Write-Host "adb: $AdbPath"

$BundlePath = Join-Path $Root 'ap2-bundle.tar.gz'

function Format-WinArg {
    param([string]$Arg)
    if ($Arg -eq '') { return '""' }
    if ($Arg -notmatch '[\s"]') { return $Arg }
    $sb = New-Object System.Text.StringBuilder
    [void]$sb.Append('"')
    $i = 0
    $len = $Arg.Length
    while ($i -lt $len) {
        $numBackslashes = 0
        while ($i -lt $len -and $Arg[$i] -eq '\') {
            $numBackslashes++
            $i++
        }
        if ($i -eq $len) {
            [void]$sb.Append('\' * ($numBackslashes * 2))
            break
        }
        elseif ($Arg[$i] -eq '"') {
            [void]$sb.Append('\' * ($numBackslashes * 2 + 1))
            [void]$sb.Append('"')
            $i++
        }
        else {
            [void]$sb.Append('\' * $numBackslashes)
            [void]$sb.Append($Arg[$i])
            $i++
        }
    }
    [void]$sb.Append('"')
    return $sb.ToString()
}

function Join-WinArgs {
    param([string[]]$ArgList)
    return (($ArgList | ForEach-Object { Format-WinArg $_ }) -join ' ')
}

function Invoke-Adb {
    param([string[]]$ArgList)
    $psi = New-Object System.Diagnostics.ProcessStartInfo
    $psi.FileName = $AdbPath
    $psi.Arguments = Join-WinArgs $ArgList
    $psi.RedirectStandardOutput = $true
    $psi.RedirectStandardError = $true
    $psi.UseShellExecute = $false
    $psi.CreateNoWindow = $true
    $p = [System.Diagnostics.Process]::Start($psi)
    $stdout = $p.StandardOutput.ReadToEnd()
    $stderr = $p.StandardError.ReadToEnd()
    $p.WaitForExit(30000) | Out-Null
    return ($stdout + "`n" + $stderr).Trim()
}

$InstallCmd = 'mkdir -p /data/local/opt && gzip -dc /data/local/tmp/ap2-bundle.tar.gz | tar -xf - -C /data/local/opt && chmod +x /data/local/opt/ap2/bin/* /data/local/opt/ap2/*.sh && mount -o remount,rw / && cp /data/local/opt/S97ap2 /etc/init.d/S97ap2 && chmod 755 /etc/init.d/S97ap2 && { [ ! -f /data/local/opt/S98tgbot ] || { cp /data/local/opt/S98tgbot /etc/init.d/S98tgbot && chmod 755 /etc/init.d/S98tgbot; }; } && rm -f /data/local/opt/S97ap2 /data/local/opt/S98tgbot /data/local/tmp/ap2-bundle.tar.gz && sync && mount -o remount,ro / && /etc/init.d/S97ap2 start && echo DEPLOY_OK'

function Do-Connect($ip) {
    return Invoke-Adb @('connect', "${ip}:5555")
}

function Do-Deploy($ip) {
    if (-not (Test-Path $BundlePath)) {
        return "ERROR: ap2-bundle.tar.gz not found next to server.ps1"
    }
    $log = ""
    $log += "> adb connect`n" + (Do-Connect $ip) + "`n`n"
    $log += "> adb push ap2-bundle.tar.gz`n" + (Invoke-Adb @('-s', "${ip}:5555", 'push', $BundlePath, '/data/local/tmp/ap2-bundle.tar.gz')) + "`n`n"
    $log += "> installing on the speaker`n" + (Invoke-Adb @('-s', "${ip}:5555", 'shell', $InstallCmd)) + "`n"
    return $log
}

function Do-Status($ip) {
    $log = ""
    $log += "> adb connect`n" + (Do-Connect $ip) + "`n`n"
    $log += "> ap2.sh status`n" + (Invoke-Adb @('-s', "${ip}:5555", 'shell', '/data/local/opt/ap2/ap2.sh status')) + "`n"
    return $log
}

function Do-Restart($ip) {
    $log = ""
    $log += "> adb connect`n" + (Do-Connect $ip) + "`n`n"
    $log += "> ap2.sh restart`n" + (Invoke-Adb @('-s', "${ip}:5555", 'shell', '/data/local/opt/ap2/ap2.sh restart')) + "`n"
    return $log
}

function Do-Raw($ip, $cmd) {
    $log = ""
    $log += "> adb connect`n" + (Do-Connect $ip) + "`n`n"
    $log += "> shell: $cmd`n" + (Invoke-Adb @('-s', "${ip}:5555", 'shell', $cmd)) + "`n"
    return $log
}

$Global:OAuthQueue = [System.Collections.Concurrent.ConcurrentQueue[string]]::new()
$Global:OAuthProc = $null
$Global:OAuthEventOut = $null
$Global:OAuthEventErr = $null

function Stop-OAuthProc {
    if ($Global:OAuthEventOut) {
        try { Unregister-Event -SourceIdentifier $Global:OAuthEventOut.Name -ErrorAction SilentlyContinue } catch {}
        try { Remove-Job -Name $Global:OAuthEventOut.Name -Force -ErrorAction SilentlyContinue } catch {}
    }
    if ($Global:OAuthEventErr) {
        try { Unregister-Event -SourceIdentifier $Global:OAuthEventErr.Name -ErrorAction SilentlyContinue } catch {}
        try { Remove-Job -Name $Global:OAuthEventErr.Name -Force -ErrorAction SilentlyContinue } catch {}
    }
    $Global:OAuthEventOut = $null
    $Global:OAuthEventErr = $null
    if ($Global:OAuthProc -and -not $Global:OAuthProc.HasExited) {
        try { $Global:OAuthProc.Kill() } catch {}
    }
    $Global:OAuthProc = $null
}

function Drain-OAuthQueue {
    $lines = New-Object System.Collections.Generic.List[string]
    $line = $null
    while ($Global:OAuthQueue.TryDequeue([ref]$line)) { $lines.Add($line) }
    return ($lines -join "`n")
}

function Start-OAuthFlow($ip) {
    Stop-OAuthProc
    Drain-OAuthQueue | Out-Null

    # an unauthenticated librespot instance may already be running on the device - kill it before login
    Invoke-Adb @('-s', "${ip}:5555", 'shell', 'for p in $(ps -o pid,args | grep /bin/librespot | grep -v grep | awk ''{print $1}''); do kill -9 $p; done; sleep 2; true') | Out-Null

    # station name built from char codes - server.ps1 must stay pure ASCII, otherwise
    # powershell.exe on this machine fails to parse non-ASCII literals correctly.
    $stationName = [string]([char]0x0421 + [char]0x0442 + [char]0x0430 + [char]0x043D + [char]0x0446 + [char]0x0438 + [char]0x044F) + ' 2'
    $remoteCmd = '/data/local/opt/ap2/bin/librespot --name "' + $stationName + '" --device-type speaker --bitrate 320 --backend subprocess --device /data/local/opt/ap2/spotify-sink.sh --format S16 --mixer alsa --alsa-mixer-device default --alsa-mixer-control Master --volume-ctrl log --volume-range 25 --cache /data/local/opt/ap2/spotify-cache --disable-audio-cache --zeroconf-backend avahi --zeroconf-port 4371 --enable-oauth --oauth-port 0'

    $psi = New-Object System.Diagnostics.ProcessStartInfo
    $psi.FileName = $AdbPath
    # -tt forces adb to allocate a PTY on the device, otherwise stdin we pipe in
    # from here is not kept open for the remote process (it sees EOF immediately).
    $psi.Arguments = Join-WinArgs @('-s', "${ip}:5555", 'shell', '-tt', $remoteCmd)
    $psi.RedirectStandardInput = $true
    $psi.RedirectStandardOutput = $true
    $psi.RedirectStandardError = $true
    $psi.StandardOutputEncoding = [System.Text.Encoding]::UTF8
    $psi.StandardErrorEncoding = [System.Text.Encoding]::UTF8
    $psi.UseShellExecute = $false
    $psi.CreateNoWindow = $true

    $p = New-Object System.Diagnostics.Process
    $p.StartInfo = $psi
    $p.EnableRaisingEvents = $true

    $outAction = {
        if ($null -ne $EventArgs.Data) { $Global:OAuthQueue.Enqueue($EventArgs.Data) }
    }
    $Global:OAuthEventOut = Register-ObjectEvent -InputObject $p -EventName OutputDataReceived -Action $outAction
    $Global:OAuthEventErr = Register-ObjectEvent -InputObject $p -EventName ErrorDataReceived -Action $outAction

    [void]$p.Start()
    $p.BeginOutputReadLine()
    $p.BeginErrorReadLine()
    $Global:OAuthProc = $p
}

function OAuth-RunningText {
    if ($Global:OAuthProc -and -not $Global:OAuthProc.HasExited) { return "running" }
    return "exited"
}

function Do-OAuthStart($ip) {
    $log = "> adb connect`n" + (Do-Connect $ip) + "`n`n"
    Start-OAuthFlow $ip
    $collected = ""
    for ($i = 0; $i -lt 12; $i++) {
        Start-Sleep -Milliseconds 1000
        $collected += (Drain-OAuthQueue) + "`n"
        if ($collected -match 'https?://') { break }
    }
    $log += "status: $(OAuth-RunningText)`n$collected"
    return $log
}

function Do-OAuthPoll() {
    $collected = Drain-OAuthQueue
    return "status: $(OAuth-RunningText)`n$collected"
}

function Do-OAuthFinish($pastedUrl) {
    if (-not $Global:OAuthProc -or $Global:OAuthProc.HasExited) {
        return "ERROR: login process is not running (it may have exited) - click Start login again"
    }
    try {
        $Global:OAuthProc.StandardInput.WriteLine($pastedUrl)
        $Global:OAuthProc.StandardInput.Flush()
    } catch {
        return "ERROR: failed to send the URL to the process: $_"
    }
    $collected = ""
    for ($i = 0; $i -lt 10; $i++) {
        Start-Sleep -Milliseconds 1000
        $collected += (Drain-OAuthQueue) + "`n"
    }
    return "status: $(OAuth-RunningText)`n$collected"
}

function Do-PushFile($ip, $name) {
    # only plain file names that sit next to server.ps1
    if ($name -notmatch '^[A-Za-z0-9._-]+$') { return "ERROR: bad file name" }
    $local = Join-Path $Root $name
    if (-not (Test-Path $local)) { return "ERROR: $name not found next to server.ps1" }
    $log = "> adb connect`n" + (Do-Connect $ip) + "`n`n"
    $log += "> adb push $name`n" + (Invoke-Adb @('-s', "${ip}:5555", 'push', $local, "/data/local/tmp/$name")) + "`n"
    return $log
}

function Do-TestPorts($ip) {
    $log = "Testing from THIS PC (not the phone) to $ip over the LAN:`n`n"
    foreach ($p in 7000, 4371, 5353) {
        try {
            $r = Test-NetConnection -ComputerName $ip -Port $p -WarningAction SilentlyContinue
            $log += "port $p : TcpTestSucceeded=$($r.TcpTestSucceeded)`n"
        } catch {
            $log += "port $p : ERROR $($_.Exception.Message)`n"
        }
    }
    return $log
}

# --- HTTP server ---
$listener = New-Object System.Net.HttpListener
$prefix = "http://localhost:$Port/"
$listener.Prefixes.Add($prefix)
try {
    $listener.Start()
} catch {
    Write-Host "Could not open port $Port. $_" -ForegroundColor Red
    Read-Host "Press Enter"
    exit 1
}
Write-Host "Panel running: $prefix"
Start-Process $prefix

$indexPath = Join-Path $Root 'index.html'

while ($true) {
    try {
        if (-not $listener.IsListening) { break }
        $ctx = $listener.GetContext()
        $req = $ctx.Request
        $resp = $ctx.Response
        try {
            if ($req.HttpMethod -eq 'GET' -and $req.Url.AbsolutePath -eq '/') {
                $html = [System.IO.File]::ReadAllText($indexPath, [System.Text.Encoding]::UTF8)
                $bytes = [System.Text.Encoding]::UTF8.GetBytes($html)
                $resp.ContentType = 'text/html; charset=utf-8'
                $resp.ContentLength64 = $bytes.Length
                $resp.OutputStream.Write($bytes, 0, $bytes.Length)
            }
            elseif ($req.HttpMethod -eq 'POST' -and $req.Url.AbsolutePath -like '/api/*') {
                $reader = New-Object System.IO.StreamReader($req.InputStream, [System.Text.Encoding]::UTF8)
                $bodyText = $reader.ReadToEnd()
                $ip = 'localhost'
                $cmd = ''
                $oauthUrl = ''
                if ($bodyText) {
                    try {
                        $obj = $bodyText | ConvertFrom-Json
                        if ($obj.ip) { $ip = $obj.ip }
                        if ($obj.cmd) { $cmd = $obj.cmd }
                        if ($obj.url) { $oauthUrl = $obj.url }
                    } catch {}
                }
                $result = switch ($req.Url.AbsolutePath) {
                    '/api/connect' { Do-Connect $ip }
                    '/api/deploy'  { Do-Deploy $ip }
                    '/api/status'  { Do-Status $ip }
                    '/api/restart' { Do-Restart $ip }
                    '/api/raw'     { Do-Raw $ip $cmd }
                    '/api/testports' { Do-TestPorts $ip }
                    '/api/oauth-start'  { Do-OAuthStart $ip }
                    '/api/oauth-poll'   { Do-OAuthPoll }
                    '/api/oauth-finish' { Do-OAuthFinish $oauthUrl }
                    '/api/push'         { Do-PushFile $ip $cmd }
                    default { "unknown route" }
                }
                $json = (@{ output = $result } | ConvertTo-Json -Compress)
                $bytes = [System.Text.Encoding]::UTF8.GetBytes($json)
                $resp.ContentType = 'application/json; charset=utf-8'
                $resp.ContentLength64 = $bytes.Length
                $resp.OutputStream.Write($bytes, 0, $bytes.Length)
            }
            else {
                $resp.StatusCode = 404
            }
        } catch {
            $errDetail = "ERROR: $($_.Exception.GetType().FullName): $($_.Exception.Message)`nLine: $($_.InvocationInfo.ScriptLineNumber) Col: $($_.InvocationInfo.OffsetInLine)`nStack: $($_.ScriptStackTrace)"
            Write-Host $errDetail -ForegroundColor Red
            try {
                $err = [System.Text.Encoding]::UTF8.GetBytes($errDetail)
                $resp.StatusCode = 500
                $resp.ContentLength64 = $err.Length
                $resp.OutputStream.Write($err, 0, $err.Length)
            } catch {}
        } finally {
            try { $resp.OutputStream.Close() } catch {}
        }
    } catch {
        Write-Host "LOOP ERROR (server kept alive): $_" -ForegroundColor Magenta
        Start-Sleep -Milliseconds 200
    }
}
