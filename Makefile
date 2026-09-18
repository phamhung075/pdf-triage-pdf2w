GO ?= go
BINARY ?= pdf-triage
CMD := ./cmd/pdf-triage
DIST := dist
AIR ?= $(shell which air 2>/dev/null || test -x $(HOME)/go/bin/air && echo $(HOME)/go/bin/air || echo air)

.PHONY: all build dev test vet fmt cross linux windows clean

all: build

## dev: run server directly from source with change on save (air or go run)
dev:
	@mkdir -p tmp
	@PDF_TRIAGE_BASE_DIR="$${PDF_TRIAGE_BASE_DIR:-$$(cd .. && pwd)}" $(AIR) || PDF_TRIAGE_BASE_DIR="$${PDF_TRIAGE_BASE_DIR:-$$(cd .. && pwd)}" $(GO) run $(CMD) serve

## build: static binary for the host platform.
build:
	CGO_ENABLED=0 $(GO) build -o $(DIST)/$(BINARY) $(CMD)

## test: the whole module's test suite.
test:
	$(GO) test ./...

## vet: go vet across the module.
vet:
	$(GO) vet ./...

## fmt: gofmt every Go source file in the module.
fmt:
	gofmt -w $$(find . -name '*.go' -not -path './vendor/*')

## cross: static binaries for linux/amd64 and windows/amd64.
cross: linux windows

linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -o $(DIST)/$(BINARY)-linux-amd64 $(CMD)

windows:
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 $(GO) build -o $(DIST)/$(BINARY)-windows-amd64.exe $(CMD)

clean:
	rm -rf $(DIST)
