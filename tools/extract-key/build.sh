#!/bin/bash
# build.sh — Build extract-key on macOS (including High Sierra 10.13+).
# Downloads Go 1.20 if needed. No root access required.
#
# Usage:
#   cd tools/extract-key
#   ./build.sh
#   ./extract-key

set -euo pipefail
cd "$(dirname "$0")"

GO_VERSION="1.20.14"
GO_TARBALL="go${GO_VERSION}.darwin-amd64.tar.gz"
GO_URL="https://go.dev/dl/${GO_TARBALL}"
LOCAL_GO="./.go-${GO_VERSION}"

# Try system Go first
if command -v go >/dev/null 2>&1 && go version >/dev/null 2>&1; then
    GO_CMD="go"
    echo "Using system Go: $(go version)"
else
    # Download Go locally
    if [ ! -x "${LOCAL_GO}/bin/go" ]; then
        echo "Go not found — downloading Go ${GO_VERSION}..."
        curl -fSL -o "${GO_TARBALL}" "${GO_URL}"
        mkdir -p "${LOCAL_GO}"
        tar -xzf "${GO_TARBALL}" --strip-components=1 -C "${LOCAL_GO}"
        rm -f "${GO_TARBALL}"
    fi
    GO_CMD="${LOCAL_GO}/bin/go"
    echo "Using local Go: $("${GO_CMD}" version)"
fi

echo "Building extract-key..."
CGO_ENABLED=1 "${GO_CMD}" build -trimpath -o extract-key .

echo ""
echo "✓ Built: $(pwd)/extract-key"
echo "  Run:   ./extract-key"
