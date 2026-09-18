GO ?= go
BINARY ?= pdf-triage
CMD := ./cmd/pdf-triage
DIST := dist

.PHONY: all build test vet fmt cross linux windows clean

all: build

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
