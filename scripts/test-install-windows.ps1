#requires -Version 5.1
[CmdletBinding()]
param(
    [string]$GoReleaserDist,
    [string]$InstalledBinaryOutput
)

Set-StrictMode -Version 2.0
$ErrorActionPreference = 'Stop'

function New-LocalRelease {
    param(
        [string]$Root,
        [string]$Version,
        [string]$Binary
    )
    $plainVersion = $Version.TrimStart('v')
    $tag = "v$plainVersion"
    $asset = "witself_${plainVersion}_windows_amd64.zip"
    $releaseDir = Join-Path $Root $tag
    $packageDir = Join-Path $releaseDir 'package'
    New-Item -ItemType Directory -Path $packageDir -Force | Out-Null
    Copy-Item -LiteralPath $Binary -Destination (Join-Path $packageDir 'witself.exe') -Force
    $archive = Join-Path $releaseDir $asset
    Compress-Archive -LiteralPath (Join-Path $packageDir 'witself.exe') -DestinationPath $archive -Force
    $hash = (Get-FileHash -LiteralPath $archive -Algorithm SHA256).Hash.ToLowerInvariant()
    Set-Content -LiteralPath (Join-Path $releaseDir 'checksums.txt') -Value "$hash  $asset" -Encoding Ascii
}

function New-LocalReleaseWithExtraArchiveEntry {
    param(
        [string]$Root,
        [string]$Version,
        [string]$Binary
    )
    $plainVersion = $Version.TrimStart('v')
    $tag = "v$plainVersion"
    $asset = "witself_${plainVersion}_windows_amd64.zip"
    $releaseDir = Join-Path $Root $tag
    $packageDir = Join-Path $releaseDir 'package'
    New-Item -ItemType Directory -Path $packageDir -Force | Out-Null
    Copy-Item -LiteralPath $Binary -Destination (Join-Path $packageDir 'witself.exe') -Force
    Set-Content `
        -LiteralPath (Join-Path $packageDir 'unexpected.txt') `
        -Value 'unexpected archive member' `
        -Encoding Ascii
    $archive = Join-Path $releaseDir $asset
    Compress-Archive -Path (Join-Path $packageDir '*') -DestinationPath $archive -Force
    $hash = (Get-FileHash -LiteralPath $archive -Algorithm SHA256).Hash.ToLowerInvariant()
    Set-Content -LiteralPath (Join-Path $releaseDir 'checksums.txt') -Value "$hash  $asset" -Encoding Ascii
}

function New-LocalReleaseWithNonRegularArchiveEntry {
    param(
        [string]$Root,
        [string]$Version
    )
    $plainVersion = $Version.TrimStart('v')
    $tag = "v$plainVersion"
    $asset = "witself_${plainVersion}_windows_amd64.zip"
    $releaseDir = Join-Path $Root $tag
    New-Item -ItemType Directory -Path $releaseDir -Force | Out-Null
    $archivePath = Join-Path $releaseDir $asset
    Add-Type -AssemblyName System.IO.Compression.FileSystem
    $archive = [IO.Compression.ZipFile]::Open(
        $archivePath,
        [IO.Compression.ZipArchiveMode]::Create
    )
    try {
        $entry = $archive.CreateEntry('witself.exe')
        # A Unix FIFO entry has the requested root name but is not a regular
        # executable. Expand-Archive may materialize it as an ordinary file,
        # so the installer must reject the signed archive metadata first.
        $entry.ExternalAttributes = 0x10000000
        $stream = $entry.Open()
        try {
            $raw = [Text.Encoding]::ASCII.GetBytes('not a regular executable')
            $stream.Write($raw, 0, $raw.Length)
        } finally {
            $stream.Dispose()
        }
    } finally {
        $archive.Dispose()
    }
    $hash = (Get-FileHash -LiteralPath $archivePath -Algorithm SHA256).Hash.ToLowerInvariant()
    Set-Content -LiteralPath (Join-Path $releaseDir 'checksums.txt') -Value "$hash  $asset" -Encoding Ascii
}

function New-StampedWitselfBinary {
    param(
        [string]$RepositoryRoot,
        [string]$Destination,
        [string]$Version
    )
    $previousGoProxy = $env:GOPROXY
    $previousGoSumDB = $env:GOSUMDB
    $previousGoToolchain = $env:GOTOOLCHAIN
    Push-Location $RepositoryRoot
    try {
        $env:GOPROXY = 'off'
        $env:GOSUMDB = 'off'
        $env:GOTOOLCHAIN = 'local'
        $ldflags = @(
            "-X github.com/witwave-ai/witself/internal/version.Version=$Version",
            '-X github.com/witwave-ai/witself/internal/version.Commit=installer-smoke',
            '-X github.com/witwave-ai/witself/internal/version.Date=synthetic'
        ) -join ' '
        & go build -trimpath -ldflags $ldflags -o $Destination ./cmd/witself
        if ($LASTEXITCODE -ne 0) {
            throw "offline go build for stamped installer fixture exited $LASTEXITCODE"
        }
    } finally {
        Pop-Location
        $env:GOPROXY = $previousGoProxy
        $env:GOSUMDB = $previousGoSumDB
        $env:GOTOOLCHAIN = $previousGoToolchain
    }
}

function Invoke-Installer {
    param(
        [string]$Installer,
        [string]$ReleaseRoot,
        [string]$InstallDir,
        [string]$Version,
        [bool]$NoPathUpdate = $true
    )
    $arguments = @(
        '-NoLogo', '-NoProfile', '-NonInteractive', '-ExecutionPolicy', 'Bypass',
        '-File', $Installer,
        '-ReleaseRoot', $ReleaseRoot,
        '-InstallDir', $InstallDir
    )
    if ($NoPathUpdate) {
        $arguments += '-NoPathUpdate'
    }
    if (-not [string]::IsNullOrWhiteSpace($Version)) {
        $arguments += @('-Version', $Version)
    }
    # Keep child-process output out of this function's success pipeline so the
    # caller always receives exactly one integer exit code. Windows PowerShell
    # can also promote native stderr to an ErrorRecord when ErrorActionPreference
    # is Stop, so collect it under Continue for the expected failure cases.
    $previousErrorActionPreference = $ErrorActionPreference
    try {
        $ErrorActionPreference = 'Continue'
        $windowsPowerShell = Join-Path $PSHOME 'powershell.exe'
        $output = @(& $windowsPowerShell @arguments 2>&1)
        $exitCode = $LASTEXITCODE
    } finally {
        $ErrorActionPreference = $previousErrorActionPreference
    }
    foreach ($line in $output) {
        Write-Host $line
    }
    return [int]$exitCode
}

function Get-InstallerFunctionBody {
    param(
        [string]$Installer,
        [string]$Name
    )
    $tokens = $null
    $parseErrors = $null
    $ast = [System.Management.Automation.Language.Parser]::ParseFile(
        $Installer,
        [ref]$tokens,
        [ref]$parseErrors
    )
    if ($parseErrors.Count -ne 0) {
        throw "installer has PowerShell parse errors: $($parseErrors[0].Message)"
    }
    $definitions = @($ast.FindAll({
        param($node)
        $node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and
            $node.Name -eq $Name
    }, $true))
    if ($definitions.Count -ne 1) {
        throw "installer function $Name resolved to $($definitions.Count) definitions"
    }
    $body = $definitions[0].Body.Extent.Text
    return [scriptblock]::Create($body.Substring(1, $body.Length - 2))
}

function Normalize-TestPathEntry {
    param([string]$Value)
    try {
        return ([IO.Path]::GetFullPath($Value)).TrimEnd([char[]]@('\', '/'))
    } catch {
        return $Value.Trim().TrimEnd([char[]]@('\', '/'))
    }
}

function Open-TestUserPathLock {
    $localAppData = [Environment]::GetFolderPath([Environment+SpecialFolder]::LocalApplicationData)
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
                throw "timed out waiting for the installer smoke user PATH lock ($lockPath)"
            }
            Start-Sleep -Milliseconds 100
        }
    }
}

function Get-TestRawUserPathStateFromKey {
    param([Microsoft.Win32.RegistryKey]$Key)
    $exists = @($Key.GetValueNames()) -contains 'Path'
    if (-not $exists) {
        return @{ Exists = $false; Value = ''; Kind = [Microsoft.Win32.RegistryValueKind]::ExpandString }
    }
    $kind = $Key.GetValueKind('Path')
    if ($kind -ne [Microsoft.Win32.RegistryValueKind]::String -and
        $kind -ne [Microsoft.Win32.RegistryValueKind]::ExpandString) {
        throw "installer smoke refuses the non-string user Path registry type $kind"
    }
    $value = $Key.GetValue(
        'Path',
        '',
        [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames
    )
    return @{ Exists = $true; Value = [string]$value; Kind = $kind }
}

function Get-TestRawUserPathState {
    $key = [Microsoft.Win32.Registry]::CurrentUser.CreateSubKey('Environment')
    try {
        return Get-TestRawUserPathStateFromKey $key
    } finally {
        $key.Dispose()
    }
}

function Initialize-TestUserPathFixture {
    $pathLock = Open-TestUserPathLock
    try {
        $key = [Microsoft.Win32.Registry]::CurrentUser.CreateSubKey('Environment')
        try {
            $original = Get-TestRawUserPathStateFromKey $key
            $sentinel = '%WITSELF_INSTALLER_PATH_SENTINEL%\bin'
            $fixtureRaw = if ([string]::IsNullOrWhiteSpace($original.Value)) {
                $sentinel
            } else {
                "$($original.Value);$sentinel"
            }
            $key.SetValue('Path', $fixtureRaw, [Microsoft.Win32.RegistryValueKind]::ExpandString)
            return @{
                Original = $original
                Sentinel = $sentinel
                FixtureRaw = $fixtureRaw
                ExpectedRaw = $null
            }
        } finally {
            $key.Dispose()
        }
    } finally {
        $pathLock.Dispose()
    }
}

function Get-UserPathEntryCount {
    param([string]$Directory)
    $normalized = Normalize-TestPathEntry $Directory
    $state = Get-TestRawUserPathState
    return @(
        $state.Value.Split(';') |
            Where-Object { -not [string]::IsNullOrWhiteSpace($_) } |
            Where-Object {
                [string]::Equals(
                    (Normalize-TestPathEntry $_),
                    $normalized,
                    [StringComparison]::OrdinalIgnoreCase
                )
            }
    ).Count
}

function Restore-TestUserPathFixture {
    param(
        [string]$Directory,
        [hashtable]$Fixture
    )
    if ($null -eq $Fixture) {
        return
    }
    $normalized = if ([string]::IsNullOrWhiteSpace($Directory)) { '' } else { Normalize-TestPathEntry $Directory }
    $pathLock = Open-TestUserPathLock
    try {
        $key = [Microsoft.Win32.Registry]::CurrentUser.CreateSubKey('Environment')
        try {
            $current = Get-TestRawUserPathStateFromKey $key
            $knownExpected = $current.Kind -eq [Microsoft.Win32.RegistryValueKind]::ExpandString -and
                ($current.Value -eq $Fixture.FixtureRaw -or
                    (-not [string]::IsNullOrWhiteSpace([string]$Fixture.ExpectedRaw) -and
                        $current.Value -eq $Fixture.ExpectedRaw))
            if ($knownExpected) {
                if ($Fixture.Original.Exists) {
                    $key.SetValue('Path', $Fixture.Original.Value, $Fixture.Original.Kind)
                } else {
                    $key.DeleteValue('Path', $false)
                }
                return
            }

            # An unrelated writer changed Path after the fixture was installed.
            # Remove only the two exact test-owned entries and retain its raw
            # spelling, empty components, and registry kind.
            if (-not $current.Exists) {
                return
            }
            $kept = New-Object System.Collections.Generic.List[string]
            foreach ($entry in $current.Value.Split(';')) {
                $isInstallDirectory = -not [string]::IsNullOrWhiteSpace($entry) -and
                    [string]::Equals(
                        (Normalize-TestPathEntry $entry),
                        $normalized,
                        [StringComparison]::OrdinalIgnoreCase
                    )
                if ($entry -ne $Fixture.Sentinel -and -not $isInstallDirectory) {
                    $kept.Add($entry)
                }
            }
            $key.SetValue('Path', [string]::Join(';', $kept), $current.Kind)
        } finally {
            $key.Dispose()
        }
    } finally {
        $pathLock.Dispose()
    }
}

function Test-GoReleaserWindowsArchive {
    param(
        [string]$Installer,
        [string]$Dist,
        [string]$TemporaryRoot,
        [string]$InstalledOutput
    )
    if ([string]::IsNullOrWhiteSpace($Dist)) {
        if (-not [string]::IsNullOrWhiteSpace($InstalledOutput)) {
            throw 'InstalledBinaryOutput requires GoReleaserDist so the output is proven to come from the release archive'
        }
        return
    }
    $distRoot = [IO.Path]::GetFullPath($Dist)
    $metadataPath = Join-Path $distRoot 'metadata.json'
    $checksumsPath = Join-Path $distRoot 'checksums.txt'
    if (-not (Test-Path -LiteralPath $metadataPath -PathType Leaf) -or
        -not (Test-Path -LiteralPath $checksumsPath -PathType Leaf)) {
        throw "GoReleaser metadata or checksums are missing from $distRoot"
    }
    $metadata = Get-Content -LiteralPath $metadataPath -Raw | ConvertFrom-Json
    $version = [string]$metadata.version
    if ([string]::IsNullOrWhiteSpace($version)) {
        throw 'GoReleaser metadata does not contain a version'
    }
    $asset = "witself_${version}_windows_amd64.zip"
    $archivePath = Join-Path $distRoot $asset
    if (-not (Test-Path -LiteralPath $archivePath -PathType Leaf)) {
        throw "GoReleaser did not produce the expected Windows archive: $archivePath"
    }
    $archiveContents = Join-Path $TemporaryRoot 'goreleaser-archive-contents'
    Expand-Archive -LiteralPath $archivePath -DestinationPath $archiveContents -Force
    $archiveBinary = Join-Path $archiveContents 'witself.exe'
    if (-not (Test-Path -LiteralPath $archiveBinary -PathType Leaf)) {
        throw 'GoReleaser Windows archive does not contain witself.exe at its documented root'
    }
    $archiveBinaryHash = (Get-FileHash -LiteralPath $archiveBinary -Algorithm SHA256).Hash

    $fixtureRoot = Join-Path $TemporaryRoot 'goreleaser-release'
    $releaseDirectory = Join-Path $fixtureRoot "v$version"
    New-Item -ItemType Directory -Path $releaseDirectory -Force | Out-Null
    Copy-Item -LiteralPath $archivePath -Destination (Join-Path $releaseDirectory $asset)
    Copy-Item -LiteralPath $checksumsPath -Destination (Join-Path $releaseDirectory 'checksums.txt')
    $installDirectory = Join-Path $TemporaryRoot 'goreleaser-installed'
    if ((Invoke-Installer $Installer $fixtureRoot $installDirectory $version $true) -ne 0) {
        throw 'installer rejected the exact GoReleaser-produced Windows archive'
    }
    $primary = Join-Path $installDirectory 'witself.exe'
    $alias = Join-Path $installDirectory 'ws.exe'
    if (-not (Test-Path -LiteralPath $primary -PathType Leaf) -or
        -not (Test-Path -LiteralPath $alias -PathType Leaf)) {
        throw 'GoReleaser archive smoke did not install both executable names'
    }
    $primaryHash = (Get-FileHash -LiteralPath $primary -Algorithm SHA256).Hash
    $aliasHash = (Get-FileHash -LiteralPath $alias -Algorithm SHA256).Hash
    if ($primaryHash -ne $archiveBinaryHash -or $aliasHash -ne $archiveBinaryHash) {
        throw 'GoReleaser archive smoke did not preserve the exact archived executable bytes'
    }
    if (-not [string]::IsNullOrWhiteSpace($InstalledOutput)) {
        $outputPath = [IO.Path]::GetFullPath($InstalledOutput)
        $outputDirectory = Split-Path -Parent $outputPath
        if (-not [string]::IsNullOrWhiteSpace($outputDirectory)) {
            New-Item -ItemType Directory -Path $outputDirectory -Force | Out-Null
        }
        Copy-Item -LiteralPath $primary -Destination $outputPath -Force
        $outputHash = (Get-FileHash -LiteralPath $outputPath -Algorithm SHA256).Hash
        if ($outputHash -ne $primaryHash) {
            throw 'copied Windows installer smoke output differs from the verified installed binary'
        }
    }
}

$repositoryRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
$installer = Join-Path $repositoryRoot 'install.ps1'
$temporaryRoot = Join-Path ([IO.Path]::GetTempPath()) ("witself-install-smoke-" + [Guid]::NewGuid().ToString('N'))
$installDir = $null
$pathFixture = $null
New-Item -ItemType Directory -Path $temporaryRoot | Out-Null
try {
    $fixtureRoot = Join-Path $temporaryRoot 'releases'
    $installDir = Join-Path $temporaryRoot 'installed'
    $goodBinary = Join-Path $temporaryRoot 'witself-good.exe'
    New-StampedWitselfBinary $repositoryRoot $goodBinary '9.9.9'

    $pathFixture = Initialize-TestUserPathFixture
    $rawPathBeforeGoReleaserSmoke = Get-TestRawUserPathState
    Test-GoReleaserWindowsArchive `
        $installer `
        $GoReleaserDist `
        $temporaryRoot `
        $InstalledBinaryOutput
    $rawPathAfterGoReleaserSmoke = Get-TestRawUserPathState
    if ($rawPathAfterGoReleaserSmoke.Value -ne $rawPathBeforeGoReleaserSmoke.Value -or
        $rawPathAfterGoReleaserSmoke.Kind -ne $rawPathBeforeGoReleaserSmoke.Kind) {
        throw 'exact GoReleaser archive smoke changed user PATH despite -NoPathUpdate'
    }

    $alternateBinary = Join-Path $temporaryRoot 'witself-alternate.exe'
    New-StampedWitselfBinary $repositoryRoot $alternateBinary '9.9.13'

    New-LocalRelease $fixtureRoot 'v9.9.9' $goodBinary
    New-LocalRelease $fixtureRoot 'v9.9.13' $alternateBinary
    Set-Content `
        -LiteralPath (Join-Path $fixtureRoot 'latest.json') `
        -Value '{"tag_name":"v9.9.9"}' `
        -Encoding Ascii

    if ((Invoke-Installer $installer $fixtureRoot $installDir '' $false) -ne 0) {
        throw 'latest-version local installer smoke failed'
    }
    if ((Get-UserPathEntryCount $installDir) -ne 1) {
        throw 'default installer did not add the install directory to the user PATH exactly once'
    }
    $rawPathAfterInstall = Get-TestRawUserPathState
    if ($rawPathAfterInstall.Kind -ne [Microsoft.Win32.RegistryValueKind]::ExpandString -or
        -not $rawPathAfterInstall.Value.Contains($pathFixture.Sentinel)) {
        throw 'installer expanded or changed the kind of an unrelated REG_EXPAND_SZ user PATH entry'
    }
    $pathFixture.ExpectedRaw = $rawPathAfterInstall.Value
    $primary = Join-Path $installDir 'witself.exe'
    $alias = Join-Path $installDir 'ws.exe'
    foreach ($path in @($primary, $alias)) {
        if (-not (Test-Path -LiteralPath $path -PathType Leaf)) {
            throw "installer did not create $path"
        }
        & $path version | Out-Null
        if ($LASTEXITCODE -ne 0) {
            throw "$path failed to run"
        }
    }

    if ((Invoke-Installer $installer $fixtureRoot $installDir 'v9.9.9' $false) -ne 0) {
        throw 'idempotent explicit-version installer smoke failed'
    }
    if ((Get-UserPathEntryCount $installDir) -ne 1) {
        throw 'idempotent installer duplicated the install directory in the user PATH'
    }
    $primaryBefore = (Get-FileHash -LiteralPath $primary -Algorithm SHA256).Hash
    $aliasBefore = (Get-FileHash -LiteralPath $alias -Algorithm SHA256).Hash

    # Checksum success does not authorize arbitrary ZIP contents. The release
    # archive contract is one regular root witself.exe and nothing else.
    New-LocalReleaseWithExtraArchiveEntry $fixtureRoot 'v9.9.15' $goodBinary
    if ((Invoke-Installer $installer $fixtureRoot $installDir 'v9.9.15' $true) -eq 0) {
        throw 'installer accepted a release archive with an unexpected extra entry'
    }
    if ((Get-FileHash -LiteralPath $primary -Algorithm SHA256).Hash -ne $primaryBefore -or
        (Get-FileHash -LiteralPath $alias -Algorithm SHA256).Hash -ne $aliasBefore) {
        throw 'archive-shape rejection changed the installed witself.exe or ws.exe bytes'
    }

    New-LocalReleaseWithNonRegularArchiveEntry $fixtureRoot 'v9.9.16'
    if ((Invoke-Installer $installer $fixtureRoot $installDir 'v9.9.16' $true) -eq 0) {
        throw 'installer accepted a non-regular release archive entry'
    }
    if ((Get-FileHash -LiteralPath $primary -Algorithm SHA256).Hash -ne $primaryBefore -or
        (Get-FileHash -LiteralPath $alias -Algorithm SHA256).Hash -ne $aliasBefore) {
        throw 'non-regular archive rejection changed the installed witself.exe or ws.exe bytes'
    }

    # Exercise the documented File.Replace 1177 namespace layout directly:
    # replacement remains staged, the old target is at backup, and the canonical
    # target is absent. Parse the production function body so this cannot drift
    # into a separate test-only implementation.
    $restoreInstalledFile = Get-InstallerFunctionBody $installer 'Restore-InstalledFile'
    $partialRoot = Join-Path $temporaryRoot 'replace-1177'
    New-Item -ItemType Directory -Path $partialRoot | Out-Null
    $partialTarget = Join-Path $partialRoot 'witself.exe'
    $partialBackup = Join-Path $partialRoot '.witself.exe.backup'
    $partialReplacement = Join-Path $partialRoot '.witself.exe.stage'
    $partialTargetQuarantine = Join-Path $partialRoot '.witself.exe.rollback-target'
    $partialBackupQuarantine = Join-Path $partialRoot '.witself.exe.rollback-backup'
    [IO.File]::WriteAllText($partialBackup, 'original partial target', [Text.Encoding]::ASCII)
    [IO.File]::WriteAllText($partialReplacement, 'staged replacement', [Text.Encoding]::ASCII)
    $partialOriginalHash = (Get-FileHash -LiteralPath $partialBackup -Algorithm SHA256).Hash.ToLowerInvariant()
    $partialReplacementHash = (Get-FileHash -LiteralPath $partialReplacement -Algorithm SHA256).Hash.ToLowerInvariant()
    & $restoreInstalledFile `
        $partialTarget `
        $partialBackup `
        $true `
        $true `
        $partialReplacementHash `
        $partialOriginalHash `
        $partialOriginalHash `
        $partialTargetQuarantine `
        $partialBackupQuarantine
    if ((Get-Content -LiteralPath $partialTarget -Raw) -ne 'original partial target' -or
        -not (Test-Path -LiteralPath $partialReplacement -PathType Leaf) -or
        (Test-Path -LiteralPath $partialBackup) -or
        (Test-Path -LiteralPath $partialTargetQuarantine) -or
        (Test-Path -LiteralPath $partialBackupQuarantine)) {
        throw '1177 partial File.Replace layout did not restore the old target conservatively'
    }

    # A second installer must not enter the mutation transaction while the
    # install-directory lock is held, and a timeout must leave the pair intact.
    $installLockPath = Join-Path $installDir '.witself-install.lock'
    $heldInstallLock = [IO.File]::Open(
        $installLockPath,
        [IO.FileMode]::OpenOrCreate,
        [IO.FileAccess]::ReadWrite,
        [IO.FileShare]::None
    )
    $previousLockTimeout = $env:WS_INSTALL_LOCK_TIMEOUT_MS
    try {
        $env:WS_INSTALL_LOCK_TIMEOUT_MS = '200'
        if ((Invoke-Installer $installer $fixtureRoot $installDir 'v9.9.9' $true) -eq 0) {
            throw 'installer ignored the held install-directory transaction lock'
        }
    } finally {
        $env:WS_INSTALL_LOCK_TIMEOUT_MS = $previousLockTimeout
        $heldInstallLock.Dispose()
    }
    if ((Get-FileHash -LiteralPath $primary -Algorithm SHA256).Hash -ne $primaryBefore -or
        (Get-FileHash -LiteralPath $alias -Algorithm SHA256).Hash -ne $aliasBefore) {
        throw 'install-lock timeout changed the installed witself.exe or ws.exe bytes'
    }

    # File.Replace may fail before changing either input (for example, when a
    # process has the destination open without delete sharing). The installer
    # must recognize that unchanged/no-backup state and restore the other alias.
    $heldAlias = [IO.File]::Open(
        $alias,
        [IO.FileMode]::Open,
        [IO.FileAccess]::Read,
        [IO.FileShare]::Read
    )
    try {
        if ((Invoke-Installer $installer $fixtureRoot $installDir 'v9.9.13' $true) -eq 0) {
            throw 'installer unexpectedly replaced an alias held without delete sharing'
        }
    } finally {
        $heldAlias.Dispose()
    }
    if ((Get-FileHash -LiteralPath $primary -Algorithm SHA256).Hash -ne $primaryBefore -or
        (Get-FileHash -LiteralPath $alias -Algorithm SHA256).Hash -ne $aliasBefore) {
        throw 'benign File.Replace failure did not leave the prior executable pair intact'
    }

    New-LocalRelease $fixtureRoot 'v9.9.10' $goodBinary
    $mismatchChecksums = Join-Path (Join-Path $fixtureRoot 'v9.9.10') 'checksums.txt'
    $mismatchAsset = 'witself_9.9.10_windows_amd64.zip'
    Set-Content -LiteralPath $mismatchChecksums -Value "$('0' * 64)  $mismatchAsset" -Encoding Ascii
    if ((Invoke-Installer $installer $fixtureRoot $installDir 'v9.9.10' $true) -eq 0) {
        throw 'installer accepted an archive whose checksum did not match'
    }
    if ((Get-FileHash -LiteralPath $primary -Algorithm SHA256).Hash -ne $primaryBefore -or
        (Get-FileHash -LiteralPath $alias -Algorithm SHA256).Hash -ne $aliasBefore) {
        throw 'checksum rejection changed the installed witself.exe or ws.exe bytes'
    }

    # A runnable, correctly checksummed binary is still rejected when its
    # embedded version does not match the requested release tag.
    New-LocalRelease $fixtureRoot 'v9.9.14' $goodBinary
    if ((Invoke-Installer $installer $fixtureRoot $installDir 'v9.9.14' $true) -eq 0) {
        throw 'installer accepted a checksummed runnable binary with the wrong version'
    }
    if ((Get-FileHash -LiteralPath $primary -Algorithm SHA256).Hash -ne $primaryBefore -or
        (Get-FileHash -LiteralPath $alias -Algorithm SHA256).Hash -ne $aliasBefore) {
        throw 'version mismatch changed the installed witself.exe or ws.exe bytes'
    }

    $badSource = Join-Path $temporaryRoot 'bad-main.go'
    $badBinary = Join-Path $temporaryRoot 'witself-bad.exe'
    Set-Content -LiteralPath $badSource -Encoding Ascii -Value @'
package main

import (
    "fmt"
    "os"
)

func main() {
    if marker := os.Getenv("WITSELF_TEST_POSTCOMMIT_COUNTER"); marker != "" {
        if _, err := os.Stat(marker); err == nil {
            os.Exit(23)
        }
        _ = os.WriteFile(marker, []byte("staged\n"), 0o600)
    }
    fmt.Println("witself 9.9.11 (commit installer-smoke, built synthetic)")
}
'@
    $previousGoProxy = $env:GOPROXY
    $previousGoSumDB = $env:GOSUMDB
    $previousGoToolchain = $env:GOTOOLCHAIN
    try {
        $env:GOPROXY = 'off'
        $env:GOSUMDB = 'off'
        $env:GOTOOLCHAIN = 'local'
        & go build -o $badBinary $badSource
        if ($LASTEXITCODE -ne 0) {
            throw "offline go build for failing installer fixture exited $LASTEXITCODE"
        }
    } finally {
        $env:GOPROXY = $previousGoProxy
        $env:GOSUMDB = $previousGoSumDB
        $env:GOTOOLCHAIN = $previousGoToolchain
    }
    New-LocalRelease $fixtureRoot 'v9.9.11' $badBinary
    $postcommitCounter = Join-Path $temporaryRoot 'postcommit-counter'
    $previousPostcommitCounter = $env:WITSELF_TEST_POSTCOMMIT_COUNTER
    try {
        $env:WITSELF_TEST_POSTCOMMIT_COUNTER = $postcommitCounter
        if ((Invoke-Installer $installer $fixtureRoot $installDir 'v9.9.11' $true) -eq 0) {
            throw 'installer accepted a binary that failed its post-commit self-test'
        }
    } finally {
        $env:WITSELF_TEST_POSTCOMMIT_COUNTER = $previousPostcommitCounter
    }
    if (-not (Test-Path -LiteralPath $postcommitCounter -PathType Leaf)) {
        throw 'post-commit rollback fixture did not pass its staged version check'
    }
    if ((Get-FileHash -LiteralPath $primary -Algorithm SHA256).Hash -ne $primaryBefore -or
        (Get-FileHash -LiteralPath $alias -Algorithm SHA256).Hash -ne $aliasBefore) {
        throw 'failed upgrade did not restore the prior witself.exe and ws.exe bytes'
    }
    & $primary version | Out-Null
    if ($LASTEXITCODE -ne 0) {
        throw 'restored Witself binary does not run'
    }

    # A failed self-test must not let rollback overwrite a file another process
    # changed after installation. This fixture changes ws.exe from the primary
    # binary's version command immediately before returning failure.
    $concurrentSource = Join-Path $temporaryRoot 'concurrent-main.go'
    $concurrentBinary = Join-Path $temporaryRoot 'witself-concurrent.exe'
    Set-Content -LiteralPath $concurrentSource -Encoding Ascii -Value @'
package main

import (
    "fmt"
    "os"
)

func main() {
    counter := os.Getenv("WITSELF_TEST_CONCURRENT_COUNTER")
    if counter != "" {
        if _, err := os.Stat(counter); err == nil {
            if path := os.Getenv("WITSELF_TEST_CONCURRENT_ALIAS"); path != "" {
                _ = os.WriteFile(path, []byte("concurrent user file\n"), 0o600)
            }
            os.Exit(24)
        }
        _ = os.WriteFile(counter, []byte("staged\n"), 0o600)
    }
    fmt.Println("witself 9.9.12 (commit installer-smoke, built synthetic)")
}
'@
    $previousGoProxy = $env:GOPROXY
    $previousGoSumDB = $env:GOSUMDB
    $previousGoToolchain = $env:GOTOOLCHAIN
    try {
        $env:GOPROXY = 'off'
        $env:GOSUMDB = 'off'
        $env:GOTOOLCHAIN = 'local'
        & go build -o $concurrentBinary $concurrentSource
        if ($LASTEXITCODE -ne 0) {
            throw "offline go build for concurrent installer fixture exited $LASTEXITCODE"
        }
    } finally {
        $env:GOPROXY = $previousGoProxy
        $env:GOSUMDB = $previousGoSumDB
        $env:GOTOOLCHAIN = $previousGoToolchain
    }
    New-LocalRelease $fixtureRoot 'v9.9.12' $concurrentBinary
    $previousConcurrentAlias = $env:WITSELF_TEST_CONCURRENT_ALIAS
    $previousConcurrentCounter = $env:WITSELF_TEST_CONCURRENT_COUNTER
    $concurrentCounter = Join-Path $temporaryRoot 'concurrent-counter'
    try {
        $env:WITSELF_TEST_CONCURRENT_ALIAS = $alias
        $env:WITSELF_TEST_CONCURRENT_COUNTER = $concurrentCounter
        if ((Invoke-Installer $installer $fixtureRoot $installDir 'v9.9.12' $true) -eq 0) {
            throw 'installer accepted a self-test that failed after a concurrent target edit'
        }
    } finally {
        $env:WITSELF_TEST_CONCURRENT_ALIAS = $previousConcurrentAlias
        $env:WITSELF_TEST_CONCURRENT_COUNTER = $previousConcurrentCounter
    }
    if ((Get-Content -LiteralPath $alias -Raw) -ne "concurrent user file`n") {
        throw 'rollback overwrote the concurrently changed ws.exe target'
    }
    if ((Get-FileHash -LiteralPath $primary -Algorithm SHA256).Hash -ne $primaryBefore) {
        throw 'concurrent-edit refusal did not restore the unchanged primary target'
    }
    $retainedAliasBackups = @(
        Get-ChildItem -LiteralPath $installDir -File |
            Where-Object { $_.Name -like '.ws.exe.backup.*' }
    )
    if ($retainedAliasBackups.Count -ne 1 -or
        (Get-FileHash -LiteralPath $retainedAliasBackups[0].FullName -Algorithm SHA256).Hash -ne $aliasBefore) {
        throw 'concurrent-edit refusal did not retain the prior ws.exe recovery backup'
    }

    if ((Get-UserPathEntryCount $installDir) -ne 1) {
        throw 'installer failure smokes changed the guarded user PATH entry despite -NoPathUpdate'
    }
    $rawPathAfterFailures = Get-TestRawUserPathState
    if ($rawPathAfterFailures.Kind -ne [Microsoft.Win32.RegistryValueKind]::ExpandString -or
        $rawPathAfterFailures.Value -ne $pathFixture.ExpectedRaw) {
        throw 'installer failure smokes changed the raw user PATH or registry value kind'
    }

    Restore-TestUserPathFixture $installDir $pathFixture
    $restoredPath = Get-TestRawUserPathState
    if ($restoredPath.Exists -ne $pathFixture.Original.Exists -or
        ($restoredPath.Exists -and
            ($restoredPath.Value -ne $pathFixture.Original.Value -or
                $restoredPath.Kind -ne $pathFixture.Original.Kind))) {
        throw 'selective installer smoke cleanup did not restore the original raw user PATH and kind'
    }
    $pathFixture = $null
    Write-Host 'Windows installer smoke passed.'
} finally {
    try {
        Restore-TestUserPathFixture $installDir $pathFixture
    } finally {
        if (Test-Path -LiteralPath $temporaryRoot) {
            Remove-Item -LiteralPath $temporaryRoot -Recurse -Force
        }
    }
}

# Expected negative installer cases leave LASTEXITCODE nonzero even after every
# assertion and cleanup succeeds. Set the script process result explicitly.
exit 0
