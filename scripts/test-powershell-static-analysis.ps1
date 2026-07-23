#requires -Version 5.1
[CmdletBinding()]
param(
    [switch]$AnalyzerChild,
    [string]$ModuleManifest,
    [string]$RepositoryRoot
)

Set-StrictMode -Version 2.0
$ErrorActionPreference = 'Stop'

$version = '1.25.0'
$expectedSha256 = '14e634c828eb98efb9f40b2918ba90f139ed5eccdf663a2a747736d996995d60'

if ($AnalyzerChild) {
    if ([string]::IsNullOrWhiteSpace($ModuleManifest) -or [string]::IsNullOrWhiteSpace($RepositoryRoot)) {
        throw 'Analyzer child requires the module manifest and repository root'
    }
    Import-Module -Name $ModuleManifest -Force
    $scripts = @(
        Join-Path $RepositoryRoot 'install.ps1'
        Join-Path $RepositoryRoot 'scripts\test-install-windows.ps1'
        Join-Path $RepositoryRoot 'scripts\test-powershell-static-analysis.ps1'
    )
    $findings = @(
        @(
            foreach ($script in $scripts) {
                Invoke-ScriptAnalyzer -Path $script -Severity @('Error', 'Warning')
            }
        ) | Where-Object {
            $_.RuleName -notin @(
                'PSAvoidUsingWriteHost',
                'PSUseApprovedVerbs',
                'PSUseShouldProcessForStateChangingFunctions'
            )
        }
    )
    if ($findings.Count -ne 0) {
        $findings | Format-Table -AutoSize | Out-String | Write-Error
        exit 1
    }
    Write-Host "Pinned PSScriptAnalyzer $version passed"
    exit 0
}

$temporaryRoot = Join-Path ([IO.Path]::GetTempPath()) ("witself-pssa-" + [Guid]::NewGuid().ToString('N'))
# Windows PowerShell 5.1's Expand-Archive accepts ZIP filenames only. The
# release package is a ZIP-format NuGet archive, so preserve its bytes under a
# compatible local extension before extraction.
$packagePath = Join-Path $temporaryRoot 'PSScriptAnalyzer.zip'
$moduleRoot = Join-Path $temporaryRoot 'module'

try {
    New-Item -ItemType Directory -Path $moduleRoot -Force | Out-Null
    $url = "https://github.com/PowerShell/PSScriptAnalyzer/releases/download/$version/PSScriptAnalyzer.$version.nupkg"
    Invoke-WebRequest -UseBasicParsing -Uri $url -OutFile $packagePath
    $actualSha256 = (Get-FileHash -LiteralPath $packagePath -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($actualSha256 -ne $expectedSha256) {
        throw "PSScriptAnalyzer package checksum mismatch: got $actualSha256"
    }
    Expand-Archive -LiteralPath $packagePath -DestinationPath $moduleRoot -Force
    $manifest = Join-Path $moduleRoot 'PSScriptAnalyzer.psd1'

    $repoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
    $powerShellExecutable = (Get-Process -Id $PID).Path
    if ([string]::IsNullOrWhiteSpace($powerShellExecutable)) {
        throw 'Could not locate the current PowerShell executable'
    }

    # Keep analyzer assemblies out of this process so Windows can remove the
    # temporary module tree after the child exits.
    $previousErrorActionPreference = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try {
        & $powerShellExecutable -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass `
            -File $PSCommandPath -AnalyzerChild -ModuleManifest $manifest -RepositoryRoot $repoRoot
        $analyzerExitCode = $LASTEXITCODE
    } finally {
        $ErrorActionPreference = $previousErrorActionPreference
    }
    if ($analyzerExitCode -ne 0) {
        throw "Pinned PSScriptAnalyzer exited with code $analyzerExitCode"
    }
} finally {
    if (Test-Path -LiteralPath $temporaryRoot) {
        [IO.Directory]::Delete($temporaryRoot, $true)
    }
}
