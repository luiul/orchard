GOLANGCI_LINT ?= $(shell command -v golangci-lint 2>/dev/null || echo "$(HOME)/go/bin/golangci-lint")

.PHONY: build vet test fmt lint check install

build:
	go build ./...

vet:
	go vet ./...

test:
	go test -race ./...

fmt:
	@test -z "$$(gofmt -l cmd internal)" || { gofmt -l cmd internal; exit 1; }

lint:
	$(GOLANGCI_LINT) run ./...

check: fmt vet build test lint

install:
	go build -ldflags "-X main.version=$$(git describe --tags --always --dirty 2>/dev/null || echo dev)" -o /tmp/orchard-build ./cmd/orchard
	install -m 0755 /tmp/orchard-build ~/.local/bin/orchard
