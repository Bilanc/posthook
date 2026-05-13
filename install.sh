#!/bin/sh
# posthook installer (v0 placeholder).
# Eventually: curl -sSL https://posthook.dev/install.sh | sh
# downloads a precompiled binary for the host platform from GitHub Releases.
# For now this script just verifies prerequisites and builds from source.

set -e

if ! command -v bun >/dev/null 2>&1; then
  echo "posthook requires Bun (https://bun.sh)."
  echo "Install it with: curl -fsSL https://bun.sh/install | bash"
  exit 1
fi

REPO_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$REPO_DIR"

echo "Installing dependencies..."
bun install

echo "Building binary..."
mkdir -p dist
bun build --compile --minify --target=bun ./bin/posthook.ts --outfile dist/posthook

INSTALL_DIR="${POSTHOOK_INSTALL_DIR:-$HOME/.local/bin}"
mkdir -p "$INSTALL_DIR"
install -m 0755 dist/posthook "$INSTALL_DIR/posthook"

echo ""
echo "posthook installed at $INSTALL_DIR/posthook"
case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *)
    echo "WARNING: $INSTALL_DIR is not on your PATH."
    echo "  Add this to your shell rc: export PATH=\"$INSTALL_DIR:\$PATH\""
    ;;
esac

echo ""
echo "Next: run 'posthook init' to install hooks for detected agents."
