#!/bin/bash
set -euo pipefail

# klax installer
# Usage: curl -fsSL https://raw.githubusercontent.com/PiDmitrius/klax/main/install.sh | bash

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
NC='\033[0m'

info()  { echo -e "${GREEN}[+]${NC} $*"; }
warn()  { echo -e "${YELLOW}[!]${NC} $*"; }
fail()  { echo -e "${RED}[x]${NC} $*"; exit 1; }

# --- Check prerequisites ---

if ! command -v go >/dev/null 2>&1; then
    fail "go not found. Install Go: https://go.dev/dl/"
fi

GO_VERSION=$(go version | grep -oP 'go\K[0-9]+\.[0-9]+')
info "go: v${GO_VERSION}"

# --- Determine GOBIN ---

GOBIN=$(go env GOBIN)
if [ -z "$GOBIN" ]; then
    GOBIN="$(go env GOPATH)/bin"
fi
info "install path: ${GOBIN}"

# --- Install klax ---

info "installing klax..."
GOPROXY=direct go install github.com/PiDmitrius/klax/cmd/klax@latest
info "installed: ${GOBIN}/klax"

# --- Ensure GOBIN is in PATH ---

if ! echo "$PATH" | tr ':' '\n' | grep -qx "$GOBIN"; then
    warn "${GOBIN} is not in your PATH"

    SHELL_NAME=$(basename "$SHELL")
    case "$SHELL_NAME" in
        bash) RC="$HOME/.bashrc" ;;
        zsh)  RC="$HOME/.zshrc" ;;
        *)    RC="" ;;
    esac

    EXPORT_LINE="export PATH=\"${GOBIN}:\$PATH\""

    if [ -n "$RC" ] && [ -f "$RC" ]; then
        if ! grep -qF "$GOBIN" "$RC"; then
            echo "" >> "$RC"
            echo "# Added by klax installer" >> "$RC"
            echo "$EXPORT_LINE" >> "$RC"
            info "added to ${RC}: ${EXPORT_LINE}"
            warn "run: source ${RC}"
        fi
    else
        warn "add to your shell profile: ${EXPORT_LINE}"
    fi

    export PATH="${GOBIN}:$PATH"
fi

# --- Verify ---

if ! command -v klax >/dev/null 2>&1; then
    fail "klax not found in PATH after install"
fi

info "$(klax version) installed successfully"
echo ""
echo "Next steps:"
echo "  klax setup     — configure bot token and allowed users"
echo "  klax start     — start the service"
