#!/bin/bash
set -euo pipefail

# klax installer
# Usage: curl -fsSL https://raw.githubusercontent.com/PiDmitrius/klax/main/install.sh | bash

REPO="PiDmitrius/klax"
INSTALL_DIR="$HOME/.local/bin"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
NC='\033[0m'

info()  { echo -e "${GREEN}[+]${NC} $*"; }
warn()  { echo -e "${YELLOW}[!]${NC} $*"; }
fail()  { echo -e "${RED}[x]${NC} $*"; exit 1; }

# --- Detect architecture ---

ARCH=$(uname -m)
case "$ARCH" in
    x86_64)  ARCH="amd64" ;;
    aarch64) ARCH="arm64" ;;
    *)       fail "unsupported architecture: $ARCH" ;;
esac

# --- Get latest version ---

info "checking latest version..."
VERSION=$(curl -sfI "https://github.com/${REPO}/releases/latest" | grep -i ^location: | sed 's|.*/v||' | tr -d '\r')
if [ -z "$VERSION" ]; then
    fail "could not determine latest version"
fi
TAG="v${VERSION}"
info "latest: ${TAG}"

# --- Download binary ---

URL="https://github.com/${REPO}/releases/download/${TAG}/klax-${TAG}-linux-${ARCH}"
info "downloading klax-${TAG}-linux-${ARCH}..."

mkdir -p "$INSTALL_DIR"
if ! curl -sfL "$URL" -o "${INSTALL_DIR}/klax"; then
    fail "download failed: ${URL}"
fi
chmod +x "${INSTALL_DIR}/klax"
info "installed: ${INSTALL_DIR}/klax"

# --- Ensure ~/.local/bin is in PATH ---

if ! echo "$PATH" | tr ':' '\n' | grep -qx "$INSTALL_DIR"; then
    warn "${INSTALL_DIR} is not in your PATH"

    SHELL_NAME=$(basename "$SHELL")
    case "$SHELL_NAME" in
        bash) RC="$HOME/.bashrc" ;;
        zsh)  RC="$HOME/.zshrc" ;;
        *)    RC="" ;;
    esac

    EXPORT_LINE="export PATH=\"${INSTALL_DIR}:\$PATH\""

    if [ -n "$RC" ] && [ -f "$RC" ]; then
        if ! grep -qF "$INSTALL_DIR" "$RC"; then
            echo "" >> "$RC"
            echo "# Added by klax installer" >> "$RC"
            echo "$EXPORT_LINE" >> "$RC"
            info "added to ${RC}: ${EXPORT_LINE}"
            warn "run: source ${RC}"
        fi
    else
        warn "add to your shell profile: ${EXPORT_LINE}"
    fi

    export PATH="${INSTALL_DIR}:$PATH"
fi

# --- Verify ---

if ! command -v klax >/dev/null 2>&1; then
    fail "klax not found in PATH after install"
fi

info "$("${INSTALL_DIR}/klax" version) installed successfully"
echo ""
echo "Next steps:"
echo "  source ~/.bashrc"
echo "  klax setup     — configure bot token and allowed users"
echo "  klax start     — start the service"
