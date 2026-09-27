BIN := wassup
PKG := ./cmd/wassup
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build test lint demo fixtures install clean

build:
	go build -ldflags "-X main.Version=$(VERSION)" -o $(BIN) $(PKG)

install:
	go install -ldflags "-X main.Version=$(VERSION)" $(PKG)

test:
	go test ./...

lint:
	@test -z "$$(gofmt -l ./cmd ./internal ./prompts ./skill)" || (gofmt -l ./cmd ./internal ./prompts ./skill; exit 1)
	go vet ./...

demo: build
	./$(BIN) demo

fixtures:
	python3 testdata/scenarios/gen.py

clean:
	rm -f $(BIN)
	rm -rf dist
