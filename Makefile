# Ballast — build, lint and test.
#
# The binary MUST be fully static (CGO_ENABLED=0; SQLite comes from the
# pure-Go modernc.org/sqlite driver), per AGENTS.md "Stack (fixed)".

BINARY  := ballast
GO      ?= go
# Spec §21.5: the tag when one exists, else the short commit (-dirty when
# the tree has changes). Tag releases with vX.Y.Z; tagging is the maintainer's step.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

LDFLAGS := -s -w -X main.version=$(if $(VERSION),$(VERSION),dev)

.PHONY: web
web: ## Build the Vue frontend into web/dist (spec §10: no Node needed at runtime, only to build)
	cd web && npm ci && npm run build

.PHONY: build
build:
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/ballast

.PHONY: vet
vet:
	$(GO) vet ./...

.PHONY: test
test:
	$(GO) test ./... -race

.PHONY: cover
cover:
	$(GO) test ./... -race -coverprofile=coverage.out
	$(GO) tool cover -func=coverage.out

.PHONY: lint
lint: ## Run golangci-lint with .golangci.yml (same version as CI: v2.14)
	golangci-lint run ./...

.PHONY: fmt
fmt:
	$(GO) fmt ./...

.PHONY: build-all
build-all: ## Cross-build linux/amd64 and linux/arm64 static binaries
	env CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY)-linux-amd64 ./cmd/ballast
	env CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY)-linux-arm64 ./cmd/ballast

.PHONY: clean
clean:
	rm -rf bin coverage.out

.PHONY: help
help:
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-10s %s\n", $$1, $$2}'
