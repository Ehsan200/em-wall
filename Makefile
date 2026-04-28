# em-wall — local Makefile (development)
# Production install lives in scripts/install.sh.

GO          ?= go
WAILS       ?= $(HOME)/go/bin/wails
BUILD_DIR   ?= build
DAEMON_BIN  := $(BUILD_DIR)/em-walld
APP_DIR     := app

.PHONY: all daemon app test test-core lint run-daemon run-app install uninstall clean tidy

all: daemon app

daemon: $(DAEMON_BIN)

$(DAEMON_BIN):
	mkdir -p $(BUILD_DIR)
	$(GO) build -o $(DAEMON_BIN) ./daemon

app:
	cd $(APP_DIR) && $(WAILS) build

test: test-core
test-core:
	$(GO) test ./core/...

run-daemon: $(DAEMON_BIN)
	@echo "running em-walld with a local DB and socket — no root, no port 53, system DNS untouched"
	mkdir -p tmp
	$(DAEMON_BIN) \
		-db ./tmp/dev.db \
		-socket ./tmp/em-wall.sock \
		-listen 127.0.0.1:5353 \
		-no-auto-activate

run-app:
	cd $(APP_DIR) && $(WAILS) dev

install:
	sudo ./scripts/install.sh

uninstall:
	sudo ./scripts/uninstall.sh

tidy:
	$(GO) mod tidy
	cd $(APP_DIR) && $(GO) mod tidy

clean:
	rm -rf $(BUILD_DIR) tmp
	cd $(APP_DIR) && rm -rf frontend/dist build/bin
