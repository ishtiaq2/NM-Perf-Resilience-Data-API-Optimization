# das-noc: the NOC fleet controller (Go, standard library only).
#
#   make            build bin/noc and bin/fleetsim
#   make check      gofmt + vet + tests with the race detector
#   make demo       NOC + 400 simulated Master Units, prints the NOC page URL
#   make scale      5 000 simulated Master Units against one NOC (5 minutes)
#   make report     render bench/results/latest.json as HTML
#   make agent-demo the real das-01 Node.js backend + agent/noc-agent.js + the NOC
#   make dist       static Linux binaries for amd64 and arm64 in dist/
#   make image      container image (needs Docker)

VERSION ?= 0.6.0
GO      ?= go
LDFLAGS := -s -w -X main.version=$(VERSION)
BUILD   := CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)'

.PHONY: all build test race vet fmt check demo scale report agent-demo dist image clean

all: build

build:
	mkdir -p bin
	$(BUILD) -o bin/noc ./cmd/noc
	$(BUILD) -o bin/fleetsim ./cmd/fleetsim

test:
	$(GO) test ./...

race:
	$(GO) test -race -count=1 ./...

vet:
	$(GO) vet ./...

fmt:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

check: fmt vet race

demo: build
	sh scripts/demo.sh

scale:
	sh bench/scale.sh
	node bench/report.js

report:
	node bench/report.js

agent-demo:
	sh scripts/agent-demo.sh

dist:
	mkdir -p dist
	GOOS=linux GOARCH=amd64 $(BUILD) -o dist/noc-linux-amd64 ./cmd/noc
	GOOS=linux GOARCH=arm64 $(BUILD) -o dist/noc-linux-arm64 ./cmd/noc
	GOOS=linux GOARCH=amd64 $(BUILD) -o dist/fleetsim-linux-amd64 ./cmd/fleetsim
	cd dist && sha256sum noc-* fleetsim-* > SHA256SUMS

image:
	docker build --build-arg VERSION=$(VERSION) -t das-noc:$(VERSION) .

clean:
	rm -rf bin dist .run
