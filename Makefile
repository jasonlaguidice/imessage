APP_NAME    := mautrix-imessage
BUNDLE_ID   := com.lrhodin.mautrix-imessage
VERSION     := 0.1.0
COMMIT      := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_TIME  := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
DATA_DIR    ?= $(shell pwd)/data

APP_BUNDLE  := $(APP_NAME).app
BINARY      := $(APP_BUNDLE)/Contents/MacOS/$(APP_NAME)
INFO_PLIST  := $(APP_BUNDLE)/Contents/Info.plist

RUST_LIB    := librustpushgo.a
RUST_SRC    := $(shell find pkg/rustpushgo/src -name '*.rs' 2>/dev/null)
RUSTPUSH_SRC:= $(shell find rustpush/src -name '*.rs' 2>/dev/null)

CGO_CFLAGS  := -I/opt/homebrew/include
CGO_LDFLAGS := -L/opt/homebrew/lib -L$(shell pwd)

LDFLAGS     := -X main.Tag=$(VERSION) -X main.Commit=$(COMMIT) -X main.BuildTime=$(BUILD_TIME)

.PHONY: build clean install uninstall rust bindings check-deps

# Check build dependencies
check-deps:
	@missing=""; \
	command -v go >/dev/null 2>&1    || missing="$$missing go"; \
	command -v cargo >/dev/null 2>&1 || missing="$$missing rust"; \
	command -v protoc >/dev/null 2>&1|| missing="$$missing protobuf"; \
	[ -f /opt/homebrew/include/olm/olm.h ] || [ -f /usr/local/include/olm/olm.h ] || missing="$$missing libolm"; \
	if [ -n "$$missing" ]; then \
		echo ""; \
		echo "Missing dependencies:$$missing"; \
		echo ""; \
		echo "  brew install$$missing"; \
		echo ""; \
		exit 1; \
	fi

# Build Rust static library
$(RUST_LIB): $(RUST_SRC) $(RUSTPUSH_SRC) pkg/rustpushgo/Cargo.toml
	cd pkg/rustpushgo && MACOSX_DEPLOYMENT_TARGET=14.2 cargo build --release
	cp pkg/rustpushgo/target/release/librustpushgo.a .

rust: $(RUST_LIB)

# Generate Go bindings from the Rust library and patch for Go 1.24+
bindings: $(RUST_LIB)
	cd pkg/rustpushgo && uniffi-bindgen-go target/release/librustpushgo.a --library --out-dir ..
	python3 scripts/patch_bindings.py

build: check-deps $(RUST_LIB) $(BINARY)
	codesign --force --deep --sign - $(APP_BUNDLE)
	@echo "Built $(APP_BUNDLE) ($(VERSION)-$(COMMIT))"

$(BINARY): $(shell find . -name '*.go') $(shell find . -name '*.m') $(shell find . -name '*.h') go.mod $(RUST_LIB)
	@mkdir -p $(APP_BUNDLE)/Contents/MacOS
	@cp Info.plist $(INFO_PLIST)
	CGO_CFLAGS="$(CGO_CFLAGS)" CGO_LDFLAGS="$(CGO_LDFLAGS)" \
		go build -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/$(APP_NAME)/

# Build, configure, and start the bridge
install: build
	@scripts/install.sh "$(BINARY)" "$(DATA_DIR)" "$(BUNDLE_ID)"

uninstall:
	-launchctl unload ~/Library/LaunchAgents/$(BUNDLE_ID).plist 2>/dev/null
	rm -f ~/Library/LaunchAgents/$(BUNDLE_ID).plist
	@echo "LaunchAgent removed. App bundle at $(APP_BUNDLE) left in place."

clean:
	rm -rf $(APP_BUNDLE)
	rm -f $(RUST_LIB)
	cd pkg/rustpushgo && cargo clean 2>/dev/null || true
