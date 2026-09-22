# Makefile for matrimail bridge
#
# Builds with the `goolm` tag, mautrix-go's pure-Go implementation of the
# Matrix Olm/Megolm crypto. Without it, mautrix-go links libolm, the
# deprecated C library, which carries unpatched vulnerabilities and has to be
# installed and shipped alongside the binary. The tag removes that dependency
# entirely; there is nothing to install.
#
# CGO stays enabled: go-sqlite3 is a cgo package and the bridge's default
# database is SQLite, so a CGO_ENABLED=0 build would compile and then fail to
# open its own database.

.PHONY: build clean install test

TAG := $(shell git describe --exact-match --tags 2>/dev/null || echo unknown)
COMMIT := $(shell git rev-parse --short=12 HEAD)
BUILD_TIME := $(shell date -Iseconds)
GO_LDFLAGS := -s -w -X main.Tag=$(TAG) -X main.Commit=$(COMMIT) -X main.BuildTime=$(BUILD_TIME)
GO_TAGS := goolm

build:
	go build -tags "$(GO_TAGS)" -ldflags "$(GO_LDFLAGS)" -o matrimail ./cmd/matrimail

clean:
	rm -f matrimail

install: build
	install -m 755 matrimail /usr/local/bin/

test:
	go test -tags "$(GO_TAGS)" ./...

run-example: build
	./matrimail --help

.DEFAULT_GOAL := build
