APP_NAME    := mautrix-imessage-v2
CMD_PKG     := mautrix-imessage
BUNDLE_ID   := com.lrhodin.mautrix-imessage
VERSION     := 0.1.0
COMMIT      := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_TIME  := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
DATA_DIR    ?= $(shell pwd)/data
UNAME_S     := $(shell uname -s)

RUST_LIB    := librustpushgo.a
RUST_SRC    := $(shell find pkg/rustpushgo/src -name '*.rs' 2>/dev/null)
RUSTPUSH_SRC:= $(shell find rustpush/src rustpush/apple-private-apis -name '*.rs' 2>/dev/null)

LDFLAGS     := -X main.Tag=$(VERSION) -X main.Commit=$(COMMIT) -X main.BuildTime=$(BUILD_TIME)

.PHONY: build clean install install-beeper uninstall rust bindings check-deps check-deps-linux

# ===========================================================================
# Platform detection
# ===========================================================================

ifeq ($(UNAME_S),Darwin)
  # macOS paths and settings
  export PATH := /opt/homebrew/bin:/opt/homebrew/sbin:$(PATH)
  APP_BUNDLE  := $(APP_NAME).app
  BINARY      := $(APP_BUNDLE)/Contents/MacOS/$(APP_NAME)
  INFO_PLIST  := $(APP_BUNDLE)/Contents/Info.plist
  CGO_CFLAGS  := -I/opt/homebrew/include
  CGO_LDFLAGS := -L/opt/homebrew/lib -L$(shell pwd)
  CARGO_ENV   := MACOSX_DEPLOYMENT_TARGET=13.0
else
  # Linux: include Go and Rust installed by bootstrap
  export PATH := /usr/local/go/bin:$(HOME)/.cargo/bin:$(PATH)
  BINARY      := $(APP_NAME)
  CGO_CFLAGS  :=
  CGO_LDFLAGS := -L$(shell pwd)
  CARGO_ENV   :=
endif

# ===========================================================================
# Dependency checks
# ===========================================================================

# macOS: auto-install via Homebrew
check-deps:
ifeq ($(UNAME_S),Darwin)
	@if ! command -v brew >/dev/null 2>&1; then \
		echo "Installing Homebrew..."; \
		NONINTERACTIVE=1 /bin/bash -c "$$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"; \
		eval "$$(/opt/homebrew/bin/brew shellenv)"; \
	fi; \
	missing=""; \
	command -v go >/dev/null 2>&1    || missing="$$missing go"; \
	command -v cargo >/dev/null 2>&1 || missing="$$missing rust"; \
	command -v protoc >/dev/null 2>&1|| missing="$$missing protobuf"; \
	[ -f /opt/homebrew/include/olm/olm.h ] || [ -f /usr/local/include/olm/olm.h ] || missing="$$missing libolm"; \
	if [ -n "$$missing" ]; then \
		echo "Installing dependencies:$$missing"; \
		brew install $$missing; \
	fi
else
	@scripts/bootstrap-linux.sh
endif

# ===========================================================================
# Rust static library
# ===========================================================================

$(RUST_LIB): $(RUST_SRC) $(RUSTPUSH_SRC) pkg/rustpushgo/Cargo.toml
	cd pkg/rustpushgo && $(CARGO_ENV) cargo build --release
	cp pkg/rustpushgo/target/release/librustpushgo.a .

rust: $(RUST_LIB)

# ===========================================================================
# Go bindings
# ===========================================================================

bindings: $(RUST_LIB)
	cd pkg/rustpushgo && uniffi-bindgen-go target/release/librustpushgo.a --library --out-dir ..
	python3 scripts/patch_bindings.py

# ===========================================================================
# Build
# ===========================================================================

ifeq ($(UNAME_S),Darwin)
build: check-deps $(RUST_LIB) $(BINARY)
	codesign --force --deep --sign - $(APP_BUNDLE)
	@echo "Built $(APP_BUNDLE) ($(VERSION)-$(COMMIT))"

$(BINARY): $(shell find . -name '*.go') $(shell find . -name '*.m') $(shell find . -name '*.h') go.mod $(RUST_LIB)
	@mkdir -p $(APP_BUNDLE)/Contents/MacOS
	@cp Info.plist $(INFO_PLIST)
	CGO_CFLAGS="$(CGO_CFLAGS)" CGO_LDFLAGS="$(CGO_LDFLAGS)" \
		go build -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/$(CMD_PKG)/
else
build: check-deps $(RUST_LIB) $(BINARY)
	@echo "Built $(BINARY) ($(VERSION)-$(COMMIT))"

$(BINARY): $(shell find . -name '*.go') go.mod $(RUST_LIB)
	CGO_CFLAGS="$(CGO_CFLAGS)" CGO_LDFLAGS="$(CGO_LDFLAGS)" \
		go build -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/$(CMD_PKG)/
endif

# ===========================================================================
# Install / uninstall (macOS)
# ===========================================================================

install: build
ifeq ($(UNAME_S),Darwin)
	@scripts/install.sh "$(BINARY)" "$(DATA_DIR)" "$(BUNDLE_ID)"
else
	@scripts/install-linux.sh "$(BINARY)" "$(DATA_DIR)"
endif

install-beeper: build
ifeq ($(UNAME_S),Darwin)
	@scripts/install-beeper.sh "$(BINARY)" "$(DATA_DIR)" "$(BUNDLE_ID)"
else
	@scripts/install-beeper-linux.sh "$(BINARY)" "$(DATA_DIR)"
endif

uninstall:
ifeq ($(UNAME_S),Darwin)
	-launchctl unload ~/Library/LaunchAgents/$(BUNDLE_ID).plist 2>/dev/null
	rm -f ~/Library/LaunchAgents/$(BUNDLE_ID).plist
	@echo "LaunchAgent removed. App bundle at $(APP_BUNDLE) left in place."
else
	@echo "On Linux, stop the service and remove the binary manually."
endif

clean:
ifeq ($(UNAME_S),Darwin)
	rm -rf $(APP_NAME).app
endif
	rm -f $(APP_NAME) $(RUST_LIB)
	cd pkg/rustpushgo && cargo clean 2>/dev/null || true
