APP_NAME    := mautrix-imessage
BUNDLE_ID   := com.lrhodin.mautrix-imessage
VERSION     := 0.1.0
COMMIT      := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_TIME  := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
DATA_DIR    := $(realpath ../mautrix-imessage-data)

APP_BUNDLE  := ../$(APP_NAME).app
BINARY      := $(APP_BUNDLE)/Contents/MacOS/$(APP_NAME)
INFO_PLIST  := $(APP_BUNDLE)/Contents/Info.plist

CGO_CFLAGS  := -I/opt/homebrew/include
CGO_LDFLAGS := -L/opt/homebrew/lib

LDFLAGS     := -X main.Tag=$(VERSION) -X main.Commit=$(COMMIT) -X main.BuildTime=$(BUILD_TIME)

.PHONY: build clean install uninstall

build: $(BINARY)
	codesign --force --deep --sign - $(APP_BUNDLE)
	@echo "Built $(APP_BUNDLE) ($(VERSION)-$(COMMIT))"

$(BINARY): $(shell find . -name '*.go') $(shell find . -name '*.m') $(shell find . -name '*.h') go.mod
	@mkdir -p $(APP_BUNDLE)/Contents/MacOS
	@cp Info.plist $(INFO_PLIST)
	CGO_CFLAGS="$(CGO_CFLAGS)" CGO_LDFLAGS="$(CGO_LDFLAGS)" \
		go build -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/$(APP_NAME)/

install: build
	open $(APP_BUNDLE) --args --setup -c $(DATA_DIR)/config.yaml

uninstall:
	-launchctl unload ~/Library/LaunchAgents/$(BUNDLE_ID).plist 2>/dev/null
	rm -f ~/Library/LaunchAgents/$(BUNDLE_ID).plist
	@echo "LaunchAgent removed. App bundle at $(APP_BUNDLE) left in place."

clean:
	rm -rf $(APP_BUNDLE)
