BINARY := filmstream
PREFIX ?= $(HOME)/.local
APPLE_DIR := clients/apple
IOS_DESTINATION ?= platform=iOS Simulator,name=iPhone 17 Pro
TVOS_DESTINATION ?= platform=tvOS Simulator,name=Apple TV 4K (3rd generation)

IOS_DEVICE_ID ?=
IOS_DEVELOPMENT_TEAM ?=
IOS_DERIVED_DATA_PATH ?=
TVOS_DEVICE_ID ?=
TVOS_DEVELOPMENT_TEAM ?=
TVOS_DERIVED_DATA_PATH ?=

.PHONY: build install test fmt apple-project apple-test ios-build ios-install tvos-build tvos-install

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

ios-build: apple-project
	xcodebuild -quiet -project $(APPLE_DIR)/FilmstreamApple.xcodeproj \
		-scheme FilmstreamIOS \
		-destination '$(IOS_DESTINATION)' \
		CODE_SIGNING_ALLOWED=NO clean build

ios-install:
	IOS_DEVICE_ID='$(IOS_DEVICE_ID)' \
	IOS_DEVELOPMENT_TEAM='$(IOS_DEVELOPMENT_TEAM)' \
	IOS_DERIVED_DATA_PATH='$(IOS_DERIVED_DATA_PATH)' \
		./scripts/install-teastream-ios

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
