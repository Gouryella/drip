<#
.SYNOPSIS
    Installs the Drip tunnel client on Windows.

.DESCRIPTION
    Downloads a release from GitHub, installs drip.exe, puts it on PATH and can
    register the Windows service that keeps configured tunnels connected.

.PARAMETER Version
    Release tag to install, or 'latest' (default).

.PARAMETER InstallDir
    Where drip.exe goes. Defaults to "$env:ProgramFiles\drip" when running
    elevated, otherwise "$env:LOCALAPPDATA\Programs\drip".

.PARAMETER Repo
    GitHub repository to install from. Defaults to nickolasdeluca/drip-ex.

.PARAMETER InstallService
    Register and start the Drip Windows service after installing. Requires an
    elevated prompt and a configured client.

.PARAMETER Tunnel
    Names of configured tunnels the service should run. Implies -InstallService.

.PARAMETER AllTunnels
    Run every tunnel in the config file as a service. Implies -InstallService.

.PARAMETER Uninstall
    Remove the service, the binary and the PATH entry.

.PARAMETER Quiet
    Never prompt. Skips the interactive configuration step.

.EXAMPLE
    .\install-client.ps1

.EXAMPLE
    .\install-client.ps1 -InstallService -AllTunnels

.EXAMPLE
    irm https://raw.githubusercontent.com/nickolasdeluca/drip-ex/main/scripts/install-client.ps1 | iex
#>

[CmdletBinding()]
param(
    [string]$Version = 'latest',
    [string]$InstallDir,
    [string]$Repo = 'nickolasdeluca/drip-ex',
    [switch]$InstallService,
    [string[]]$Tunnel,
    [switch]$AllTunnels,
    [switch]$Uninstall,
    [switch]$Quiet
)

#Requires -Version 5.1

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

# Windows PowerShell renders a progress bar for every chunk of a download, which
# costs more time than the transfer itself.
$ProgressPreference = 'SilentlyContinue'

# Older Windows PowerShell defaults to SSL3/TLS1.0, which GitHub refuses.
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

$BinaryName = 'drip.exe'
$ServiceName = 'drip'
$PathEnvKey = 'HKLM:\SYSTEM\CurrentControlSet\Control\Session Manager\Environment'

function Write-Step { param([string]$Message) Write-Host "==> $Message" -ForegroundColor Cyan }
function Write-Ok { param([string]$Message) Write-Host "  + $Message" -ForegroundColor Green }
function Write-Note { param([string]$Message) Write-Host "  ! $Message" -ForegroundColor Yellow }

function Test-Administrator {
    $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
    $principal = New-Object Security.Principal.WindowsPrincipal($identity)
    return $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
}

function Get-TargetArchitecture {
    switch ($env:PROCESSOR_ARCHITECTURE) {
        'AMD64' { return 'amd64' }
        'ARM64' { return 'arm64' }
        'x86' {
            # A 32-bit PowerShell on a 64-bit machine still reports x86.
            if ($env:PROCESSOR_ARCHITEW6432 -eq 'AMD64') { return 'amd64' }
            if ($env:PROCESSOR_ARCHITEW6432 -eq 'ARM64') { return 'arm64' }
            return '386'
        }
        default { throw "Unsupported processor architecture: $env:PROCESSOR_ARCHITECTURE" }
    }
}

function Get-DefaultInstallDir {
    if (Test-Administrator) {
        return (Join-Path $env:ProgramFiles 'drip')
    }
    return (Join-Path $env:LOCALAPPDATA 'Programs\drip')
}

function Get-Release {
    param([string]$Repository, [string]$Tag)

    $url = if ($Tag -eq 'latest') {
        "https://api.github.com/repos/$Repository/releases/latest"
    } else {
        "https://api.github.com/repos/$Repository/releases/tags/$Tag"
    }

    try {
        return Invoke-RestMethod -Uri $url -Headers @{ 'User-Agent' = 'drip-installer' } -UseBasicParsing
    } catch {
        throw "Failed to query $url. GitHub said: $($_.Exception.Message)"
    }
}

function Select-ReleaseAsset {
    param($Release, [string]$Architecture)

    # Match on the platform rather than a fixed file name, so a change in the
    # archive format or the project name does not break the installer.
    $candidates = @($Release.assets | Where-Object {
        $_.name -match '(?i)windows' -and
        $_.name -match "(?i)[_-]$Architecture\." -and
        $_.name -match '(?i)\.(zip|tar\.gz|tgz)$'
    })

    if ($candidates.Count -eq 0) {
        $available = ($Release.assets | ForEach-Object { $_.name }) -join ', '
        throw "No windows/$Architecture asset in release $($Release.tag_name). Available: $available"
    }

    return $candidates[0]
}

function Test-AssetChecksum {
    param($Release, [string]$AssetName, [string]$FilePath)

    $checksums = $Release.assets | Where-Object { $_.name -match '(?i)checksums?.*\.txt$' } | Select-Object -First 1
    if (-not $checksums) {
        Write-Note 'Release has no checksum file; skipping verification'
        return
    }

    $listPath = Join-Path ([IO.Path]::GetTempPath()) $checksums.name
    Invoke-WebRequest -Uri $checksums.browser_download_url -OutFile $listPath -UseBasicParsing

    try {
        $expected = $null
        foreach ($line in (Get-Content -LiteralPath $listPath)) {
            $fields = $line -split '\s+' | Where-Object { $_ -ne '' }
            if ($fields.Count -ge 2 -and ($fields[-1] -replace '^\*', '') -eq $AssetName) {
                $expected = $fields[0]
                break
            }
        }

        if (-not $expected) {
            Write-Note "$AssetName is not listed in $($checksums.name); skipping verification"
            return
        }

        $actual = (Get-FileHash -LiteralPath $FilePath -Algorithm SHA256).Hash
        if ($actual -ne $expected.ToUpperInvariant()) {
            throw "Checksum mismatch for ${AssetName}: expected $expected, got $actual"
        }

        Write-Ok 'Checksum verified'
    } finally {
        Remove-Item -LiteralPath $listPath -Force -ErrorAction SilentlyContinue
    }
}

function Expand-ReleaseArchive {
    param([string]$ArchivePath, [string]$Destination)

    New-Item -ItemType Directory -Path $Destination -Force | Out-Null

    if ($ArchivePath -match '(?i)\.zip$') {
        Expand-Archive -LiteralPath $ArchivePath -DestinationPath $Destination -Force
    } else {
        # bsdtar ships with Windows 10 1803 and later.
        if (-not (Get-Command tar.exe -ErrorAction SilentlyContinue)) {
            throw 'tar.exe is required to extract a .tar.gz archive but was not found. Install a newer Windows build or download the .zip asset manually.'
        }
        & tar.exe -xzf $ArchivePath -C $Destination
        if ($LASTEXITCODE -ne 0) {
            throw "tar failed to extract $ArchivePath (exit code $LASTEXITCODE)"
        }
    }

    $binary = Get-ChildItem -Path $Destination -Filter $BinaryName -Recurse -File | Select-Object -First 1
    if (-not $binary) {
        throw "$BinaryName was not found inside $ArchivePath"
    }

    return $binary.FullName
}

function Get-InstalledService {
    return Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
}

function Get-PathEntries {
    param([ValidateSet('Machine', 'User')][string]$Scope)

    if ($Scope -eq 'Machine') {
        # Read the raw value: GetEnvironmentVariable expands %SystemRoot% and the
        # like, and writing that expansion back would flatten everyone's PATH.
        $key = Get-Item -LiteralPath $PathEnvKey
        return [string]$key.GetValue('Path', '', 'DoNotExpandEnvironmentNames')
    }

    $key = Get-Item -LiteralPath 'HKCU:\Environment' -ErrorAction SilentlyContinue
    if (-not $key) { return '' }
    return [string]$key.GetValue('Path', '', 'DoNotExpandEnvironmentNames')
}

function Set-PathEntries {
    param([ValidateSet('Machine', 'User')][string]$Scope, [string]$Value)

    $keyPath = if ($Scope -eq 'Machine') { $PathEnvKey } else { 'HKCU:\Environment' }
    if (-not (Test-Path -LiteralPath $keyPath)) {
        New-Item -Path $keyPath -Force | Out-Null
    }

    # ExpandString keeps %SystemRoot% style entries working for everything else
    # already on PATH.
    New-ItemProperty -LiteralPath $keyPath -Name 'Path' -Value $Value -PropertyType ExpandString -Force | Out-Null
}

function Add-ToPath {
    param([string]$Directory)

    $scope = if (Test-Administrator) { 'Machine' } else { 'User' }
    $current = Get-PathEntries -Scope $scope
    $entries = @($current -split ';' | Where-Object { $_ -ne '' })

    if ($entries | Where-Object { $_.TrimEnd('\') -ieq $Directory.TrimEnd('\') }) {
        Write-Ok "$Directory is already on the $scope PATH"
    } else {
        $updated = (@($entries) + $Directory) -join ';'
        Set-PathEntries -Scope $scope -Value $updated
        Write-Ok "Added $Directory to the $scope PATH"
    }

    if (($env:Path -split ';') -notcontains $Directory) {
        $env:Path = "$env:Path;$Directory"
    }
}

function Remove-FromPath {
    param([string]$Directory)

    foreach ($scope in @('Machine', 'User')) {
        if ($scope -eq 'Machine' -and -not (Test-Administrator)) { continue }

        $current = Get-PathEntries -Scope $scope
        if (-not $current) { continue }

        $entries = @($current -split ';' | Where-Object { $_ -ne '' })
        $kept = @($entries | Where-Object { $_.TrimEnd('\') -ine $Directory.TrimEnd('\') })

        if ($kept.Count -ne $entries.Count) {
            Set-PathEntries -Scope $scope -Value ($kept -join ';')
            Write-Ok "Removed $Directory from the $scope PATH"
        }
    }
}

function Stop-RunningService {
    $service = Get-InstalledService
    if (-not $service -or $service.Status -ne 'Running') {
        return $false
    }

    if (-not (Test-Administrator)) {
        throw "The $ServiceName service is running and holds the binary open. Re-run this script from an elevated prompt, or stop the service first."
    }

    Write-Step "Stopping the $ServiceName service so the binary can be replaced"
    Stop-Service -Name $ServiceName -Force
    (Get-Service -Name $ServiceName).WaitForStatus('Stopped', [TimeSpan]::FromSeconds(30))

    return $true
}

function Install-Binary {
    param([string]$SourcePath, [string]$TargetDir)

    New-Item -ItemType Directory -Path $TargetDir -Force | Out-Null
    $target = Join-Path $TargetDir $BinaryName

    try {
        Copy-Item -LiteralPath $SourcePath -Destination $target -Force
    } catch {
        throw "Cannot write $target ($($_.Exception.Message)). Re-run this script from an elevated prompt, or pass -InstallDir with a writable path."
    }

    return $target
}

function Invoke-Drip {
    param([string]$BinaryPath, [string[]]$Arguments)

    & $BinaryPath @Arguments
    if ($LASTEXITCODE -ne 0) {
        throw "'$BinaryName $($Arguments -join ' ')' failed with exit code $LASTEXITCODE"
    }
}

function Initialize-Configuration {
    param([string]$BinaryPath)

    if ($Quiet) { return }

    Write-Host ''
    $answer = Read-Host 'Configure the client now? [Y/n]'
    if ($answer -match '^[Nn]') { return }

    $server = ''
    while (-not $server) {
        $server = (Read-Host 'Server address (e.g. tunnel.example.com:443)').Trim()
    }

    $token = ''
    while (-not $token) {
        $token = (Read-Host 'Authentication token').Trim()
    }

    Invoke-Drip -BinaryPath $BinaryPath -Arguments @('config', 'set', '--server', $server, '--token', $token)
    Write-Ok 'Configuration saved'
}

function Register-DripService {
    param([string]$BinaryPath)

    if (-not (Test-Administrator)) {
        throw 'Installing the service requires an elevated prompt (Run as administrator).'
    }

    if (Get-InstalledService) {
        Write-Note "The $ServiceName service is already installed; leaving it alone. Run 'drip service uninstall' first to reinstall it."
        return
    }

    $arguments = @('service', 'install')
    if ($Tunnel) {
        foreach ($name in $Tunnel) { $arguments += @('--tunnel', $name) }
    } else {
        $arguments += '--all'
    }

    Write-Step 'Installing the Drip service'
    Invoke-Drip -BinaryPath $BinaryPath -Arguments $arguments

    Write-Step 'Starting the Drip service'
    Invoke-Drip -BinaryPath $BinaryPath -Arguments @('service', 'start')
}

function Invoke-Install {
    $architecture = Get-TargetArchitecture

    if (-not $InstallDir) {
        $InstallDir = Get-DefaultInstallDir
    }
    $InstallDir = [IO.Path]::GetFullPath($InstallDir)

    Write-Step "Installing the Drip client (windows/$architecture)"

    $release = Get-Release -Repository $Repo -Tag $Version
    $asset = Select-ReleaseAsset -Release $release -Architecture $architecture
    Write-Ok "Release $($release.tag_name): $($asset.name)"

    $workDir = Join-Path ([IO.Path]::GetTempPath()) ("drip-install-" + [Guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $workDir -Force | Out-Null

    try {
        $archivePath = Join-Path $workDir $asset.name

        Write-Step 'Downloading'
        Invoke-WebRequest -Uri $asset.browser_download_url -OutFile $archivePath -UseBasicParsing

        Test-AssetChecksum -Release $release -AssetName $asset.name -FilePath $archivePath

        $extracted = Expand-ReleaseArchive -ArchivePath $archivePath -Destination (Join-Path $workDir 'unpacked')

        $serviceWasRunning = Stop-RunningService

        Write-Step "Installing to $InstallDir"
        $binaryPath = Install-Binary -SourcePath $extracted -TargetDir $InstallDir
        Write-Ok $binaryPath

        Add-ToPath -Directory $InstallDir

        $installedVersion = (& $binaryPath version --short) -join ' '
        Write-Ok "Installed: $installedVersion"

        if ($serviceWasRunning) {
            Write-Step "Restarting the $ServiceName service"
            Start-Service -Name $ServiceName
            Write-Ok 'Service restarted'
        } else {
            Initialize-Configuration -BinaryPath $binaryPath

            if ($InstallService -or $AllTunnels -or $Tunnel) {
                Register-DripService -BinaryPath $binaryPath
            }
        }
    } finally {
        Remove-Item -LiteralPath $workDir -Recurse -Force -ErrorAction SilentlyContinue
    }

    Write-Host ''
    Write-Host 'Done. Usage:' -ForegroundColor Cyan
    Write-Host '  drip http 3000                 Expose a local HTTP server'
    Write-Host '  drip tcp 5432                  Expose a local TCP service'
    Write-Host '  drip config show               Show the current configuration'
    Write-Host '  drip service install --all     Run configured tunnels as a Windows service'
    Write-Host '  drip service status            Check the service'
    Write-Host ''
    Write-Note 'Open a new terminal for the PATH change to take effect.'
}

function Invoke-Uninstall {
    Write-Step 'Uninstalling the Drip client'

    if (-not $InstallDir) {
        $existing = Get-Command $BinaryName -ErrorAction SilentlyContinue
        if ($existing) {
            $InstallDir = Split-Path -Parent $existing.Source
        } else {
            $InstallDir = Get-DefaultInstallDir
        }
    }
    $InstallDir = [IO.Path]::GetFullPath($InstallDir)

    $binaryPath = Join-Path $InstallDir $BinaryName

    if (Get-InstalledService) {
        if (-not (Test-Administrator)) {
            throw "The $ServiceName service is installed; removing it requires an elevated prompt."
        }
        if (Test-Path -LiteralPath $binaryPath) {
            Write-Step "Removing the $ServiceName service"
            Invoke-Drip -BinaryPath $binaryPath -Arguments @('service', 'uninstall')
        } else {
            Write-Note "The $ServiceName service is installed but $binaryPath is gone; remove it with 'sc.exe delete $ServiceName'"
        }
    }

    if (Test-Path -LiteralPath $binaryPath) {
        Remove-Item -LiteralPath $binaryPath -Force
        Write-Ok "Removed $binaryPath"

        # Only take the directory if this installer created it and it is now empty.
        if (-not (Get-ChildItem -LiteralPath $InstallDir -Force)) {
            Remove-Item -LiteralPath $InstallDir -Force
        }
    } else {
        Write-Note "$binaryPath not found"
    }

    Remove-FromPath -Directory $InstallDir

    if (-not $Quiet) {
        Write-Host ''
        $answer = Read-Host 'Remove configuration and logs too? [y/N]'
        if ($answer -match '^[Yy]') {
            foreach ($dir in @((Join-Path $env:USERPROFILE '.drip'), (Join-Path $env:ProgramData 'drip'))) {
                if (Test-Path -LiteralPath $dir) {
                    Remove-Item -LiteralPath $dir -Recurse -Force
                    Write-Ok "Removed $dir"
                }
            }
        }
    }

    Write-Host ''
    Write-Ok 'Uninstalled'
}

try {
    if ($Uninstall) {
        Invoke-Uninstall
    } else {
        Invoke-Install
    }
} catch {
    Write-Host ''
    Write-Host "Error: $($_.Exception.Message)" -ForegroundColor Red
    exit 1
}
