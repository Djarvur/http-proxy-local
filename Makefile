# Makefile — task runner for http-proxy-local.
# mise.toml pins the tool versions; this file defines the commands. Both
# surfaces are kept in sync so `make <task>` and `mise run <task>` are
# equivalent.

BINARY := bin/http-proxy-local
PKG := ./...

.PHONY: build test test-race lint run clean tidy

## build: compile the binary into ./bin
build:
	go build -o $(BINARY) ./cmd/http-proxy-local

## test: run the full test suite
test:
	go test $(PKG)

## test-race: run tests with the race detector (used by CI)
test-race:
	go test -race $(PKG)

## lint: run golangci-lint with the strict config (zero findings required)
lint:
	golangci-lint run $(PKG)

## run: build and run locally (pass ARGS="--config /path" to override config)
run: build
	./$(BINARY) $(ARGS)

## tidy: normalize go.mod / go.sum
tidy:
	go mod tidy

## clean: remove build artifacts
clean:
	rm -rf bin/
