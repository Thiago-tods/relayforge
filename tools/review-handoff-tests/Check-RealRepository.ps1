<#
.SYNOPSIS
    Non-destructive validation of Prepare-ReviewHandoff.ps1 against the real repository.

.DESCRIPTION
    Runs the script with its default selection against the repository that contains it, writes the package to a directory
    OUTSIDE the repository (-OutputRoot), and then compares the repository state before and after: HEAD, branch, index
    file, working-tree status, the modified tracked files, the review artifacts in the repository root, the files of the
    root repository/ copy, and the existing review packages. Reports PASS or FAIL per comparison. Nothing is staged,
    committed or deleted.

.PARAMETER OutputRoot
    Where the package is written. Must be outside the repository. Defaults to a new directory under %TEMP%.

.PARAMETER OutputFile
    Also writes the result to this file.
#>
[CmdletBinding()]
param(
    [string]$OutputRoot = '',
    [string]$OutputFile = ''
)

$ErrorActionPreference = 'Stop'
$here = Split-Path -Parent $MyInvocation.MyCommand.Path
$script = Join-Path (Split-Path -Parent $here) 'Prepare-ReviewHandoff.ps1'
$repo = (& git -C $here rev-parse --show-toplevel).Trim().Replace('/', '\')
if ($OutputRoot -eq '') { $OutputRoot = Join-Path $env:TEMP ('rh-real-' + [guid]::NewGuid().ToString('N').Substring(0, 8)) }
if ($OutputRoot.TrimEnd('\').StartsWith($repo.TrimEnd('\') + '\', [System.StringComparison]::OrdinalIgnoreCase)) { throw 'OutputRoot must be outside the repository for this check.' }

function Get-State {
    Push-Location $repo
    try {
        $status = @(& git status --porcelain=v1 -uall | Where-Object { $_ -notmatch '^\?\? review-handoff/' })
        [pscustomobject]@{
            Head      = (& git rev-parse HEAD).Trim()
            Branch    = (& git branch --show-current).Trim()
            Index     = (Get-FileHash -LiteralPath (Join-Path $repo '.git\index')).Hash
            Status    = ($status -join "`n")
            Modified  = ((& git ls-files -m | ForEach-Object { $_ + ':' + (Get-FileHash -LiteralPath $_).Hash }) -join ',')
            Artifacts = ((Get-ChildItem -File -Force | Where-Object { $_.Name -match '^(phase|F1d|repository)' } | Sort-Object Name | ForEach-Object { $_.Name + ':' + (Get-FileHash -LiteralPath $_.FullName).Hash }) -join ',')
            RepoCopy  = ((Get-ChildItem -LiteralPath (Join-Path $repo 'repository') -Recurse -File -Force -ErrorAction SilentlyContinue | Sort-Object FullName | ForEach-Object { (Get-FileHash -LiteralPath $_.FullName).Hash }) -join '')
            Packages  = ((Get-ChildItem -LiteralPath (Join-Path $repo 'review-handoff') -Force -ErrorAction SilentlyContinue | ForEach-Object {
                        if ($_.PSIsContainer) { $_.Name + '/:' + ((Get-ChildItem -LiteralPath $_.FullName -Recurse -File | ForEach-Object { (Get-FileHash -LiteralPath $_.FullName).Hash }) -join '') }
                        else { $_.Name + ':' + (Get-FileHash -LiteralPath $_.FullName).Hash } }) -join '|')
        }
    } finally { Pop-Location }
}

$lines = New-Object System.Collections.Generic.List[string]
$lines.Add('Real-repository validation (non-destructive); the package is written outside the repository.')
$before = Get-State
$lines.Add("Branch $($before.Branch), HEAD $($before.Head)")
$lines.Add('')
$sw = [Diagnostics.Stopwatch]::StartNew()
$global:LASTEXITCODE = 0
$result = & $script -Name 'real-repository-check' -OutputRoot $OutputRoot -Summary 'Validation run against the real repository.' 6>$null 3>$null
$exit = $LASTEXITCODE
$secs = [Math]::Round($sw.Elapsed.TotalSeconds, 1)
$after = Get-State
$obj = $result | Where-Object { $_ -is [pscustomobject] -and $_.PSObject.Properties['PackagePath'] } | Select-Object -Last 1
$lines.Add("Script exit code: $exit, elapsed: $secs s")
if ($obj) {
    $lines.Add("Included files: $(@($obj.Included).Count)")
    $lines.Add('Left out:')
    foreach ($x in $obj.LeftOut) { $lines.Add("  $x") }
}
$lines.Add('')
$ok = $true
foreach ($p in 'Head', 'Branch', 'Index', 'Status', 'Modified', 'Artifacts', 'RepoCopy', 'Packages') {
    $same = ($before.$p -eq $after.$p)
    if (-not $same) { $ok = $false }
    $lines.Add(('{0,-5} {1,-10} unchanged before and after' -f $(if ($same) { 'PASS' } else { 'FAIL' }), $p))
}
$lines.Add(('{0,-5} {1}' -f $(if ($exit -eq 0 -and $obj) { 'PASS' } else { 'FAIL' }), 'the script finished with exit code 0 and produced a package'))
$lines | ForEach-Object { $_ }
if ($OutputFile -ne '') { [System.IO.File]::WriteAllLines($OutputFile, $lines, (New-Object System.Text.UTF8Encoding($false))) }
if ($ok -and $exit -eq 0) { exit 0 } else { exit 1 }
