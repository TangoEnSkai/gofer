.PHONY: build test vet fmt

build:
	go build -o bin/gofer ./cmd/gofer

test:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -s -w .
