#requires -Version 5.1
[CmdletBinding()]
param([switch]$Cleanup)

Set-StrictMode -Version 2.0
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

if ([string]::IsNullOrWhiteSpace($env:RUNNER_TEMP)) {
    throw 'PostgreSQL proof requires runner temp'
}
$proofRoot = Join-Path $env:RUNNER_TEMP 'lc-pg16'
if ($Cleanup) {
    if (Test-Path -LiteralPath $proofRoot) {
        Remove-Item -LiteralPath $proofRoot -Recurse -Force
    }
    if (Test-Path -LiteralPath $proofRoot) {
        throw 'PostgreSQL proof cleanup failed'
    }
    exit 0
}

$pin = Get-Content -LiteralPath (Join-Path $PSScriptRoot 'ci-postgres16-windows.json') -Raw | ConvertFrom-Json
if ($pin.major -ne 16 -or $pin.sha256 -cnotmatch '^[a-f0-9]{64}$' -or
    $pin.url -cnotmatch '^https://get\.enterprisedb\.com/postgresql/postgresql-16\.[0-9]+-[0-9]+-windows-x64-binaries\.zip$') {
    throw 'PostgreSQL proof pin is incomplete or invalid'
}
if (Test-Path -LiteralPath $proofRoot) {
    throw 'PostgreSQL proof temp tree already exists'
}
New-Item -ItemType Directory -Path $proofRoot | Out-Null
New-Item -ItemType Directory -Path (Join-Path $proofRoot 'testtmp') | Out-Null
$archivePath = Join-Path $proofRoot 'postgresql.zip'
Invoke-WebRequest -UseBasicParsing -Uri $pin.url -OutFile $archivePath
$actualDigest = (Get-FileHash -LiteralPath $archivePath -Algorithm SHA256).Hash.ToLowerInvariant()
if ($actualDigest -cne $pin.sha256) {
    throw 'PostgreSQL proof archive checksum mismatch'
}

# Preserve all runtime DLLs and the full bin/lib/share trees from the vendor
# archive. Do not install a service or use the runner's ambient PostgreSQL.
Add-Type -AssemblyName System.IO.Compression.FileSystem
$archive = [IO.Compression.ZipFile]::OpenRead($archivePath)
try {
    $rootPrefix = [IO.Path]::GetFullPath($proofRoot) + [IO.Path]::DirectorySeparatorChar
    foreach ($entry in $archive.Entries) {
        $relative = $entry.FullName.Replace('\', '/')
        if ($relative -cnotmatch '^pgsql/(bin|lib|share)/') { continue }
        $destination = [IO.Path]::GetFullPath((Join-Path $proofRoot $relative))
        if (-not $destination.StartsWith($rootPrefix, [StringComparison]::OrdinalIgnoreCase)) {
            throw 'PostgreSQL proof archive path is invalid'
        }
        if ($relative.EndsWith('/')) {
            New-Item -ItemType Directory -Path $destination -Force | Out-Null
        } else {
            New-Item -ItemType Directory -Path ([IO.Path]::GetDirectoryName($destination)) -Force | Out-Null
            [IO.Compression.ZipFileExtensions]::ExtractToFile($entry, $destination, $false)
        }
    }
} finally {
    $archive.Dispose()
}
foreach ($directory in @('bin', 'lib', 'share')) {
    if (-not (Test-Path -LiteralPath (Join-Path $proofRoot "pgsql/$directory") -PathType Container)) {
        throw 'PostgreSQL proof runtime tree is incomplete'
    }
}
foreach ($name in @('initdb', 'pg_ctl', 'postgres', 'psql', 'createdb')) {
    $executable = Join-Path $proofRoot "pgsql/bin/$name.exe"
    if (-not (Test-Path -LiteralPath $executable -PathType Leaf)) {
        throw 'PostgreSQL proof executable is missing'
    }
    $version = & $executable --version 2>&1 | Out-String
    if ($LASTEXITCODE -ne 0 -or $version -notmatch '\(PostgreSQL\) 16\.[0-9]+') {
        throw 'PostgreSQL proof executable did not report major sixteen'
    }
}
Write-Output 'Pinned PostgreSQL sixteen runtime verified'
