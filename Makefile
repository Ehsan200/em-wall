# em-wall — local Makefile (development)
#
# The shipped flow is: build the .app via `make app-bundle`, then end
# users install/uninstall em-wall from inside the app itself (the UI
# escalates via osascript admin prompt). There is no CLI install path —
# the in-app installer at app/internal/installer/ is the only source
# of truth for what gets written where.

GO          ?= go
WAILS       ?= $(HOME)/go/bin/wails
BUILD_DIR   ?= build
DAEMON_BIN  := $(BUILD_DIR)/em-walld
APP_DIR     := app

# Resources embedded into the Wails app binary at build time. Populated
# by the `app-resources` target, read by app/internal/installer/embed.go.
APP_RES_DIR := $(APP_DIR)/internal/installer/resources
APP_RES_BIN     := $(APP_RES_DIR)/em-walld
APP_RES_PLIST   := $(APP_RES_DIR)/com.em-wall.daemon.plist
APP_RES_ANCHOR  := $(APP_RES_DIR)/em-wall.pf.anchor

.PHONY: all daemon app app-bundle app-resources test test-core lint \
        run-daemon run-app clean tidy

all: daemon app

daemon: $(DAEMON_BIN)

$(DAEMON_BIN):
	mkdir -p $(BUILD_DIR)
	$(GO) build -buildvcs=false -o $(DAEMON_BIN) ./daemon

# `make app` builds the Wails .app *without* the embedded daemon.
# Useful for fast iteration on the UI. The resulting binary will
# refuse to run Install (ErrNotPackaged), so this is dev-only.
app:
	cd $(APP_DIR) && $(WAILS) build

# `make app-bundle` is the primary user-facing build target. It
# refreshes the embedded resources, then runs `wails build` so the
# resulting .app is fully self-contained.
app-bundle: app-resources
	cd $(APP_DIR) && $(WAILS) build

# Stage the daemon binary, plist and pf anchor stub into the embed
# resources dir. Run as a dependency of app-bundle and run-app so
# `wails dev` and the bundled .app see the same files.
app-resources: $(APP_RES_BIN) $(APP_RES_PLIST) $(APP_RES_ANCHOR)

$(APP_RES_BIN): $(DAEMON_BIN)
	mkdir -p $(APP_RES_DIR)
	cp $(DAEMON_BIN) $(APP_RES_BIN)
	chmod 0755 $(APP_RES_BIN)

$(APP_RES_PLIST): launchd/com.em-wall.daemon.plist
	mkdir -p $(APP_RES_DIR)
	cp launchd/com.em-wall.daemon.plist $(APP_RES_PLIST)

$(APP_RES_ANCHOR):
	mkdir -p $(APP_RES_DIR)
	test -f $(APP_RES_ANCHOR) || printf '# em-wall pf anchor — rewritten at runtime by core/pfctl\n' > $(APP_RES_ANCHOR)

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

# `wails dev` builds the app from source. Stage resources first so the
# install panel can be tested end-to-end against the production paths.
run-app: app-resources
	cd $(APP_DIR) && $(WAILS) dev

tidy:
	$(GO) mod tidy
	cd $(APP_DIR) && $(GO) mod tidy

clean:
	rm -rf $(BUILD_DIR) tmp
	rm -f $(APP_RES_BIN) $(APP_RES_PLIST) $(APP_RES_ANCHOR)
	cd $(APP_DIR) && rm -rf frontend/dist build/bin
