.PHONY: build test run

build:
	go build -trimpath -o bin/codearts2api-panel ./cmd/server

test:
	go test ./...

run:
	go run ./cmd/server
