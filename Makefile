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

# daemon is PHONY — `go build` is the only thing that knows whether
# the binary is current relative to its sources, and Go's build cache
# makes a no-op rebuild sub-second. Letting Make decide via mtime on a
# single output file silently misses changes in daemon/*.go, core/**,
# go.mod, etc. and ships a stale daemon to the .app — which is the
# bug that caused "reinstall didn't bring the new methods".
daemon:
	@mkdir -p $(BUILD_DIR)
	$(GO) build -buildvcs=false -o $(DAEMON_BIN) ./daemon

# `make app` builds the Wails .app *without* the embedded daemon.
# Useful for fast iteration on the UI. The resulting binary will
# refuse to run Install (ErrNotPackaged), so this is dev-only.
app:
	cd $(APP_DIR) && $(WAILS) build

# `make app-bundle` is the primary user-facing build target. It
# refreshes the embedded resources (always rebuilds the daemon from
# source, see above) then runs `wails build` so the resulting .app is
# fully self-contained.
app-bundle: app-resources
	cd $(APP_DIR) && $(WAILS) build

# Stage the daemon binary, plist and pf anchor stub into the embed
# resources dir. Always runs — see the daemon target for why we don't
# trust mtimes here. The cp is cheap; the daemon rebuild is what
# matters.
app-resources: daemon
	@mkdir -p $(APP_RES_DIR)
	cp $(DAEMON_BIN) $(APP_RES_BIN)
	chmod 0755 $(APP_RES_BIN)
	cp launchd/com.em-wall.daemon.plist $(APP_RES_PLIST)
	@test -f $(APP_RES_ANCHOR) || printf '# em-wall pf anchor — rewritten at runtime by core/pfctl\n' > $(APP_RES_ANCHOR)

test: test-core
test-core:
	$(GO) test ./core/...

run-daemon: daemon
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
