#!/bin/sh
# install.sh — universal installer for Witself release binaries.
#
#   # witself (default):
#   curl -fsSL https://raw.githubusercontent.com/witwave-ai/witself/main/install.sh | sh
#   # witself-infra:
#   curl -fsSL https://raw.githubusercontent.com/witwave-ai/witself/main/install.sh | sh -s witself-infra
#   # witself-admin:
#   curl -fsSL https://raw.githubusercontent.com/witwave-ai/witself/main/install.sh | sh -s witself-admin
#
# Downloads the selected binary for your OS/arch from the GitHub releases,
# verifies its SHA-256 checksum, and installs it on your PATH.
#
# Usage:
#   sh                 install latest witself
#   sh -s BINARY       install latest witself, witself-infra, witself-server, or witself-admin
#   sh -s BINARY VER   install a specific binary version
#   sh -s VER          install a specific witself version
#
# Environment:
#   WITSELF_BINARY   back-compat binary selector; positional BINARY wins
#   WS_VERSION       version to install (e.g. v0.0.1); default: latest release
#   WS_INSTALL_DIR   install directory; default: /usr/local/bin (sudo if needed)
#
# Note: witself-infra drives the `pulumi` engine at runtime. Unlike `brew install`
# (which pulls it automatically), this installer cannot — install pulumi yourself.

set -eu

REPO="witwave-ai/witself"
INSTALL_DIR="${WS_INSTALL_DIR:-/usr/local/bin}"

err() { printf 'install: %s\n' "$1" >&2; exit 1; }
info() { printf '%s\n' "$1" >&2; }
have() { command -v "$1" >/dev/null 2>&1; }

BINARY="${WITSELF_BINARY:-witself}"
version="${WS_VERSION:-}"

case "${1:-}" in
  "")
    [ "$#" -eq 0 ] || err "empty binary/version argument"
    ;;
  witself | ws | witself-infra | witself-server | witself-admin)
    BINARY="$1"
    version="${2:-${WS_VERSION:-}}"
    [ "$#" -le 2 ] || err "too many arguments (usage: sh -s [BINARY] [VERSION])"
    ;;
  *)
    version="$1"
    [ "$#" -le 1 ] || err "too many arguments (usage: sh -s [BINARY] [VERSION])"
    ;;
esac

# "ws" is the muscle-memory alias for the renamed tenant CLI.
[ "$BINARY" = "ws" ] && BINARY="witself"

case "$BINARY" in
  witself | witself-infra | witself-server | witself-admin) ;;
  *) err "unknown binary \"${BINARY}\" (want witself|witself-infra|witself-server|witself-admin)" ;;
esac

download() { # url dest
  if have curl; then curl -fsSL "$1" -o "$2"
  elif have wget; then wget -qO "$2" "$1"
  else err "need curl or wget"; fi
}
fetch() { # url -> stdout
  if have curl; then curl -fsSL "$1"
  elif have wget; then wget -qO- "$1"
  else err "need curl or wget"; fi
}

# Detect OS and architecture.
os=$(uname -s)
case "$os" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) err "unsupported OS: $os (linux and darwin only)" ;;
esac
arch=$(uname -m)
case "$arch" in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) err "unsupported architecture: $arch (amd64 and arm64 only)" ;;
esac

# Resolve the version: positional arg, then WS_VERSION, then the latest release.
if [ -z "$version" ]; then
  info "Resolving latest ${BINARY} release..."
  version=$(fetch "https://api.github.com/repos/${REPO}/releases/latest" |
    grep '"tag_name"' | head -1 | sed -e 's/.*"tag_name":[[:space:]]*"//' -e 's/".*//')
  [ -n "$version" ] || err "could not resolve the latest version"
fi
# The tag carries a leading v; the asset name uses the version without it.
case "$version" in
  v*) tag="$version"; ver="${version#v}" ;;
  *) tag="v$version"; ver="$version" ;;
esac

asset="${BINARY}_${ver}_${os}_${arch}.tar.gz"
base="https://github.com/${REPO}/releases/download/${tag}"

info "Installing ${BINARY} ${tag} (${os}/${arch})..."

tmp=$(mktemp -d 2>/dev/null || mktemp -d -t ws-install)
trap 'rm -rf "$tmp"' EXIT INT TERM

download "${base}/${asset}" "${tmp}/${asset}"
download "${base}/checksums.txt" "${tmp}/checksums.txt"

# Verify the SHA-256 checksum before trusting the binary.
expected=$(awk -v f="$asset" '$2 == f {print $1}' "${tmp}/checksums.txt")
[ -n "$expected" ] || err "no checksum found for ${asset}"
if have sha256sum; then actual=$(sha256sum "${tmp}/${asset}" | awk '{print $1}')
elif have shasum; then actual=$(shasum -a 256 "${tmp}/${asset}" | awk '{print $1}')
else err "need sha256sum or shasum to verify the download"; fi
[ "$expected" = "$actual" ] || err "checksum mismatch for ${asset} (expected ${expected}, got ${actual})"
info "Checksum verified."

# Extract.
tar -xzf "${tmp}/${asset}" -C "${tmp}"
[ -f "${tmp}/${BINARY}" ] || err "binary ${BINARY} not found in the archive"
chmod +x "${tmp}/${BINARY}"

# Install, with a sudo fallback and a ~/.local/bin fallback.
install_to() { # dir
  mkdir -p "$1" 2>/dev/null || return 1
  if [ -w "$1" ]; then mv "${tmp}/${BINARY}" "$1/${BINARY}"
  elif have sudo; then info "Elevating with sudo to write $1..."; sudo mv "${tmp}/${BINARY}" "$1/${BINARY}"
  else return 1; fi
}
if install_to "$INSTALL_DIR"; then dest="$INSTALL_DIR"
elif install_to "$HOME/.local/bin"; then dest="$HOME/.local/bin"; info "Installed to ~/.local/bin — ensure it is on your PATH."
else err "could not install to ${INSTALL_DIR} or ~/.local/bin"; fi

info "Installed ${BINARY} to ${dest}/${BINARY}"

# The tenant CLI also gets its muscle-memory alias (brew does the same).
if [ "$BINARY" = "witself" ]; then
  if [ -w "$dest" ]; then ln -sf "witself" "${dest}/ws" && info "Aliased ${dest}/ws -> witself"
  elif have sudo; then sudo ln -sf "witself" "${dest}/ws" && info "Aliased ${dest}/ws -> witself"
  fi
fi

case "$BINARY" in
  witself-infra)
    "${dest}/${BINARY}" help >/dev/null 2>&1 || err "installed ${BINARY} failed to run"
    if ! have pulumi; then
      info ""
      info "Note: witself-infra drives the 'pulumi' engine at runtime, which this"
      info "installer does not fetch. Install it with:  brew install pulumi"
      info "  (or: curl -fsSL https://get.pulumi.com | sh)"
    fi
    ;;
  *)
    "${dest}/${BINARY}" version
    ;;
esac
