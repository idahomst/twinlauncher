# twinlauncher - the unofficial [TwinStar Launcher](https://twinstar-wow.com/launcher/latest.zip) alternative

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Latest release](https://img.shields.io/github/v/release/idahomst/twinlauncher)](https://github.com/idahomst/twinlauncher/releases/latest)

A lightweight Windows launcher for the [TwinStar](https://twinstar-wow.com) World of Warcraft private server, written in Go. Designed to replace the original .NET/WPF launcher with no .NET dependency.

Runs cleanly under Wine on Linux and macOS which was the main motivation for this launcher.

## Download

**[Download twinlauncher.exe](https://github.com/idahomst/twinlauncher/releases/latest/download/twinlauncher.exe)** (latest release, see [all releases](https://github.com/idahomst/twinlauncher/releases))

Use the link above or the *Assets* of a release. Saving the file from the repository's file view gives you an HTML page, not the program.

## What it does

- Selects the WoW expansion: **Cataclysm**, **Mists of Pandaria**, or **Vanilla**
- Refuses to update a folder that holds another expansion's client (version check like the original)
- Downloads and updates game files from the TwinStar CDN (resumable, with progress, picks up partial downloads of the original launcher)
- Lets you choose the game language for Mists of Pandaria (same list as TwinStar Launcher 1.0.3)
- Patches `Config.wtf` and `realmlist.wtf` with the correct realmlist before launching
- Launches the WoW client (`Wow-64.exe`, `Wow.exe`, or `WoW.exe`)
- Saves your settings (expansion, paths, realmlist) between sessions

Settings are stored in `twinlauncher_settings.json` next to the exe.

## Screenshot
![twinlauncher screenshot](https://github.com/idahomst/twinlauncher/blob/main/twinlauncher.png?raw=true)

## Setup

Requirements: a 64-bit Wine bottle (Bottles, plain Wine, ...) or a Windows machine. No extra dependencies, fonts or .NET are needed.

1. [Download `twinlauncher.exe`](https://github.com/idahomst/twinlauncher/releases/latest/download/twinlauncher.exe)
2. Copy it into your bottle **next to** `appsettings.json` from the original [TwinStar Launcher](https://twinstar-wow.com/launcher/latest.zip) (the folder with `TwinStar Launcher.exe`, or copy just `appsettings.json` next to twinlauncher)
3. Run `twinlauncher.exe` via Wine or Bottles
4. Select your expansion (and for MoP the game language) and set the game path (Windows-style path, e.g. `C:\Games\WoW_Cata`)
5. Click **Check & Update** to download/verify game files
6. Click **Play** to launch WoW

## Building from source

Requirements: Go 1.24 or later (`build.sh` downloads it if missing). The build is pure Go (no CGO, no mingw).

```bash
bash build.sh
```

or directly:

```bash
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 \
  go build -trimpath -ldflags="-s -w -H windowsgui" -o twinlauncher.exe .
```

The Windows application manifest (common controls v6, DPI awareness) lives in `twinlauncher.manifest` and is embedded via `rsrc_windows_amd64.syso`. After editing the manifest, regenerate it:

```bash
go run github.com/akavel/rsrc@v0.10.2 -manifest twinlauncher.manifest -arch amd64 -o rsrc_windows_amd64.syso
```

### Tests

The package only builds for Windows, so run the tests under Wine:

```bash
GOOS=windows go test -exec wine .
```

### Releasing

The exe is not committed to the repository; it is published as a release asset:

```bash
bash build.sh
sha256sum twinlauncher.exe > SHA256SUMS.txt
gh release create vX.Y.Z twinlauncher.exe SHA256SUMS.txt --target main --title "twinlauncher vX.Y.Z" --generate-notes
```

## License

MIT — see [LICENSE](LICENSE).

## Credits

Code written by [Claude](https://claude.ai) (Anthropic), directed and tested by [idaho@mst.cz](mailto:idaho@mst.cz).
