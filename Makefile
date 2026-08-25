.PHONY: hygiene fmt vet test build

hygiene: fmt vet test

fmt:
	gofmt -l -w .

vet:
	go vet ./...

test:
	go test ./... -count=1

build:
	go build ./...