APP := beixiaoguo
VERSION ?= $(shell cat VERSION)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: run test build build-arm64 clean

run:
	go run ./cmd/beixiaoguo

test:
	go test ./...

build:
	mkdir -p dist
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o dist/$(APP)-linux-amd64 ./cmd/beixiaoguo

build-arm64:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="$(LDFLAGS)" -o dist/$(APP)-linux-arm64 ./cmd/beixiaoguo

clean:
	rm -rf dist
