# ADR-0010: ResourceType Implementation Binding and Update Transition Semantics

- Status: Accepted
- Date: 2026-08-22

## Context

ADR-0009 made ResourceTypes first-class discoverable contracts, but no contract has ever been executed: PostgreSQLDatabase/v1 is validated and discoverable while every lifecycle execution still runs against test fakes. Liftr must now prove the full flow — developer intent through admission, orchestration, the provider-neutral Provisioner contract, a real Pulumi program, real infrastructure, normalized observation, and lifecycle status — without leaking any implementation concept into `ResourceSpec`, discovery, the HTTP API, or the domain.

Four tensions had to be resolved. First, where the binding between a developer contract and a concrete program lives, and which private configuration it may carry. Second, whether capability `update` means "some transitions" or "every schema-valid transition", and who decides. Third, how infrastructure identity can be deterministic without being globally ambiguous, given that a `ResourceID` is unique only within one control plane. Fourth, how much readiness truth execution evidence actually carries.

Two invariants were re-examined and are reaffirmed unchanged:

- **Ambiguous submission (ADR-0004/0005/0007).** Once a Pulumi process has been launched, absence of history does not prove absence of side effects. A nonzero exit, provider rejection, quota refusal, plugin failure, backend failure, or process crash after launch remains Unknown; it is never converted to NotFound from "zero history entries plus no update in progress". Only failures detected strictly before the invocation boundary — invalid request input, unsupported type/capability, missing required declared environment variables, source digest mismatch, workspace preparation failure, missing executable or invalid registration — are conclusive NotFound+Failed. Only a fresh, explicit Observe `NotFound`, against an attempt with no confirmed acceptance, authorizes a new submission attempt.
- **Observation honesty (ADR-0007).** Execution evidence is not independent observation. The adapter continues to report generic facts as Unknown for create/update success.

## Decision

### Implementation Binding by Composition

The binding between PostgreSQLDatabase/v1 and its Pulumi program is expressed exclusively in startup composition. The contract package (`internal/resourcetypes/postgresqldatabase`) stays free of provisioning imports — enforced by import-boundary tests — and the adapter package stays resource-agnostic. Composition registers one `Program` per `ResourceTypeRef` with trusted source, digest, project identity, capabilities, and an input encoder. Program sources live under the private provisioning tree; prebuilt binaries are build artifacts and are never committed.

The encoder translates neutral execution intent into a versioned private input envelope. Mapping rules for PostgreSQLDatabase/v1: `version` passes through as the engine major version; `storageGB` is consumed through shared integral-number semantics so `int64(20)` and `float64(20.0)` produce byte-identical canonical values, while fractional, non-finite, non-numeric, or precision-unsafe values are rejected before encoding; `highAvailability` selects availability mode under platform policy. The stored `ResourceSpec` representation is never altered by this mapping.

### Contract Versus Platform Configuration

Developer configuration is exactly the published spec schema. Platform implementation configuration — cloud account/subscription identity, location, SKU, network posture, naming namespace, provider authentication, plugin settings — is supplied privately to composition at startup. Provisioner configuration (backend URL, workspace root, CLI/Go runtimes, history bounds, stack naming version) stays in the adapter registration. No category collapses into another, and none appears outside its layer.

### Environment-Name Allowlist

Program registrations declare the names of additional child-process environment variables they require. Values are supplied at execution time by the platform callback, validated against the declaration, and exist only inside the isolated workspace. Undeclared supplied keys are rejected; declared-but-missing names cause a conclusive pre-invocation rejection because Liftr itself detected the gap before launch. Reserved channels (backend URL, passphrase, adapter plumbing, process basics) cannot be declared per-program.

### Update Transitions Are Contract Semantics

Capability `update` means "the transitions this contract declares legal are supported"; it does not mean "any schema-valid pair". The contract owns transition legality through `ValidateUpdate(oldSpec, newSpec)`, enforced synchronously during update admission — after idempotency replay resolution, after structural validation, and before any desired-state mutation — so illegal transitions return structured 422 RESOURCE_SPEC_INVALID violations carrying the reserved `transition` keyword with zero durable effects. Application consumes this via its own port; concrete ResourceType implementations author rules using resourcetypes-local types and never import application.

PostgreSQLDatabase/v1 declares, as developer-contract semantics that every implementation claiming the type must satisfy — not as observations about any particular vendor:

- `version` is immutable after creation; changing engine major versions is a migration workflow v1 does not offer.
- `storageGB` may stay equal or grow; shrinking allocated storage is not offered.
- `highAvailability` may toggle freely in both directions.

Replay precedence is preserved: a replayed previously admitted request resolves from its Idempotency-Key even when the same transition would be rejected if submitted fresh today.

### Platform-Scoped Infrastructure Naming

Private infrastructure names digest stable platform identity, implementation namespace, ResourceTypeRef, and ResourceID into a fixed-width, backend-safe value. Two installations configured with distinct identities therefore derive distinct names for identical ResourceIDs. OperationID, generation, and attempt number are never inputs. Create, update, retry, observe, and delete derive the same name for the same logical Resource. This infrastructure name is separate from Pulumi stack identity, whose v1 algorithm is unchanged.

### Readiness Meaning and Observation Facts

For M9, lifecycle completion means "the latest desired-generation execution succeeded." Mainstream managed-database provider plugins poll cloud readiness during creation, so execution success carries secondhand readiness evidence, and Liftr reports state Ready on that basis. It is not independent verification: the adapter reports Presence/Readiness/Drift as Unknown for create/update outcomes, drift stays permanently Unknown without Refresh or a read API, and passive observation produces no fabricated facts. The designated future mechanism for evidence-based facts is a per-program observation hook behind the existing Provisioner contract; nothing is built now.

A conclusively correlated successful destroy does prove the stack's managed resources were removed, so delete success reports `Presence = NotFound` with Readiness and Drift Unknown. Lifecycle delete completion continues to set the Deleted tombstone independently of facts.

### Secrets

No credentials exist in `ResourceSpec`. Provider authentication flows through the environment-name allowlist into child processes only. Database administrator passwords are generated inside the reference program as Pulumi secrets, persist encrypted in platform-owned state, remain stable across updates (the generating resource's URN and inputs never change), and are excluded from update diffs. Outputs stay suppressed; no retrieval path exists.

### Reference Implementation

The first concrete implementation targets Azure Database for PostgreSQL Flexible Server as the private **reference**. The execution architecture is proven by the deterministic credential-free CI path; the Azure program itself ships with an opt-in, cost-bearing acceptance suite and is regarded as **unvalidated until that suite has run successfully** against a live subscription. This is a private choice recorded as an example; it does not make Liftr an Azure product, and no Azure vocabulary appears in any public surface. Normal CI uses a clearly named deterministic test program exercising real Pulumi invocation, file-backend state, envelope validation, stack identity, history correlation, create/update/delete orchestration, and recovery behavior without credentials. The Azure acceptance run is opt-in, cost-bearing, explicitly gated, and excluded from default verification.

### Initial Runtime Composition

`liftr-server` composes persistence, catalog, provisioner, HTTP handler, and a ticker-driven outbox worker loop when durable storage is configured. Shutdown is context-driven with bounded waits; there is no tight spin loop. Crashed or leased work remains recoverable purely through existing lease expiry, and multiple replicas running workers concurrently are safe by lease fencing. Provider availability never influences `/readyz`. API-serving and worker-execution deployment topologies are deliberately separable: callers may compose the handler and run workers elsewhere.

## Consequences

- Developers can create, grow, retarget availability, inspect, and delete real PostgreSQL databases through a fully provider-neutral API.
- Illegal transitions fail synchronously with structured violations instead of failing asynchronously in infrastructure.
- Distinct installations cannot collide on infrastructure names even with identical ResourceIDs.
- Misconfigured environments fail conclusively before launch; everything after launch preserves ambiguity, trading operator convenience for safety.
- `Ready` is documented reconciliation semantics, not a health check; drift is invisible until a read-API design lands.
- The reference program's dependency tree is isolated in a nested module so the control plane build stays small.

## Alternatives Considered

### Binding Inside the Contract Package

Would let implementations describe themselves but couples developer contracts to execution technology and breaks ADR-0001's boundary. Rejected.

### Implementation-Owned Transition Rules

Would make legality vary by backend and push knowable failures into async execution errors. Rejected; contracts advertise what update means.

### Absent-History Implies Not-Started

A nonzero exit with no history entry and no in-progress update would look conclusive, but launched executions can mutate infrastructure before recording anything. Rejected as unsound; ambiguity stands after possible invocation.

### Deriving Infrastructure Names From ResourceID Alone

ResourceIDs collide across installations sharing a backend. Rejected; names digest platform identity.

### Docker or State-File Reference Implementations

Cannot faithfully implement highAvailability or storageGB, or would fake infrastructure semantics entirely. Rejected for production paths; the credential-free CI program exists precisely to keep those concerns out of normal tests.

### Reading Outputs or Refreshing State For Facts

Violates the ADR-0007 data boundary and adds mutation risk for evidence Liftr cannot continuously verify. Deferred to a future observation-hook design.
