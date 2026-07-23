#requires -Version 5.1
<#
.SYNOPSIS
Installs the native Windows x64 Witself CLI from a checksummed release archive.

.EXAMPLE
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12; irm https://raw.githubusercontent.com/witwave-ai/witself/main/install.ps1 | iex

.EXAMPLE
.\install.ps1 -Version v0.0.201 -NoPathUpdate

.NOTES
WS_INSTALL_LOCK_TIMEOUT_MS may bound how long concurrent installers wait for
the per-install-directory transaction lock (default: 30000).
#>
[CmdletBinding()]
param(
    [Parameter(Position = 0)]
    [string]$Version = $env:WS_VERSION,

    [string]$InstallDir = $env:WS_INSTALL_DIR,

    [switch]$NoPathUpdate,

    # An HTTPS release/download root or a local fixture root. Local roots use
    # <root>\<tag>\{asset,checksums.txt} and may provide <root>\latest.json.
    [string]$ReleaseRoot = $env:WS_RELEASE_ROOT
)

& {
param(
    [string]$Version,
    [string]$InstallDir,
    [bool]$NoPathUpdate,
    [string]$ReleaseRoot
)

Set-StrictMode -Version 2.0
$ErrorActionPreference = 'Stop'

function Test-WebSource {
    param([string]$Value)
    return $Value -match '^https://'
}

function Copy-ReleaseItem {
    param(
        [string]$Source,
        [string]$Destination
    )
    if (Test-WebSource $Source) {
        [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
        Invoke-WebRequest `
            -UseBasicParsing `
            -Headers @{ 'User-Agent' = 'witself-install.ps1' } `
            -Uri $Source `
            -OutFile $Destination
        return
    }
    if (-not (Test-Path -LiteralPath $Source -PathType Leaf)) {
        throw "release item not found: $Source"
    }
    Copy-Item -LiteralPath $Source -Destination $Destination -Force
}

function Get-ReleaseItemSource {
    param(
        [string]$Root,
        [string]$Tag,
        [string]$Name
    )
    if (Test-WebSource $Root) {
        return "$($Root.TrimEnd('/'))/$Tag/$Name"
    }
    return Join-Path (Join-Path ([IO.Path]::GetFullPath($Root)) $Tag) $Name
}

function Get-LatestVersion {
    param([string]$Root)
    if (-not (Test-WebSource $Root)) {
        $latestPath = Join-Path ([IO.Path]::GetFullPath($Root)) 'latest.json'
        if (-not (Test-Path -LiteralPath $latestPath -PathType Leaf)) {
            throw "latest release metadata not found: $latestPath"
        }
        $latest = Get-Content -LiteralPath $latestPath -Raw | ConvertFrom-Json
        return [string]$latest.tag_name
    }
    [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
    $latest = Invoke-RestMethod `
        -UseBasicParsing `
        -Headers @{ 'User-Agent' = 'witself-install.ps1' } `
        -Uri 'https://api.github.com/repos/witwave-ai/witself/releases/latest'
    return [string]$latest.tag_name
}

function Assert-ReplaceableTarget {
    param([string]$Path)
    if (-not (Test-Path -LiteralPath $Path)) {
        return
    }
    $item = Get-Item -LiteralPath $Path -Force
    if ($item.PSIsContainer -or ($item.Attributes -band [IO.FileAttributes]::ReparsePoint)) {
        throw "refusing to replace non-regular install target: $Path"
    }
}

function Open-InstallDirectoryLock {
    param([string]$Path)
    $timeoutMilliseconds = 30000
    if (-not [string]::IsNullOrWhiteSpace($env:WS_INSTALL_LOCK_TIMEOUT_MS)) {
        $parsedTimeout = 0
        if (-not [int]::TryParse($env:WS_INSTALL_LOCK_TIMEOUT_MS, [ref]$parsedTimeout) -or
            $parsedTimeout -lt 0 -or $parsedTimeout -gt 300000) {
            throw 'WS_INSTALL_LOCK_TIMEOUT_MS must be an integer from 0 through 300000'
        }
        $timeoutMilliseconds = $parsedTimeout
    }
    $deadline = [DateTime]::UtcNow.AddMilliseconds($timeoutMilliseconds)
    while ($true) {
        try {
            return [IO.File]::Open(
                $Path,
                [IO.FileMode]::OpenOrCreate,
                [IO.FileAccess]::ReadWrite,
                [IO.FileShare]::None
            )
        } catch [IO.IOException] {
            if ([DateTime]::UtcNow -ge $deadline) {
                throw "timed out waiting for another Witself installer to release $Path"
            }
            Start-Sleep -Milliseconds 100
        }
    }
}

function Install-StagedFile {
    param(
        [string]$Stage,
        [string]$Target,
        [string]$Backup,
        [ref]$HadOriginal,
        [ref]$Changed,
        [ref]$OriginalHash
    )
    Assert-ReplaceableTarget $Target
    if (Test-Path -LiteralPath $Target -PathType Leaf) {
        $HadOriginal.Value = $true
        $OriginalHash.Value = (Get-FileHash -LiteralPath $Target -Algorithm SHA256).Hash.ToLowerInvariant()
        $Changed.Value = $true
        [IO.File]::Replace($Stage, $Target, $Backup, $true)
    } else {
        [IO.File]::Move($Stage, $Target)
        $Changed.Value = $true
    }
}

function Restore-InstalledFile {
    param(
        [string]$Target,
        [string]$Backup,
        [bool]$HadOriginal,
        [bool]$Changed,
        [string]$ExpectedInstalledHash,
        [string]$ExpectedOriginalHash,
        [string]$ExpectedBackupHash,
        [string]$TargetQuarantine,
        [string]$BackupQuarantine
    )
    if (-not $Changed) {
        return
    }

    # File.Replace can fail without changing either input. In that state no
    # backup exists and there is nothing to roll back. The comparison is
    # read-only, so a later writer is left untouched rather than deleted.
    if ($HadOriginal -and -not (Test-Path -LiteralPath $Backup -PathType Leaf)) {
        if (-not (Test-Path -LiteralPath $Target -PathType Leaf)) {
            throw "replacement state is uncertain: the original target and backup are both missing ($Target, $Backup)"
        }
        $item = Get-Item -LiteralPath $Target -Force
        if ($item.PSIsContainer -or ($item.Attributes -band [IO.FileAttributes]::ReparsePoint)) {
            throw "refusing to inspect a target changed to a non-regular file: $Target"
        }
        $actualTargetHash = (Get-FileHash -LiteralPath $Target -Algorithm SHA256).Hash.ToLowerInvariant()
        if (-not [string]::IsNullOrWhiteSpace($ExpectedOriginalHash) -and
            $actualTargetHash -eq $ExpectedOriginalHash) {
            return
        }
        throw "replacement state is uncertain; the current target was left untouched and no recovery backup exists: $Target"
    }

    $targetQuarantined = $false
    if (Test-Path -LiteralPath $Target) {
        $item = Get-Item -LiteralPath $Target -Force
        if ($item.PSIsContainer -or ($item.Attributes -band [IO.FileAttributes]::ReparsePoint)) {
            throw "refusing to restore over a target changed to a non-regular file: $Target"
        }
        if (Test-Path -LiteralPath $TargetQuarantine) {
            throw "rollback quarantine already exists: $TargetQuarantine"
        }
        # File.Move is a same-directory, no-overwrite rename. It captures the
        # exact mutation-time target before any hash decision, so a later writer
        # can only win the now-vacant name; it is never overwritten by rollback.
        [IO.File]::Move($Target, $TargetQuarantine)
        $targetQuarantined = $true
        $actualInstalledHash = (Get-FileHash -LiteralPath $TargetQuarantine -Algorithm SHA256).Hash.ToLowerInvariant()
        if ($actualInstalledHash -ne $ExpectedInstalledHash) {
            try {
                [IO.File]::Move($TargetQuarantine, $Target)
            } catch {
                throw "concurrent target was preserved at $TargetQuarantine because its original name could not be restored: $($_.Exception.Message)"
            }
            throw "refusing to replace a concurrently changed install target; prior recovery backup remains at $Backup"
        }
    }
    if (-not $HadOriginal) {
        if ($targetQuarantined) {
            Remove-Item -LiteralPath $TargetQuarantine -Force
        }
        return
    }
    $backupQuarantined = $false
    try {
        if (-not (Test-Path -LiteralPath $Backup -PathType Leaf)) {
            throw "backup needed for restoration is missing: $Backup"
        }
        if (Test-Path -LiteralPath $BackupQuarantine) {
            throw "backup quarantine already exists: $BackupQuarantine"
        }
        [IO.File]::Move($Backup, $BackupQuarantine)
        $backupQuarantined = $true
        $actualBackupHash = (Get-FileHash -LiteralPath $BackupQuarantine -Algorithm SHA256).Hash.ToLowerInvariant()
        if ([string]::IsNullOrWhiteSpace($ExpectedBackupHash) -or $actualBackupHash -ne $ExpectedBackupHash) {
            throw "backup changed before restoration: $BackupQuarantine"
        }
        [IO.File]::Move($BackupQuarantine, $Target)
        $backupQuarantined = $false
        if ($targetQuarantined) {
            Remove-Item -LiteralPath $TargetQuarantine -Force
        }
    } catch {
        $restoreFailure = $_.Exception.Message
        $recoveryErrors = New-Object System.Collections.Generic.List[string]
        if (-not (Test-Path -LiteralPath $Target) -and
            (Test-Path -LiteralPath $TargetQuarantine -PathType Leaf)) {
            try {
                [IO.File]::Move($TargetQuarantine, $Target)
            } catch {
                $recoveryErrors.Add("restore rejected target from ${TargetQuarantine}: $($_.Exception.Message)")
            }
        }
        if ($backupQuarantined -and -not (Test-Path -LiteralPath $Backup) -and
            (Test-Path -LiteralPath $BackupQuarantine -PathType Leaf)) {
            try {
                [IO.File]::Move($BackupQuarantine, $Backup)
            } catch {
                $recoveryErrors.Add("restore prior backup from ${BackupQuarantine}: $($_.Exception.Message)")
            }
        }
        if ($recoveryErrors.Count -gt 0) {
            throw "$restoreFailure; rollback recovery was incomplete: $([string]::Join('; ', $recoveryErrors))"
        }
        throw $restoreFailure
    }
}

function Normalize-PathEntry {
    param([string]$Value)
    if ([string]::IsNullOrWhiteSpace($Value)) {
        return ''
    }
    try {
        return ([IO.Path]::GetFullPath($Value)).TrimEnd([char[]]@('\', '/'))
    } catch {
        return $Value.Trim().TrimEnd([char[]]@('\', '/'))
    }
}

function Open-UserPathLock {
    $localAppData = [Environment]::GetFolderPath([Environment+SpecialFolder]::LocalApplicationData)
    if ([string]::IsNullOrWhiteSpace($localAppData)) {
        throw 'could not resolve LocalApplicationData for the user PATH lock'
    }
    $lockDirectory = Join-Path $localAppData 'Witself\locks'
    New-Item -ItemType Directory -Path $lockDirectory -Force | Out-Null
    $lockPath = Join-Path $lockDirectory 'user-path.lock'
    $deadline = [DateTime]::UtcNow.AddSeconds(30)
    while ($true) {
        try {
            return [IO.File]::Open(
                $lockPath,
                [IO.FileMode]::OpenOrCreate,
                [IO.FileAccess]::ReadWrite,
                [IO.FileShare]::None
            )
        } catch [IO.IOException] {
            if ([DateTime]::UtcNow -ge $deadline) {
                throw "timed out waiting to update the user PATH ($lockPath)"
            }
            Start-Sleep -Milliseconds 100
        }
    }
}

function Get-RawUserPathState {
    param([Microsoft.Win32.RegistryKey]$Key)
    $exists = @($Key.GetValueNames()) -contains 'Path'
    if (-not $exists) {
        return @{ Exists = $false; Value = ''; Kind = [Microsoft.Win32.RegistryValueKind]::ExpandString }
    }
    $kind = $Key.GetValueKind('Path')
    if ($kind -ne [Microsoft.Win32.RegistryValueKind]::String -and
        $kind -ne [Microsoft.Win32.RegistryValueKind]::ExpandString) {
        throw "refusing to update the user Path registry value because its type is $kind"
    }
    $value = $Key.GetValue(
        'Path',
        '',
        [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames
    )
    return @{ Exists = $true; Value = [string]$value; Kind = $kind }
}

function Ensure-UserPath {
    param([string]$Directory)
    $normalized = Normalize-PathEntry $Directory
    # The HKCU Path value is shared across login sessions. A per-user file lock
    # under LocalAppData coordinates console, RDP, and scheduled-task installers
    # without relying on session-scoped named kernel objects.
    $pathLock = Open-UserPathLock
    try {
        $environmentKey = [Microsoft.Win32.Registry]::CurrentUser.CreateSubKey('Environment')
        if ($null -eq $environmentKey) {
            throw 'could not open HKCU\Environment for user PATH update'
        }
        try {
            # Read the raw registry text under the cross-session lock. Using
            # DoNotExpandEnvironmentNames preserves unrelated %VAR% entries and
            # SetValue receives the exact pre-existing REG_SZ/REG_EXPAND_SZ kind.
            $pathState = Get-RawUserPathState $environmentKey
            $userPath = [string]$pathState.Value
            $entries = @()
            if (-not [string]::IsNullOrWhiteSpace($userPath)) {
                $entries = @($userPath.Split(';') | Where-Object { -not [string]::IsNullOrWhiteSpace($_) })
            }
            $present = $false
            foreach ($entry in $entries) {
                if ([string]::Equals((Normalize-PathEntry $entry), $normalized, [StringComparison]::OrdinalIgnoreCase)) {
                    $present = $true
                    break
                }
            }
            if (-not $present) {
                $updated = if ([string]::IsNullOrWhiteSpace($userPath)) { $Directory } else { "$userPath;$Directory" }
                $environmentKey.SetValue('Path', $updated, $pathState.Kind)
                Write-Host "Added $Directory to the user PATH."
            }
        } finally {
            $environmentKey.Dispose()
        }
    } finally {
        $pathLock.Dispose()
    }

    $processEntries = @()
    if (-not [string]::IsNullOrWhiteSpace($env:Path)) {
        $processEntries = @($env:Path.Split(';') | Where-Object { -not [string]::IsNullOrWhiteSpace($_) })
    }
    $processPresent = $false
    foreach ($entry in $processEntries) {
        if ([string]::Equals((Normalize-PathEntry $entry), $normalized, [StringComparison]::OrdinalIgnoreCase)) {
            $processPresent = $true
            break
        }
    }
    if (-not $processPresent) {
        $env:Path = if ([string]::IsNullOrWhiteSpace($env:Path)) { $Directory } else { "$($env:Path);$Directory" }
    }
}

try {
    $architecture = if (-not [string]::IsNullOrWhiteSpace($env:PROCESSOR_ARCHITEW6432)) {
        $env:PROCESSOR_ARCHITEW6432
    } else {
        $env:PROCESSOR_ARCHITECTURE
    }
    if ($architecture -ne 'AMD64') {
        throw "unsupported Windows architecture '$architecture' (windows/amd64 only)"
    }

    if ([string]::IsNullOrWhiteSpace($ReleaseRoot)) {
        $ReleaseRoot = 'https://github.com/witwave-ai/witself/releases/download'
    }
    if ($ReleaseRoot -match '^[A-Za-z][A-Za-z0-9+.-]*://' -and
        -not (Test-WebSource $ReleaseRoot)) {
        throw 'remote release roots must use HTTPS'
    }
    if ([string]::IsNullOrWhiteSpace($InstallDir)) {
        $localAppData = [Environment]::GetFolderPath([Environment+SpecialFolder]::LocalApplicationData)
        if ([string]::IsNullOrWhiteSpace($localAppData)) {
            throw 'could not resolve the per-user LocalApplicationData directory'
        }
        $InstallDir = Join-Path $localAppData 'Witself\bin'
    }
    $InstallDir = [IO.Path]::GetFullPath($InstallDir)

    if ([string]::IsNullOrWhiteSpace($Version)) {
        Write-Host 'Resolving latest Witself release...'
        $Version = Get-LatestVersion $ReleaseRoot
    }
    $Version = $Version.Trim()
    $versionMatch = [regex]::Match(
        $Version,
        '^v?([0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?)$'
    )
    if (-not $versionMatch.Success) {
        throw "invalid release version '$Version'"
    }
    $versionWithoutPrefix = $versionMatch.Groups[1].Value
    $tag = "v$versionWithoutPrefix"
    $asset = "witself_${versionWithoutPrefix}_windows_amd64.zip"

    Write-Host "Installing Witself $tag (windows/amd64)..."
    $temporaryRoot = Join-Path ([IO.Path]::GetTempPath()) ("witself-install-" + [Guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $temporaryRoot | Out-Null
    try {
        $archivePath = Join-Path $temporaryRoot $asset
        $checksumsPath = Join-Path $temporaryRoot 'checksums.txt'
        Copy-ReleaseItem (Get-ReleaseItemSource $ReleaseRoot $tag $asset) $archivePath
        Copy-ReleaseItem (Get-ReleaseItemSource $ReleaseRoot $tag 'checksums.txt') $checksumsPath

        $expected = $null
        foreach ($line in Get-Content -LiteralPath $checksumsPath) {
            $match = [regex]::Match($line, '^\s*([0-9A-Fa-f]{64})\s+\*?(.+?)\s*$')
            if ($match.Success -and $match.Groups[2].Value -eq $asset) {
                if ($null -ne $expected) {
                    throw "duplicate checksum entries for $asset"
                }
                $expected = $match.Groups[1].Value.ToLowerInvariant()
            }
        }
        if ($null -eq $expected) {
            throw "no checksum found for $asset"
        }
        $actual = (Get-FileHash -LiteralPath $archivePath -Algorithm SHA256).Hash.ToLowerInvariant()
        if ($actual -ne $expected) {
            throw "checksum mismatch for $asset (expected $expected, got $actual)"
        }
        Write-Host 'Checksum verified.'

        $extractRoot = Join-Path $temporaryRoot 'extract'
        Expand-Archive -LiteralPath $archivePath -DestinationPath $extractRoot -Force
        $extracted = Join-Path $extractRoot 'witself.exe'
        if (-not (Test-Path -LiteralPath $extracted -PathType Leaf)) {
            throw 'witself.exe was not found at the root of the release archive'
        }
        $expectedInstalledHash = (Get-FileHash -LiteralPath $extracted -Algorithm SHA256).Hash.ToLowerInvariant()

        New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
        $installLockPath = Join-Path $InstallDir '.witself-install.lock'
        $installLock = Open-InstallDirectoryLock $installLockPath
        try {
        $destination = Join-Path $InstallDir 'witself.exe'
        $alias = Join-Path $InstallDir 'ws.exe'
        Assert-ReplaceableTarget $destination
        Assert-ReplaceableTarget $alias

        $transaction = [Guid]::NewGuid().ToString('N')
        $primaryStage = Join-Path $InstallDir (".witself.exe.stage.$transaction")
        $aliasStage = Join-Path $InstallDir (".ws.exe.stage.$transaction")
        $primaryBackup = Join-Path $InstallDir (".witself.exe.backup.$transaction")
        $aliasBackup = Join-Path $InstallDir (".ws.exe.backup.$transaction")
        $primaryTargetQuarantine = Join-Path $InstallDir (".witself.exe.rollback-target.$transaction")
        $aliasTargetQuarantine = Join-Path $InstallDir (".ws.exe.rollback-target.$transaction")
        $primaryBackupQuarantine = Join-Path $InstallDir (".witself.exe.rollback-backup.$transaction")
        $aliasBackupQuarantine = Join-Path $InstallDir (".ws.exe.rollback-backup.$transaction")
        Copy-Item -LiteralPath $extracted -Destination $primaryStage
        Copy-Item -LiteralPath $extracted -Destination $aliasStage
        $hadPrimary = Test-Path -LiteralPath $destination -PathType Leaf
        $hadAlias = Test-Path -LiteralPath $alias -PathType Leaf
        $primaryOriginalHash = $null
        $aliasOriginalHash = $null
        $primaryBackupHash = $null
        $aliasBackupHash = $null
        $primaryChanged = $false
        $aliasChanged = $false
        $preserveBackups = $false
        try {
            Install-StagedFile `
                $primaryStage `
                $destination `
                $primaryBackup `
                ([ref]$hadPrimary) `
                ([ref]$primaryChanged) `
                ([ref]$primaryOriginalHash)
            if (Test-Path -LiteralPath $primaryBackup -PathType Leaf) {
                # A target appeared after preflight and was preserved by
                # File.Replace. Treat that mutation-time file as the original.
                $hadPrimary = $true
                $primaryBackupHash = $primaryOriginalHash
            }
            Install-StagedFile `
                $aliasStage `
                $alias `
                $aliasBackup `
                ([ref]$hadAlias) `
                ([ref]$aliasChanged) `
                ([ref]$aliasOriginalHash)
            if (Test-Path -LiteralPath $aliasBackup -PathType Leaf) {
                $hadAlias = $true
                $aliasBackupHash = $aliasOriginalHash
            }

            $versionOutput = @(& $destination version 2>&1)
            if ($LASTEXITCODE -ne 0) {
                throw "installed witself.exe failed its version self-test with exit code $LASTEXITCODE"
            }
            $aliasOutput = @(& $alias version 2>&1)
            if ($LASTEXITCODE -ne 0) {
                throw "installed ws.exe alias failed its version self-test with exit code $LASTEXITCODE"
            }
            $primaryInstalledHash = (Get-FileHash -LiteralPath $destination -Algorithm SHA256).Hash.ToLowerInvariant()
            $aliasInstalledHash = (Get-FileHash -LiteralPath $alias -Algorithm SHA256).Hash.ToLowerInvariant()
            if ($primaryInstalledHash -ne $expectedInstalledHash -or $aliasInstalledHash -ne $expectedInstalledHash) {
                throw 'installed witself.exe and ws.exe do not match the verified release artifact'
            }
        } catch {
            $installFailure = $_.Exception.Message
            # File.Replace may have crossed its namespace boundary before
            # reporting an I/O failure. A backup is authoritative evidence that
            # replacement occurred and must participate in recovery, including
            # when the destination appeared after the initial preflight.
            if (Test-Path -LiteralPath $primaryBackup -PathType Leaf) {
                $hadPrimary = $true
                $primaryChanged = $true
                if ([string]::IsNullOrWhiteSpace($primaryBackupHash)) {
                    # A File.Replace error may still have created the backup.
                    # Fence it to the pre-call target hash; never bless bytes
                    # first observed only after the failure boundary.
                    $primaryBackupHash = $primaryOriginalHash
                }
            }
            if (Test-Path -LiteralPath $aliasBackup -PathType Leaf) {
                $hadAlias = $true
                $aliasChanged = $true
                if ([string]::IsNullOrWhiteSpace($aliasBackupHash)) {
                    $aliasBackupHash = $aliasOriginalHash
                }
            }
            $restorationErrors = New-Object System.Collections.Generic.List[string]
            foreach ($restore in @(
                @{
                    Target = $alias
                    Backup = $aliasBackup
                    HadOriginal = $hadAlias
                    Changed = $aliasChanged
                    ExpectedOriginalHash = $aliasOriginalHash
                    ExpectedBackupHash = $aliasBackupHash
                    TargetQuarantine = $aliasTargetQuarantine
                    BackupQuarantine = $aliasBackupQuarantine
                },
                @{
                    Target = $destination
                    Backup = $primaryBackup
                    HadOriginal = $hadPrimary
                    Changed = $primaryChanged
                    ExpectedOriginalHash = $primaryOriginalHash
                    ExpectedBackupHash = $primaryBackupHash
                    TargetQuarantine = $primaryTargetQuarantine
                    BackupQuarantine = $primaryBackupQuarantine
                }
            )) {
                try {
                    Restore-InstalledFile `
                        $restore.Target `
                        $restore.Backup `
                        ([bool]$restore.HadOriginal) `
                        ([bool]$restore.Changed) `
                        $expectedInstalledHash `
                        $restore.ExpectedOriginalHash `
                        $restore.ExpectedBackupHash `
                        $restore.TargetQuarantine `
                        $restore.BackupQuarantine
                } catch {
                    $restorationErrors.Add($_.Exception.Message)
                }
            }
            if ($restorationErrors.Count -gt 0) {
                $preserveBackups = $true
                $details = [string]::Join('; ', $restorationErrors)
                throw "installation failed ($installFailure) and restoration was incomplete ($details); recovery files were retained in $InstallDir"
            }
            throw "$installFailure; the previous installation was restored"
        } finally {
            foreach ($path in @($primaryStage, $aliasStage)) {
                if (Test-Path -LiteralPath $path) {
                    Remove-Item -LiteralPath $path -Force
                }
            }
            if (-not $preserveBackups) {
                foreach ($path in @($primaryBackup, $aliasBackup)) {
                    if (Test-Path -LiteralPath $path) {
                        Remove-Item -LiteralPath $path -Force
                    }
                }
            }
        }

        foreach ($line in $versionOutput) {
            Write-Host $line
        }
        Write-Host "Installed Witself to $destination"
        Write-Host "Installed alias to $alias"
        } finally {
            $installLock.Dispose()
        }
    } finally {
        if (Test-Path -LiteralPath $temporaryRoot) {
            Remove-Item -LiteralPath $temporaryRoot -Recurse -Force
        }
    }

    if (-not $NoPathUpdate) {
        Ensure-UserPath $InstallDir
    } else {
        Write-Host 'Skipped user PATH update (-NoPathUpdate).'
    }
    Write-Host 'Next: witself integrations'
    if ($NoPathUpdate) {
        Write-Host "Direct path: & '$destination' integrations"
    }
} catch {
    throw "install: $($_.Exception.Message)"
}
} $Version $InstallDir ([bool]$NoPathUpdate) $ReleaseRoot
