#!/usr/bin/env bash
# build.sh — Install Go locally (no root) and cross-compile twinlauncher.exe
#
# Usage:  bash build.sh
# Output: twinlauncher.exe  (copy it next to appsettings.json in your WoW bottle)

set -e

GO_VERSION="1.22.4"
GO_TARBALL="go${GO_VERSION}.linux-amd64.tar.gz"
GO_URL="https://go.dev/dl/${GO_TARBALL}"
GO_DIR="${HOME}/.local/go"
GOPATH="${HOME}/.local/gopath"

# ── 1. Install Go if not present ─────────────────────────────────────────────
if [ ! -x "${GO_DIR}/bin/go" ]; then
    echo ">>> Downloading Go ${GO_VERSION}..."
    TMP=$(mktemp -d)
    curl -fL "${GO_URL}" -o "${TMP}/${GO_TARBALL}"
    echo ">>> Extracting to ${GO_DIR}..."
    mkdir -p "${HOME}/.local"
    tar -C "${HOME}/.local" -xzf "${TMP}/${GO_TARBALL}"
    rm -rf "${TMP}"
    echo ">>> Go installed at ${GO_DIR}"
else
    echo ">>> Go already at ${GO_DIR}  ($("${GO_DIR}/bin/go" version))"
fi

export PATH="${GO_DIR}/bin:${PATH}"
export GOPATH="${GOPATH}"

# ── 2. Check for mingw-w64 (required for CGO cross-compilation) ───────────────
if ! command -v x86_64-w64-mingw32-gcc &>/dev/null; then
    echo "ERROR: x86_64-w64-mingw32-gcc not found."
    echo "Install it with:  sudo apt install mingw-w64"
    exit 1
fi

# ── 3. Cross-compile for Windows amd64 ───────────────────────────────────────
echo ">>> Building twinlauncher.exe  (CGO + GOOS=windows GOARCH=amd64)..."
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "${SCRIPT_DIR}"

CGO_ENABLED=1 CC=x86_64-w64-mingw32-gcc GOOS=windows GOARCH=amd64 go build \
    -ldflags="-s -w -H windowsgui" \
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
