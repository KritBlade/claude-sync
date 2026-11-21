# ccsync

A wrapper around `claude-sync` and `antigravity-sync` for keeping two or more machines in step.

- `ccsync pull` runs `claude-sync pull`, moves the files the other machine deleted to `~/.claude-sync/trash/`,
  runs `claude-sync rebuild-history`, then runs `antigravity-sync pull`.
- `ccsync push` runs `claude-sync push`, then `antigravity-sync push`.
- `ccsync sync` runs the pull steps, then the push steps.
- Every other command passes straight through to `claude-sync`.

The Antigravity steps run only when `antigravity-sync` is on PATH (see `antigravity-sync/README.md`). ccsync runs
its pull with `--stop-antigravity`, which stops the agy hub an editor extension runs, and with it any Antigravity
task in progress; the extension starts the hub again when its panel is next used. The pull still refuses while
the Antigravity app runs. A failed Antigravity step does not stop the Claude steps; ccsync exits with its code at
the end. Which Claude files are moved, and when ccsync asks first, is described in the header of each script.

## Files

- `ccsync`: macOS and Linux. Needs bash and python3.
- `ccsync.ps1` and `ccsync.cmd`: Windows, under Windows PowerShell 5.1. `ccsync.cmd` starts the script.

The two copies implement the same behaviour and carry the same version, so change both together.

## Install

`claude-sync` must already be on PATH; put `antigravity-sync` on PATH too to sync Antigravity.

- macOS / Linux: copy `ccsync` into a folder on PATH, such as `~/.local/bin`, and make it executable
  (`chmod +x`).
- Windows: copy `ccsync.ps1` and `ccsync.cmd` into one folder on PATH, such as `%USERPROFILE%\.local\bin`.

## Test

```
bash ccsync/tests/run-scenarios.sh
```

This runs both copies through the same scenarios against fake `claude-sync` and `antigravity-sync` programs and
a throwaway home folder. The PowerShell copy runs only on Windows, under Git Bash.
