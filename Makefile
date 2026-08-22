.PHONY: build fmt fmt-check test test-race test-integration build-programs test-acceptance-azure vet verify

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

# Builds the prebuilt binary for every registered Pulumi program. The
# binaries are never committed; registration digests cover the source tree.
build-programs:
	cd internal/provisioning/pulumi/programs/azureflexiblepostgresql && \
		GOTOOLCHAIN=local go build -o program .

# Opt-in, cost-bearing acceptance run against real Azure infrastructure.
# Requires LIFTR_ACCEPTANCE_AZURE=1 plus the configuration and credential
# variables documented in README.md. Never part of `verify`.
test-acceptance-azure: build-programs
	LIFTR_ACCEPTANCE_AZURE=1 go test ./internal/provisioning/pulumi -count=1 -run TestAzureFlexibleServerLifecycle -v -timeout 3h

vet:
	go vet ./...

verify: fmt-check vet test
