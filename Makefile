SHELL := /usr/bin/env bash

GO       ?= go
BUILD_DIR ?= .builds
BUILD_BIN ?= $(BUILD_DIR)/bin
PYTHON   ?= .venv/bin/python

.PHONY: help
## display help
help:
	@echo
	@echo "Usage:"
	@echo "  make <target>"
	@echo
	@awk '/^## / { desc = substr($$0, 4); next } /^[A-Za-z0-9_.\/-]+:/ { if (desc != "") { split($$0, parts, ":"); printf "  \033[36m%-22s\033[0m %s\n", parts[1], desc; desc = "" } }' $(MAKEFILE_LIST)

$(BUILD_BIN):
	@mkdir -p $(BUILD_BIN)

.PHONY: fmt
## format Go sources with goimports (or gofmt fallback)
fmt: $(BUILD_BIN)
	@if command -v goimports >/dev/null; then cd backend && goimports -w .; else cd backend && $$($(GO) env GOROOT)/bin/gofmt -w .; fi

.PHONY: test
## run Go, Python pipeline and frontend tests
test:
	cd backend && $(GO) test ./...
	PYTHONPATH=backend $(PYTHON) -m pytest backend/pipeline/tests -q
	cd frontend && npm test

.PHONY: build
## build the CLI to .builds/bin
build: $(BUILD_BIN)
	cd backend && CGO_ENABLED=0 $(GO) build -trimpath -o ../$(BUILD_BIN)/musicd ./cmd/musicd

.PHONY: build-images
## build CPU Compose images
build-images:
	docker compose build api migrate dispatcher worker web

.PHONY: compose-up
## start the CPU stack
compose-up:
	docker compose up -d --build

.PHONY: compose-down
## stop the stack, preserving volumes and data
compose-down:
	docker compose down
