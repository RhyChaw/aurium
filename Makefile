# The toolchain is pinned in go.mod (toolchain go1.27.1), so `go build` will
# fetch and use it regardless of what is on PATH. GO is overridable only for
# the case where the pinned toolchain is already installed somewhere unusual.
GO ?= go
PKGS = ./...
GOBIN ?= $(HOME)/sdk/gobin

.PHONY: run run-web run-desktop build test test-race vet fmt lint tools clean

# The one command. Starts the daemon and opens Aurium — in its own window if the
# Rust toolchain is here, in a browser tab otherwise.
#
# Preferring the window is not cosmetic. The page carries a credential, so a
# browser tab means a token in a window with a URL bar, forty other tabs and a
# history; the shell has none of those and adds native notifications, which is
# the only thing that reaches you when an agent is blocked and the window is
# behind something else.
run:
	@if command -v cargo >/dev/null 2>&1 && cargo tauri --version >/dev/null 2>&1; then $(MAKE) --no-print-directory run-desktop; else echo "No Tauri toolchain found, opening in a browser instead. For the native window: cargo install tauri-cli --version '^2'"; $(MAKE) --no-print-directory run-web; fi

# Explicitly the browser. Useful on a machine with no Rust, and for looking at
# the page with devtools.
run-web: build
	./bin/aurium up

# Explicitly the native window. The daemon is detached here because the window,
# not this terminal, is the thing you close to be done — and `cargo tauri dev`
# already owns the foreground.
run-desktop: build
	./bin/aurium up --detach --no-open
	cd desktop && cargo tauri dev

build: shim
	$(GO) build -o bin/aurium ./cmd/aurium
	$(GO) build -o bin/auriumd ./cmd/auriumd
	$(GO) build -o bin/aurium-mcp ./cmd/aurium-mcp

# Cross-compiles the in-container shim and installs it where the daemon looks.
#
# `build` depends on this. It used to be a separate target nobody ran, so the
# docker driver could not build an image on a fresh machine — and failed with
# "read aurium-mcp from ~/.aurium/bin/aurium-mcp: no such file", which named a
# path nothing had ever written to rather than the step that was missing.
.PHONY: shim
shim:
	GOOS=linux GOARCH=amd64 $(GO) build -o bin/linux-amd64/aurium-mcp ./cmd/aurium-mcp
	GOOS=linux GOARCH=arm64 $(GO) build -o bin/linux-arm64/aurium-mcp ./cmd/aurium-mcp
	@mkdir -p $(HOME)/.aurium/bin
	@cp bin/linux-amd64/aurium-mcp $(HOME)/.aurium/bin/aurium-mcp-linux-amd64
	@cp bin/linux-arm64/aurium-mcp $(HOME)/.aurium/bin/aurium-mcp-linux-arm64
	@echo "installed aurium-mcp shims to $(HOME)/.aurium/bin"

test:
	$(GO) test $(PKGS)

test-race:
	$(GO) test -race $(PKGS)

# Container-runtime integration tests. NOTE: no such tests exist yet — this
# target currently runs the same suite with an unused tag. See the
# stubs-and-gaps table in documents/superpowers/plans/.
.PHONY: test-docker
test-docker:
	$(GO) test -tags docker $(PKGS)

vet:
	$(GO) vet $(PKGS)

fmt:
	$(GO) fmt $(PKGS)

tools:
	GOBIN=$(GOBIN) $(GO) install github.com/kisielk/errcheck@latest
	GOBIN=$(GOBIN) $(GO) install honnef.co/go/tools/cmd/staticcheck@latest

# -blank is the important flag: it catches `_ = f()`, which is the pattern
# that hid the dropped IPC.Send error.
#
# KNOWN BROKEN: as of Go 1.27 neither tool can load this module (errcheck
# silently reports nothing and exits 0; staticcheck reports "matched no
# packages"). Both work on a trivial module, so this is a tool-vs-toolchain
# compatibility gap, not a clean bill of health. Do not read a passing `make
# lint` as evidence of anything until this is fixed.
lint: vet
	$(GOBIN)/errcheck -blank -ignoretests $(PKGS)
	$(GOBIN)/staticcheck $(PKGS)

clean:
	$(RM) -r bin dist
