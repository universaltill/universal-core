.PHONY: build test test-integration vet fmt

build:
	go build -o bin/universal-core ./cmd/universal-core

test:
	go test ./...

# Requires TEST_DATABASE_URL, e.g.:
#   postgres://postgres@localhost:5432/universal_core_test?sslmode=disable
test-integration:
	go test ./... -run . -v

vet:
	go vet ./...
	gofmt -l .

fmt:
	gofmt -w .
