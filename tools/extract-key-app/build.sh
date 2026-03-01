#!/bin/bash
# build.sh — Build the Extract Key SwiftUI app for Intel Macs.
#
# On an Apple Silicon Mac (M1/M2/M3/M4), this cross-compiles for x86_64.
# The resulting binary runs natively on Intel Macs.
#
# Usage:
#   cd tools/extract-key-app
#   ./build.sh
#   # Binary:  .build/release/ExtractKeyApp
#   # App:     ExtractKey.app/

set -euo pipefail
cd "$(dirname "$0")"

echo "Building ExtractKeyApp for x86_64 (Intel)..."
swift build -c release --arch x86_64

BINARY=".build/release/ExtractKeyApp"
if [ ! -f "$BINARY" ]; then
    # SPM may place it in an arch-specific subdirectory
    BINARY=$(find .build -name ExtractKeyApp -type f -perm +111 2>/dev/null | head -1)
fi

if [ ! -f "$BINARY" ]; then
    echo "ERROR: Build succeeded but binary not found"
    exit 1
fi

echo ""
echo "Binary: $BINARY"
file "$BINARY"

# Wrap in .app bundle
APP="ExtractKey.app"
rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS"
cp "$BINARY" "$APP/Contents/MacOS/ExtractKeyApp"

cat > "$APP/Contents/Info.plist" << 'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>CFBundleIdentifier</key>
    <string>com.lrhodin.extract-key</string>
    <key>CFBundleName</key>
    <string>Extract Key</string>
    <key>CFBundleDisplayName</key>
    <string>Hardware Key Extractor</string>
    <key>CFBundleExecutable</key>
    <string>ExtractKeyApp</string>
    <key>CFBundlePackageType</key>
    <string>APPL</string>
    <key>CFBundleVersion</key>
    <string>1.0</string>
    <key>CFBundleShortVersionString</key>
    <string>1.0</string>
    <key>LSMinimumSystemVersion</key>
    <string>11.0</string>
    <key>NSHighResolutionCapable</key>
    <true/>
</dict>
</plist>
PLIST

# Ad-hoc sign (required for Gatekeeper on recent macOS)
codesign --force --sign - "$APP" 2>/dev/null || true

echo ""
echo "App bundle: $(pwd)/$APP"
echo ""
echo "To run on this Mac (under Rosetta if Apple Silicon):"
echo "  open $APP"
echo ""
echo "To run on an Intel Mac:"
echo "  Copy $APP to the Intel Mac and double-click it."
