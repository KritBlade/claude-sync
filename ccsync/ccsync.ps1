# ccsync v1.17.2-patch1 - wrapper around claude-sync, which syncs ~/.claude through encrypted cloud storage
#
#   ccsync pull     -> claude-sync pull, then apply the other machine's deletions, then claude-sync rebuild-history
#   ccsync push     -> claude-sync push
#   ccsync sync     -> pull (as above) then push
#   ccsync version  -> this wrapper's version and claude-sync's
#   anything else   -> passed through untouched (status, diff, conflicts, ...)
#   --dry-run on pull/sync previews the pull and the deletions, and changes nothing.
#   -h/--help on pull/sync shows claude-sync's pull help and runs nothing else.
#
# Applying deletions: claude-sync pull only downloads. A file the other machine deleted and pushed stays here,
# and returns to the cloud as soon as it changes here or the sync state is reset. After each pull, ccsync moves
# such a file to ~/.claude-sync/trash/<timestamp>/<same relative path> when all of these hold:
#   - `claude-sync diff` lists it as "Local only" (it is not in the cloud),
#   - its name holds no ".conflict.": the remote side of a conflict, written and recorded in state.json by the
#     pull that just ran, passes every other test, and `claude-sync conflicts` finds it only by that mark,
#   - its path stays inside ~/.claude and reaches the file through no symlink, junction or other reparse point,
#     so it leads where it appears to (claude-sync does not sync linked content, so such a path should never be
#     listed; if it is, the file lives outside ~/.claude and must not be moved),
#   - ~/.claude-sync/state.json records it with a hash (this machine synced it before, so it is not new here),
#   - its SHA-256 equals that hash (it has no edits here that were never pushed).
# A file that fails only the last test has edits here: it is kept and reported, and the next push uploads it
# again. A conflict copy, a path that fails the path test and a file that cannot be hashed are kept and reported
# with the reason. The next push drops the moved files from state.json (it finds them gone and deletes their
# cloud keys, which are already absent). The list of moved files is written beside the trash folder, as
# <timestamp>.moved.txt, so it can never overwrite a moved file.
#
# Nothing is moved when there is no state.json yet, when the cloud lists no synced file, when the pull reported
# cloud keys this machine cannot map (unknown path token), when the pull ran with -q/--quiet (which hides that
# report), or when diff fails or its output cannot be parsed. A dry run cannot see unmappable keys, so its
# preview may list deletions that the real run then refuses.
# Confirmation is asked for when the deletions are more than $ConfirmAbove files, or more than
# $ConfirmSharePercent% of the files diff counts as synced: a bucket that was partly emptied looks exactly like
# the other machine deleting many files, and the share catches that even when the count alone is unremarkable.
# Without a console to ask, nothing is moved.
# To restore a moved file, copy it from the trash folder back to the same path under ~/.claude.
#
# Why the history rebuild on every pull: history.jsonl is synced as a single file and is last-writer-wins, so
# a plain pull replaces this machine's prompt history with the other machine's copy and the /resume picker
# goes short. The rebuild walks the local session files, merges their prompts back in, and keeps the previous
# file as history.jsonl.bak. It only ever adds entries, and it runs after the deletions so removed sessions add
# none.

$CcsyncVersion = 'v1.17.2-patch1'
$ConfirmAbove = 20
$ConfirmSharePercent = 25
# claude-sync writes the remote side of a conflict to "<path>.conflict.<timestamp>" and finds those files again
# by looking for this in the file name, so ccsync has to recognise exactly the same thing.
$ConflictMarker = '.conflict.'
$ListLimit = 25
$ClaudeDir = Join-Path $env:USERPROFILE '.claude'
$SyncStateDir = Join-Path $env:USERPROFILE '.claude-sync'
$StatePath = Join-Path $SyncStateDir 'state.json'
$TrashRoot = Join-Path $SyncStateDir 'trash'
$AnsiPattern = "$([char]27)\[[0-9;]*[A-Za-z]"

# claude-sync reads a bool flag's "=value" the way Go's strconv.ParseBool does; these are its true spellings.
function Test-TrueFlagValue {
    param([string]$Value)
    return $Value -cin @('1', 't', 'T', 'TRUE', 'true', 'True')
}

# Parses `claude-sync diff` output into the cloud's synced-file count and the "Local only" paths.
# Throws when the output does not have the expected shape, so a format change never moves files.
function Read-DiffReport {
    param([string[]]$Lines)
    $cleanLines = @($Lines | ForEach-Object { ($_ -replace $AnsiPattern, '').TrimEnd("`r") })
    $summaryLine = $cleanLines | Where-Object { $_ -match '^Summary: \d+ synced, \d+ local only, \d+ remote only, \d+ modified$' } | Select-Object -Last 1
    if (-not $summaryLine) {
        throw "diff printed no summary line"
    }
    $summary = [regex]::Match($summaryLine, '^Summary: (\d+) synced, (\d+) local only')
    $localOnlyPaths = New-Object System.Collections.Generic.List[string]
    $isInLocalOnlySection = $false
    foreach ($line in $cleanLines) {
        if ($line -match '^Local only \(\d+ files\):$') {
            $isInLocalOnlySection = $true
            continue
        }
        if (-not $isInLocalOnlySection) { continue }
        if ($line -eq '') {
            $isInLocalOnlySection = $false
            continue
        }
        $entry = [regex]::Match($line, '^  \+ (.+) \([^()]+\)$')
        if (-not $entry.Success) {
            throw "unexpected line in diff's Local only list: $line"
        }
        $localOnlyPaths.Add($entry.Groups[1].Value)
    }
    $declaredLocalOnly = [int]$summary.Groups[2].Value
    if ($declaredLocalOnly -ne $localOnlyPaths.Count) {
        throw "diff summary says $declaredLocalOnly local-only files but listed $($localOnlyPaths.Count)"
    }
    return [pscustomobject]@{
        SyncedCount    = [int]$summary.Groups[1].Value
        LocalOnlyPaths = $localOnlyPaths.ToArray()
    }
}

# True when a reparse point - a symlink, a junction, or anything else Windows redirects, such as a cloud
# placeholder - stands between $ClaudeRoot and $FullPath, or is $FullPath itself. Moving through one would take a
# file from wherever it really lives, which is outside what this sync owns, so such a path is reported and kept
# instead. ~/.claude itself is not inspected: claude-sync already treats it as the root, and a link there is the
# install's own layout. A component that cannot be inspected counts as a link, because a path we cannot describe
# is not one to move files out of.
function Test-PathCrossesLink {
    param([string]$FullPath, [string]$ClaudeRoot)
    $current = $FullPath
    while ($current -and $current.StartsWith($ClaudeRoot + '\', [System.StringComparison]::OrdinalIgnoreCase)) {
        try {
            $item = Get-Item -LiteralPath $current -Force -ErrorAction Stop
        } catch {
            Write-Warning "ccsync: cannot inspect ${current}: $_"
            return $true
        }
        if ($item.Attributes -band [System.IO.FileAttributes]::ReparsePoint) { return $true }
        $current = Split-Path -Parent $current
    }
    return $false
}

# Sorts local-only paths into: deleted on the other machine (safe to move), edited here since the last sync
# (kept), new here (never synced, kept), conflict copies (kept), and skipped (kept, each as "<path>  -- <reason>").
# Reads state.json with a case-sensitive JSON reader, because claude-sync's state keys are case-sensitive.
function Resolve-DeletionPlan {
    param([string[]]$LocalOnlyPaths, [string]$StateFile, [string]$ClaudeRoot)
    Add-Type -AssemblyName System.Web.Extensions
    $serializer = New-Object System.Web.Script.Serialization.JavaScriptSerializer
    $serializer.MaxJsonLength = [int]::MaxValue
    $state = $serializer.DeserializeObject([System.IO.File]::ReadAllText($StateFile, [System.Text.Encoding]::UTF8))
    if ($state -isnot [System.Collections.IDictionary] -or -not $state.ContainsKey('files') -or $state['files'] -isnot [System.Collections.IDictionary]) {
        throw "state.json has no files map"
    }
    $syncedFiles = $state['files']
    $rootPrefix = $ClaudeRoot.TrimEnd('\') + '\'
    $deletedElsewhere = New-Object System.Collections.Generic.List[string]
    $editedHere = New-Object System.Collections.Generic.List[string]
    $newHere = New-Object System.Collections.Generic.List[string]
    $conflictCopies = New-Object System.Collections.Generic.List[string]
    $skipped = New-Object System.Collections.Generic.List[string]
    foreach ($relativePath in $LocalOnlyPaths) {
        # A conflict copy looks exactly like a file the other machine deleted - the pull that wrote it also
        # recorded it in state.json with its own hash, and it has no cloud key of its own - so every test below
        # passes and it would be moved to the trash, which is where `claude-sync conflicts` then fails to find it.
        if ([System.IO.Path]::GetFileName($relativePath).Contains($ConflictMarker)) {
            $conflictCopies.Add($relativePath)
            continue
        }
        # Never act on a path that leaves ~/.claude, however it got listed.
        try {
            $fullPath = [System.IO.Path]::GetFullPath((Join-Path $ClaudeRoot ($relativePath -replace '/', '\')))
        } catch {
            $skipped.Add("$relativePath  -- not a valid path here: $_")
            continue
        }
        if (-not $fullPath.StartsWith($rootPrefix, [System.StringComparison]::OrdinalIgnoreCase)) {
            $skipped.Add("$relativePath  -- escapes ~/.claude")
            continue
        }
        # diff listed it a moment ago; if it is gone or no longer a file, there is nothing left to move.
        if (-not (Test-Path -LiteralPath $fullPath -PathType Leaf)) { continue }
        if (Test-PathCrossesLink -FullPath $fullPath -ClaudeRoot $ClaudeRoot) {
            $skipped.Add("$relativePath  -- reached through a symlink, junction or other reparse point")
            continue
        }
        if (-not $syncedFiles.ContainsKey($relativePath) -or $null -eq $syncedFiles[$relativePath]) {
            $newHere.Add($relativePath)
            continue
        }
        $recorded = $syncedFiles[$relativePath]
        $recordedHash = ''
        if ($recorded -is [System.Collections.IDictionary] -and $recorded.ContainsKey('hash')) {
            $recordedHash = [string]$recorded['hash']
        }
        if (-not $recordedHash) {
            $skipped.Add("$relativePath  -- no hash recorded in state.json")
            continue
        }
        try {
            $localHash = (Get-FileHash -LiteralPath $fullPath -Algorithm SHA256 -ErrorAction Stop).Hash
        } catch {
            $skipped.Add("$relativePath  -- could not hash it: $_")
            continue
        }
        # -eq compares strings case-insensitively; Get-FileHash gives uppercase hex, claude-sync records lowercase.
        if ($localHash -eq $recordedHash) {
            $deletedElsewhere.Add($relativePath)
        } else {
            $editedHere.Add($relativePath)
        }
    }
    return [pscustomobject]@{
        DeletedElsewhere = $deletedElsewhere.ToArray()
        EditedHere       = $editedHere.ToArray()
        NewHere          = $newHere.ToArray()
        ConflictCopies   = $conflictCopies.ToArray()
        Skipped          = $skipped.ToArray()
    }
}

# Removes directories left empty by a move, walking up from $Directory; never touches the first two levels
# under ~/.claude (e.g. projects\ and projects\<project key>\).
function Remove-EmptyParents {
    param([string]$Directory, [string]$ClaudeRoot)
    $current = $Directory
    while ($current.StartsWith($ClaudeRoot + '\', [System.StringComparison]::OrdinalIgnoreCase)) {
        $depth = ($current.Substring($ClaudeRoot.Length).Trim('\') -split '\\').Count
        if ($depth -lt 3) { return }
        try {
            $entryCount = @(Get-ChildItem -LiteralPath $current -Force -ErrorAction Stop).Count
        } catch {
            Write-Warning "ccsync: could not list $current to see whether it is empty: $_"
            return
        }
        if ($entryCount -gt 0) { return }
        try {
            Remove-Item -LiteralPath $current -ErrorAction Stop
        } catch {
            Write-Warning "ccsync: could not remove empty folder ${current}: $_"
            return
        }
        $current = Split-Path -Parent $current
    }
}

# Moves each file to the same relative path under $TrashDirectory, which must already exist, then writes the
# list of what moved beside that folder as <folder>.moved.txt.
function Move-ToTrash {
    param([string[]]$RelativePaths, [string]$ClaudeRoot, [string]$TrashDirectory)
    $moved = New-Object System.Collections.Generic.List[string]
    foreach ($relativePath in $RelativePaths) {
        $source = Join-Path $ClaudeRoot ($relativePath -replace '/', '\')
        $destination = Join-Path $TrashDirectory ($relativePath -replace '/', '\')
        try {
            New-Item -ItemType Directory -Force -Path (Split-Path -Parent $destination) -ErrorAction Stop | Out-Null
        } catch {
            Write-Warning "ccsync: could not create the trash folder for ${relativePath}: $_"
            continue
        }
        try {
            Move-Item -LiteralPath $source -Destination $destination -ErrorAction Stop
        } catch {
            Write-Warning "ccsync: could not move $relativePath to the trash: $_"
            continue
        }
        $moved.Add($relativePath)
        Remove-EmptyParents -Directory (Split-Path -Parent $source) -ClaudeRoot $ClaudeRoot
    }
    if ($moved.Count -gt 0) {
        $movedListPath = $TrashDirectory + '.moved.txt'
        try {
            [System.IO.File]::WriteAllLines($movedListPath, [string[]]$moved, (New-Object System.Text.UTF8Encoding $false))
        } catch {
            Write-Warning "ccsync: could not write the list of moved files to ${movedListPath}: $_"
        }
    }
    return ,$moved.ToArray()
}

function Write-PathList {
    param([string[]]$Paths, [string]$Indent = '    ')
    $Paths | Select-Object -First $ListLimit | ForEach-Object { Write-Host "$Indent$_" }
    if ($Paths.Count -gt $ListLimit) {
        Write-Host "$Indent... and $($Paths.Count - $ListLimit) more"
    }
}

# Applies the other machine's deletions to this machine after a pull (see the header). Never throws.
function Invoke-DeletionSync {
    param([string]$SyncBinary, [bool]$IsDryRun)
    $diffLines = @(& $SyncBinary diff 2>&1 | ForEach-Object { "$_" })
    if ($LASTEXITCODE -ne 0) {
        Write-Warning "ccsync: deletions not applied: claude-sync diff failed (exit $LASTEXITCODE): $($diffLines -join ' | ')"
        return
    }
    try {
        $report = Read-DiffReport -Lines $diffLines
        if ($report.SyncedCount -eq 0) {
            Write-Warning "ccsync: deletions not applied: the cloud lists no synced file, which looks like an empty or unreachable bucket"
            return
        }
        $plan = Resolve-DeletionPlan -LocalOnlyPaths $report.LocalOnlyPaths -StateFile $StatePath -ClaudeRoot $ClaudeDir
    } catch {
        Write-Warning "ccsync: deletions not applied: $_"
        return
    }
    if ($plan.ConflictCopies.Count -gt 0) {
        Write-Host "ccsync: left $($plan.ConflictCopies.Count) conflict copy(ies) alone; run claude-sync conflicts to resolve them:" -ForegroundColor Yellow
        Write-PathList $plan.ConflictCopies
    }
    if ($plan.Skipped.Count -gt 0) {
        Write-Warning "ccsync: skipped $($plan.Skipped.Count) path(s) that must not be moved:"
        Write-PathList $plan.Skipped
    }
    if ($plan.EditedHere.Count -gt 0) {
        Write-Host "ccsync: kept $($plan.EditedHere.Count) file(s) the other machine deleted but this machine has changed since; the next push uploads them again:" -ForegroundColor Yellow
        Write-PathList $plan.EditedHere
    }
    $deletedCount = $plan.DeletedElsewhere.Count
    if ($deletedCount -eq 0) {
        Write-Host "ccsync: no deletions from the other machine to apply" -ForegroundColor DarkGray
        return
    }
    # Ask when there are many, or when the share of the synced files is large enough to look like data loss
    # upstream rather than deliberate deletes. Integer arithmetic, so the share test is exact.
    $needsConfirmation = ($deletedCount -gt $ConfirmAbove) -or (($deletedCount * 100) -gt ($report.SyncedCount * $ConfirmSharePercent))
    $sharePercent = (100.0 * $deletedCount / $report.SyncedCount).ToString('0.0', [System.Globalization.CultureInfo]::InvariantCulture)
    if ($IsDryRun) {
        $wouldAsk = if ($needsConfirmation) { ' - and would ask first' } else { '' }
        Write-Host "ccsync: would move $deletedCount file(s) deleted on the other machine to the trash - $sharePercent% of the $($report.SyncedCount) synced file(s)${wouldAsk}:" -ForegroundColor Cyan
        Write-PathList $plan.DeletedElsewhere
        Write-Host "ccsync: a dry run cannot see cloud keys this machine cannot map (unknown path token); if the real pull reports any, it moves nothing" -ForegroundColor DarkGray
        return
    }
    if ($needsConfirmation) {
        Write-Host "ccsync: $deletedCount of the $($report.SyncedCount) synced file(s) ($sharePercent%) are gone from the cloud:" -ForegroundColor Cyan
        Write-PathList $plan.DeletedElsewhere
        if ([Console]::IsInputRedirected) {
            Write-Warning "ccsync: needs confirmation and there is no console to ask; nothing moved. Run ccsync pull by hand."
            return
        }
        $answer = Read-Host "Move them to the trash? [y/N]"
        if ($answer -notmatch '^(y|yes)$') {
            Write-Host "ccsync: nothing moved" -ForegroundColor DarkGray
            return
        }
    }
    $trashDirectory = Join-Path $TrashRoot (Get-Date -Format 'yyyyMMdd-HHmmss')
    try {
        New-Item -ItemType Directory -Force -Path $trashDirectory -ErrorAction Stop | Out-Null
    } catch {
        Write-Warning "ccsync: deletions not applied: could not create ${trashDirectory}: $_"
        return
    }
    $moved = Move-ToTrash -RelativePaths $plan.DeletedElsewhere -ClaudeRoot $ClaudeDir -TrashDirectory $trashDirectory
    Write-Host "ccsync: moved $($moved.Count) file(s) deleted on the other machine to $trashDirectory" -ForegroundColor Cyan
    Write-PathList $moved
}

# Dot-sourcing (for tests) loads the functions above without running anything.
if ($MyInvocation.InvocationName -eq '.') { return }

$exe = Get-Command claude-sync -ErrorAction SilentlyContinue
if (-not $exe) {
    Write-Host "ccsync: claude-sync not found on PATH" -ForegroundColor Red
    exit 1
}
$bin = $exe.Source
[Console]::OutputEncoding = New-Object System.Text.UTF8Encoding $false

if ($args.Count -eq 0) {
    & $bin --help
    exit $LASTEXITCODE
}

$verb = [string]$args[0]
$rest = @()
if ($args.Count -gt 1) { $rest = @($args[1..($args.Count - 1)] | ForEach-Object { [string]$_ }) }

if ($verb -in @('version', '--version', '-v')) {
    Write-Host "ccsync $CcsyncVersion"
    & $bin --version
    exit $LASTEXITCODE
}

if ($verb -eq 'pull' -or $verb -eq 'sync') {
    $isDryRun = $false
    $isQuiet = $false
    $isHelp = $false
    $pullArgumentList = New-Object System.Collections.Generic.List[string]
    $pullArgumentList.Add('pull')
    # A repeated flag takes its last value, as it does in claude-sync.
    foreach ($argument in $rest) {
        if ($argument -ceq '--dry-run') {
            $isDryRun = $true
        } elseif ($argument -clike '--dry-run=*') {
            $isDryRun = Test-TrueFlagValue $argument.Substring($argument.IndexOf('=') + 1)
        } elseif ($argument -ceq '-q' -or $argument -ceq '--quiet') {
            $isQuiet = $true
        } elseif ($argument -clike '-q=*' -or $argument -clike '--quiet=*') {
            $isQuiet = Test-TrueFlagValue $argument.Substring($argument.IndexOf('=') + 1)
        } elseif ($argument -ceq '-h' -or $argument -ceq '--help') {
            $isHelp = $true
        }
        # --rebuild-history runs as its own step below, after the deletions.
        if ($argument -ceq '--rebuild-history' -or $argument -clike '--rebuild-history=*') { continue }
        $pullArgumentList.Add($argument)
    }
    $pullArgs = $pullArgumentList.ToArray()

    if ($isHelp) {
        & $bin @pullArgs
        exit $LASTEXITCODE
    }

    $hasSyncState = Test-Path -LiteralPath $StatePath
    Write-Host "> claude-sync $($pullArgs -join ' ')" -ForegroundColor DarkGray
    $pullOutput = @()
    if ($hasSyncState) {
        & $bin @pullArgs | Tee-Object -Variable pullOutput | Out-Host
    } else {
        & $bin @pullArgs
    }
    $pullExitCode = $LASTEXITCODE
    if ($pullExitCode -ne 0) {
        Write-Host "ccsync: pull failed (exit $pullExitCode)$(if ($verb -eq 'sync') { ', skipping push' })" -ForegroundColor Red
        exit $pullExitCode
    }

    if (-not $hasSyncState) {
        Write-Host "ccsync: no sync state yet, so there are no deletions to apply" -ForegroundColor DarkGray
    } elseif ($isQuiet) {
        Write-Warning "ccsync: deletions not applied: -q/--quiet hides the pull's report of cloud keys this machine cannot map; run without it to apply them"
    } else {
        $unmappedKeyCount = @($pullOutput | Where-Object { "$_" -match 'unknown path token' }).Count
        if ($unmappedKeyCount -gt 0) {
            Write-Warning "ccsync: deletions not applied: the pull reported $unmappedKeyCount cloud key(s) this machine cannot map (unknown path token); add the path_map entry first"
        } else {
            Invoke-DeletionSync -SyncBinary $bin -IsDryRun $isDryRun
        }
    }

    if ($isDryRun) { exit 0 }
    Write-Host "> claude-sync rebuild-history" -ForegroundColor DarkGray
    & $bin rebuild-history
    if ($LASTEXITCODE -ne 0) {
        Write-Host "ccsync: rebuild-history failed (exit $LASTEXITCODE)$(if ($verb -eq 'sync') { ', skipping push' })" -ForegroundColor Red
        exit $LASTEXITCODE
    }
    if ($verb -eq 'pull') { exit 0 }

    Write-Host "> claude-sync push" -ForegroundColor DarkGray
    & $bin push
    exit $LASTEXITCODE
}

$passthrough = @($args | ForEach-Object { [string]$_ })
Write-Host "> claude-sync $($passthrough -join ' ')" -ForegroundColor DarkGray
& $bin @passthrough
exit $LASTEXITCODE
