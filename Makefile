.PHONY: build fmt fmt-check test test-race test-integration vet verify

build:
	go build ./cmd/...

fmt:
	gofmt -w $$(find . -name '*.go' -not -path './vendor/*')

fmt-check:
	@test -z "$$(gofmt -l $$(find . -name '*.go' -not -path './vendor/*'))" || \
		(echo "Go files are not formatted. Run 'make fmt'." && exit 1)

test:
	go test ./...

test-race:
	go test -race ./...

test-integration:
	@test -n "$$LIFTR_TEST_DATABASE_URL" || (echo "LIFTR_TEST_DATABASE_URL is required." && exit 1)
	go test ./internal/persistence/postgres -count=1

vet:
	go vet ./...

verify: fmt-check vet test
