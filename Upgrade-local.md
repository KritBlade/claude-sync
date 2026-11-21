# Upgrading this fork to a new upstream release

This repository is a fork. `main` is always **the latest upstream release plus this fork's own commits on top**.
It is brought up to date only when upstream ships a release, by rebasing; never by merging. Read this before any
`git pull`, upgrade or re-clone.

- `origin`: https://github.com/KritBlade/claude-sync (this fork; push here)
- `upstream`: https://github.com/tawanorg/claude-sync (the original project; only fetch from here)

## What this fork carries

The commits above the release tag are the fork's own work. List them with:

```bash
git log --oneline $(git describe --tags --abbrev=0 main)..main
```

They cover:

- **Drive-letter case on Windows** (`internal/sync/paths.go`, `state.go`, `migrate.go`): one project folder is one
  identity whichever case its drive letter has.
- **500MB download cap and large-session warning** (`internal/storage/storage.go`, `internal/sync/sync.go`,
  `cmd/claude-sync/main.go`).
- **Windows test isolation** (`cmd/claude-sync/backup_test.go`, `internal/config/config_test.go`): tests set
  `USERPROFILE` as well as `HOME`, so they never touch the real `~/.claude`.
- **Junctions skipped when listing files** (`internal/sync/state.go`).
- **The ccsync wrapper** (`ccsync/`): bash and PowerShell copies, their tests, `.gitattributes`, `.npmignore`.

When upstream adopts one of these, the rebase drops this fork's copy by itself. Then remove its line here.

## Never do these

- `git pull upstream main` or `git merge upstream/...`: it merges upstream's history into `main` with a merge
  commit, and the fork stops being "release + own commits on top".
- Re-clone from upstream: the fork's commits are not there.
- `git push --force` without `--with-lease`.
- Enable GitHub Actions on the fork: `release.yml` would then run semantic-release on every push to `main`, which
  tags, commits a changelog and tries to publish to npm.

## Upgrading to a new release (e.g. v1.18.0)

Run these in the repository on one machine.

1. **Fetch the release** (downloads only, changes nothing):
   ```bash
   git fetch upstream --tags
   ```
2. **Keep a way back:**
   ```bash
   git branch backup/before-v1.18.0 main
   ```
3. **Replay the fork's commits onto the release:**
   ```bash
   git switch main
   git rebase v1.18.0
   ```
   If git stops on a conflict: fix the file, `git add <file>`, then `git rebase --continue`.
   `git rebase --abort` returns everything to how it was. Conflicts are most likely in
   `internal/sync/paths.go` and `internal/sync/state.go`, where upstream's path mapping and this fork's
   drive-letter and junction changes meet. Keep upstream's change and re-apply the fork's on top of it.
4. **Test.** See "Testing on Windows" below. Compare against a clean run of the release tag: the same tests may
   fail on both, none may fail only on `main`.
   ```bash
   go test ./...
   bash ccsync/tests/run-scenarios.sh
   ```
5. **Check ccsync against the new claude-sync.** ccsync reads claude-sync's text output, and the scenario tests use
   a fake claude-sync, so they cannot catch an upstream format change. ccsync depends on:
   - `claude-sync diff`: the `Local only (N files):` header, the `  + <path> (<size>)` lines and the
     `Summary: N synced, N local only, N remote only, N modified` line (the `diff` command in
     `cmd/claude-sync/main.go`);
   - the `unknown path token` error text (`internal/sync/sync.go`);
   - the pull flags `--dry-run`, `--rebuild-history` and `-q`/`--quiet`.

   If any of these changed, update both `ccsync/ccsync` and `ccsync/ccsync.ps1`, and the fakes in
   `ccsync/tests/`. Then bump the ccsync version in both copies (the header line and `CCSYNC_VERSION` /
   `$CcsyncVersion`), e.g. to `v1.18.0-patch1`, and commit.
6. **Build** (same flags as the Makefile):
   ```bash
   V=$(git describe --tags --always --dirty)
   GOOS=windows GOARCH=amd64 go build -ldflags "-s -w -X main.version=$V" -o bin/claude-sync-windows-amd64.exe ./cmd/claude-sync
   GOOS=darwin GOARCH=arm64 go build -ldflags "-s -w -X main.version=$V" -o bin/claude-sync-darwin-arm64 ./cmd/claude-sync
   ```
7. **Install.** Keep each replaced file beside the new one with its version as a suffix, so going back is a rename.
   - Builds folder (this PC): `H:\Github\Dev\misc\claude-sync-builds`, holding `claude-sync-windows-amd64.exe`,
     `claude-sync-darwin-arm64` and `ccsync-darwin` (a copy of `ccsync/ccsync`).
   - Windows: `C:\Users\Goten\go\bin\claude-sync.exe` and `C:\Users\Goten\.local\bin\ccsync.ps1` (plus `ccsync.cmd`).
   - Mac: copy `claude-sync-darwin-arm64` and `ccsync-darwin` over the files `command -v claude-sync` and
     `command -v ccsync` show, then `chmod +x` both (Windows does not keep the execute bit).
   - Then run `ccsync pull --dry-run` once on each machine: it changes nothing, and it warns if it cannot parse
     claude-sync's output.
8. **Push.** The rebase rewrote `main`, so the push has to replace it; `--force-with-lease` refuses if GitHub holds
   something this machine has not seen:
   ```bash
   git push --force-with-lease origin main
   ```
9. Once the new build has run well for a while: `git branch -D backup/before-v1.18.0`.

## On the other machine after an upgrade

A plain `git pull` there would try to merge the old `main` with the rewritten one. If that clone has no work of
its own that is not on GitHub, move it to the new `main`:

```bash
git fetch origin
git status                    # must show nothing to commit
git reset --hard origin/main
```

## Setting up a new machine

```bash
git clone https://github.com/KritBlade/claude-sync
cd claude-sync
git remote add upstream https://github.com/tawanorg/claude-sync.git
git fetch upstream --tags
```

## Testing on Windows

- Tests that open sockets (`cmd/claude-sync`, `internal/storage/webdav`) are blocked by the firewall when run with
  plain `go test`, because each run builds the test program under a new random path. Build them to fixed paths
  that are allowed in the firewall, and run each from its package folder:
  ```bash
  go test -c -o bin/tests/webdav.test.exe ./internal/storage/webdav/
  go test -c -o bin/tests/claude-sync-cmd.test.exe ./cmd/claude-sync/
  (cd internal/storage/webdav && ../../../bin/tests/webdav.test.exe)
  (cd cmd/claude-sync && ../../bin/tests/claude-sync-cmd.test.exe)
  ```
  Run every other package with `go test`.
- Four tests expect Unix file modes and fail on Windows, upstream's tree included:
  `TestCreateBackupSetsRestrictivePermissions`, `TestSaveFilePermissions`, `TestCrossDeviceSessionSync` and
  `TestPullSetsRestrictivePermissions`.
- `gofmt -l .` lists every file because the checkout has CRLF line endings. Check the committed content instead:
  `git show HEAD:<file> | gofmt -l`.

## Sending fixes upstream

Every fork commit upstream merges is one fewer to carry through each rebase. The junction fix and the Windows test
isolation are general bugs and the best candidates for a pull request to `tawanorg/claude-sync`.
