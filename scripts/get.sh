#!/bin/sh
# Install the latest bifrost release binary to ~/.local/bin (Linux and macOS).
#
#   curl -fsSL https://raw.githubusercontent.com/UCR-Research-Computing/ursa-bifrost/main/scripts/get.sh | sh
#
# Options (environment): BIFROST_VERSION=v0.8.1 (default: latest), BIFROST_DIR=~/bin.
# Checks the SHA256 against the release's SHA256SUMS. If there is no binary for your
# platform it falls back to `go install` when Go is available.
set -eu

REPO="UCR-Research-Computing/ursa-bifrost"
DIR="${BIFROST_DIR:-$HOME/.local/bin}"
VERSION="${BIFROST_VERSION:-}"

say() { printf '%s\n' "$*" >&2; }
need() { command -v "$1" >/dev/null 2>&1 || { say "bifrost install: needs $1"; exit 1; }; }
need curl
need tar

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) arch=unknown ;;
esac

if [ -z "$VERSION" ]; then
  VERSION=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" |
    sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n1)
fi
[ -n "$VERSION" ] || { say "bifrost install: could not find the latest release"; exit 1; }

name="bifrost_${VERSION#v}_${os}_${arch}"
url="https://github.com/$REPO/releases/download/$VERSION/$name.tar.gz"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

if curl -fsSL -o "$tmp/$name.tar.gz" "$url"; then
  curl -fsSL -o "$tmp/SHA256SUMS" "https://github.com/$REPO/releases/download/$VERSION/SHA256SUMS"
  want=$(grep " $name.tar.gz\$" "$tmp/SHA256SUMS" | cut -d' ' -f1)
  if command -v sha256sum >/dev/null 2>&1; then
    got=$(sha256sum "$tmp/$name.tar.gz" | cut -d' ' -f1)
  else
    got=$(shasum -a 256 "$tmp/$name.tar.gz" | cut -d' ' -f1)
  fi
  [ -n "$want" ] && [ "$want" = "$got" ] || { say "bifrost install: checksum mismatch, not installing"; exit 1; }
  tar -xzf "$tmp/$name.tar.gz" -C "$tmp"
  mkdir -p "$DIR"
  install -m 0755 "$tmp/$name/bifrost" "$DIR/bifrost"
elif command -v go >/dev/null 2>&1; then
  say "No release binary for $os/$arch; building with go install."
  GOBIN="$DIR" go install "github.com/$REPO/cmd/bifrost@$VERSION"
else
  say "bifrost install: no binary for $os/$arch and Go is not installed."
  say "Install Go (https://go.dev/dl) and run: go install github.com/$REPO/cmd/bifrost@latest"
  exit 1
fi

say "Installed $("$DIR/bifrost" version) to $DIR/bifrost"
case ":$PATH:" in *":$DIR:"*) ;; *) say "Add $DIR to your PATH." ;; esac
say "Next: bifrost config init && bifrost doctor   (or use the hosted server: docs/CLIENTS.md)"
