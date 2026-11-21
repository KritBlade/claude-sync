# antigravity-sync

Syncs Antigravity conversations between devices, Windows and macOS included, through claude-sync's encrypted
bucket. The folder paths inside each conversation are converted for the receiving device, so a conversation
started on one machine opens on the other in the same project, with its uploaded images and its workspace.

```
antigravity-sync pull   [--dry-run] [--confirm-deletions] [--stop-antigravity]
antigravity-sync push   [--dry-run] [--confirm-deletions]
antigravity-sync status          # what pull and push would do
antigravity-sync version
```

`ccsync` runs it after claude-sync on `pull`, `push` and `sync`, when it is on PATH.

## What it syncs

Under `~/.gemini` (`%USERPROFILE%\.gemini` on Windows):

- `antigravity/conversations/<id>.db`: a conversation (SQLite). Older conversations are `<id>.pb` files, which
  Antigravity stores in a form this tool cannot read, so they travel unchanged and keep their original paths.
- `antigravity/brain/<id>/`: the conversation's plans, notes, logs and uploaded images.
- `antigravity/annotations/<id>.pbtxt`: its title and when it was last viewed.
- `config/projects/<id>.json`: the projects, which tie conversations to workspace folders.

Not synced: SQLite's `-wal`/`-shm` side files, `config/projects/default-cli-project.json` (every installation
makes its own), and everything else under `antigravity/` (browser recordings, logs, caches, the search index
`conversation_summaries.db`, which Antigravity rebuilds for conversations it finds).

In the bucket the files live under `_external/antigravity/`, encrypted with claude-sync's key. claude-sync's own
pull and diff skip `_external/`, so the two tools never see each other's files.

## Path conversion

Each device replaces its own folders with tokens on push, and puts its own folders back on pull. The folders
come from claude-sync's config: the home folder is `HOME`, and every `path_map` entry is its token. Both devices
must use the same token for the same project folder, e.g.

```yaml
path_map:                      # Windows                       # macOS
  H:\Github\Dev: WORK          #                               /Users/goten/Github/Dev: WORK
```

A folder is found in every spelling Antigravity writes: `H:\Github\Dev`, JSON-escaped `H:\\Github\\Dev`,
`H:/Github/Dev`, `file:///h%3A/Github/Dev` and `file:///h:/Github/Dev`. Inside a conversation database the paths
sit in protobuf string fields; each changed field gets its length rewritten. On Windows, paths are written back
with forward slashes (valid everywhere, and safe inside JSON), and workspace URIs as `file:///h%3A/...`. A token
the receiving device has no folder for is left as it is and reported.

## Safety

- **Pull needs Antigravity closed**, because it replaces files Antigravity holds open. It refuses while the
  Antigravity app or an agy hub using `~/.gemini/antigravity` is running. `--stop-antigravity` stops the hub that
  editor extensions start (the extension starts it again); the app must be closed by hand. Push reads
  conversations through SQLite and can run while Antigravity is open.
- **Change detection** compares each file with what was last synced. A conversation is compared by its rows, so
  Antigravity opening it (which rewrites the file's header) does not count as a change.
- **Changed on both devices**: pull keeps this device's copy and saves the cloud's copy to
  `~/.claude-sync/antigravity/conflicts/<time>/`; the next push uploads this device's. Push skips any file that
  changed in the cloud since this device last synced, until a pull has run. Title files changed on both sides keep
  this device's copy without saving the other.
- **Deletions**: a file deleted on one device is moved to `~/.claude-sync/antigravity/trash/<time>/` on the other.
  Nothing is deleted when the cloud holds no Antigravity files at all, and a run that would delete more than 25%
  of the synced files stops until it is repeated with `--confirm-deletions`.
- State lives in `~/.claude-sync/antigravity/state.json`.

## Build and test

This folder is its own Go module. It imports claude-sync's config, encryption and storage packages through a
`replace` to the parent folder, so it changes no claude-sync file, and its SQLite dependency stays out of
claude-sync's `go.mod`. A root `go test ./...` does not include it.

```bash
cd antigravity-sync
go test ./...
ANTIGRAVITY_SYNC_SAMPLE_DATABASE=/path/to/windows/conversation.db go test -run Real -v ./...   # optional, real data
V=$(git describe --tags --always --dirty)
GOOS=darwin  GOARCH=arm64 go build -ldflags "-s -w -X main.version=$V" -o bin/antigravity-sync-darwin-arm64 .
GOOS=windows GOARCH=amd64 go build -ldflags "-s -w -X main.version=$V" -o bin/antigravity-sync-windows-amd64.exe .
```

It depends on Antigravity's own storage, which Antigravity can change in any release: the folder layout above,
conversations as SQLite files with protobuf blobs, and the workspace URI in `trajectory_metadata_blob`. A protobuf
field it cannot interpret is left unconverted and reported as a warning on push.
