<#
.SYNOPSIS
    Regression harness for tools/Prepare-ReviewHandoff.ps1.

.DESCRIPTION
    Runs the handoff script against isolated temporary repositories with synthetic fixtures only. The real repository
    is never written. Every test prints PASS, FAIL, INFO or NOT-RUN with an id. The ids that start with H1, H2, M1 to
    M5 and L1 to L8 correspond to the findings of the independent review (see tools/ReviewHandoff-SECURITY.md); each of
    those tests reproduces the original failure and shows that it is now prevented. Privilege-dependent tests (symbolic
    links) report NOT-RUN when they cannot run.

    Exit code: 0 when no test failed, 1 otherwise.

.PARAMETER Script
    The script under test. Defaults to ..\Prepare-ReviewHandoff.ps1.

.PARAMETER WorkRoot
    Parent directory for the temporary repositories. Defaults to a new directory under %TEMP%. Nothing is deleted.

.PARAMETER OutputFile
    Also writes the result lines to this file.
#>
[CmdletBinding()]
param(
    [string]$Script = '',
    [string]$WorkRoot = '',
    [string]$OutputFile = '',
    [string]$OldScript = '',
    [string]$PreviousScript = ''
)

$ErrorActionPreference = 'Continue'
$here = Split-Path -Parent $MyInvocation.MyCommand.Path
if ($Script -eq '') { $Script = Join-Path (Split-Path -Parent $here) 'Prepare-ReviewHandoff.ps1' }
. (Join-Path $here 'Fixtures.ps1')
if ($WorkRoot -eq '') { $WorkRoot = Join-Path $env:TEMP ('rh-tests-' + [guid]::NewGuid().ToString('N').Substring(0, 8)) }
New-Item -ItemType Directory -Force $WorkRoot | Out-Null
$utf8 = New-Object System.Text.UTF8Encoding($false)

$results = New-Object System.Collections.Generic.List[string]
$counts = @{ PASS = 0; FAIL = 0; INFO = 0; 'NOT-RUN' = 0 }
function Rec([string]$Id, [string]$Status, [string]$Text) {
    $results.Add(("{0,-9} {1,-7} {2}" -f $Id, $Status, $Text))
    $counts[$Status]++
}
function Check([string]$Id, [bool]$Ok, [string]$Text) { if ($Ok) { Rec $Id 'PASS' $Text } else { Rec $Id 'FAIL' $Text } }
function Short([string]$s, [int]$n = 100) { if ($s.Length -gt $n) { return $s.Substring(0, $n) } else { return $s } }

function Invoke-Handoff([string]$Root, [hashtable]$P) {
    $global:LASTEXITCODE = 0
    try {
        $o = & $Script -RepoRoot $Root @P 6>$null 3>$null
        $code = $LASTEXITCODE
        $obj = $o | Where-Object { $_ -is [pscustomobject] -and $_.PSObject.Properties['PackagePath'] } | Select-Object -Last 1
        return [pscustomobject]@{ Ok = $true; Out = $obj; Err = ''; Exit = $code }
    } catch {
        return [pscustomobject]@{ Ok = $false; Out = $null; Err = $_.Exception.Message; Exit = 1 }
    }
}
function Included($r, [string]$p) { return ($r.Ok -and $r.Out -and (@($r.Out.Included) -contains $p)) }
function LeftOutText($r) { if ($r.Ok -and $r.Out) { return (@($r.Out.LeftOut) -join "`n") } else { return '' } }
function Read-Pkg($r, [string]$rel) { return [System.IO.File]::ReadAllText((Join-Path $r.Out.PackagePath $rel)) }

# True when none of the markers occurs anywhere in the package directory or inside its ZIP (every entry is decoded).
function Test-NoMarker($r, [string[]]$Markers) {
    $hits = New-Object System.Collections.Generic.List[string]
    foreach ($f in Get-ChildItem -LiteralPath $r.Out.PackagePath -Recurse -File) {
        $t = [System.IO.File]::ReadAllText($f.FullName)
        foreach ($m in $Markers) { if ($t.Contains($m)) { $hits.Add("dir:$($f.Name)") } }
    }
    Add-Type -AssemblyName System.IO.Compression.FileSystem
    $z = [System.IO.Compression.ZipFile]::OpenRead($r.Out.ZipPath)
    foreach ($e in $z.Entries) {
        $sr = New-Object System.IO.StreamReader($e.Open(), $utf8)
        $t = $sr.ReadToEnd(); $sr.Dispose()
        foreach ($m in $Markers) { if ($t.Contains($m)) { $hits.Add("zip:$($e.FullName)") } }
    }
    $z.Dispose()
    return ($hits.Count -eq 0)
}

function Get-RepoSnapshot([string]$Root) {
    $h = (Get-ChildItem -LiteralPath $Root -Recurse -File -Force | Where-Object { $_.FullName -notmatch '\\\.git\\' -and $_.FullName -notmatch '\\review-handoff\\' } |
            Sort-Object FullName | ForEach-Object { $_.FullName.Substring($Root.Length) + ':' + (Get-FileHash -LiteralPath $_.FullName).Hash }) -join "`n"
    [pscustomobject]@{
        Head = (& git -C $Root rev-parse HEAD 2>&1) -join ''
        Status = (& git -C $Root status --porcelain=v1 -uall 2>&1 | Where-Object { $_ -notmatch 'review-handoff' }) -join "`n"
        Index = (Get-FileHash -LiteralPath (Join-Path $Root '.git\index')).Hash
        Staged = (& git -C $Root diff --cached --name-status 2>&1) -join "`n"
        Refs = (& git -C $Root show-ref 2>&1) -join "`n"
        Stash = (& git -C $Root stash list 2>&1) -join "`n"
        Tree = $h
    }
}

function Start-Child([string]$Root, [string[]]$ExtraArgs) {
    $psi = New-Object System.Diagnostics.ProcessStartInfo
    $psi.FileName = 'powershell.exe'
    $psi.Arguments = "-NoProfile -ExecutionPolicy Bypass -File `"$Script`" -RepoRoot `"$Root`" " + ($ExtraArgs -join ' ')
    $psi.UseShellExecute = $false; $psi.CreateNoWindow = $true; $psi.RedirectStandardOutput = $true; $psi.RedirectStandardError = $true
    $p = [System.Diagnostics.Process]::Start($psi)
    $null = $p.StandardOutput.ReadToEndAsync(); $null = $p.StandardError.ReadToEndAsync()
    return $p
}

$E9 = [string][char]0xE9
$JA = ([string][char]0x65E5) + ([string][char]0x672C)

Rec 'ENV' 'INFO' ("PowerShell $($PSVersionTable.PSVersion); git $((& git --version) -replace 'git version ',''); work root is a temporary directory")

# =================================================================================================================
# G: general safety (nothing is changed, patches are exact, ZIP equals directory, manifest, suffixes)
# =================================================================================================================
$g = New-TestRepo (Join-Path $WorkRoot 'g')
foreach ($n in 'src/ren-old.ts', 'src/mode.ts', 'src/del.ts', 'src/staged-del.ts', 'src/spaced name.ts', 'src/keep.ts', 'src/partial.ts') { Put-File $g $n "base of $n`n" }
Save-Commit $g base
$gBase = (& git -C $g rev-parse HEAD).Trim()
& git -C $g mv src/ren-old.ts src/ren-new.ts | Out-Null
Put-File $g 'src/ren-new.ts' "base of src/ren-old.ts`nedited after rename`n"
Remove-Item (Join-Path $g 'src\del.ts')
& git -C $g rm -q src/staged-del.ts | Out-Null
Put-File $g 'src/spaced name.ts' "changed`n"
Put-File $g 'src/partial.ts' "staged part`n"
& git -C $g add src/partial.ts | Out-Null
Put-File $g 'src/partial.ts' "staged part`nunstaged part`n"
Put-File $g 'src/new.ts' "brand new`n"
Put-File $g 'src/new dir/with space.ts' "new in spaced dir`n"
Put-File $g 'src/empty.ts' ''
[System.IO.File]::WriteAllText((Join-Path $g 'src\nonewline.ts'), 'no newline at the end', $utf8)
[System.IO.File]::WriteAllText((Join-Path $g 'src\crlf.ts'), "line1`r`nline2`r`n", $utf8)
& git -C $g update-index --chmod=+x src/mode.ts | Out-Null
Put-File $g '.env' "KEEP_OUT=$($script:VAL)"
$gBefore = Get-RepoSnapshot $g
$gr = Invoke-Handoff $g @{ Name = 'g1'; Summary = 'general'; Scope = 'scope text' }
$gAfter = Get-RepoSnapshot $g
Check 'G-01' ($gr.Ok -and $gr.Exit -eq 0) "run succeeds (ok=$($gr.Ok), exit=$($gr.Exit), error='$(Short $gr.Err 80)')"
Check 'G-02' ($gBefore.Head -eq $gAfter.Head -and $gBefore.Status -eq $gAfter.Status -and $gBefore.Index -eq $gAfter.Index -and $gBefore.Staged -eq $gAfter.Staged) 'HEAD, git status, index file and staged set are unchanged'
Check 'G-03' (($gBefore.Refs -eq $gAfter.Refs) -and ($gBefore.Stash -eq $gAfter.Stash) -and ($gBefore.Tree -eq $gAfter.Tree)) 'refs, stash and every working-tree file are byte-identical'
foreach ($want in 'src/ren-new.ts', 'src/spaced name.ts', 'src/partial.ts', 'src/new.ts', 'src/new dir/with space.ts', 'src/empty.ts', 'src/nonewline.ts', 'src/crlf.ts', 'src/mode.ts') {
    Check 'G-04' (Included $gr $want) "included: $want"
}
Check 'G-05' (-not (Included $gr '.env')) '.env is not included'
$patch = [System.IO.File]::ReadAllText((Join-Path $gr.Out.PackagePath 'changes.patch'))
Check 'G-06' ($patch.Contains('deleted file mode') -and $patch.Contains('src/del.ts') -and $patch.Contains('src/staged-del.ts')) 'staged and unstaged deletions are in the patch'
Check 'G-07' ($patch.Contains('old mode') -or $patch.Contains('new mode')) 'mode-only change is in the patch'
$clone = Join-Path $WorkRoot 'g-clone'
& git -c core.autocrlf=false clone -q $g $clone 2>&1 | Out-Null
& git -C $clone -c core.autocrlf=false checkout -q $gBase 2>&1 | Out-Null
$null = & git -C $clone -c core.autocrlf=false apply --whitespace=nowarn (Join-Path $gr.Out.PackagePath 'changes.patch') 2>&1
Check 'G-08' ($LASTEXITCODE -eq 0) "changes.patch applies to a clean checkout of the base (exit $LASTEXITCODE)"
$bad = @()
foreach ($f in @(& git -C $g ls-files) + @(& git -C $g ls-files --others --exclude-standard)) {
    if ($f -like 'review-handoff/*' -or $f -eq '.env') { continue }
    $a = Join-Path $g $f; $b = Join-Path $clone $f
    if ((Test-Path -LiteralPath $a) -ne (Test-Path -LiteralPath $b)) { $bad += "$f (existence)" }
    elseif ((Test-Path -LiteralPath $a) -and ((Get-FileHash -LiteralPath $a).Hash -ne (Get-FileHash -LiteralPath $b).Hash)) { $bad += "$f (content)" }
}
Check 'G-09' ($bad.Count -eq 0) "the patched clone equals the working tree byte for byte, including new, empty, CRLF and no-newline files ($($bad -join ', '))"
Add-Type -AssemblyName System.IO.Compression.FileSystem
$zip = [System.IO.Compression.ZipFile]::OpenRead($gr.Out.ZipPath)
$zm = @{}
foreach ($e in $zip.Entries) { $s = $e.Open(); $m = New-Object IO.MemoryStream; $s.CopyTo($m); $s.Close(); $zm[$e.FullName] = [BitConverter]::ToString([Security.Cryptography.SHA256]::Create().ComputeHash($m.ToArray())) }
$zip.Dispose()
$dm = @{}
$pn = Split-Path -Leaf $gr.Out.PackagePath
foreach ($f in Get-ChildItem -LiteralPath $gr.Out.PackagePath -Recurse -File) { $dm["$pn/" + $f.FullName.Substring($gr.Out.PackagePath.Length + 1).Replace('\', '/')] = [BitConverter]::ToString([Security.Cryptography.SHA256]::Create().ComputeHash([IO.File]::ReadAllBytes($f.FullName))) }
Check 'G-10' ((($zm.Keys | Sort-Object) -join '|') -eq (($dm.Keys | Sort-Object) -join '|') -and -not ($dm.Keys | Where-Object { $zm[$_] -ne $dm[$_] })) 'ZIP and package directory hold the same files with the same bytes'
Check 'G-11' (-not ($zm.Keys | Where-Object { $_.Contains('\') -or $_ -match '(^|/)\.\.(/|$)' -or $_ -match '^[A-Za-z]:' })) 'ZIP entry names use forward slashes and have no absolute or parent-relative entries'
$mok = $true
foreach ($line in [IO.File]::ReadAllLines((Join-Path $gr.Out.PackagePath 'MANIFEST.txt'))) {
    if ($line -match '^([0-9a-f]{64})  (.+?)  \(') { if ((Get-FileHash -LiteralPath (Join-Path $gr.Out.PackagePath $Matches[2]) -Algorithm SHA256).Hash.ToLowerInvariant() -ne $Matches[1]) { $mok = $false } }
}
Check 'G-12' $mok 'MANIFEST.txt hashes match the package files'
$first = (Get-FileHash (Join-Path $gr.Out.PackagePath 'REVIEW.md')).Hash
$gr2 = Invoke-Handoff $g @{ Name = 'g1' }
Check 'G-13' ($gr2.Ok -and (Split-Path -Leaf $gr2.Out.PackagePath) -eq 'g1-2' -and (Get-FileHash (Join-Path $gr.Out.PackagePath 'REVIEW.md')).Hash -eq $first) 'L1/existing package: a second run with the same name gets a suffix and the first package is untouched'
$gr3 = Invoke-Handoff $g @{ Name = 'g3'; Files = @('src/new.ts') }
Check 'G-14' ($gr3.Ok -and (@($gr3.Out.Included) -join ',') -eq 'src/new.ts') '-Files includes only the selection'
$leftHidden = @(Get-ChildItem (Join-Path $g 'review-handoff') -Force | Where-Object { $_.Name -like '.incomplete-*' })
Check 'G-15' ($leftHidden.Count -eq 0) 'no staging leftovers after successful runs'

# BaseRef
$b = New-TestRepo (Join-Path $WorkRoot 'base')
Put-File $b 'src/a.ts' "a1`n"; Put-File $b 'src/b.ts' "b1`n"; Put-File $b 'src/gone.ts' "gone`n"; Put-File $b 'src/mv.ts' "mv`n"
Save-Commit $b c1
$c1 = (& git -C $b rev-parse HEAD).Trim()
Put-File $b 'src/a.ts' "a2`n"; Remove-Item (Join-Path $b 'src\gone.ts'); & git -C $b mv src/mv.ts src/mv2.ts | Out-Null; Put-File $b 'src/added.ts' "added`n"
Save-Commit $b c2
Put-File $b 'src/b.ts' "b2 uncommitted`n"; Put-File $b 'src/untracked.ts' "untracked`n"
foreach ($ref in $c1, 'HEAD~1') {
    $tag = ($ref -replace '[^A-Za-z0-9]', ''); if ($tag.Length -gt 8) { $tag = $tag.Substring(0, 8) }
    $r = Invoke-Handoff $b @{ Name = "base-$tag"; BaseRef = $ref }
    $okApply = $false; $diffs = @()
    if ($r.Ok) {
        $cl = Join-Path $WorkRoot "base-clone-$tag"
        & git -c core.autocrlf=false clone -q $b $cl 2>&1 | Out-Null
        & git -C $cl -c core.autocrlf=false checkout -q $c1 2>&1 | Out-Null
        $null = & git -C $cl -c core.autocrlf=false apply (Join-Path $r.Out.PackagePath 'changes.patch') 2>&1
        $okApply = ($LASTEXITCODE -eq 0)
        foreach ($f in 'src/a.ts', 'src/b.ts', 'src/added.ts', 'src/mv2.ts', 'src/untracked.ts') { if (-not (Test-Path (Join-Path $cl $f)) -or (Get-FileHash (Join-Path $cl $f)).Hash -ne (Get-FileHash (Join-Path $b $f)).Hash) { $diffs += $f } }
        foreach ($f in 'src/gone.ts', 'src/mv.ts') { if (Test-Path (Join-Path $cl $f)) { $diffs += "$f present" } }
    }
    Check 'G-16' ($okApply -and $diffs.Count -eq 0) "-BaseRef $(Short $ref 10): patch applies and reproduces committed, uncommitted and untracked changes ($($diffs -join ', '))"
}
$r = Invoke-Handoff $b @{ Name = 'base-bad'; BaseRef = 'no-such-rev' }
Check 'G-17' ((-not $r.Ok) -and -not (Test-Path (Join-Path $b 'review-handoff\base-bad'))) 'an unknown -BaseRef is an error and leaves no package'

# =================================================================================================================
# H1: secret detection (every credential format of the review is now blocked)
# =================================================================================================================
$h = New-TestRepo (Join-Path $WorkRoot 'h1')
Put-File $h 'a.txt' "base`n"
Save-Commit $h base
$blocked = Get-BlockedCases
$allowed = Get-AllowedCases
foreach ($k in $blocked.Keys) { Put-File $h $k $blocked[$k] }
[System.IO.File]::WriteAllText((Join-Path $h 'src\utf8bom.ts'), "const $($script:KW) = '$($script:VAL)'", (New-Object System.Text.UTF8Encoding($true)))
[System.IO.File]::WriteAllText((Join-Path $h 'src\utf16bom.ts'), "const $($script:KW) = '$($script:VAL)'", [System.Text.Encoding]::Unicode)
[System.IO.File]::WriteAllText((Join-Path $h 'src\utf16nobom.ts'), "const $($script:KW) = '$($script:VAL)'", (New-Object System.Text.UnicodeEncoding($false, $false)))
[System.IO.File]::WriteAllBytes((Join-Path $h 'src\latin1.ts'), [System.Text.Encoding]::GetEncoding(28591).GetBytes("// caf" + [char]0xE9 + "`nconst $($script:KW) = '$($script:VAL)'"))
$extra = @('src/utf8bom.ts', 'src/utf16bom.ts', 'src/utf16nobom.ts', 'src/latin1.ts')
foreach ($k in $allowed.Keys) { Put-File $h $k $allowed[$k] }
$knownGaps = Get-KnownGaps
foreach ($k in $knownGaps.Keys) { Put-File $h $k $knownGaps[$k] }

# default selection (no -Files): the whole working tree
$hr = Invoke-Handoff $h @{ Name = 'h1-default' }
Check 'H1-00' ($hr.Ok -and $hr.Exit -eq 0) "default run succeeds ($(Short $hr.Err 80))"
foreach ($k in @($blocked.Keys) + $extra) {
    Check 'H1-01' (-not (Included $hr $k)) "[v1 $(if ($blocked.Contains($k) -and $k -match '\.ts$|\.cs$') { 'checked' } else { 'finding' })] blocked in the default selection: $k"
}
foreach ($k in $allowed.Keys) { Check 'H1-02' (Included $hr $k) "legitimate content is not hidden: $k" }
foreach ($k in $knownGaps.Keys) { Rec 'H1-03' 'INFO' "known limitation, not detected by design (documented): $k included=$(Included $hr $k)" }
$left = LeftOutText $hr
Check 'H1-04' ($left -notmatch [regex]::Escape($script:VAL) -and $left -notmatch 'hunter') 'the left-out list names path, rule and line but never a secret value'
Check 'H1-05' (Test-NoMarker $hr @($script:VAL, 'hunter22', 'Zx9fQw', $script:AWS, $script:GH, $script:SLACK, $script:NPMTOKEN, $script:JWT, 'MIIEvQIBADANBgkq')) 'no secret value appears anywhere in the package directory or the ZIP'

# explicit -Files cannot bypass any check
$explicit = @($blocked.Keys) + $extra + @($allowed.Keys)
$hx = Invoke-Handoff $h @{ Name = 'h1-explicit'; Files = $explicit }
$bypass = @(@($blocked.Keys) + $extra | Where-Object { Included $hx $_ })
Check 'H1-06' ($hx.Ok -and $bypass.Count -eq 0) "explicit -Files cannot bypass the checks (bypassed: $($bypass -join ', '))"
Check 'H1-07' ($hx.Ok -and (Included $hx 'internal/auth/credentials.go')) 'explicit -Files still includes legitimate source'
$hd = Invoke-Handoff $h @{ Name = 'h1-dir'; Files = @('src', 'keys', '.') }
Check 'H1-08' ($hd.Ok -and -not (@(@($blocked.Keys) + $extra | Where-Object { Included $hd $_ }).Count)) 'explicit directories cannot bypass the checks either'

# =================================================================================================================
# H2: secrets in the patch (removed lines, context lines, deleted files)
# =================================================================================================================
$d = New-TestRepo (Join-Path $WorkRoot 'h2')
$old = 'REMOVED' + 'Zq8Lm4' + 'Xt2Rn7'
Put-File $d 'src/cfg.ts' "const $($script:KW) = '$old'`nexport const k = 1`n"
Put-File $d '.env.production' "REMOVED_ENV=$old`n"
Put-File $d 'src/old-creds.ts' "const $($script:KW) = '$old'`n"
Put-File $d 'src/ctx.ts' "line1`nline2`nconst $($script:KW) = '$old'`nline4`nline5`nline6`nline7`nline8`n"
Put-File $d 'src/clean.ts' "export const clean = 1`n"
Save-Commit $d base
Put-File $d 'src/cfg.ts' "const $($script:KW) = process.env.APP_KEY`nexport const k = 2`n"      # the secret is REMOVED
Remove-Item (Join-Path $d '.env.production')                                                         # a credential file is DELETED
Remove-Item (Join-Path $d 'src\old-creds.ts')                                                        # deleted, name looks harmless
Put-File $d 'src/ctx.ts' "line1`nCHANGED`nconst $($script:KW) = '$old'`nline4`nline5`nline6`nline7`nline8`n"   # secret only in context lines
Put-File $d 'src/clean.ts' "export const clean = 2`n"
$dr = Invoke-Handoff $d @{ Name = 'h2' }
Check 'H2-00' ($dr.Ok -and $dr.Exit -eq 0) "run succeeds ($(Short $dr.Err 80))"
Check 'H2-01' ($dr.Ok -and (Test-NoMarker $dr @($old))) '[v1 leaked] a secret removed by the change, a deleted credential file, a deleted harmless-named file with a secret, and a secret in context lines appear nowhere in the package or ZIP'
$pt = Read-Pkg $dr 'changes.patch'
Check 'H2-02' ($pt.Contains('src/clean.ts') -and -not $pt.Contains('src/cfg.ts')) 'clean changes stay in the patch and the unsafe diffs are omitted'
$lo = LeftOutText $dr
Check 'H2-03' ($lo -match '\.env\.production' -and $lo -match 'src/old-creds\.ts' -and $lo -match 'src/cfg\.ts' -and $lo -match 'src/ctx\.ts') 'every omitted diff is reported with its path and reason'
Check 'H2-04' ((Included $dr 'src/cfg.ts') -and (Read-Pkg $dr 'files\src\cfg.ts').Contains('process.env.APP_KEY')) 'the current (clean) file is still offered in files/ when only its diff was unsafe'
Check 'H2-05' ((Read-Pkg $dr 'REVIEW.md') -match 'Security scan') 'REVIEW.md contains the security scan section'

# =================================================================================================================
# M1: scanner performance
# =================================================================================================================
$perf = New-TestRepo (Join-Path $WorkRoot 'perf')
Put-File $perf 'a.txt' "base`n"
Save-Commit $perf base
Put-File $perf 'src/line40k.js' ('x' * 40000)
$sw = [Diagnostics.Stopwatch]::StartNew()
$pr = Invoke-Handoff $perf @{ Name = 'perf1'; Files = @('src/line40k.js') }
$t40 = [Math]::Round($sw.Elapsed.TotalSeconds, 1)
Check 'M1-01' ($pr.Ok -and $t40 -lt 10) "[v1: 32.8 s] one 40,000-character line of letters is handled in $t40 s"
Put-File $perf 'src/line200k.js' ('x' * 200000)
$sw.Restart(); $pr = Invoke-Handoff $perf @{ Name = 'perf2'; Files = @('src/line200k.js') }; $t200 = [Math]::Round($sw.Elapsed.TotalSeconds, 1)
Check 'M1-02' ($pr.Ok -and $t200 -lt 20) "a 200,000-character line is handled in $t200 s (linear, not quadratic)"
Put-File $perf 'src/big.txt' ('a' * 3000000)
$pr = Invoke-Handoff $perf @{ Name = 'perf3'; Files = @('src/big.txt') }
Check 'M1-03' ($pr.Ok -and -not (Included $pr 'src/big.txt') -and (LeftOutText $pr) -match 'larger than') 'a file above the size limit is left out with a reason'
Rec 'M1-04' 'INFO' 'the 400-file and 600-file benchmarks of the review are in Measure-ReviewHandoff.ps1 (results are documented in tools/ReviewHandoff-SECURITY.md)'

# =================================================================================================================
# M2/L6: junctions, symbolic links, traversal, outside-root evidence
# =================================================================================================================
$j = New-TestRepo (Join-Path $WorkRoot 'j\a\b\repo')
$jBase = Join-Path $WorkRoot 'j'
Put-File $j 'a.txt' "base`n"
Save-Commit $j base
[System.IO.File]::WriteAllText((Join-Path $jBase 'outside.txt'), 'OUTSIDE-SENTINEL')
[System.IO.File]::WriteAllText((Join-Path $jBase 'victim.txt'), 'ATTACKER-CONTENT')
Put-File $j 'victim.txt' 'VICTIM-ORIGINAL'
$r = Invoke-Handoff $j @{ Name = 'j-trav'; Files = @('../../../outside.txt') }
Check 'L6-01' ($r.Ok -and -not (Included $r '../../../outside.txt') -and (LeftOutText $r) -match 'path traversal') '[v1: relied on a git error] path traversal is rejected explicitly with a clear reason and the run continues'
$r = Invoke-Handoff $j @{ Name = 'j-trav2'; Files = @('src/../../../outside.txt', './../outside.txt') }
Check 'L6-02' ($r.Ok -and (@($r.Out.Included).Count -eq 0)) 'traversal hidden behind other segments is rejected too'
$r = Invoke-Handoff $j @{ Name = 'j-abs'; Files = @('C:\Windows\win.ini', "$jBase\outside.txt") }
Check 'L6-03' ($r.Ok -and (@($r.Out.Included).Count -eq 0) -and (LeftOutText $r) -match 'absolute paths are not allowed') 'absolute paths in -Files are rejected explicitly (no silent skip)'
Check 'L6-04' ((Get-Content (Join-Path $j 'victim.txt') -Raw) -eq 'VICTIM-ORIGINAL') 'no file inside the repository was overwritten'
$r = Invoke-Handoff $j @{ Name = 'j-ev1'; Evidence = @((Join-Path $jBase 'outside.txt')) }
Check 'M2-01' ($r.Ok -and -not (Test-Path (Join-Path $r.Out.PackagePath 'evidence')) -and (LeftOutText $r) -match 'outside the repository') '[v1: copied silently] -Evidence outside the repository is not read without -AllowedRoot'
$r = Invoke-Handoff $j @{ Name = 'j-ev2'; Evidence = @((Join-Path $jBase 'outside.txt')); AllowedRoot = @($jBase) }
Check 'M2-02' ($r.Ok -and (Test-Path (Join-Path $r.Out.PackagePath 'evidence\outside.txt'))) 'the same file is read when its directory is named with -AllowedRoot'
$lt = Join-Path $WorkRoot 'jl-target'
New-Item -ItemType Directory $lt | Out-Null
[System.IO.File]::WriteAllText((Join-Path $lt 'leak.txt'), 'LINKED-OUTSIDE-SENTINEL')
$jl = Join-Path $j 'jlink'
cmd /c "mklink /J ""$jl"" ""$lt""" 2>&1 | Out-Null
if (Test-Path $jl) {
    $r = Invoke-Handoff $j @{ Name = 'j-junction' }
    Check 'M2-03' ($r.Ok -and -not (@($r.Out.Included) | Where-Object { $_ -like 'jlink*' }) -and (Test-NoMarker $r @('LINKED-OUTSIDE-SENTINEL'))) '[v1: followed] files behind an untracked junction are not copied (default selection)'
    Check 'M2-04' ($r.Ok -and (LeftOutText $r) -match 'jlink') 'the junction is reported, not silently skipped'
    $r = Invoke-Handoff $j @{ Name = 'j-junction2'; Files = @('jlink', 'jlink/leak.txt') }
    Check 'M2-05' ($r.Ok -and (@($r.Out.Included).Count -eq 0)) 'explicitly naming the junction or a file behind it does not follow it'
    $r = Invoke-Handoff $j @{ Name = 'j-junction3'; Evidence = @((Join-Path $jl 'leak.txt')) }
    Check 'M2-06' ($r.Ok -and -not (Test-Path (Join-Path $r.Out.PackagePath 'evidence'))) '-Evidence through a junction is rejected'
    $r = Invoke-Handoff $j @{ Name = 'j-junction4'; Evidence = @((Join-Path $jl 'leak.txt')); AllowedRoot = @($WorkRoot) }
    Check 'M2-07' ($r.Ok -and -not (Test-Path (Join-Path $r.Out.PackagePath 'evidence')) -and (LeftOutText $r) -match 'reparse') '-Evidence through a junction is rejected even inside an allowed root'
} else { Rec 'M2-03' 'NOT-RUN' 'could not create a junction' }
$sl = Join-Path $j 'slink.txt'
$symOk = $false
try { New-Item -ItemType SymbolicLink -Path $sl -Target (Join-Path $jBase 'outside.txt') -ErrorAction Stop | Out-Null; $symOk = $true } catch { }
if ($symOk) {
    $r = Invoke-Handoff $j @{ Name = 'j-symlink' }
    Check 'M2-08' ($r.Ok -and -not (Included $r 'slink.txt') -and (Test-NoMarker $r @('OUTSIDE-SENTINEL'))) 'a symbolic link to an outside file is not followed'
} else { Rec 'M2-08' 'NOT-RUN' 'symbolic links need a privilege or Developer Mode; creating one failed (privilege-dependent test)' }

# =================================================================================================================
# M3/L2: Unicode file names
# =================================================================================================================
# -----------------------------------------------------------------------------------------------------------------
# M2b: the -AllowedRoot boundary itself (a junction or symbolic link AS the allowed root, or above it)
# -----------------------------------------------------------------------------------------------------------------
$ar = Join-Path $WorkRoot 'ar'
$arReal = Join-Path $ar 'real'
$arTarget = Join-Path $ar 'outside-target'
$arLink = Join-Path $ar 'root-link'
New-Item -ItemType Directory -Force $arReal, $arTarget, (Join-Path $arTarget 'sub') | Out-Null
$arMarker = 'ALLOWEDROOT-OUTSIDE-SENTINEL'
[System.IO.File]::WriteAllText((Join-Path $arReal 'ok.txt'), 'ordinary allowed file text')
[System.IO.File]::WriteAllText((Join-Path $arTarget 'leak.txt'), $arMarker)
[System.IO.File]::WriteAllText((Join-Path $arTarget 'sub\leak2.txt'), $arMarker)
$ap = New-TestRepo (Join-Path $WorkRoot 'ar-repo')
Put-File $ap 'a.txt' "base`n"
Save-Commit $ap base
Put-File $ap 'src/x.ts' "x`n"
Put-File $ap 'notes/inside.txt' "evidence inside the repository`n"
$r = Invoke-Handoff $ap @{ Name = 'ar-ok'; Evidence = @((Join-Path $arReal 'ok.txt')); TestLogs = @((Join-Path $arReal 'ok.txt')); AllowedRoot = @($arReal) }
Check 'M2b-01' ($r.Ok -and (Test-Path (Join-Path $r.Out.PackagePath 'evidence\ok.txt')) -and ((Read-Pkg $r 'tests.log') -match 'ordinary allowed file text')) 'an ordinary, explicitly authorized directory is accepted for -Evidence and -TestLogs'
$r = Invoke-Handoff $ap @{ Name = 'ar-ok2'; Evidence = @('notes/inside.txt') }
Check 'M2b-02' ($r.Ok -and (Test-Path (Join-Path $r.Out.PackagePath 'evidence\inside.txt'))) 'evidence inside the repository is accepted (the repository root is not rejected)'
cmd /c "mklink /J ""$arLink"" ""$arTarget""" 2>&1 | Out-Null
if (Test-Path $arLink) {
    $r = Invoke-Handoff $ap @{ Name = 'ar-root-ev'; Evidence = @((Join-Path $arLink 'leak.txt')); AllowedRoot = @($arLink) }
    Check 'M2b-03' ($r.Ok -and -not (Test-Path (Join-Path $r.Out.PackagePath 'evidence')) -and (LeftOutText $r) -match 'allowed root') '[previous version read it] -Evidence through an -AllowedRoot that IS a junction to an outside directory is rejected'
    Check 'M2b-04' ($r.Ok -and (Test-NoMarker $r @($arMarker))) 'the file inside the external target is never copied (not in the package directory or the ZIP)'
    $r = Invoke-Handoff $ap @{ Name = 'ar-root-log'; TestLogs = @((Join-Path $arLink 'leak.txt')); AllowedRoot = @($arLink) }
    Check 'M2b-05' ($r.Ok -and -not ((Read-Pkg $r 'tests.log').Contains($arMarker)) -and (LeftOutText $r) -match 'allowed root') '-TestLogs through an -AllowedRoot that is a junction is rejected and its text is not in tests.log'
    Check 'M2b-06' ($r.Ok -and (Test-NoMarker $r @($arMarker))) 'the external text is absent from the whole package and ZIP'
    $r = Invoke-Handoff $ap @{ Name = 'ar-root-sub'; Evidence = @((Join-Path $arLink 'sub\leak2.txt')); AllowedRoot = @((Join-Path $arLink 'sub')) }
    Check 'M2b-07' ($r.Ok -and -not (Test-Path (Join-Path $r.Out.PackagePath 'evidence')) -and (Test-NoMarker $r @($arMarker))) 'an -AllowedRoot that lies BEHIND a junction (parent is the junction) is rejected'
    $r = Invoke-Handoff $ap @{ Name = 'ar-root-slash'; Evidence = @((Join-Path $arLink 'leak.txt')); AllowedRoot = @($arLink + '\') }
    Check 'M2b-08' ($r.Ok -and -not (Test-Path (Join-Path $r.Out.PackagePath 'evidence'))) 'a trailing backslash on the allowed root does not bypass the check'
    $r = Invoke-Handoff $ap @{ Name = 'ar-mixed'; Evidence = @((Join-Path $arLink 'leak.txt'), (Join-Path $arReal 'ok.txt')); AllowedRoot = @($arLink, $arReal) }
    Check 'M2b-09' ($r.Ok -and (Test-Path (Join-Path $r.Out.PackagePath 'evidence\ok.txt')) -and -not (Test-Path (Join-Path $r.Out.PackagePath 'evidence\leak.txt')) -and (Test-NoMarker $r @($arMarker))) 'with an ordinary root and a junction root together, only the file under the ordinary root is accepted'
    $below = Join-Path $arReal 'junction-below'
    cmd /c "mklink /J ""$below"" ""$arTarget""" 2>&1 | Out-Null
    $r = Invoke-Handoff $ap @{ Name = 'ar-below-ev'; Evidence = @((Join-Path $below 'leak.txt')); AllowedRoot = @($arReal) }
    Check 'M2b-10' ($r.Ok -and -not (Test-Path (Join-Path $r.Out.PackagePath 'evidence')) -and (LeftOutText $r) -match 'reparse') 'a junction BELOW an ordinary allowed root is still rejected for -Evidence'
    $r = Invoke-Handoff $ap @{ Name = 'ar-below-log'; TestLogs = @((Join-Path $below 'leak.txt')); AllowedRoot = @($arReal) }
    Check 'M2b-11' ($r.Ok -and -not ((Read-Pkg $r 'tests.log').Contains($arMarker)) -and (Test-NoMarker $r @($arMarker))) 'a junction below an ordinary allowed root is still rejected for -TestLogs'
    $r = Invoke-Handoff $ap @{ Name = 'ar-still-ok'; Evidence = @((Join-Path $arReal 'ok.txt')); AllowedRoot = @($arReal) }
    Check 'M2b-12' ($r.Ok -and (Test-Path (Join-Path $r.Out.PackagePath 'evidence\ok.txt'))) 'the ordinary allowed root still works next to the junctions'
    if ($PreviousScript -ne '' -and (Test-Path -LiteralPath $PreviousScript)) {
        $global:LASTEXITCODE = 0
        try {
            $po = & $PreviousScript -RepoRoot $ap -Name 'ar-previous' -Evidence @((Join-Path $arLink 'leak.txt')) -AllowedRoot @($arLink) 6>$null 3>$null | Where-Object { $_ -is [pscustomobject] } | Select-Object -Last 1
            $leaked = $po -and (Test-Path (Join-Path $po.PackagePath 'evidence\leak.txt'))
            Rec 'M2b-13' 'INFO' "the previous version (before this correction) read the file behind the allowed-root junction and put it in evidence/: $leaked"
        } catch { Rec 'M2b-13' 'INFO' "the previous version failed on this case: $(Short $_.Exception.Message 80)" }
    }
} else { Rec 'M2b-03' 'NOT-RUN' 'could not create a junction' }
$arSym = Join-Path $ar 'root-symlink'
$arSymOk = $false
try { New-Item -ItemType SymbolicLink -Path $arSym -Target $arTarget -ErrorAction Stop | Out-Null; $arSymOk = $true } catch { }
if ($arSymOk) {
    $r = Invoke-Handoff $ap @{ Name = 'ar-symroot'; Evidence = @((Join-Path $arSym 'leak.txt')); AllowedRoot = @($arSym) }
    Check 'M2b-14' ($r.Ok -and -not (Test-Path (Join-Path $r.Out.PackagePath 'evidence')) -and (Test-NoMarker $r @($arMarker))) 'an -AllowedRoot that is a symbolic link to an outside directory is rejected'
} else { Rec 'M2b-14' 'NOT-RUN' 'symbolic links need a privilege or Developer Mode; creating one failed (privilege-dependent test)' }

$u = New-TestRepo (Join-Path $WorkRoot 'uni')
Put-File $u "src/caf$E9.ts" "v1`n"
Put-File $u "src/$JA.ts" "v1`n"
Put-File $u "src/gone-$E9.ts" "gone`n"
Put-File $u 'src/keep.ts' "keep`n"
Save-Commit $u base
$uBase = (& git -C $u rev-parse HEAD).Trim()
Put-File $u "src/caf$E9.ts" "v2`n"
Put-File $u "src/$JA.ts" "v2`n"
Remove-Item (Join-Path $u "src\gone-$E9.ts")
Put-File $u "src/new-$E9.ts" "new`n"
Put-File $u "src/$JA-new.ts" "new`n"
$r = Invoke-Handoff $u @{ Name = 'uni' }
Check 'M3-01' ($r.Ok) "[v1: Illegal characters in path] a run with modified, deleted and new non-ASCII file names succeeds ($(Short $r.Err 80))"
foreach ($n in "src/caf$E9.ts", "src/$JA.ts", "src/new-$E9.ts", "src/$JA-new.ts") { Check 'M3-02' (Included $r $n) "included: $n" }
$ucl = Join-Path $WorkRoot 'uni-clone'
& git -c core.autocrlf=false clone -q $u $ucl 2>&1 | Out-Null
& git -C $ucl -c core.autocrlf=false checkout -q $uBase 2>&1 | Out-Null
$null = & git -C $ucl -c core.autocrlf=false apply (Join-Path $r.Out.PackagePath 'changes.patch') 2>&1
$uok = ($LASTEXITCODE -eq 0) -and ((Get-Content (Join-Path $ucl "src\caf$E9.ts") -Raw) -eq "v2`n") -and (Test-Path (Join-Path $ucl "src\new-$E9.ts")) -and (Test-Path (Join-Path $ucl "src\$JA-new.ts")) -and -not (Test-Path (Join-Path $ucl "src\gone-$E9.ts"))
Check 'M3-03' $uok 'the patch with non-ASCII names applies to the base and reproduces the changes (modified, new and deleted)'
Add-Type -AssemblyName System.IO.Compression.FileSystem
$z = [System.IO.Compression.ZipFile]::OpenRead($r.Out.ZipPath)
$names = @($z.Entries | ForEach-Object { $_.FullName })
$z.Dispose()
$pnm = Split-Path -Leaf $r.Out.PackagePath
Check 'L2-01' (($names -contains "$pnm/files/src/caf$E9.ts") -and ($names -contains "$pnm/files/src/$JA.ts")) 'ZIP entry names round-trip through System.IO.Compression'
# The UTF-8 flag (bit 11) must be set exactly on the entries whose names are not plain ASCII.
$zb = [System.IO.File]::ReadAllBytes($r.Out.ZipPath)
$flagBad = @(); $flagged = 0; $p0 = 0
while ($p0 -lt $zb.Length - 46) {
    if ($zb[$p0] -eq 0x50 -and $zb[$p0 + 1] -eq 0x4b -and $zb[$p0 + 2] -eq 1 -and $zb[$p0 + 3] -eq 2) {
        $fl = [BitConverter]::ToUInt16($zb, $p0 + 8); $nl = [BitConverter]::ToUInt16($zb, $p0 + 28); $el = [BitConverter]::ToUInt16($zb, $p0 + 30); $cl = [BitConverter]::ToUInt16($zb, $p0 + 32)
        $nameBytes = $zb[($p0 + 46)..($p0 + 45 + $nl)]
        $nonAscii = [bool]($nameBytes | Where-Object { $_ -ge 128 })
        $isSet = (($fl -band 0x800) -ne 0)
        if ($nonAscii) { $flagged++ }
        if ($nonAscii -ne $isSet) { $flagBad += [Text.Encoding]::UTF8.GetString($nameBytes) }
        $p0 += 46 + $nl + $el + $cl
    } else { $p0++ }
}
Check 'L2-02' ($flagBad.Count -eq 0 -and $flagged -ge 4) "[v1: flag unset] the UTF-8 name flag is set on all $flagged non-ASCII entries and on none of the ASCII ones ($($flagBad -join ', '))"
$tar = Get-Command tar.exe -ErrorAction SilentlyContinue
if ($tar) {
    $u2 = New-TestRepo (Join-Path $WorkRoot 'uni2')
    Put-File $u2 'a.txt' "base`n"
    Save-Commit $u2 base
    Put-File $u2 "src/caf$E9.ts" "x`n"
    Put-File $u2 "src/ma$([char]0xF1)ana.ts" "x`n"
    $r2 = Invoke-Handoff $u2 @{ Name = 'uni2' }
    $td = Join-Path $WorkRoot 'uni2-tar'
    New-Item -ItemType Directory $td | Out-Null
    & tar.exe -xf $r2.Out.ZipPath -C $td 2>&1 | Out-Null
    $tarExit = $LASTEXITCODE
    Check 'L2-04' ($tarExit -eq 0 -and (Test-Path -LiteralPath (Join-Path $td "uni2\files\src\caf$E9.ts")) -and (Test-Path -LiteralPath (Join-Path $td "uni2\files\src\ma$([char]0xF1)ana.ts"))) "[v1: bsdtar could not read the names] bsdtar extracts accented file names correctly (exit $tarExit)"
    & tar.exe -tf $r.Out.ZipPath 2>&1 | Out-Null
    Rec 'L2-05' 'INFO' "bsdtar on this machine cannot convert Japanese names to the legacy console code page (listing exit $LASTEXITCODE); Expand-Archive and System.IO.Compression read them correctly (environment limitation of tar.exe, not of the archive)"
} else { Rec 'L2-04' 'NOT-RUN' 'tar.exe not found' }
$ex = Join-Path $WorkRoot 'uni-expand'
Expand-Archive -LiteralPath $r.Out.ZipPath -DestinationPath $ex -ErrorAction SilentlyContinue
Check 'L2-03' ((Test-Path -LiteralPath (Join-Path $ex "$pnm\files\src\caf$E9.ts")) -and (Test-Path -LiteralPath (Join-Path $ex "$pnm\files\src\$JA.ts"))) 'Expand-Archive restores the Unicode file names'

# =================================================================================================================
# M4: precise exclusions (nothing relevant is hidden by a word in a path)
# =================================================================================================================
$m = New-TestRepo (Join-Path $WorkRoot 'm4')
foreach ($p in 'internal/build/builder.go', 'cmd/coverage/main.go', 'internal/auth/credentials.go', 'web/src/dist/format.ts', 'docs/review-handoff/guide.md', 'web/src/lib/build.ts', 'review-handoff/README.md') { Put-File $m $p "v1`n" }
Save-Commit $m base
foreach ($p in 'internal/build/builder.go', 'cmd/coverage/main.go', 'internal/auth/credentials.go', 'web/src/dist/format.ts', 'docs/review-handoff/guide.md', 'web/src/lib/build.ts', 'review-handoff/README.md') { Put-File $m $p "v2`n" }
Put-File $m 'web/dist/bundle.js' "built`n"
Put-File $m 'node_modules/pkg/index.js' "dep`n"
Put-File $m 'bin/tool.go' "x`n"
Put-File $m 'review-handoff/untracked-package/REVIEW.md' "an earlier package`n"
$r = Invoke-Handoff $m @{ Name = 'm4' }
foreach ($p in 'internal/build/builder.go', 'cmd/coverage/main.go', 'internal/auth/credentials.go', 'web/src/dist/format.ts', 'docs/review-handoff/guide.md', 'web/src/lib/build.ts', 'review-handoff/README.md') {
    Check 'M4-01' (Included $r $p) "[v1 hid it] tracked source is included: $p"
}
foreach ($p in 'web/dist/bundle.js', 'node_modules/pkg/index.js', 'bin/tool.go', 'review-handoff/untracked-package/REVIEW.md') {
    Check 'M4-02' ($r.Ok -and -not (Included $r $p)) "known generated or output path is excluded: $p"
}
$m2 = New-TestRepo (Join-Path $WorkRoot 'm4b')
Put-File $m2 'a.txt' "base`n"
Save-Commit $m2 base
Put-File $m2 'review-handoff/README.md' "# documentation of the output directory`n"
Put-File $m2 'review-handoff/some-package/REVIEW.md' "an earlier package`n"
$r = Invoke-Handoff $m2 @{ Name = 'm4b' }
Check 'M4-04' ((Included $r 'review-handoff/README.md') -and -not (Included $r 'review-handoff/some-package/REVIEW.md')) '[v1: needed -Evidence] a new, untracked review-handoff/README.md is source and is included; packages next to it are not'
$cats = (Get-Content (Join-Path (Split-Path -Parent $here) 'ReviewHandoff-SECURITY.md') -Raw -ErrorAction SilentlyContinue)
Check 'M4-03' ($cats -and $cats -match 'Exclusion categories') 'every exclusion category is documented in tools/ReviewHandoff-SECURITY.md'

# =================================================================================================================
# M5/L5/L8: -RunChecks (output scanning, exit codes, timeouts)
# =================================================================================================================
$c = New-TestRepo (Join-Path $WorkRoot 'checks')
Put-File $c '.gitignore' "node_modules/`nreview-handoff/`n"
$emit = @(
    "console.log('before the secret');",
    "console.log(""$($script:KW) = '$($script:VAL)'"");",
    "console.log('$($script:PEM)');",
    "console.log('MIIEvQIBADANBgkqhkiG9w0BAQEFAASC');",
    "console.log('$($script:PEMEND)');",
    "console.log('Authorization: Bearer $($script:JWT)');",
    "console.log('tests ok');"
) -join "`n"
Put-File $c 'web/emit.js' $emit
$pkg = [ordered]@{ name = 'x'; version = '1.0.0'; scripts = [ordered]@{
        'format:check' = 'node -e "console.log(''format ok'')"'
        'lint'         = "node -e ""console.log('lint FAILED');process.exit(3)"""
        'test'         = 'node emit.js'
        'build'        = 'node -e "process.exit(0)"'
    } }
Put-File $c 'web/package.json' ($pkg | ConvertTo-Json -Depth 4)
Put-File $c 'web/src/a.ts' "a`n"
Save-Commit $c base
Put-File $c 'web/src/a.ts' "a2`n"
New-Item -ItemType Directory (Join-Path $c 'web\node_modules') -Force | Out-Null
$r = Invoke-Handoff $c @{ Name = 'checks'; RunChecks = $true }
Check 'M5-00' ($r.Ok) "run with -RunChecks succeeds ($(Short $r.Err 80))"
if ($r.Ok) {
    $log = Read-Pkg $r 'tests.log'; $rev = Read-Pkg $r 'REVIEW.md'
    Check 'M5-01' (-not $log.Contains($script:VAL) -and -not $log.Contains('MIIEvQIBADANBgkq')) '[v1: unscanned] secret-like output of a check (assignment and private-key block) is not in tests.log'
    Check 'M5-02' ($log -match 'line withheld') 'withheld lines are marked, and the rest of the output is kept'
    Check 'M5-03' ($log -match 'before the secret' -and $log -match 'tests ok' -and $log -match 'lint FAILED') 'useful output around the redacted lines is preserved'
    Check 'M5-04' ((Test-NoMarker $r @($script:VAL, 'MIIEvQIBADANBgkq', $script:JWT))) 'the secret values (assignment, private key block, Bearer token) are absent from the whole package and ZIP'
    Check 'M5-05' (([regex]::Matches($log, 'line withheld')).Count -ge 3) "the assignment, the key block and the Bearer header are each withheld ($(([regex]::Matches($log, 'line withheld')).Count) lines)"
    Check 'L5-01' ($log -match 'npm run lint[\s\S]*?exit code: 3') 'a failing check records its real exit code (3)'
    Check 'L5-02' ($rev -match 'VERIFICATION FAILED' -and $rev -match '\| npm run lint \| 3 \| FAILED') '[v1: only a table] REVIEW.md states in words that verification failed'
    Check 'L5-03' ($log -match 'npm run typecheck[\s\S]*?exit code: [1-9]') 'a missing script is a failure, not a pass'
    Check 'L5-04' ($r.Exit -eq 2 -and $r.Out.ChecksFailed) "[v1: exit 0] the script exit code is 2 when a check failed (exit=$($r.Exit))"
}
$null = & powershell.exe -NoProfile -ExecutionPolicy Bypass -File $Script -RepoRoot $c -Name 'checks-exit' -RunChecks 2>&1
Check 'L5-05' ($LASTEXITCODE -eq 2) "the process exit code is 2 when started as a separate process (exit=$LASTEXITCODE)"
$ok = New-TestRepo (Join-Path $WorkRoot 'checks-ok')
Put-File $ok '.gitignore' "node_modules/`nreview-handoff/`n"
Put-File $ok 'web/package.json' (([ordered]@{ name = 'x'; version = '1.0.0'; scripts = [ordered]@{ 'format:check' = 'node -e "0"'; 'lint' = 'node -e "0"'; 'typecheck' = 'node -e "0"'; 'test' = 'node -e "0"'; 'build' = 'node -e "0"' } }) | ConvertTo-Json -Depth 4)
Put-File $ok 'web/src/a.ts' "a`n"
Save-Commit $ok base
Put-File $ok 'web/src/a.ts' "a2`n"
New-Item -ItemType Directory (Join-Path $ok 'web\node_modules') -Force | Out-Null
$null = & powershell.exe -NoProfile -ExecutionPolicy Bypass -File $Script -RepoRoot $ok -Name 'ok' -RunChecks 2>&1
Check 'L5-06' ($LASTEXITCODE -eq 0) "all checks passing exits 0 (exit=$LASTEXITCODE)"
# timeout
$tm = New-TestRepo (Join-Path $WorkRoot 'checks-timeout')
Put-File $tm '.gitignore' "node_modules/`nreview-handoff/`n"
$mark = 'RHTIMEOUT' + [guid]::NewGuid().ToString('N').Substring(0, 8)
Put-File $tm 'web/package.json' (([ordered]@{ name = 'x'; version = '1.0.0'; scripts = [ordered]@{ 'format:check' = "node -e ""/*$mark*/setTimeout(function(){},120000)""" } }) | ConvertTo-Json -Depth 4)
Put-File $tm 'web/src/a.ts' "a`n"
Save-Commit $tm base
Put-File $tm 'web/src/a.ts' "a2`n"
New-Item -ItemType Directory (Join-Path $tm 'web\node_modules') -Force | Out-Null
$sw = [Diagnostics.Stopwatch]::StartNew()
$p = Start-Child $tm @('-Name', 'timeout', '-RunChecks', '-CheckTimeoutSeconds', '4')
$finished = $p.WaitForExit(180000)
$secs = [int]$sw.Elapsed.TotalSeconds
$stillRunning = @(Get-CimInstance Win32_Process -ErrorAction SilentlyContinue | Where-Object { $_.CommandLine -and $_.CommandLine.Contains($mark) })
Check 'L8-01' ($finished -and $secs -lt 120) "[v1: no timeout] a hanging check is stopped by -CheckTimeoutSeconds (finished in $secs s)"
Check 'L8-02' ($stillRunning.Count -eq 0) 'the hanging check and its child processes were killed'
Check 'L8-03' ($finished -and $p.ExitCode -eq 2) "a timed-out check makes the script exit with code 2 (exit=$($p.ExitCode))"
$tpk = @(Get-ChildItem (Join-Path $tm 'review-handoff') -Directory -ErrorAction SilentlyContinue | Where-Object { $_.Name -notlike '.*' })
if ($tpk.Count -gt 0) {
    $tl = [System.IO.File]::ReadAllText((Join-Path $tpk[0].FullName 'tests.log'))
    Check 'L8-04' ($tl -match 'exit code: timeout' -and $tl -match 'killed after 4 s') 'the timeout is recorded in tests.log'
} else { Rec 'L8-04' 'FAIL' 'no package was produced after the timeout' }
# reserved names
foreach ($bad in 'con', 'NUL', 'COM1', 'aux.v1', 'lpt3', 'name.') {
    $r = Invoke-Handoff $g @{ Name = $bad }
    Check 'L8-05' (-not $r.Ok) "[v1: accepted] reserved package name is refused: '$bad'"
}
$resRoot = New-TestRepo (Join-Path $WorkRoot 'reserved')
Put-File $resRoot 'src/ok.ts' "ok`n"
Save-Commit $resRoot base
$made = $false
try { [System.IO.File]::WriteAllText('\\?\' + (Join-Path $resRoot 'src\nul.txt'), 'x'); $made = $true } catch { }
if ($made) {
    $r = Invoke-Handoff $resRoot @{ Name = 'reserved' }
    Check 'L8-06' ($r.Ok -and -not (Included $r 'src/nul.txt')) "a file named like a Windows device (nul.txt) is never copied (ok=$($r.Ok); $(Short $r.Err 60))"
} else { Rec 'L8-06' 'NOT-RUN' 'Windows refused to create a file named nul.txt in this environment' }

# =================================================================================================================
# L1: atomic packages (interrupted runs, failures after staging, concurrency)
# =================================================================================================================
$a = New-TestRepo (Join-Path $WorkRoot 'atomic')
Put-File $a 'a.txt' "base`n"
Save-Commit $a base
Put-File $a 'src/x.ts' "x`n"
$pre = Invoke-Handoff $a @{ Name = 'keepme' }
$preHash = (Get-FileHash (Join-Path $pre.Out.PackagePath 'REVIEW.md')).Hash + (Get-FileHash $pre.Out.ZipPath).Hash
# failure after staging started: an evidence file that cannot be read
$lockFile = Join-Path $WorkRoot 'locked-evidence.txt'
[System.IO.File]::WriteAllText($lockFile, 'log text')
$fs = [System.IO.File]::Open($lockFile, 'Open', 'ReadWrite', 'None')
$r = Invoke-Handoff $a @{ Name = 'keepme'; Evidence = @($lockFile); AllowedRoot = @($WorkRoot) }
$fs.Dispose()
$outDir = Join-Path $a 'review-handoff'
$names = @(Get-ChildItem $outDir -Force | ForEach-Object { $_.Name })
Check 'L1-01' ((-not $r.Ok) -or $r.Exit -ne 0) "a run that fails after staging started reports the failure (ok=$($r.Ok), exit=$($r.Exit), error='$(Short $r.Err 70)')"
Check 'L1-02' (-not ($names | Where-Object { $_ -like '.incomplete-*' })) '[v1: orphan directory] the failed run left no staging leftovers'
Check 'L1-03' (((Get-FileHash (Join-Path $pre.Out.PackagePath 'REVIEW.md')).Hash + (Get-FileHash $pre.Out.ZipPath).Hash) -eq $preHash) 'the existing package and ZIP are byte-identical after the failed run'
# kill during ZIP creation
$k = New-TestRepo (Join-Path $WorkRoot 'kill')
Put-File $k 'a.txt' "base`n"
Save-Commit $k base
1..1500 | ForEach-Object { Put-File $k "src/k/f$_.ts" ((1..30 | ForEach-Object { "export const v$_ = $_" }) -join "`n") }
$kp0 = Invoke-Handoff $k @{ Name = 'older'; Files = @('a.txt') }
$olderHash = (Get-FileHash $kp0.Out.ZipPath).Hash
$p = Start-Child $k @('-Name', 'killme')
$killed = $false
$deadline = (Get-Date).AddSeconds(240)
while (-not $p.HasExited -and (Get-Date) -lt $deadline) {
    if (@(Get-ChildItem (Join-Path $k 'review-handoff') -Force -Filter '.incomplete-killme-*.zip' -ErrorAction SilentlyContinue).Count -gt 0) { Start-Sleep -Milliseconds 20; try { $p.Kill() } catch { }; $killed = $true; break }
    Start-Sleep -Milliseconds 5
}
$p.WaitForExit()
if ($killed) {
    $items = @(Get-ChildItem (Join-Path $k 'review-handoff') -Force | ForEach-Object { $_.Name })
    Check 'L1-04' (-not ($items -contains 'killme') -and -not ($items -contains 'killme.zip')) "[v1: truncated killme.zip] an interrupted run leaves no package or ZIP under the final name (items: $($items -join ', '))"
    Check 'L1-05' (($items | Where-Object { $_ -like '.incomplete-killme-*' }).Count -ge 1) 'the interrupted run is visible only as hidden .incomplete-* staging items'
    $again = Invoke-Handoff $k @{ Name = 'killme'; Files = @('a.txt') }
    Check 'L1-06' ($again.Ok -and (Split-Path -Leaf $again.Out.PackagePath) -eq 'killme') 'the next run can use the same name (nothing complete was in the way)'
    Check 'L1-07' ((Get-FileHash $kp0.Out.ZipPath).Hash -eq $olderHash) 'an unrelated existing package is untouched'
} else { Rec 'L1-04' 'NOT-RUN' 'could not interrupt the run during ZIP creation (it finished too fast or failed)' }
# concurrency: two runs, same name
$cc = New-TestRepo (Join-Path $WorkRoot 'concurrent')
Put-File $cc 'a.txt' "base`n"
Save-Commit $cc base
Put-File $cc 'src/x.ts' "x`n"
$ps = 1..3 | ForEach-Object { Start-Child $cc @('-Name', 'same') }
$ps | ForEach-Object { $_.WaitForExit() }
$codes = @($ps | ForEach-Object { $_.ExitCode })
$od = Join-Path $cc 'review-handoff'
$dirs = @(Get-ChildItem $od -Directory -Force | ForEach-Object { $_.Name })
$zips = @(Get-ChildItem $od -File -Filter '*.zip' | ForEach-Object { $_.BaseName })
$complete = @($dirs | Where-Object { $_ -notlike '.*' })
Check 'L1-08' ($complete.Count -ge 1 -and -not ($complete | Where-Object { $zips -notcontains $_ }) -and -not ($dirs | Where-Object { $_ -like '.incomplete-*' })) "3 concurrent runs with one name: every published package has its ZIP, no staging leftovers, nothing overwritten (exit codes $($codes -join ','); packages: $($complete -join ','))"

# =================================================================================================================
# L3: external diff drivers and text conversion
# =================================================================================================================
$x = New-TestRepo (Join-Path $WorkRoot 'drivers')
Put-File $x 'a.txt' "base`n"
Put-File $x 'b.txt' "base`n"
Save-Commit $x base
Put-File $x 'a.txt' "changed`n"
Put-File $x 'b.txt' "changed`n"
$marker1 = Join-Path $WorkRoot 'ext-diff-ran.txt'
$marker2 = Join-Path $WorkRoot 'textconv-ran.txt'
$marker3 = Join-Path $WorkRoot 'env-ext-diff-ran.txt'
$ext = Join-Path $WorkRoot 'ext.cmd'
[System.IO.File]::WriteAllText($ext, "@echo ran> `"$marker1`"`r`n@exit /b 0`r`n")
$tc = Join-Path $WorkRoot 'tc.cmd'
[System.IO.File]::WriteAllText($tc, "@echo ran> `"$marker2`"`r`n@type %1`r`n")
$ev = Join-Path $WorkRoot 'envext.cmd'
[System.IO.File]::WriteAllText($ev, "@echo ran> `"$marker3`"`r`n@exit /b 0`r`n")
& git -C $x config diff.external ($ext -replace '\\', '/')
& git -C $x config diff.spy.textconv ($tc -replace '\\', '/')
Put-File $x '.gitattributes' "b.txt diff=spy`n"
$env:GIT_EXTERNAL_DIFF = ($ev -replace '\\', '/')
$r = Invoke-Handoff $x @{ Name = 'drivers' }
Remove-Item Env:\GIT_EXTERNAL_DIFF -ErrorAction SilentlyContinue
Check 'L3-01' ($r.Ok -and -not (Test-Path $marker1)) '[v1: executed] diff.external from the repository configuration is not executed'
Check 'L3-02' (-not (Test-Path $marker2)) 'a textconv driver from .gitattributes is not executed'
Check 'L3-03' (-not (Test-Path $marker3)) 'GIT_EXTERNAL_DIFF from the environment is not executed'
Check 'L3-04' ($r.Ok -and (Read-Pkg $r 'changes.patch').Contains('+changed')) 'the patch is still the plain Git diff'

# =================================================================================================================
# L4: custom output directories
# =================================================================================================================
$o4 = New-TestRepo (Join-Path $WorkRoot 'outroot')
Put-File $o4 'a.txt' "base`n"
Save-Commit $o4 base
Put-File $o4 'src/x.ts' "x`n"
$null = Invoke-Handoff $o4 @{ Name = 'p1'; OutputRoot = (Join-Path $o4 'out') }
$r = Invoke-Handoff $o4 @{ Name = 'p2'; OutputRoot = (Join-Path $o4 'out') }
Check 'L4-01' ($r.Ok -and -not (@($r.Out.Included) | Where-Object { $_ -like 'out/*' })) '[v1: re-included] a custom in-repo output directory is not re-included in the next package'
$r = Invoke-Handoff $o4 @{ Name = 'p3' }
$r = Invoke-Handoff $o4 @{ Name = 'p4' }
Check 'L4-02' ($r.Ok -and -not (@($r.Out.Included) | Where-Object { $_ -like 'review-handoff/*' })) 'the default output directory is not re-included either'

# =================================================================================================================
# Local paths and user names are redacted
# =================================================================================================================
$lp = New-TestRepo (Join-Path $WorkRoot 'paths')
Put-File $lp 'a.txt' "base`n"
Save-Commit $lp base
Put-File $lp 'src/x.ts' "x`n"
$evFile = Join-Path $WorkRoot 'evidence-paths.txt'
[System.IO.File]::WriteAllText($evFile, "log from $env:USERPROFILE\AppData\Local\Temp\run1`nC:\Users\$env:USERNAME\project\x.log`n/c/Users/$env:USERNAME/repo`nuser $env:USERNAME ran it`n")
$r = Invoke-Handoff $lp @{ Name = 'paths'; Evidence = @($evFile); TestLogs = @($evFile); AllowedRoot = @($WorkRoot); Summary = "see $env:USERPROFILE\x and $evFile" }
$userHit = $false
if ($r.Ok) {
    foreach ($f in Get-ChildItem -LiteralPath $r.Out.PackagePath -Recurse -File) {
        $t = [System.IO.File]::ReadAllText($f.FullName)
        if ($t -match ('(?i)(?<![A-Za-z0-9])' + [regex]::Escape($env:USERNAME) + '(?![A-Za-z0-9])') -or $t.Contains($env:USERPROFILE) -or $t.Contains([System.IO.Path]::GetTempPath().TrimEnd('\'))) { $userHit = $true }
    }
}
Check 'PATH-1' ($r.Ok -and -not $userHit) "[v1: usernames in logs] the Windows user name, profile path and temp path are absent from every file of the package (ok=$($r.Ok))"
Check 'PATH-2' ($r.Ok -and ((Read-Pkg $r 'tests.log') -match '<user>|<USERPROFILE>|<TEMP>')) 'they are replaced by neutral placeholders'
Check 'PATH-3' ($r.Ok -and (Read-Pkg $r 'git-status.txt') -match 'Repository   : <repo>') 'git-status.txt shows <repo> instead of the local path'

# =================================================================================================================
# L7: CLAUDE.md content (static checks of the instructions)
# =================================================================================================================
$cm = Get-Content (Join-Path (Split-Path -Parent (Split-Path -Parent $here)) 'CLAUDE.md') -Raw -ErrorAction SilentlyContinue
if ($cm) {
    Check 'L7-01' ($cm -match '(?i)only when' -and $cm -match '(?i)independent review') 'CLAUDE.md triggers only for actual independent-review or artifact requests'
    Check 'L7-02' ($cm -match '(?i)generic|not.*(every|each).*request|do not.*trigger') 'CLAUDE.md says not to trigger on generic requests for evidence'
    Check 'L7-03' ($cm -match '(?i)-RunChecks.*only|only.*-RunChecks') 'CLAUDE.md does not always run the whole test suite'
    Check 'L7-04' ($cm -match '(?i)default selection|every changed') 'CLAUDE.md explains the default file selection'
    Check 'L7-05' ($cm -match '(?i)review the package|inspect the package|sensitive' -and $cm -match '(?i)before.*shar') 'CLAUDE.md requires reviewing the package for sensitive data before external sharing'
} else { Rec 'L7-01' 'FAIL' 'CLAUDE.md not found' }

# =================================================================================================================
# V1: reproduction of the original failures with the first version of the script (only when -OldScript is given)
# =================================================================================================================
if ($OldScript -ne '' -and (Test-Path -LiteralPath $OldScript)) {
    function Invoke-Old([string]$Root, [hashtable]$P) {
        $global:LASTEXITCODE = 0
        try { $o = & $OldScript -RepoRoot $Root @P 6>$null 3>$null; return [pscustomobject]@{ Ok = $true; Out = ($o | Select-Object -Last 1); Err = ''; Exit = $LASTEXITCODE } }
        catch { return [pscustomobject]@{ Ok = $false; Out = $null; Err = $_.Exception.Message; Exit = 1 } }
    }
    $oh = Invoke-Old $h @{ Name = 'v1-h1' }
    if ($oh.Ok) {
        $leaked = @(@($blocked.Keys) + $extra | Where-Object { @($oh.Out.Included) -contains $_ })
        Rec 'V1-H1' 'INFO' "first version INCLUDED $($leaked.Count) of $(@($blocked.Keys).Count + $extra.Count) credential fixtures: $($leaked -join ', ')"
        $stillNew = @(@($blocked.Keys) + $extra | Where-Object { Included $hr $_ })
        Check 'V1-H1b' ($stillNew.Count -eq 0) 'the current version includes none of them'
    } else { Rec 'V1-H1' 'INFO' "first version failed on the fixture repository: $(Short $oh.Err 90)" }
    $od2 = Invoke-Old $d @{ Name = 'v1-h2' }
    if ($od2.Ok) {
        $pt1 = [System.IO.File]::ReadAllText((Join-Path $od2.Out.PackagePath 'changes.patch'))
        Rec 'V1-H2' 'INFO' "first version leaked the removed or deleted secrets into changes.patch: $($pt1.Contains($old))"
    } else { Rec 'V1-H2' 'INFO' "first version failed: $(Short $od2.Err 90)" }
    $ou = Invoke-Old $u @{ Name = 'v1-uni' }
    Rec 'V1-M3' 'INFO' "first version with a modified non-ASCII file name: ok=$($ou.Ok) $(if (-not $ou.Ok) { Short $ou.Err 80 })"
    $om = Invoke-Old $m @{ Name = 'v1-m4' }
    if ($om.Ok) { $hid = @('internal/build/builder.go', 'cmd/coverage/main.go', 'internal/auth/credentials.go', 'web/src/dist/format.ts', 'docs/review-handoff/guide.md' | Where-Object { @($om.Out.Included) -notcontains $_ }); Rec 'V1-M4' 'INFO' "first version HID $($hid.Count) tracked source files: $($hid -join ', ')" }
    $oj = Invoke-Old $j @{ Name = 'v1-m2' }
    if ($oj.Ok) { Rec 'V1-M2' 'INFO' "first version followed the junction and copied $(@($oj.Out.Included | Where-Object { $_ -like 'jlink*' }).Count) file(s) from outside the repository" }
    Remove-Item $marker1, $marker2 -ErrorAction SilentlyContinue
    $ox = Invoke-Old $x @{ Name = 'v1-l3' }
    Rec 'V1-L3' 'INFO' "first version executed the external diff driver: $(Test-Path $marker1)"
    $oo = Invoke-Old $o4 @{ Name = 'v1-p5'; OutputRoot = (Join-Path $o4 'out') }
    $oo = Invoke-Old $o4 @{ Name = 'v1-p6'; OutputRoot = (Join-Path $o4 'out') }
    if ($oo.Ok) { Rec 'V1-L4' 'INFO' "first version re-included earlier packages: $(@($oo.Out.Included | Where-Object { $_ -like 'out/*' }).Count) file(s)" }
    $null = & powershell.exe -NoProfile -ExecutionPolicy Bypass -File $OldScript -RepoRoot $c -Name 'v1-checks' -RunChecks 2>&1
    Rec 'V1-L5' 'INFO' "first version exit code with a failing check: $LASTEXITCODE"
    $orr = Invoke-Old $g @{ Name = 'con' }
    Rec 'V1-L8' 'INFO' "first version accepted the reserved package name 'con': $($orr.Ok)"
}

# =================================================================================================================
$results | ForEach-Object { $_ }
''
"TOTALS: PASS=$($counts['PASS'])  FAIL=$($counts['FAIL'])  INFO=$($counts['INFO'])  NOT-RUN=$($counts['NOT-RUN'])"
"WORK ROOT: $WorkRoot"
if ($OutputFile -ne '') { [System.IO.File]::WriteAllLines($OutputFile, @($results) + @('', "TOTALS: PASS=$($counts['PASS'])  FAIL=$($counts['FAIL'])  INFO=$($counts['INFO'])  NOT-RUN=$($counts['NOT-RUN'])"), $utf8) }
if ($counts['FAIL'] -gt 0) { exit 1 } else { exit 0 }
