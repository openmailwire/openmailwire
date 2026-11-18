.PHONY: build vet test test-all

build:
	go build -trimpath -o bin/openmailwire ./cmd/openmailwire

vet:
	go vet ./...

test:
	go test ./...

# Needs OMW_TEST_DATABASE_URL pointing at a throwaway database.
test-all:
	go test -race ./...
