BINARY := filmstream
PREFIX ?= $(HOME)/.local
APPLE_DIR := clients/apple
TVOS_DESTINATION ?= platform=tvOS Simulator,name=Apple TV 4K (3rd generation)

TVOS_DEVICE_ID ?=
TVOS_DEVELOPMENT_TEAM ?=
TVOS_DERIVED_DATA_PATH ?=

.PHONY: build install test fmt apple-project apple-test tvos-build tvos-install

build:
	go build -o bin/$(BINARY) ./cmd/filmstream

install:
	install -d $(PREFIX)/bin
	go build -o $(PREFIX)/bin/$(BINARY) ./cmd/filmstream

fmt:
	gofmt -w cmd internal

test:
	go test ./...

apple-project:
	cd $(APPLE_DIR) && xcodegen generate

apple-test:
	cd $(APPLE_DIR)/Packages/FilmstreamCore && swift test

tvos-build: apple-project
	xcodebuild -quiet -project $(APPLE_DIR)/FilmstreamApple.xcodeproj \
		-scheme FilmstreamTV \
		-destination '$(TVOS_DESTINATION)' \
		CODE_SIGNING_ALLOWED=NO clean build

tvos-install:
	TVOS_DEVICE_ID='$(TVOS_DEVICE_ID)' \
	TVOS_DEVELOPMENT_TEAM='$(TVOS_DEVELOPMENT_TEAM)' \
	TVOS_DERIVED_DATA_PATH='$(TVOS_DERIVED_DATA_PATH)' \
		./scripts/install-teastream-tvos
