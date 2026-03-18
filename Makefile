.PHONY: all build test lint fmt tidy

all: fmt lint test build

build:
	go build -o ./bin/registry-push .

test:
	go test ./...

lint:
	golangci-lint run

fmt:
	gofumpt -w .
	goimports -w .

tidy:
	go mod tidy
