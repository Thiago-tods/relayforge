<#
.SYNOPSIS
    Reproducible benchmarks for the content scanner of Prepare-ReviewHandoff.ps1.

.DESCRIPTION
    Reproduces the cases that made the first version of the scanner slow in the independent review: single long lines of
    letters (a quadratic pattern), a minified-style line, ordinary short lines, 600 ordinary files, and 400 files that
    each hold one 60,000-character line. Every case runs the script in a separate process against a temporary
    repository with synthetic content, with a time limit; a run that does not finish is killed and reported as such.

    Pass -OldScript with a copy of the first version to compare. Without it only the current script is measured.

.PARAMETER NewScript
    The script to measure. Defaults to ..\Prepare-ReviewHandoff.ps1.

.PARAMETER OldScript
    Optional copy of the first version of the script, for the side-by-side comparison.

.PARAMETER LimitSeconds
    Time limit per run (default 180).

.PARAMETER OutputFile
    Also writes the table to this file.
#>
[CmdletBinding()]
param(
    [string]$NewScript = '',
    [string]$OldScript = '',
    [int]$LimitSeconds = 180,
    [string]$OutputFile = ''
)

$ErrorActionPreference = 'Continue'
$here = Split-Path -Parent $MyInvocation.MyCommand.Path
if ($NewScript -eq '') { $NewScript = Join-Path (Split-Path -Parent $here) 'Prepare-ReviewHandoff.ps1' }
. (Join-Path $here 'Fixtures.ps1')
$work = Join-Path $env:TEMP ('rh-bench-' + [guid]::NewGuid().ToString('N').Substring(0, 8))
New-Item -ItemType Directory $work | Out-Null

function Measure-Run([string]$Script, [string]$Root, [string]$Name, [string[]]$Extra) {
    $psi = New-Object System.Diagnostics.ProcessStartInfo
    $psi.FileName = 'powershell.exe'
    $psi.Arguments = "-NoProfile -ExecutionPolicy Bypass -File `"$Script`" -Name $Name -RepoRoot `"$Root`" " + ($Extra -join ' ')
    $psi.UseShellExecute = $false; $psi.CreateNoWindow = $true; $psi.RedirectStandardOutput = $true; $psi.RedirectStandardError = $true
    $sw = [Diagnostics.Stopwatch]::StartNew()
    $p = [System.Diagnostics.Process]::Start($psi)
    $null = $p.StandardOutput.ReadToEndAsync(); $null = $p.StandardError.ReadToEndAsync()
    if (-not $p.WaitForExit($LimitSeconds * 1000)) { try { $p.Kill() } catch { }; $p.WaitForExit(); return "not finished in $LimitSeconds s (killed)" }
    return ('{0:N1} s (exit {1})' -f $sw.Elapsed.TotalSeconds, $p.ExitCode)
}

function New-Case([string]$Id, [scriptblock]$Fill, $Arg = $null) {
    $root = New-TestRepo (Join-Path $work $Id)
    Put-File $root 'a.txt' "base`n"
    Save-Commit $root base
    & $Fill $root $Arg
    return $root
}

$rows = New-Object System.Collections.Generic.List[object]
function Add-Row([string]$Case, [string]$Root, [string[]]$Extra) {
    $new = Measure-Run $NewScript $Root "new-$([guid]::NewGuid().ToString('N').Substring(0, 6))" $Extra
    $old = '-'
    if ($OldScript -ne '') { $old = Measure-Run $OldScript $Root "old-$([guid]::NewGuid().ToString('N').Substring(0, 6))" $Extra }
    $rows.Add([pscustomobject]@{ Case = $Case; 'First version' = $old; 'Current version' = $new })
}

foreach ($n in 5000, 10000, 20000, 40000) {
    $fill = { param($root, $len) Put-File $root 'src/blob.js' ('x' * $len) }
    $r = New-Case "letters-$n" $fill $n
    Add-Row "one line of $n letters" $r @('-Files', 'src/blob.js')
}
$r = New-Case 'minified' { param($root) Put-File $root 'src/blob.js' (('function f(a,b){return a+b}var c=[1,2,3];' * 1000).Substring(0, 40000)) }
Add-Row 'one minified-style line of 40000 characters' $r @('-Files', 'src/blob.js')
$r = New-Case 'lines' { param($root) Put-File $root 'src/blob.js' ((1..4000 | ForEach-Object { 'const value' + $_ + ' = compute(' + $_ + ');' }) -join "`n") }
Add-Row '4000 ordinary short lines' $r @('-Files', 'src/blob.js')
$r = New-Case 'many' { param($root) 1..600 | ForEach-Object { Put-File $root "src/m/f$_.ts" ((1..20 | ForEach-Object { "export const v$_ = $_" }) -join "`n") } }
Add-Row '600 ordinary files (default selection)' $r @()
$r = New-Case 'wide' { param($root) 1..400 | ForEach-Object { Put-File $root "src/w/f$_.ts" ('x' * 60000) } }
Add-Row '400 files with one 60000-letter line each (default selection)' $r @()

$lines = @('Benchmark of the content scanner (whole script run in a separate process, synthetic content)', '')
$lines += ($rows | Format-Table -AutoSize -Wrap | Out-String -Width 220).TrimEnd().Split("`n")
$lines += ''
$lines += "Work directory: $work"
$lines | ForEach-Object { $_ }
if ($OutputFile -ne '') { [System.IO.File]::WriteAllLines($OutputFile, $lines, (New-Object System.Text.UTF8Encoding($false))) }
