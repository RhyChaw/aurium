GO ?= $(HOME)/sdk/go/bin/go
PKGS = ./...

.PHONY: build test test-docker test-tmux vet fmt clean

build:
	$(GO) build -o bin/aurium ./cmd/aurium
	$(GO) build -o bin/auriumd ./cmd/auriumd
	$(GO) build -o bin/aurium-mcp ./cmd/aurium-mcp

test:
	$(GO) test $(PKGS)

test-docker:
	$(GO) test -tags docker $(PKGS)

test-tmux:
	$(GO) test -tags tmux $(PKGS)

vet:
	$(GO) vet $(PKGS)

fmt:
	$(GO) fmt $(PKGS)

clean:
	$(RM) -r bin dist
