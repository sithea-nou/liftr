VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build fmt fmt-check test test-race test-integration test-opentofu build-programs build-acceptance-azure-storage-program test-acceptance-azure test-acceptance-azure-storage test-acceptance-azure-storage-pulumi test-acceptance-azure-storage-opentofu prepare-acceptance-azure-storage-opentofu vet verify verify-backstage demo-build demo-up demo demo-backstage-up demo-backstage demo-backstage-test demo-down

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

test-opentofu:
	@test -n "$$LIFTR_TEST_OPENTOFU_BIN" || (echo "LIFTR_TEST_OPENTOFU_BIN is required." && exit 1)
	go test ./internal/provisioning/opentofu -run 'TestL2|TestHTTP' -count=1 -v

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

# Opt-in, cost-bearing Azure Storage qualification. ObjectStorage/v1 remains
# acceptance-only and is not registered by the production server composition.
# The OpenTofu package is downloaded into ignored .demo state and verified
# against the committed dependency lock before adapter execution.
OPENTOFU_BIN ?= $(HOME)/.opentofu/bin/tofu
AZURE_STORAGE_TOFU_MIRROR ?= $(abspath .demo/opentofu-provider-mirror)
AZURE_STORAGE_TOFU_PLATFORM ?= $(shell go env GOOS)_$(shell go env GOARCH)

prepare-acceptance-azure-storage-opentofu:
	@test -x "$(OPENTOFU_BIN)" || (echo "OPENTOFU_BIN must point to executable OpenTofu 1.12.6." && exit 1)
	@mkdir -p "$(AZURE_STORAGE_TOFU_MIRROR)"
	cd internal/provisioning/opentofu/testdata/azureobjectstorage && \
		"$(OPENTOFU_BIN)" providers mirror -platform="$(AZURE_STORAGE_TOFU_PLATFORM)" "$(AZURE_STORAGE_TOFU_MIRROR)"

build-acceptance-azure-storage-program:
	cd internal/provisioning/pulumi/programs/azureobjectstorage && \
		GOTOOLCHAIN=local go build -o program .

test-acceptance-azure-storage-pulumi: build-acceptance-azure-storage-program
	LIFTR_ACCEPTANCE_AZURE_STORAGE_PULUMI=1 go test ./internal/provisioning/pulumi \
		-count=1 -run '^TestAzureStorageLifecyclePulumi$$' -v -timeout 2h

test-acceptance-azure-storage-opentofu: prepare-acceptance-azure-storage-opentofu
	LIFTR_TEST_OPENTOFU_BIN="$(OPENTOFU_BIN)" \
	LIFTR_ACCEPTANCE_OPENTOFU_PROVIDER_MIRROR="$(AZURE_STORAGE_TOFU_MIRROR)" \
	LIFTR_ACCEPTANCE_AZURE_STORAGE_OPENTOFU=1 go test ./internal/provisioning/opentofu \
		-count=1 -run '^TestAzureStorageLifecycleOpenTofu$$' -v -timeout 2h

test-acceptance-azure-storage: test-acceptance-azure-storage-pulumi test-acceptance-azure-storage-opentofu

lint:
	@command -v golangci-lint >/dev/null 2>&1 || (echo "golangci-lint is required. Install via: go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest" && exit 1)
	golangci-lint run ./...

vet:
	go vet ./...

verify: fmt-check lint vet test

# ---- Local demo (examples/demo). Non-production: the demo server registers
# ---- demo-only ResourceTypes and a deterministic provisioner, runs insecure
# ---- dev authentication, and refuses non-loopback listeners by default.

DEMO_DIR := .demo
DEMO_BINDIR := $(DEMO_DIR)/bin
DEMO_LOG := $(DEMO_DIR)/server.log
DEMO_PID := $(DEMO_DIR)/server.pid
# Retained only so demo-down can clean processes created by older revisions;
# current Backstage demo targets run exclusively through Docker Compose.
DEMO_BACKSTAGE_APP_PID := $(DEMO_DIR)/backstage-app.pid
DEMO_BACKSTAGE_BACKEND_PID := $(DEMO_DIR)/backstage-backend.pid
DEMO_DB ?= liftr_demo
DEMO_DATABASE_URL ?= postgres://liftr:liftr@127.0.0.1:55432/$(DEMO_DB)?sslmode=disable
# docker (default) runs the demo server as a compose service; native builds
# and runs the binary on the host instead.
DEMO_RUNTIME ?= docker
# The generic demo walkthrough uses the host CLI. The Backstage walkthrough
# does not, so its target disables this host build and stays container-only.
DEMO_BUILD_CLI ?= 1

demo-build:
	go build -o $(DEMO_BINDIR)/liftr-demo-server ./cmd/liftr-demo-server
	go build -o $(DEMO_BINDIR)/liftr ./cmd/liftr

demo-up:
	@mkdir -p $(DEMO_DIR)
	@if [ -f "$(DEMO_PID)" ] && kill -0 "$$(cat $(DEMO_PID))" 2>/dev/null; then \
		echo "stale native demo server (pid $$(cat $(DEMO_PID))) found; killing it"; \
		kill "$$(cat $(DEMO_PID))" 2>/dev/null; sleep 1; \
	fi
	@rm -f $(DEMO_PID)
	@docker compose up -d postgres >/dev/null 2>&1 || docker-compose up -d postgres >/dev/null
	@until docker exec liftr-postgres-1 pg_isready -U liftr -d liftr >/dev/null 2>&1; do sleep 0.5; done
	@docker exec liftr-postgres-1 psql -U liftr -d liftr -tAc \
		"SELECT 1 FROM pg_database WHERE datname='$(DEMO_DB)'" | grep -q 1 || \
		docker exec liftr-postgres-1 createdb -U liftr $(DEMO_DB)
	@if [ "$(DEMO_RUNTIME)" = "docker" ]; then \
		if [ "$(DEMO_BUILD_CLI)" = "1" ]; then go build -o $(DEMO_BINDIR)/liftr ./cmd/liftr; fi; \
		docker compose --profile demo up -d --build --force-recreate demo-server swagger-ui || exit 1; \
	else \
		go build -o $(DEMO_BINDIR)/liftr-demo-server ./cmd/liftr-demo-server && \
		go build -o $(DEMO_BINDIR)/liftr ./cmd/liftr && \
		if [ -f "$(DEMO_PID)" ] && kill -0 "$$(cat $(DEMO_PID))" 2>/dev/null; then \
			echo "demo server already running (pid $$(cat $(DEMO_PID)))"; \
		else \
			LIFTR_DEMO_DATABASE_URL='$(DEMO_DATABASE_URL)' \
			LIFTR_POLICY_FILE=examples/demo/policy.json \
			nohup $(DEMO_BINDIR)/liftr-demo-server >> $(DEMO_LOG) 2>&1 & \
			echo $$! > $(DEMO_PID); \
		fi; \
	fi
	@for i in $$(seq 1 100); do \
		if curl -fsS http://127.0.0.1:18080/readyz >/dev/null 2>&1; then \
			echo "demo server ready ($(DEMO_RUNTIME)) on http://127.0.0.1:18080 (admin http://127.0.0.1:18090)"; \
			[ "$(DEMO_RUNTIME)" = "docker" ] && echo "Swagger UI: http://127.0.0.1:18081"; exit 0; fi; \
		sleep 0.3; \
	done; \
	echo "demo server did not become ready"; \
	[ "$(DEMO_RUNTIME)" = "native" ] && tail -20 $(DEMO_LOG); \
	docker compose --profile demo logs --tail 40 demo-server; exit 1

demo:
	@bash examples/demo/demo.sh

demo-backstage-up: DEMO_RUNTIME=docker
demo-backstage-up: DEMO_BUILD_CLI=0
demo-backstage-up: demo-up
	@docker compose --profile demo --profile backstage up -d --build --force-recreate backstage-backend backstage-app
	@for i in $$(seq 1 180); do \
		if curl -fsS http://localhost:3000/liftr >/dev/null 2>&1 && \
			curl -fsS -H 'Origin: http://localhost:3000' \
				http://127.0.0.1:7007/api/auth/guest/refresh >/dev/null 2>&1 && \
			curl -fsS -D - -o /dev/null -X OPTIONS \
				-H 'Origin: http://backstage-app.liftr.orb.local' \
				-H 'Access-Control-Request-Method: GET' \
				http://127.0.0.1:7007/api/auth/guest/refresh 2>/dev/null | \
				grep -Fqi 'Access-Control-Allow-Origin: http://backstage-app.liftr.orb.local'; then \
			echo "Containerized Backstage demo ready on http://localhost:3000/liftr"; exit 0; fi; \
		sleep 0.5; \
	done; \
	echo "Backstage frontend did not become ready"; \
	docker compose --profile demo --profile backstage logs --tail 80 backstage-backend backstage-app; exit 1

demo-backstage:
	@echo "Containerized Backstage developer plane: http://localhost:3000/liftr"
	@echo "OrbStack DNS (when available): http://backstage-app.liftr.orb.local/liftr"
	@echo "Release a held demo Resource outside Backstage: curl -fsS -X POST http://127.0.0.1:18099/release/RESOURCE_ID"
	@echo "Walkthrough: examples/demo/backstage.md"

demo-backstage-test:
	@docker compose --profile demo --profile backstage build backstage-test
	@docker compose --profile demo --profile backstage run --rm backstage-test

demo-opentofu:
	@bash examples/demo/demo-opentofu.sh

demo-down:
	@for pidfile in $(DEMO_BACKSTAGE_APP_PID) $(DEMO_BACKSTAGE_BACKEND_PID); do \
		if [ -f "$$pidfile" ] && kill -0 "$$(cat $$pidfile)" 2>/dev/null; then kill "$$(cat $$pidfile)" 2>/dev/null || true; fi; \
		rm -f "$$pidfile"; \
	done
	@rm -f $(DEMO_PID)
	@docker compose --profile demo --profile backstage rm -sf backstage-app backstage-backend demo-server swagger-ui >/dev/null 2>&1 && echo "demo containers removed" || true
	@if [ -f "$(DEMO_PID).old" ]; then rm -f "$(DEMO_PID).old"; fi
	@if pgrep -fl liftr-demo-server >/dev/null 2>&1; then \
		echo "native demo server still running; killing it"; \
		pkill -f liftr-demo-server 2>/dev/null || true; sleep 1; \
	fi
	@for i in 1 2 3 4 5; do \
		if docker exec liftr-postgres-1 dropdb -U liftr --force --if-exists $(DEMO_DB) 2>/dev/null; then \
			echo "demo database dropped"; break; fi; sleep 1; done
	@docker exec liftr-postgres-1 psql -U liftr -d liftr -tAc \
		"SELECT 1 FROM pg_database WHERE datname='$(DEMO_DB)'" | grep -q 1 && \
		echo "warning: $(DEMO_DB) could not be dropped yet" || true

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
