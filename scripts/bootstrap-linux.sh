#!/bin/bash
# Bootstrap all build dependencies on Linux (Ubuntu/Debian).
# Equivalent to macOS's Homebrew auto-install in check-deps.
set -euo pipefail

MIN_GO="1.26"
# Rust 1.95+: rustpush (>= 8983677) uses atomic_try_update, stable since 1.95.
# Keep in sync with RUST_MIN_VERSION in the Makefile.
MIN_RUST="1.95"

echo ""
echo "Checking Linux build dependencies..."
echo ""

# ── System packages (apt) ─────────────────────────────────────
APT_PACKAGES=""
command -v cmake  >/dev/null 2>&1 || APT_PACKAGES="$APT_PACKAGES cmake"
command -v protoc >/dev/null 2>&1 || APT_PACKAGES="$APT_PACKAGES protobuf-compiler"
command -v git    >/dev/null 2>&1 || APT_PACKAGES="$APT_PACKAGES git"
command -v curl   >/dev/null 2>&1 || APT_PACKAGES="$APT_PACKAGES curl"
command -v wget   >/dev/null 2>&1 || APT_PACKAGES="$APT_PACKAGES wget"
command -v make   >/dev/null 2>&1 || APT_PACKAGES="$APT_PACKAGES build-essential"
command -v cc     >/dev/null 2>&1 || APT_PACKAGES="$APT_PACKAGES build-essential"
command -v g++    >/dev/null 2>&1 || APT_PACKAGES="$APT_PACKAGES build-essential"
dpkg -s pkg-config   >/dev/null 2>&1 || APT_PACKAGES="$APT_PACKAGES pkg-config"
dpkg -s libolm-dev   >/dev/null 2>&1 || APT_PACKAGES="$APT_PACKAGES libolm-dev"
dpkg -s libclang-dev >/dev/null 2>&1 || APT_PACKAGES="$APT_PACKAGES libclang-dev"
dpkg -s libssl-dev   >/dev/null 2>&1 || APT_PACKAGES="$APT_PACKAGES libssl-dev"
# libunicorn-dev: pre-built Unicorn Engine (CPU emulator). Without this,
# the Rust unicorn-engine-sys crate tries to build QEMU from source via CMake,
# which often fails on WSL2 due to qemu/configure issues.
dpkg -s libunicorn-dev >/dev/null 2>&1 || APT_PACKAGES="$APT_PACKAGES libunicorn-dev"
dpkg -s libheif-dev    >/dev/null 2>&1 || APT_PACKAGES="$APT_PACKAGES libheif-dev"
command -v sqlite3 >/dev/null 2>&1 || APT_PACKAGES="$APT_PACKAGES sqlite3"

# Deduplicate
APT_PACKAGES=$(echo "$APT_PACKAGES" | tr ' ' '\n' | sort -u | tr '\n' ' ')

if [ -n "$APT_PACKAGES" ]; then
    echo "Installing system packages:$APT_PACKAGES"
    SUDO=""; [ "$(id -u)" != "0" ] && SUDO="sudo"
    $SUDO apt-get update -qq
    $SUDO apt-get install -y -qq $APT_PACKAGES
    echo "✓ System packages installed"
else
    echo "✓ System packages already installed"
fi

# ── Rust (via rustup) ─────────────────────────────────────────
install_rust() {
    echo "Installing Rust via rustup..."
    curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y --default-toolchain stable
    source "$HOME/.cargo/env"
    echo "✓ Rust $(rustc --version | awk '{print $2}') installed"
}

rust_too_old() {
    local ver major minor need_minor
    ver=$(rustc --version | awk '{print $2}')
    major=$(echo "$ver" | cut -d. -f1)
    minor=$(echo "$ver" | cut -d. -f2)
    need_minor=$(echo "$MIN_RUST" | cut -d. -f2)
    [ "$major" -lt 1 ] || { [ "$major" -eq 1 ] && [ "$minor" -lt "$need_minor" ]; }
}

if command -v rustc >/dev/null 2>&1; then
    RUST_VER=$(rustc --version | awk '{print $2}')
    if rust_too_old; then
        echo "Rust $RUST_VER is too old (need $MIN_RUST+), upgrading..."
        if command -v rustup >/dev/null 2>&1; then
            rustup update stable
            # A default pinned to an older numbered toolchain is not moved by
            # `rustup update stable`; switch the default to stable in that case.
            if rust_too_old; then
                rustup default stable
            fi
            if rust_too_old; then
                echo "✗ Rust is still $(rustc --version | awk '{print $2}') after updating (need $MIN_RUST+)." >&2
                echo "  Check for a rust-toolchain override or RUSTUP_TOOLCHAIN in your environment." >&2
                exit 1
            fi
            echo "✓ Rust updated to $(rustc --version | awk '{print $2}')"
        else
            install_rust
        fi
    else
        echo "✓ Rust $RUST_VER"
    fi
else
    install_rust
fi

# Make sure cargo is on PATH for this session
if [ -f "$HOME/.cargo/env" ]; then
    source "$HOME/.cargo/env"
fi

# ── Go ────────────────────────────────────────────────────────
install_go() {
    echo "Installing Go from go.dev..."
    local GO_VERSION
    GO_VERSION=$(curl -sSL 'https://go.dev/dl/?mode=json' | grep -o '"version": *"go[0-9.]*"' | head -1 | grep -o 'go[0-9.]*')
    if [ -z "$GO_VERSION" ]; then
        GO_VERSION="go1.26.0"
    fi
    local ARCH
    case "$(uname -m)" in
        x86_64)  ARCH=amd64 ;;
        aarch64) ARCH=arm64 ;;
        *)       ARCH=amd64 ;;
    esac
    echo "  Downloading $GO_VERSION (linux/$ARCH)..."
    curl -sSL "https://go.dev/dl/${GO_VERSION}.linux-${ARCH}.tar.gz" -o /tmp/go.tar.gz
    sudo rm -rf /usr/local/go
    sudo tar -C /usr/local -xzf /tmp/go.tar.gz
    rm -f /tmp/go.tar.gz
    export PATH="/usr/local/go/bin:$PATH"
    # Persist for future shells
    if ! grep -q '/usr/local/go/bin' "$HOME/.profile" 2>/dev/null; then
        echo 'export PATH=/usr/local/go/bin:$PATH' >> "$HOME/.profile"
    fi
    echo "✓ Go $(go version | awk '{print $3}') installed to /usr/local/go"
}

if command -v go >/dev/null 2>&1; then
    GO_VER=$(go version | awk '{print $3}' | sed 's/^go//')
    GO_MAJOR=$(echo "$GO_VER" | cut -d. -f1)
    GO_MINOR=$(echo "$GO_VER" | cut -d. -f2)
    NEED_MINOR=$(echo "$MIN_GO" | cut -d. -f2)
    if [ "$GO_MAJOR" -lt 1 ] || { [ "$GO_MAJOR" -eq 1 ] && [ "$GO_MINOR" -lt "$NEED_MINOR" ]; }; then
        echo "Go $GO_VER is too old (need $MIN_GO+), upgrading..."
        install_go
    else
        echo "✓ Go $GO_VER"
    fi
else
    install_go
fi

# ── Apple Root CA ─────────────────────────────────────────────
# Apple's identity servers use a cert signed by Apple Root CA,
# which isn't in the default Ubuntu/Debian trust store.
if openssl s_client -connect identity.ess.apple.com:443 </dev/null 2>&1 | grep -q "Verify return code: 0"; then
    echo "✓ Apple Root CA already trusted"
else
    echo "Installing Apple Root CA..."
    wget -qO /tmp/AppleRootCA.cer 'https://www.apple.com/appleca/AppleIncRootCertificate.cer'
    sudo openssl x509 -inform DER -in /tmp/AppleRootCA.cer \
        -out /usr/local/share/ca-certificates/AppleRootCA.crt
    sudo update-ca-certificates --fresh >/dev/null 2>&1
    rm -f /tmp/AppleRootCA.cer
    echo "✓ Apple Root CA installed"
fi

echo ""
echo "All dependencies ready."
echo ""
