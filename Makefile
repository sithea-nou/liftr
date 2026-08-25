VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build fmt fmt-check test test-race test-integration build-programs test-acceptance-azure vet verify verify-backstage

build:
	go build -ldflags "-X main.version=$(VERSION)" ./cmd/...

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

# Backstage integration checks. Never part of `verify`: Go contributors do
# not need Node. CI runs this as an independent job.
# Yarn is pinned via packageManager + Corepack; installation is immutable and
# must never modify the working tree (enforced by the git diff gate).
verify-backstage:
	@command -v node >/dev/null 2>&1 || { echo "Node.js >= 20 is required for verify-backstage."; exit 1; }
	cd integrations/backstage && \
		COREPACK_ENABLE_DOWNLOAD_PROMPT=0 corepack yarn@4.9.2 install --immutable && \
		COREPACK_ENABLE_DOWNLOAD_PROMPT=0 corepack yarn@4.9.2 typecheck && \
		COREPACK_ENABLE_DOWNLOAD_PROMPT=0 corepack yarn@4.9.2 lint && \
		COREPACK_ENABLE_DOWNLOAD_PROMPT=0 corepack yarn@4.9.2 test && \
		COREPACK_ENABLE_DOWNLOAD_PROMPT=0 corepack yarn@4.9.2 verify:host && \
		git diff --exit-code -- . ':(exclude).yarn'
