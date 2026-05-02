# U.GG Memory Wrangler

A lightweight Windows utility that restarts [U.GG](https://u.gg) to reclaim memory that accumulates during long sessions. Double-click to run — no configuration required.

---

## Why does this exist?

U.GG is an Electron-based desktop app. Like many Electron apps, it tends to hold onto RAM over time. After a few hours of play, it can balloon to several hundred MB or more. Restarting it manually takes only a few seconds but frees that memory immediately. This tool automates that with a single click and shows you exactly how much was reclaimed.

---

## How to use

### Option 1 — Download the pre-built release (easiest)

1. Go to the [**Releases**](../../releases/latest) page
2. Download `ugg-memory-wrangler.exe`
3. _(Optional)_ Verify the download by comparing the file's SHA-256 hash against `checksums.txt` in the same release
4. Place the `.exe` anywhere convenient (e.g. your Desktop or a tools folder)
5. **Double-click** `ugg-memory-wrangler.exe` while U.GG is running

> **Note on Windows SmartScreen:** Because this executable is not signed with a paid code-signing certificate, Windows may show a "Windows protected your PC" warning the first time you run it. Click **More info → Run anyway** to proceed. The warning disappears as the release accumulates download reputation. If you prefer not to bypass SmartScreen, use Option 2 and build from source.

### Option 2 — Build from source

Requires [Go 1.22+](https://go.dev/dl/) installed.

```powershell
git clone https://github.com/kevinslawinski/ugg-memory-wrangler.git
cd ugg-memory-wrangler

# GUI build (no console window — recommended for double-click use)
go build -ldflags="-H windowsgui -s -w" -o ugg-memory-wrangler.exe .

# Debug / CLI build (console window visible, useful for flag-based usage)
go build -o ugg-memory-wrangler.exe .
```

Binaries you build yourself are treated as locally-trusted by Windows and skip SmartScreen entirely.

---

## What it does

1. Detects the running `U.GG.exe` process and records its path
2. Measures U.GG's current memory usage (Working Set – Private, matching Task Manager)
3. Terminates U.GG and waits for it to fully exit
4. Relaunches U.GG from the saved path and waits for its window to appear
5. Waits a short warmup period, then measures memory again
6. Reports how much was freed, and tracks a running lifetime total

A live progress window appears during the restart and shows the before/after figures when done. A Windows toast notification is also fired with the summary.

---

## Advanced usage (CLI flags)

When run from a terminal instead of double-clicking, the following flags are available:

| Flag            | Default                         | Description                                                                   |
| --------------- | ------------------------------- | ----------------------------------------------------------------------------- |
| `-name`         | `U.GG.exe`                      | Image name of the process to manage                                           |
| `-path`         | _(auto-detect)_                 | Full path to the U.GG executable                                              |
| `-delay`        | `2s`                            | Wait time after kill before restarting                                        |
| `-warmup`       | `5s`                            | Wait time after launch before measuring memory                                |
| `-data-dir`     | `%APPDATA%\ugg-memory-wrangler` | Directory for metrics and logs                                                |
| `-kill-only`    | `false`                         | Terminate without restarting                                                  |
| `-popup`        | `false`                         | Show result popup even in CLI mode                                            |
| `-wait`         | `false`                         | Wait for Enter before exiting (useful when run by double-click in a terminal) |
| `-track-memory` | `true`                          | Measure before/after memory and persist lifetime totals                       |

> **Note:** The release build uses `-H windowsgui`, which suppresses the console window for clean double-click operation. CLI flag output is not visible with that build. Use a debug build (Option 2, no `-H windowsgui`) for flag-based usage.

**Examples:**

```powershell
# Kill and restart with a longer warmup
ugg-memory-wrangler.exe -warmup 10s

# Kill only, no restart
ugg-memory-wrangler.exe -kill-only

# Specify exact path if auto-detection fails
ugg-memory-wrangler.exe -path "C:\Users\you\AppData\Local\Programs\U.GG\U.GG.exe"
```

---

## Data & privacy

All data stays on your machine. Nothing is sent anywhere.

| File           | Location                         | Contents                                              |
| -------------- | -------------------------------- | ----------------------------------------------------- |
| `config.json`  | `%APPDATA%\ugg-memory-wrangler\` | Saved U.GG executable path                            |
| `metrics.json` | `%APPDATA%\ugg-memory-wrangler\` | Lifetime run count and total bytes freed              |
| `runs.log`     | `%APPDATA%\ugg-memory-wrangler\` | TSV log of each run (timestamp, before, after, freed) |

To reset all data, delete the `%APPDATA%\ugg-memory-wrangler\` folder.

---

## Requirements

- Windows 10 or 11
- U.GG desktop app installed
- PowerShell 5.1+ (included with Windows — no extra install needed)

---

## Building a release (maintainer notes)

Releases use a two-step process to satisfy GitHub's immutable release setting (assets must be uploaded before a release is published):

**Step 1 — Push a version tag.** The Actions workflow builds `ugg-memory-wrangler.exe`, generates `checksums.txt`, and creates a **draft** release with both files attached.

```powershell
git tag v1.0.0
git push origin v1.0.0
```

**Step 2 — Publish the draft.** Go to the repo's [Releases](../../releases) page, review the auto-generated release notes, and click **Publish release**.

Tags containing a `-` (e.g. `v1.0.0-rc.1`) are automatically marked as pre-releases.

---

## Tech stack

- **Go 1.22** — single source file (`main.go`), no external dependencies
- **Windows syscalls** (`user32.dll`, `kernel32.dll`) for native dialogs
- **PowerShell + WPF** for the live progress window and toast notifications
- **GitHub Actions** for automated release builds
