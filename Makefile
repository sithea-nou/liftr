.PHONY: build fmt fmt-check test vet verify

build:
	go build ./cmd/liftr-server

fmt:
	gofmt -w $$(find . -name '*.go' -not -path './vendor/*')

fmt-check:
	@test -z "$$(gofmt -l $$(find . -name '*.go' -not -path './vendor/*'))" || \
		(echo "Go files are not formatted. Run 'make fmt'." && exit 1)

test:
	go test ./...

vet:
	go vet ./...

verify: fmt-check vet test
