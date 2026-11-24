.PHONY: build test vet fmt lint run

build:
	go build -o bin/agentvault ./cmd/agentvault

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

lint: fmt vet test

run: build
	./bin/agentvault --help
