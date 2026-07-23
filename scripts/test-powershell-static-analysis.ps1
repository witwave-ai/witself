#requires -Version 5.1
[CmdletBinding()]
param()

Set-StrictMode -Version 2.0
$ErrorActionPreference = 'Stop'

$version = '1.25.0'
$expectedSha256 = '14e634c828eb98efb9f40b2918ba90f139ed5eccdf663a2a747736d996995d60'
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
    Import-Module -Name $manifest -Force

    $repoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
    $scripts = @(
        Join-Path $repoRoot 'install.ps1'
        Join-Path $repoRoot 'scripts\test-install-windows.ps1'
        Join-Path $repoRoot 'scripts\test-powershell-static-analysis.ps1'
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
} finally {
    if (Test-Path -LiteralPath $temporaryRoot) {
        [IO.Directory]::Delete($temporaryRoot, $true)
    }
}
