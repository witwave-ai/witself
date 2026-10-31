#requires -Version 7.0
[CmdletBinding()]
param()
Set-StrictMode -Version 2.0
$ErrorActionPreference = 'Stop'
$repositoryRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
. (Join-Path $repositoryRoot 'install.ps1') -VerifierOnly
Initialize-WSComponentVerifier
$vectorRoot = Join-Path $repositoryRoot 'internal/localcell/trust/testdata/vectors'
$keyRoot = Join-Path $repositoryRoot 'internal/localcell/trust/testdata/keys'
$index = Get-Content -LiteralPath (Join-Path $vectorRoot 'index.json') -Raw | ConvertFrom-Json
$closedReasons = @(
    'accepted','no_trust_root','document_too_large','envelope_too_large','invalid_json',
    'duplicate_key','profile_violation','unknown_critical','unknown_field','invalid_document',
    'wrong_kind','wrong_algorithm','invalid_key','invalid_encoding','signature_length',
    'signature_count','signature_invalid','unknown_key','revoked_key','wrong_signer',
    'state_invalid','state_io','sequence_invalid','replay','rollback_denied','wrong_tag',
    'wrong_platform','schema_invalid','postgresql_invalid','extensions_unsupported',
    'roles_invalid','path_invalid','inventory_invalid','mode_invalid','not_installed',
    'mixed_set','expired','not_yet_valid','unknown_clock'
)
# No B1 corpus cases need a reason exception. Unassigned Unicode code points
# are a conservative bootstrap subset; none occur in the frozen B1 corpus.
$subsetReasonExceptions = @{}
function Get-VectorOption {
    param($Case, [string]$Name, $Default)
    $property = $Case.PSObject.Properties[$Name]
    if ($null -eq $property) { return $Default }
    return $property.Value
}
$positive = 0
$negative = 0
$negativeSignature = 0
$watch = [Diagnostics.Stopwatch]::StartNew()
foreach ($case in $index.cases) {
    $roots = [string[]]@($case.roots | ForEach-Object { Join-Path $keyRoot "TEST-ONLY-$_.public.pem" })
    $verifier = New-Object WSB2Verifier -ArgumentList (,$roots)
    $now = [DateTime]::Parse((Get-VectorOption $case 'now' '2026-10-27T12:00:00Z')).ToUniversalTime()
    foreach ($setup in $case.setup) {
        try {
            $null = $verifier.Verify((Join-Path $vectorRoot $setup.document), (Join-Path $vectorRoot $setup.envelope), $setup.kind, $now, $true, '', '', '', $false)
        } catch { throw "PowerShell parity setup failed: $($case.name) $(Get-WSComponentRefusal $_)" }
    }
    $reason = 'accepted'
    try {
        $null = $verifier.Verify(
            (Join-Path $vectorRoot $case.document), (Join-Path $vectorRoot $case.envelope), $case.kind,
            $now, (-not [bool](Get-VectorOption $case 'untrusted_clock' $false)),
            [string](Get-VectorOption $case 'expected_tag' ''), [string](Get-VectorOption $case 'os' ''),
            [string](Get-VectorOption $case 'arch' ''), [bool](Get-VectorOption $case 'allow_rollback' $false))
    } catch { $reason = Get-WSComponentRefusal $_ }
    if ($closedReasons -cnotcontains $reason) { throw "PowerShell parity returned an open reason: $($case.name)" }
    if ($case.positive) {
        $positive++
        if ($reason -cne 'accepted') { throw "PowerShell parity refused positive case: $($case.name) $reason" }
    } else {
        $negative++
        if (Get-VectorOption $case 'negative_signature' $false) { $negativeSignature++ }
        if ($reason -ceq 'accepted') { throw "PowerShell parity accepted negative case: $($case.name)" }
        if ($reason -cne $case.expected_reason -and -not $subsetReasonExceptions.ContainsKey($case.name)) {
            throw "PowerShell parity reason mismatch: $($case.name) expected $($case.expected_reason) got $reason"
        }
    }
}
$watch.Stop()
Write-Host "PowerShell bootstrap parity: $($index.cases.Count) cases, $positive positive, $negative negative, $negativeSignature negative-signature; $($watch.Elapsed.TotalSeconds.ToString('F2'))s"
