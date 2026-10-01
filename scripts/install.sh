#!/usr/bin/env bash
# Build and install bifrost to ~/.local/bin with the version stamped in.
# Usage: scripts/install.sh            (version from git describe)
set -euo pipefail
cd "$(dirname "$0")/.."
export GOTOOLCHAIN=${GOTOOLCHAIN:-go1.26.8}
VERSION=$(git describe --tags --always --dirty 2>/dev/null || echo dev)
mkdir -p "$HOME/.local/bin"
go build -trimpath -ldflags "-s -w -X github.com/charles-forsyth/ursa-bifrost/internal/version.Version=${VERSION}" \
  -o "$HOME/.local/bin/bifrost" ./cmd/bifrost
echo "installed $HOME/.local/bin/bifrost ${VERSION}"
"$HOME/.local/bin/bifrost" version
