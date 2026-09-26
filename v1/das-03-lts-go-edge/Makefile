# das-edge: one static binary for the DAS Master Node (Go standard library only).

VERSION ?= 3.0.0
LDFLAGS := -s -w -X main.version=$(VERSION)
BUILD   := CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)'
PORT    ?= 18380

.PHONY: all build test race vet cross sizes run demo-ingest conformance strangler bench e2e clean

all: vet test build

build:
	$(BUILD) -o bin/das-edge ./cmd/das-edge
	$(BUILD) -o bin/das-nodesim ./cmd/das-nodesim

test:
	go test ./...

race:
	go test -race -count=1 ./...

vet:
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed:"; gofmt -l .; exit 1)
	go vet ./...

# Master Node CPUs: 64-bit ARM, 32-bit ARMv7 (hard float), x86-64.
cross:
	GOOS=linux GOARCH=arm64       $(BUILD) -o dist/das-edge-linux-arm64 ./cmd/das-edge
	GOOS=linux GOARCH=arm GOARM=7 $(BUILD) -o dist/das-edge-linux-armv7 ./cmd/das-edge
	GOOS=linux GOARCH=amd64       $(BUILD) -o dist/das-edge-linux-amd64 ./cmd/das-edge
	GOOS=linux GOARCH=arm64       $(BUILD) -o dist/das-nodesim-linux-arm64 ./cmd/das-nodesim

sizes: cross
	@for f in dist/das-edge-*; do printf '%-32s %6.1f MB  (gzip %4.1f MB)\n' $$f $$(echo "$$(stat -c %s $$f) / 1048576" | bc -l) $$(echo "$$(gzip -9c $$f | wc -c) / 1048576" | bc -l); done

run: build
	./bin/das-edge -listen :8080 -log text

# Real ingest path: the edge without its internal simulator, 300 nodes streaming over h2c.
demo-ingest: build
	sh scripts/demo-ingest.sh

conformance: build
	sh scripts/conformance.sh $(PORT)

# Migration steps 1-4 against the shipped behaviour (../das-01-hotfix-node --legacy).
strangler: build
	sh scripts/strangler-steps.sh

bench: build
	node bench/edge-under-load.js

# Angular reference client (../das-04-angular-client) end to end against the Go edge.
e2e: build
	sh scripts/e2e-angular.sh

clean:
	rm -rf bin dist .run
