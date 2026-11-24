#!/usr/bin/env bash
# build.sh — cross-compile twinlauncher.exe (installs Go locally if needed, no root)
#
# Usage:  bash build.sh
# Output: twinlauncher.exe  (copy it next to appsettings.json in your WoW bottle)

set -e

GO_VERSION="1.24.4"   # keep in sync with the toolchain line in go.mod
GO_TARBALL="go${GO_VERSION}.linux-amd64.tar.gz"
GO_URL="https://go.dev/dl/${GO_TARBALL}"
GO_DIR="${HOME}/.local/go"

# ── 1. Find or install Go ────────────────────────────────────────────────────
if command -v go &>/dev/null; then
    GO=go
elif [ -x "${GO_DIR}/bin/go" ]; then
    GO="${GO_DIR}/bin/go"
else
    echo ">>> Downloading Go ${GO_VERSION}..."
    TMP=$(mktemp -d)
    curl -fL "${GO_URL}" -o "${TMP}/${GO_TARBALL}"
    echo ">>> Extracting to ${GO_DIR}..."
    mkdir -p "${HOME}/.local"
    tar -C "${HOME}/.local" -xzf "${TMP}/${GO_TARBALL}"
    rm -rf "${TMP}"
    GO="${GO_DIR}/bin/go"
fi
echo ">>> Using $("${GO}" version)"

# ── 2. Cross-compile for Windows amd64 (pure Go, no CGO / mingw needed) ──────
# The application manifest (common controls v6, DPI awareness) is embedded
# from rsrc_windows_amd64.syso. Regenerate it after editing twinlauncher.manifest:
#   go run github.com/akavel/rsrc@v0.10.2 -manifest twinlauncher.manifest -arch amd64 -o rsrc_windows_amd64.syso
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "${SCRIPT_DIR}"

echo ">>> Building twinlauncher.exe (GOOS=windows GOARCH=amd64)..."
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 "${GO}" build \
    -trimpath -ldflags="-s -w -H windowsgui" \
    -o twinlauncher.exe \
    .

echo ""
echo ">>> Done!  Output: ${SCRIPT_DIR}/twinlauncher.exe"
echo ""
echo "Next steps:"
echo "  1. Copy twinlauncher.exe into your Wine bottle NEXT TO appsettings.json"
echo "     (same folder as the original TwinStar Launcher.exe)"
echo "  2. In Bottles / Wine: run twinlauncher.exe"
echo "  3. Select your expansion, set the game path, choose a realmlist."
echo "  4. Click 'Check && Update' to update, then 'Play' to launch WoW."
echo "  Settings are saved in twinlauncher_settings.json next to the exe."
