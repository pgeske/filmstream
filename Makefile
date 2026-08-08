BINARY := filmstream
PREFIX ?= $(HOME)/.local

.PHONY: build install test fmt

build:
	go build -o bin/$(BINARY) ./cmd/filmstream

install:
	install -d $(PREFIX)/bin
	go build -o $(PREFIX)/bin/$(BINARY) ./cmd/filmstream

fmt:
	gofmt -w cmd internal

test:
	go test ./...
