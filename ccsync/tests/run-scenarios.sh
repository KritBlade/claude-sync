#!/usr/bin/env bash
# Runs the bash and PowerShell ccsync copies through the same scenarios against a fake claude-sync and a
# throwaway home folder. HOME and USERPROFILE both point at the throwaway home, so even a real claude-sync
# would find no config there, and the run stops before any scenario unless each copy resolves the fake.
# The PowerShell copy runs only where powershell.exe exists (Windows, under Git Bash).
#
#   bash ccsync/tests/run-scenarios.sh [<folder holding ccsync/>] [<work folder>]
#
# The work folder defaults to a new temporary one, removed when every scenario passes and kept for
# inspection when one fails.
set -uo pipefail

HARNESS_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO="${1:-$(cd "$HARNESS_DIR/../.." && pwd)}"
if [ -n "${2:-}" ]; then
    WORK="$2"
    isWorkTemporary='no'
else
    WORK="$(mktemp -d "${TMPDIR:-/tmp}/ccsync-scenarios.XXXXXX")" || { echo "could not create a work folder"; exit 2; }
    isWorkTemporary='yes'
fi
IMPLEMENTATIONS='bash'
if command -v powershell.exe > /dev/null 2>&1; then
    IMPLEMENTATIONS='bash powershell'
fi
failures=0
passes=0

pass() { passes=$((passes + 1)); }
fail_case() { failures=$((failures + 1)); printf '  FAIL [%s] %s\n' "$IMPL/$SCENARIO" "$*"; }

assert_exists()  { [ -e "$1" ] && pass || fail_case "expected to exist: ${1#"$ROOT"/}"; }
assert_missing() { [ ! -e "$1" ] && pass || fail_case "expected to be gone: ${1#"$ROOT"/}"; }
assert_calls() {
    local expected="$1" actual
    actual="$(tr -d '\r' < "$FAKE_DIR/calls.log" | sed 's/[[:space:]]*$//' | paste -sd ';' -)"
    [ "$actual" = "$expected" ] && pass || fail_case "calls were [$actual], expected [$expected]"
}
assert_exit_code() { [ "$CCSYNC_EXIT_CODE" = "$1" ] && pass || fail_case "exit code $CCSYNC_EXIT_CODE, expected $1"; }
assert_output_has() { grep -qF -- "$1" "$ROOT/output.txt" && pass || fail_case "output lacks: $1"; }
trash_dir() { find "$HOME_DIR/.claude-sync/trash" -mindepth 1 -maxdepth 1 -type d 2>/dev/null | head -1; }

sha() { printf '%s' "$1" | python3 -c 'import sys, hashlib; print(hashlib.sha256(sys.stdin.buffer.read()).hexdigest())'; }

# Builds a fresh home with ~/.claude files, state.json entries and the fake's canned diff output.
# Each spec is "<relative path>|<content on disk or ->|<state: match, other, nohash, null, absent>".
new_fixture() {
    local syncedCount="$1"; shift
    ROOT="$WORK/$IMPL-$SCENARIO"
    rm -rf "$ROOT"; mkdir -p "$ROOT"
    HOME_DIR="$ROOT/home"; FAKE_DIR="$ROOT/fake"; TMP_DIR="$ROOT/tmp"
    mkdir -p "$HOME_DIR/.claude" "$HOME_DIR/.claude-sync" "$FAKE_DIR" "$TMP_DIR"
    : > "$FAKE_DIR/calls.log"
    local stateEntries='' diffEntries='' count=0 spec path content stateKind entry
    for spec in "$@"; do
        IFS='|' read -r path content stateKind <<< "$spec"
        if [ "$content" != '-' ]; then
            mkdir -p "$(dirname "$HOME_DIR/.claude/$path")"
            printf '%s' "$content" > "$HOME_DIR/.claude/$path"
        fi
        case "$stateKind" in
            match)  entry="\"$path\": {\"hash\": \"$(sha "$content")\"}" ;;
            other)  entry="\"$path\": {\"hash\": \"$(sha "content as last synced")\"}" ;;
            nohash) entry="\"$path\": {\"size\": 1}" ;;
            null)   entry="\"$path\": null" ;;
            absent) entry='' ;;
        esac
        if [ -n "$entry" ]; then
            stateEntries="${stateEntries:+$stateEntries, }$entry"
        fi
        diffEntries="$diffEntries  + $path (1 B)"$'\n'
        count=$((count + 1))
    done
    printf '{"files": {%s}}' "$stateEntries" > "$HOME_DIR/.claude-sync/state.json"
    {
        printf 'Local only (%d files):\n%s\n' "$count" "$diffEntries"
        printf 'Summary: %d synced, %d local only, 0 remote only, 0 modified\n' "$syncedCount" "$count"
    } > "$FAKE_DIR/diff.txt"
    printf 'Pull complete\n' > "$FAKE_DIR/pull.txt"
}

run_ccsync() {
    local fakeBin
    if [ "$IMPL" = bash ]; then
        fakeBin="$HARNESS_DIR/fake-bash"
        HOME="$HOME_DIR" USERPROFILE="$HOME_DIR" TMPDIR="$TMP_DIR" FAKE_DIR="$FAKE_DIR" PATH="$fakeBin:$PATH" \
            bash "$REPO/ccsync/ccsync" "$@" < /dev/null > "$ROOT/output.txt" 2>&1
        CCSYNC_EXIT_CODE=$?
    else
        fakeBin="$HARNESS_DIR/fake-cmd"
        HOME="$HOME_DIR" USERPROFILE="$HOME_DIR" FAKE_DIR="$(cygpath -w "$FAKE_DIR")" PATH="$fakeBin:$PATH" \
            powershell.exe -NoProfile -ExecutionPolicy Bypass -File "$(cygpath -w "$REPO/ccsync/ccsync.ps1")" "$@" \
            < /dev/null > "$ROOT/output.txt" 2>&1
        CCSYNC_EXIT_CODE=$?
    fi
}

# Refuses to run anything unless the fakes are the claude-sync and antigravity-sync each copy resolves.
check_fake_resolves() {
    local resolved program
    for program in claude-sync antigravity-sync; do
        resolved="$(PATH="$HARNESS_DIR/fake-bash:$PATH" command -v "$program")"
        [ "$resolved" = "$HARNESS_DIR/fake-bash/$program" ] || { echo "bash resolves $resolved, not the fake $program"; exit 2; }
    done
    case " $IMPLEMENTATIONS " in
        *' powershell '*) ;;
        *) return 0 ;;
    esac
    for program in claude-sync antigravity-sync; do
        resolved="$(PATH="$HARNESS_DIR/fake-cmd:$PATH" powershell.exe -NoProfile -Command "(Get-Command $program).Source" | tr -d '\r')"
        case "$resolved" in
            *fake-cmd\\$program.cmd) ;;
            *) echo "PowerShell resolves $resolved, not the fake $program"; exit 2 ;;
        esac
    done
}

check_fake_resolves

for IMPL in $IMPLEMENTATIONS; do
    SCENARIO=classify
    new_fixture 100 \
        'projects/p1/s1/deleted.jsonl|gone elsewhere|match' \
        'projects/p1/s1/edited.jsonl|edited here|other' \
        'projects/p1/s1/notes.jsonl.conflict.20261121-120000|remote side|match' \
        'projects/p1/s1/new.jsonl|never synced|absent' \
        'projects/p1/s1/nohash.jsonl|no hash|nohash' \
        'projects/p1/s1/nullentry.jsonl|null entry|null' \
        'projects/../../escape.txt|-|absent'
    run_ccsync pull
    assert_missing "$HOME_DIR/.claude/projects/p1/s1/deleted.jsonl"
    trash="$(trash_dir)"
    assert_exists "$trash/projects/p1/s1/deleted.jsonl"
    assert_exists "$trash.moved.txt"
    assert_missing "$trash/moved.txt"
    for kept in edited.jsonl notes.jsonl.conflict.20261121-120000 new.jsonl nohash.jsonl nullentry.jsonl; do
        assert_exists "$HOME_DIR/.claude/projects/p1/s1/$kept"
    done
    assert_output_has 'escapes ~/.claude'
    assert_output_has 'no hash recorded in state.json'
    assert_output_has 'left 1 conflict copy'
    assert_output_has 'kept 1 file(s) the other machine deleted'
    assert_calls 'pull;diff;rebuild-history;antigravity-sync pull'
    assert_exit_code 0

    SCENARIO=prune
    new_fixture 100 'projects/p1/s2/only.jsonl|only file|match'
    run_ccsync pull
    assert_missing "$HOME_DIR/.claude/projects/p1/s2"
    assert_exists "$HOME_DIR/.claude/projects/p1"

    SCENARIO=share-just-over
    specs=(); for n in $(seq 1 13); do specs+=("projects/p1/s1/f$n.jsonl|file $n|match"); done
    new_fixture 51 "${specs[@]}"
    run_ccsync pull
    assert_exists "$HOME_DIR/.claude/projects/p1/s1/f1.jsonl"
    assert_output_has 'nothing moved'
    assert_output_has '25.5%'

    SCENARIO=share-exactly-25
    new_fixture 52 "${specs[@]}"
    run_ccsync pull
    assert_missing "$HOME_DIR/.claude/projects/p1/s1/f1.jsonl"

    SCENARIO=dry-run-equals-true
    new_fixture 100 'projects/p1/s1/deleted.jsonl|gone elsewhere|match'
    run_ccsync sync --dry-run=true
    assert_exists "$HOME_DIR/.claude/projects/p1/s1/deleted.jsonl"
    assert_output_has 'would move 1 file(s)'
    assert_output_has 'a dry run cannot see cloud keys'
    assert_calls 'pull --dry-run=true;diff;antigravity-sync pull --dry-run'

    SCENARIO=dry-run-count-trigger
    specs=(); for n in $(seq 1 21); do specs+=("projects/p1/s1/f$n.jsonl|file $n|match"); done
    new_fixture 1000 "${specs[@]}"
    run_ccsync pull --dry-run
    assert_output_has 'and would ask first'

    SCENARIO=dry-run-equals-false
    new_fixture 100 'projects/p1/s1/deleted.jsonl|gone elsewhere|match'
    run_ccsync pull --dry-run=false
    assert_missing "$HOME_DIR/.claude/projects/p1/s1/deleted.jsonl"
    assert_calls 'pull --dry-run=false;diff;rebuild-history;antigravity-sync pull'

    SCENARIO=quiet
    new_fixture 100 'projects/p1/s1/deleted.jsonl|gone elsewhere|match'
    run_ccsync sync -q
    assert_exists "$HOME_DIR/.claude/projects/p1/s1/deleted.jsonl"
    assert_output_has 'deletions not applied: -q/--quiet'
    assert_calls 'pull -q;rebuild-history;antigravity-sync pull;push;antigravity-sync push'

    SCENARIO=unknown-token
    new_fixture 100 'projects/p1/s1/deleted.jsonl|gone elsewhere|match'
    printf 'Errors:\n  - projects/${WORK}/x.jsonl: unknown path token; add the matching path_map entry on this device\n' > "$FAKE_DIR/pull.txt"
    run_ccsync pull
    assert_exists "$HOME_DIR/.claude/projects/p1/s1/deleted.jsonl"
    assert_output_has 'cannot map (unknown path token)'

    SCENARIO=help
    new_fixture 100 'projects/p1/s1/deleted.jsonl|gone elsewhere|match'
    run_ccsync pull -h
    assert_exists "$HOME_DIR/.claude/projects/p1/s1/deleted.jsonl"
    assert_calls 'pull -h'

    SCENARIO=rebuild-history-flag
    new_fixture 100 'projects/p1/s1/kept.jsonl|edited here|other'
    run_ccsync pull --rebuild-history=true
    assert_calls 'pull;diff;rebuild-history;antigravity-sync pull'

    SCENARIO=push
    new_fixture 100 'projects/p1/s1/kept.jsonl|edited here|other'
    run_ccsync push --include-mcp
    assert_calls 'push --include-mcp;antigravity-sync push'
    assert_exit_code 0

    SCENARIO=stop-antigravity
    new_fixture 100 'projects/p1/s1/kept.jsonl|edited here|other'
    run_ccsync pull --stop-antigravity
    assert_calls 'pull;diff;rebuild-history;antigravity-sync pull --stop-antigravity'

    SCENARIO=antigravity-failure
    new_fixture 100 'projects/p1/s1/kept.jsonl|edited here|other'
    printf '3' > "$FAKE_DIR/antigravity-exit-code"
    run_ccsync sync
    assert_calls 'pull;diff;rebuild-history;antigravity-sync pull;push;antigravity-sync push'
    assert_output_has 'antigravity-sync pull failed (exit 3)'
    assert_exit_code 3

    SCENARIO=version
    new_fixture 100 'projects/p1/s1/kept.jsonl|edited here|other'
    run_ccsync version
    assert_calls '--version;antigravity-sync version'
    assert_output_has 'ccsync v1.17.2-patch2'

    SCENARIO=passthrough
    new_fixture 100 'projects/p1/s1/kept.jsonl|edited here|other'
    run_ccsync status --verbose
    assert_calls 'status --verbose'
    if [ "$IMPL" = bash ]; then
        leftover="$(find "$TMP_DIR" -mindepth 1 -maxdepth 1 | head -1)"
        [ -z "$leftover" ] && pass || fail_case "passthrough left a temp folder: $leftover"
    fi
done

printf '%d passed, %d failed (%s)\n' "$passes" "$failures" "$IMPLEMENTATIONS"
if [ "$failures" -ne 0 ]; then
    printf 'work folder kept for inspection: %s\n' "$WORK"
    exit 1
fi
if [ "$isWorkTemporary" = 'yes' ]; then
    rm -rf "$WORK"
fi
