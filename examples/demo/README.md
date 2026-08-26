# Liftr Local Demo (M21.5)

A fully local, deterministic, reproducible demonstration of the Liftr control
plane. Everything you see flows through the real product boundaries:

```
HTTP API -> application orchestration -> PostgreSQL -> transactional outbox
         -> worker loop -> provisioner -> observation -> HTTP/CLI result
```

> **Non-production.** This composition registers three demo-only ResourceTypes
> (`DemoDatabase/v1`, `DemoApp/v1`, `DemoFault/v1`) backed by a deterministic
> provisioner that models no real infrastructure, and it runs the explicit
> insecure development authentication mode. It exists only under
> `cmd/liftr-demo-server` + this directory; production composition
> (`cmd/liftr-server`) is untouched and never sees any of it.

## Prerequisites

- Docker with compose (the whole stack runs containerized by default; under
  OrbStack, published ports appear on localhost automatically)
- Go toolchain (see `go.mod`) - used to build the CLI and, in native mode, the
  server
- bash, curl, python3 (no jq needed)
- No cloud credentials. No Kubernetes cluster. No Pulumi/OpenTofu binaries.

## One-command startup

```bash
make demo-up     # builds images/binaries, starts PostgreSQL, migrates, launches
make demo        # runs the 8-step guided walkthrough (~30 seconds)
make demo-down   # removes the server, drops the liftr_demo database
```

For the Backstage developer-plane proof, use
[`examples/demo/backstage.md`](backstage.md):

```bash
make demo-down
make demo-backstage-up
make demo-backstage
```

The server runs as a Docker Compose service (`compose.yaml`, profile `demo`)
by default: `DEMO_RUNTIME=docker make demo-up`. To build and run the server as
a native host process instead: `DEMO_RUNTIME=native make demo-up`. Both modes
expose exactly the same URLs on 127.0.0.1.

`demo-up` is idempotent: it reuses the running compose PostgreSQL container,
creates the dedicated `liftr_demo` database if missing, applies migrations,
and starts `liftr-demo-server` (container log: `docker compose --profile demo
logs demo-server`; native mode logs to `.demo/server.log`). Re-running the
walkthrough after a completed run: `make demo-down && make demo-up` first
(the script uses fixed Resource IDs).

## Demo architecture

```
+--------------------------------------------------------------+
| 127.0.0.1 (host exposure loopback-only via ports mapping)    |
|                                                              |
|  :18080 developer API      :18090 admin API    :18099 control |
|  /v1/resources ...         /admin/v1/...       POST /release/x |
|        |                        |                  |         |
|        +//////// internal/server.Compose /////////+         |
|        |   application service + outbox worker loop           |
|        |                        |                            |
|  PostgreSQL 17 (docker compose, database liftr_demo)          |
|                                |                             |
|                 deterministic demo provisioner <--------------+
|                 DemoApp/v1, DemoDatabase/v1, DemoFault/v1     |
+--------------------------------------------------------------+
|  Default mode: the server block runs as compose service      |
|  `demo-server` (examples/demo/Dockerfile). DEMO_RUNTIME=native|
|  builds and runs it on the host instead. Same URLs either way.|
+--------------------------------------------------------------+
```

What is REAL vs deterministic:

| Component                | Status |
|--------------------------|--------|
| HTTP API, DTOs, Problems | real production code path |
| AuthN/AuthZ middleware   | real; insecure dev principal (explicit opt-in) |
| PostgreSQL durability    | real PostgreSQL 17 + migrations 000001..000011 |
| Transactional outbox     | real |
| Worker loop / leases     | real (100 ms interval) |
| Lifecycle engine/gates   | real (dependency wait, wake, RESOURCE_IN_USE) |
| Admin diagnostics/recovery | real |
| Admission policy/quota   | real (`examples/demo/policy.json`) |
| Provisioner backend      | DETERMINISTIC demo stand-in; no real infra |

## The story the demo tells

1. **Discovery** - developers browse ResourceType contracts (`resource-type
   list/get`); DemoApp declares a hard-dependency slot `database`.
2. **Dependency** - `db-a` is created with `hold:true`; the simulated backend
   accepts the work but keeps reporting NotReady, so db-a stays Pending.
3. **Dependent** - `app-1` references `db-a`; admission succeeds but the
   operation waits durably: `DependenciesReady=False`
   (`WaitingForDependencies`) and provider submissions = 0 (proved via admin
   diagnostics).
4. **Ready + Wake** - the control plane releases db-a; its active observation
   reports success; the target transition enqueues a versioned
   `WakeDependents` work item, app-1's Drive passes the gate, submits exactly
   once, and converges Ready.
5. **Delete protection** - deleting `db-a` while referenced returns
   `409 RESOURCE_IN_USE`. Relationships govern lifecycle, not just metadata.
6. **Desired vs applied** - app-1 is re-pointed at a still-Pending `db-b`:
   the desired edge registers, the update waits with zero submissions, and
   BOTH deletes are refused (db-a protected by applied evidence, db-b by
   desired intent). After release, protection advances to db-b and db-a
   deletes cleanly. An identical retried PUT replays idempotently.
7. **Operator plane** - conclusive failure + explicit M13 retry; an ambiguous
   submission resolved by Observe with `attemptCount=1` (never resubmitted);
   observe-on-terminal refused (`ACTION_NOT_APPLICABLE`); idempotent passive
   observe; M18 policy denial (`403 POLICY_DENIED`) and quota denial
   (`409 QUOTA_EXCEEDED`).
8. **Cleanup** - while app-1 is Deleting, db-b is STILL protected; once
   Deleted, edges release atomically and everything tears down cleanly.

Raw SQL is never required: private invariants are surfaced through behavior
and the operator plane.

## Browsing the API and OpenAPI

The demo server serves the handwritten contract documents from `docs/openapi/`
verbatim, plus a small landing page at the root. Production servers do not
expose either.

| URL | What you get |
|---|---|
| `http://127.0.0.1:18080/` | Landing page with links |
| `http://127.0.0.1:18081` | **Swagger UI** for the developer API (`make demo-up` starts it) |
| `http://127.0.0.1:18080/openapi/v1.yaml` | OpenAPI 3.0.3 — developer API |
| `http://127.0.0.1:18080/openapi/admin-v1.yaml` | OpenAPI 3.0.3 — operator plane |
| `http://127.0.0.1:18080/v1/resource-types` | ResourceType discovery (JSON) |
| `http://127.0.0.1:18080/v1/resources` | Resource inventory (JSON) |
| `http://127.0.0.1:18080/healthz`, `/readyz` | Health/readiness |

The UI is a pinned read-only viewer mounted straight from `docs/openapi/v1`,
so it always renders the repository's current contract. The admin spec stays
available as raw YAML from the URL above.

## Troubleshooting

- The demo server logs to `.demo/server.log`; stop/start with `make demo-down`
  / `make demo-up`.
- The OpenAPI URLs answer 503 if the binary is not started from the repository
  root; always launch through `make demo-up` (or set `LIFTR_OPENAPI_DIR`).
- Re-running `make demo` after a completed walkthrough needs a fresh database
  (the script uses fixed Resource IDs): `make demo-down && make demo-up`.

## Security notes

- Host exposure is governed exclusively by the compose `ports` mappings, which
  are pinned to `127.0.0.1`; inside the container the listeners bind the
  container interface (`LIFTR_DEMO_ALLOW_REMOTE=1` is required for that and is
  set only in this demo composition). Native mode enforces loopback binds in
  the binary itself and refuses other addresses unless the same variable is
  set explicitly.
- Insecure authentication means ANY caller accepted on :18080/:18090 - safe
  only because of the loopback-only host exposure.
- The :18099 control listener simulates backend convergence. It exists ONLY in
  the demo binary; Liftr has no forced-success primitive anywhere.
- Production defaults are unchanged: `liftr-server` still requires OIDC
  configuration (or explicit `LIFTR_AUTH_MODE=insecure`), distinct admin
  audience, and never registers these ResourceTypes.

## Optional: real OpenTofu proof (no cloud)

A second guided walkthrough provisions `DemoWorkload/v1` through the
PRODUCTION OpenTofu adapter (M19): a real pinned `tofu` binary runs init/plan/
apply as a fenced child process, durable attempt evidence lands in PostgreSQL,
contract outputs publish through the developer API, and delete performs a real
destroy — all against the built-in `terraform_data` resource, so no cloud and
no provider downloads are involved.

```bash
make demo-opentofu    # requires OpenTofu 1.12.6 at ~/.opentofu/bin/tofu
                      # (or LIFTR_DEMO_TOFU_BIN=/path/to/tofu)
```

Expected highlights: outputs like
`{"endpoint": "workload-1-gen2.demo.liftr.internal", "generation": 2}`
advancing across generations, and the local terraform state file's `serial`
incrementing on every real apply/destroy (path printed during the run).

This composition is explicitly non-production: it uses the sanctioned
DEVELOPMENT LOCAL file state backend and insecure loopback auth. Production
OpenTofu requires an operator-supplied conformant HTTPS HTTP backend and full
authentication (ADR-0020).

For automated qualification instead of the guided tour:

```bash
LIFTR_TEST_OPENTOFU_BIN="$HOME/.opentofu/bin/tofu" make test-opentofu
```

## Optional: real Azure proof (cost-bearing; NOT run automatically)

The production path provisions Azure PostgreSQL Flexible Server through the
registered Pulumi program:

```bash
# Requires ARM_* credentials plus LIFTR_ACCEPTANCE_AZURE=1 and the
# LIFTR_ACCEPTANCE_* variables documented in the root README.md.
make build-programs
make test-acceptance-azure
```

WARNING: this creates REAL billable Azure resources and typically costs a few
dollars per hour while running. Delete the created Resource through Liftr
immediately after the run and verify the resource group is empty. Never wire
this into CI.
