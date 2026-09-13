# The toolchain is pinned in go.mod (toolchain go1.27.1), so `go build` will
# fetch and use it regardless of what is on PATH. GO is overridable only for
# the case where the pinned toolchain is already installed somewhere unusual.
GO ?= go
PKGS = ./...
GOBIN ?= $(HOME)/sdk/gobin

.PHONY: build test test-race vet fmt lint tools clean

build:
	$(GO) build -o bin/aurium ./cmd/aurium
	$(GO) build -o bin/auriumd ./cmd/auriumd
	$(GO) build -o bin/aurium-mcp ./cmd/aurium-mcp

# Cross-compiles the in-container shim. The image build copies these in, so
# without them `aurium container create` on the docker driver cannot produce
# an image at all.
.PHONY: shim
shim:
	GOOS=linux GOARCH=amd64 $(GO) build -o bin/linux-amd64/aurium-mcp ./cmd/aurium-mcp
	GOOS=linux GOARCH=arm64 $(GO) build -o bin/linux-arm64/aurium-mcp ./cmd/aurium-mcp

test:
	$(GO) test $(PKGS)

test-race:
	$(GO) test -race $(PKGS)

# Container-runtime integration tests. NOTE: no such tests exist yet — this
# target currently runs the same suite with an unused tag. See the
# stubs-and-gaps table in docs/superpowers/plans/.
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
