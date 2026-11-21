# ccsync

A wrapper around `claude-sync` for keeping two or more machines in step.

- `ccsync pull` runs `claude-sync pull`, moves the files the other machine deleted to `~/.claude-sync/trash/`,
  then runs `claude-sync rebuild-history`.
- `ccsync sync` does the same, then runs `claude-sync push`.
- Every other command passes straight through to `claude-sync`.

Which files are moved, and when ccsync asks first, is described in the header of each script.

## Files

- `ccsync`: macOS and Linux. Needs bash and python3.
- `ccsync.ps1` and `ccsync.cmd`: Windows, under Windows PowerShell 5.1. `ccsync.cmd` starts the script.

The two copies implement the same behaviour and carry the same version, so change both together.

## Install

`claude-sync` must already be on PATH.

- macOS / Linux: copy `ccsync` into a folder on PATH, such as `~/.local/bin`, and make it executable
  (`chmod +x`).
- Windows: copy `ccsync.ps1` and `ccsync.cmd` into one folder on PATH, such as `%USERPROFILE%\.local\bin`.

## Test

```
bash ccsync/tests/run-scenarios.sh
```

This runs both copies through the same scenarios against a fake `claude-sync` and a throwaway home folder. The
PowerShell copy runs only on Windows, under Git Bash.
