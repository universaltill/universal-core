.PHONY: build test test-integration vet fmt

build:
	go build -o bin/universal-core ./cmd/universal-core

test:
	go test ./...

# Requires TEST_DATABASE_URL, e.g.:
#   postgres://postgres@localhost:5432/universal_core_test?sslmode=disable
# -p 1: every package shares one live database — see ci.yml's comment on
# the same flag for why internal/worker's background pollers need this.
test-integration:
	go test ./... -run . -v -p 1

vet:
	go vet ./...
	gofmt -l .

fmt:
	gofmt -w .
