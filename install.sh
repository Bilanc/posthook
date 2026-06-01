#!/bin/sh
# posthook installer — downloads a prebuilt release and sets posthook up.
#
#   Local / OSS:   curl -fsSL https://raw.githubusercontent.com/Bilanc/posthook/main/install.sh | sh
#   Team (cloud):  curl -fsSL "https://api.bilanc.co/posthook/install.sh?apiKey=KEY" | sh
#
# The team link is served by the cloud API, which prepends POSTHOOK_API_KEY (and
# POSTHOOK_CLOUD_ENDPOINT) to this exact script. When POSTHOOK_API_KEY is set we
# also configure cloud sync and install a background sync daemon; without it this
# is a plain local-only install.
#
# What it does:
#   1. Download the posthook binary for this OS/arch from GitHub Releases (sha256
#      verified) and install it to ~/.local/bin.
#   2. Download the platform-independent dashboard bundle into ~/.posthook/dash
#      (best-effort; `posthook dash` needs Node >=24 at runtime).
#   3. If POSTHOOK_API_KEY is set: configure cloud sync.
#   4. Run `posthook init` (agent hooks + git shadow).
#   5. If POSTHOOK_API_KEY is set: install the background sync daemon.
#
# Env overrides:
#   POSTHOOK_API_KEY        team ingest key — enables cloud sync + daemon
#   POSTHOOK_CLOUD_ENDPOINT ingest base URL (default https://api.bilanc.co)
#   POSTHOOK_VERSION        pin a release, e.g. 0.1.0 (default: latest)
#   POSTHOOK_INSTALL_DIR    where to put the binary (default ~/.local/bin)

set -e

REPO="Bilanc/posthook"
INSTALL_DIR="${POSTHOOK_INSTALL_DIR:-$HOME/.local/bin}"
DEFAULT_ENDPOINT="https://api.bilanc.co"

info() { printf '%s\n' "$*"; }
warn() { printf '%s\n' "$*" >&2; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }

# fetch URL to stdout (curl, falling back to wget).
fetch() {
  if command -v curl >/dev/null 2>&1; then curl -fsSL "$1"
  elif command -v wget >/dev/null 2>&1; then wget -qO- "$1"
  else die "need curl or wget to download posthook"; fi
}

# download URL to a file.
download() {
  if command -v curl >/dev/null 2>&1; then curl -fsSL "$1" -o "$2"
  elif command -v wget >/dev/null 2>&1; then wget -qO "$2" "$1"
  else die "need curl or wget to download posthook"; fi
}

# verify_sha256 FILE CHECKSUMS — abort on mismatch; warn-and-skip if no tool.
verify_sha256() {
  file="$1"; sums="$2"; name=$(basename "$file")
  expected=$(awk -v f="$name" '$2==f {print $1}' "$sums")
  [ -n "$expected" ] || die "no checksum listed for $name"
  if command -v sha256sum >/dev/null 2>&1; then
    actual=$(sha256sum "$file" | awk '{print $1}')
  elif command -v shasum >/dev/null 2>&1; then
    actual=$(shasum -a 256 "$file" | awk '{print $1}')
  else
    warn "no sha256 tool found — skipping checksum verification"; return 0
  fi
  [ "$expected" = "$actual" ] || die "checksum mismatch for $name (expected $expected, got $actual)"
}

# ---------------------------------------------------------------------------
# Resolve platform + version
# ---------------------------------------------------------------------------
os=$(uname -s)
case "$os" in
  Darwin) os=darwin ;;
  Linux)  os=linux ;;
  *) die "unsupported OS: $os (posthook supports macOS and Linux)" ;;
esac

arch=$(uname -m)
case "$arch" in
  x86_64|amd64)  arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) die "unsupported architecture: $arch" ;;
esac

ver="${POSTHOOK_VERSION:-}"
if [ -z "$ver" ]; then
  info "Resolving latest posthook release..."
  ver=$(fetch "https://api.github.com/repos/$REPO/releases/latest" \
        | grep -m1 '"tag_name"' \
        | sed -E 's/.*"tag_name"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/')
  [ -n "$ver" ] || die "could not resolve the latest release (set POSTHOOK_VERSION to install a specific one)"
fi
ver="${ver#v}" # archives are named without the leading v
base="https://github.com/$REPO/releases/download/v$ver"
asset="posthook_${ver}_${os}_${arch}.tar.gz"

# ---------------------------------------------------------------------------
# 1. Download + verify + install the binary
# ---------------------------------------------------------------------------
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

info "Downloading posthook $ver ($os/$arch)..."
download "$base/$asset" "$tmp/$asset"
download "$base/checksums.txt" "$tmp/checksums.txt"
verify_sha256 "$tmp/$asset" "$tmp/checksums.txt"

tar -xzf "$tmp/$asset" -C "$tmp"
[ -f "$tmp/posthook" ] || die "release archive did not contain a posthook binary"

mkdir -p "$INSTALL_DIR"
install -m 0755 "$tmp/posthook" "$INSTALL_DIR/posthook"
BIN="$INSTALL_DIR/posthook"
info "Installed posthook -> $BIN"

# ---------------------------------------------------------------------------
# 2. Download the dashboard bundle (best-effort; needs Node >=24 at runtime)
# ---------------------------------------------------------------------------
if download "$base/posthook-dash.tar.gz" "$tmp/dash.tar.gz" 2>/dev/null; then
  DASH_DIR="$HOME/.posthook/dash"
  rm -rf "$DASH_DIR"
  mkdir -p "$DASH_DIR"
  tar -xzf "$tmp/dash.tar.gz" -C "$DASH_DIR"
  info "Installed dashboard bundle -> $DASH_DIR (run it with: posthook dash; needs Node >=24)"
else
  warn "Dashboard bundle not found for $ver — skipping. The CLI is fully functional; \`posthook dash\` won't be available."
fi

# ---------------------------------------------------------------------------
# 3. Configure cloud sync (only with a team key)
# ---------------------------------------------------------------------------
if [ -n "${POSTHOOK_API_KEY:-}" ]; then
  endpoint="${POSTHOOK_CLOUD_ENDPOINT:-$DEFAULT_ENDPOINT}"
  info ""
  info "Configuring cloud sync -> $endpoint"
  "$BIN" sync --set-endpoint "$endpoint" --set-token "$POSTHOOK_API_KEY" --set-enabled true
fi

# ---------------------------------------------------------------------------
# 4. Wire up hooks + git shadow
# ---------------------------------------------------------------------------
info ""
info "Setting up agent hooks + git shadow..."
"$BIN" init

# ---------------------------------------------------------------------------
# 5. Install the background sync daemon (only with a team key)
# ---------------------------------------------------------------------------
if [ -n "${POSTHOOK_API_KEY:-}" ]; then
  info ""
  info "Installing the background sync daemon..."
  if ! "$BIN" service install; then
    warn "Could not install the background sync daemon. Sync is configured; start it yourself with: posthook sync --loop"
  fi
fi

# ---------------------------------------------------------------------------
# PATH guidance
# ---------------------------------------------------------------------------
info ""
case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *)
    warn "NOTE: $INSTALL_DIR is not on your PATH."
    warn "  Add this to your shell rc, then restart your shell:"
    warn "    export PATH=\"$INSTALL_DIR:\$PATH\""
    ;;
esac

info "Done. Verify with: posthook status"
