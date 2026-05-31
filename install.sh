#!/bin/sh
# posthook installer (v0 placeholder).
# Eventually: curl -sSL https://posthook.dev/install.sh | sh
# downloads a precompiled binary for the host platform from GitHub Releases.
# For now this script just verifies prerequisites and builds from source.
#
# This builds two things:
#   1. the posthook Go CLI  -> ~/.local/bin/posthook
#   2. the Next.js dashboard -> staged into ~/.posthook/dash (served by `posthook dash`)
# The dashboard build is skipped (with a warning) if Node.js is unavailable;
# the CLI still installs fine without it.

set -e

if ! command -v go >/dev/null 2>&1; then
  echo "posthook requires Go 1.23 or newer (https://go.dev/dl/)."
  echo "Install it with: brew install go"
  exit 1
fi

REPO_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$REPO_DIR"

# ---------------------------------------------------------------------------
# 1. Build + install the CLI binary
# ---------------------------------------------------------------------------
echo "Building binary..."
mkdir -p dist
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o dist/posthook ./cmd/posthook

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

# ---------------------------------------------------------------------------
# 2. Build + stage the web dashboard
# ---------------------------------------------------------------------------
# Build on the host so better-sqlite3's native binding matches this platform,
# then stage the self-contained standalone output into ~/.posthook/dash so the
# `posthook dash` command can spawn it independent of this source tree.
DASH_STAGE="$HOME/.posthook/dash"

if command -v node >/dev/null 2>&1 && command -v npm >/dev/null 2>&1; then
  echo ""
  echo "Building dashboard..."
  cd "$REPO_DIR/dash"

  if [ -f package-lock.json ]; then
    NPM_INSTALL="npm ci"
  else
    NPM_INSTALL="npm install"
  fi

  # Install deps. A global npm cache poisoned by root-owned entries (e.g. a past
  # `sudo npm`) fails with EACCES/EEXIST on rename. If the normal install fails,
  # retry with a throwaway repo-local cache so a broken ~/.npm can't block the
  # build. (`if ! cmd` is safe under `set -e` — the failure is caught here.)
  if ! $NPM_INSTALL; then
    LOCAL_CACHE="$REPO_DIR/dash/.npm-cache"
    echo ""
    echo "npm install failed (often a root-owned ~/.npm cache)."
    echo "Retrying with a local cache at $LOCAL_CACHE ..."
    if ! npm_config_cache="$LOCAL_CACHE" $NPM_INSTALL; then
      echo "" >&2
      echo "ERROR: dashboard dependency install failed even with a local cache." >&2
      echo "  Fix the global cache once with: sudo chown -R \"\$(whoami)\" ~/.npm" >&2
      echo "  then re-run ./install.sh. (The CLI above is already installed.)" >&2
      exit 1
    fi
  fi
  npm run build

  echo "Staging dashboard to $DASH_STAGE ..."
  rm -rf "$DASH_STAGE"
  mkdir -p "$DASH_STAGE"
  # The standalone bundle: server.js, package.json, node_modules, traced .next/.
  cp -R .next/standalone/. "$DASH_STAGE"/
  # Next does NOT copy these into standalone; the server expects them relative
  # to its working directory (which `posthook dash` sets to the stage dir).
  mkdir -p "$DASH_STAGE/.next/static"
  cp -R .next/static/. "$DASH_STAGE/.next/static"/
  if [ -d public ]; then
    cp -R public "$DASH_STAGE/public"
  fi

  cd "$REPO_DIR"
  echo "Dashboard staged. Launch it any time with: posthook dash"
else
  echo ""
  echo "WARNING: Node.js + npm not found — skipping dashboard build."
  echo "  The CLI is installed and fully functional."
  echo "  Install Node.js >=20, then re-run ./install.sh to enable 'posthook dash'."
fi

echo ""
echo "Next: run 'posthook init' to install hooks for detected agents."
