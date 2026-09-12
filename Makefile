.PHONY: build run dev clean generate templ tidy deps test vet fmt check

# ---- variables ----
BINARY := kontrolplane-feed
CMD    := ./cmd/server

# Finding 19: keep in lockstep with the github.com/a-h/templ require line in
# go.mod and with TEMPL_VERSION in the Dockerfile. A CLI newer than the runtime
# library emits code the library cannot compile.
TEMPL_VERSION := v0.3.1020

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

# ---- build ----
build: generate
	go build -trimpath -ldflags="$(LDFLAGS)" -o $(BINARY) $(CMD)

run: build
	./$(BINARY)

dev:
	air

# ---- codegen ----
# Single codegen path: the locally installed, pinned templ binary (`make deps`).
# This used to shell out to `docker run ghcr.io/a-h/templ:latest`, which was both
# a second, unpinned source of generated code and a hard Docker dependency for
# anyone running `make build`.
generate:
	templ generate

# Backwards-compatible alias for the old target name.
templ: generate

# ---- quality ----
test:
	go test ./...

vet:
	go vet ./...

fmt:
	go fmt ./...

check: fmt vet test

# ---- deps ----
tidy:
	go mod tidy

deps:
	go install github.com/a-h/templ/cmd/templ@$(TEMPL_VERSION)
	# air is a developer-only file watcher, never an input to generated or
	# compiled output, so @latest carries none of the drift risk templ does.
	go install github.com/air-verse/air@latest
	go mod tidy

# ---- clean ----
clean:
	rm -f $(BINARY) server feed.db feed.db-shm feed.db-wal
	rm -rf tmp/
