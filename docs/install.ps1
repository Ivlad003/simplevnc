# SimpleVNC installer and updater for Windows.
#
#   irm https://ivlad003.github.io/simplevnc/install.ps1 | iex
#
# Downloads the latest release, checks its SHA-256 and installs it as a Windows
# service that starts with Windows. Run the same command again to update: the
# service is restarted on the new version and keeps its password, port and settings.
#
# Options (set before the command):
#   $env:SIMPLEVNC_ARGS = '-p mypass'   extra parameters for the service, for example a password
#   $env:SIMPLEVNC_RUN  = '1'           do not install a service, just download and start the program
#
# The messages are in English on purpose: Windows PowerShell 5.1 garbles non-ASCII
# text in scripts that are piped from the web.

& {
    $ErrorActionPreference = 'Stop'
    $ProgressPreference = 'SilentlyContinue' # the progress bar makes downloads very slow in PowerShell 5.1
    [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

    $repo = 'Ivlad003/simplevnc'
    $base = "https://github.com/$repo/releases/latest/download"
    $dir = Join-Path $env:LOCALAPPDATA 'SimpleVNC'
    $id = [Guid]::NewGuid().ToString('N').Substring(0, 8)
    $new = Join-Path $dir "SimpleVNC-$id.exe"
    $sums = Join-Path $dir "SHA256SUMS-$id.txt"

    New-Item -ItemType Directory -Force -Path $dir | Out-Null
    try {
        Write-Host 'Downloading the latest SimpleVNC...'
        Invoke-WebRequest -Uri "$base/SimpleVNC.exe" -OutFile $new -UseBasicParsing
        Invoke-WebRequest -Uri "$base/SHA256SUMS.txt" -OutFile $sums -UseBasicParsing

        $line = Get-Content -Path $sums | Where-Object { $_ -match '\sSimpleVNC\.exe$' } | Select-Object -First 1
        $want = if ($line) { ($line -split '\s+')[0] } else { '' }
        $have = (Get-FileHash -Algorithm SHA256 -Path $new).Hash
        if (-not $want -or $want -ne $have) {
            throw 'The downloaded file does not match its checksum. Nothing was installed; please try again.'
        }
        $version = (& $new -version | Out-String).Trim()
        Write-Host "Downloaded $version, checksum OK."

        if ($env:SIMPLEVNC_RUN) {
            $exe = Join-Path $dir 'SimpleVNC.exe'
            try {
                Move-Item -Force -Path $new -Destination $exe
            } catch {
                $exe = $new # the previous copy is still running: start the new file next to it
            }
            $new = $null
            Write-Host "Starting $exe ..."
            if ($env:SIMPLEVNC_ARGS) { Start-Process -FilePath $exe -ArgumentList $env:SIMPLEVNC_ARGS } else { Start-Process -FilePath $exe }
            Write-Host 'SimpleVNC is running in its own window. The connection page opens in your browser.'
            return
        }

        Write-Host 'Installing the Windows service. Confirm the Windows prompt (administrator rights are needed).'
        Write-Host 'A new window shows the result; press Enter there when you have read it.'
        $arguments = '-install -elevated'
        if ($env:SIMPLEVNC_ARGS) { $arguments = "-install $($env:SIMPLEVNC_ARGS) -elevated" }
        $process = Start-Process -FilePath $new -ArgumentList $arguments -Verb RunAs -Wait -PassThru
        if ($process.ExitCode -ne 0) {
            throw "The installer finished with code $($process.ExitCode). The service was not changed."
        }
        Write-Host ''
        Write-Host "Done: the SimpleVNC service runs $version."
        Write-Host 'Connection page on this PC: http://127.0.0.1:5980/'
        Write-Host 'To update later, run the same command again.'
    } finally {
        foreach ($file in @($new, $sums)) {
            if ($file -and (Test-Path $file)) { Remove-Item -Force -Path $file -ErrorAction SilentlyContinue }
        }
    }
}
