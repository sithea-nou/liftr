# ADR-0008: HTTP Resource API Contract (v1)

- Status: Accepted
- Date: 2026-08-22

## Context

ADRs 0002 through 0007 define the provisioner-neutral domain model, deterministic lifecycle engine, provider-neutral provisioning contract, durable PostgreSQL orchestration, and a Pulumi adapter. Liftr now has a complete control-plane core but no public API: developers cannot create, inspect, update, or delete Resources over transport. The architecture positions portals, CLIs, and automation as clients of exactly such an API, so Milestone 7 defines the first stable public surface.

Two constraints shaped the contract. First, the developer-facing Resource contract must stay provisioner-neutral: Pulumi, persistence, worker, lease, attempt, and fingerprint concepts must have no representation in payloads. Second, long-running infrastructure work is asynchronous by design, so mutations are admissions of lifecycle intent rather than completed effects, and clients need explicit means to monitor progress, avoid duplicate submissions, and detect concurrent modification.

Three corrections were applied to the approved design before implementation and are recorded here as final decisions: no generation wildcard exists in v1, the Liftr-Generation response header has strict Resource-only scope, and the latest Operation of a Resource is selected deterministically even when timestamps tie.

## Decision

Liftr exposes a versioned HTTP Resource API on the standard library. The transport layer is a thin adapter over the existing application boundary: it uses only application use cases — including new minimal read paths `GetResource`, `GetOperation`, and `GetResourceOperation` backed by a `LatestForResource` repository port implemented for both PostgreSQL and the test store — and never queries repositories, provisioners, or lifecycle policy directly.

### Surface

```text
POST   /v1/resources          admit create
GET    /v1/resources/{id}     read retained Resource
PUT    /v1/resources/{id}     admit spec revision
DELETE /v1/resources/{id}     admit delete
GET    /v1/operations/{id}    read lifecycle Operation
GET    /healthz               process liveness
GET    /readyz                readiness, probes durable state
```

Lists, PATCH, Event APIs, ResourceType discovery, retry endpoints, authentication, and streaming are out of scope for v1.

### Public Representations

Resource exposes only: `id`, `type` (name/version), `owner` (kind/id), `generation`, `spec`, `status`, `latestOperation`, `createdAt`, `updatedAt`. `spec` remains opaque, ResourceType-defined JSON. `latestOperation` carries `id`, `capability`, `state`, `targetGeneration`, and `href`.

ResourceStatus is Liftr-owned and provider-neutral: `state` (`Unknown|Pending|Ready|Deleting|Deleted|Failed`), `observedGeneration`, `conditions` (type/status/reason/message/observedGeneration/lastTransitionAt), `updatedAt`. Conditions report normalized facts; observedGeneration tracks evaluated desired generations without implying reconciliation success.

Operation exposes only: `id`, `resourceId`, `capability`, `state` (`Pending|Running|Succeeded|Failed|Canceled`), `targetGeneration`, `requestedAt`, optional `startedAt`, optional `completedAt`, and optional `failure{reason,message}`. Internal execution phases have no public representation in v1; they remain internal lifecycle machinery and may never be serialized.

Provisioner references, execution handles, attempts, submission evidence, storage record versions, fingerprints, outbox and worker concepts are never part of any payload.

### Mutation Semantics

POST returns 201 Created with `Location` pointing at `/v1/resources/{id}` and a `Link rel="monitor"` pointing at the admitted Operation. PUT and DELETE return 202 Accepted with `Location` pointing at `/v1/operations/{operationID}` and the same monitor Link. Infrastructure readiness stays asynchronous in every case; bodies carry the current Resource snapshot rather than provider outcomes.

Clients never supply fingerprints or lifecycle identifiers; the server mints Operation and Event identifiers per admission. Canonical application-owned fingerprinting determines replay identity.

### Generation Preconditions Without Wildcards

PUT and DELETE require `If-Liftr-Generation: <uint64>` carrying one concrete decimal generation:

- missing header → 428 PRECONDITION_REQUIRED
- malformed or non-uint64 value → 400 INVALID_ARGUMENT
- mismatched generation → 409 GENERATION_CONFLICT
- matching generation → proceed

Wildcard semantics do not exist in v1. Simulating "whatever generation exists now" at the transport layer would require a non-atomic read-then-mutate sequence that provides no real concurrency guarantee, so no wildcard spelling is accepted anywhere. Application-level any-generation semantics would be a new concurrency model and is deliberately not introduced here; the existing mutation API keeps its concrete expected-generation requirement.

### Liftr-Generation Header Scope

`Liftr-Generation` reports `Resource.Generation` for the Resource represented by this response. It appears on all four Resource responses — POST, GET, PUT, DELETE under `/v1/resources` — and nowhere else. GET `/v1/operations/{id}`, `/healthz`, and `/readyz` represent no Resource and never emit it merely because the server happens to know a generation. GENERATION_CONFLICT problems may expose the narrow extension field `currentGeneration` so clients can recover without guessing.

Versioned Resource and Operation responses set `Cache-Control: no-store`: ResourceStatus and Operation state are mutable and v1 intentionally ships no representation validator, so cached copies could be stale. Response caching and representation validators are deferred to a future decision.

### Deterministic Latest Operation

`latestOperation` on Resource representations and the underlying read path select the most recent Operation for a Resource using a total order: newest `requested_at` first, ties broken by descending Operation ID compared byte-wise. The identifier tiebreak is locale-independent, so repeated calls always select the same Operation even when two Operations share a timestamp, and every repository implementation applies the same logical ordering. This ordering is part of the public contract because `latestOperation` is observable in payloads.

### Idempotency and Replays

Every mutation requires an `Idempotency-Key` header; requests without one are invalid. Replaying the same key with byte-equivalent request content resolves to the original admission:

- CREATE replays answer 201 with `Idempotency-Replayed: true`
- UPDATE/DELETE replays answer 202 with `Idempotency-Replayed: true`
- Location and Link identify the original lifecycle Operation
- the body contains the current Resource snapshot

Because later mutations may have succeeded meanwhile, a replay body's `latestOperation` may be newer than the Operation its Location/Link references. This asymmetry is deliberate: Location/Link answer "which execution did my original request start", while the body answers "what does the Resource look like now". Reusing a key with different content yields 409 IDEMPOTENCY_CONFLICT.

### Tombstones

A retained Deleted Resource is a real representation: GET returns 200 with `status.state = "Deleted"`. 404 means Liftr holds no retained record for the ID at all. POST using an ID with any retained record — live or tombstone — returns 409 RESOURCE_ALREADY_EXISTS. Tombstone resurrection is not supported in v1; recreation flows that reuse identity belong to a future decision.

### Errors

Errors use RFC 9457 Problem Details with `application/problem+json`, standard fields (`type`, `title`, `status`, `detail`, `instance`) and Liftr extensions `code` plus `requestId`. Approved codes:

| Code | HTTP |
| --- | --- |
| INVALID_ARGUMENT | 400 |
| UNSUPPORTED_RESOURCE_TYPE | 422 |
| RESOURCE_NOT_FOUND | 404 |
| OPERATION_NOT_FOUND | 404 |
| RESOURCE_ALREADY_EXISTS | 409 |
| IDEMPOTENCY_CONFLICT | 409 |
| GENERATION_CONFLICT | 409 |
| OPERATION_ACTIVE | 409 |
| RESOURCE_STATE_CONFLICT | 409 |
| UNSUPPORTED_CAPABILITY | 409 (reserved) |
| PRECONDITION_REQUIRED | 428 |
| PROVISIONER_UNAVAILABLE | 503 |
| PERSISTENCE_UNAVAILABLE | 503 |
| INTERNAL | 500 |

Details are curated client-safe sentences. Raw provider, Go, or persistence errors never reach a client; unclassifiable failures render as opaque INTERNAL problems. UNSUPPORTED_CAPABILITY is registered for contract completeness but has no producing route in v1 because capabilities arrive only through fixed method routing.

### JSON Rules

Request envelopes reject unknown fields; `spec` allows arbitrary keys at every depth. Numbers decode with exact literal preservation: integer literals become 64-bit integers, decimal and exponent literals become floats, and overflowing integers are rejected rather than coerced. Consequently `1` and `1.0` are semantically distinct spec values and yield distinct fingerprints; reusing one Idempotency-Key across them conflicts instead of silently matching. Bodies exceeding a small size bound are rejected before parsing.

### Request Identity

Every response carries an authoritative server-generated `X-Request-ID`; client-supplied values never replace it. An optional `X-Correlation-ID` remains strictly separate and is echoed verbatim. Problem responses embed the authoritative `requestId`.

### OpenAPI Contract

A handwritten OpenAPI document at `docs/openapi/v1/openapi.yaml` is authoritative for the public surface and explicitly models Resource, ResourceStatus, Condition, Operation, and Problem schemas, keeping only `ResourceSpec` open-ended. Contract tests verify the document against the running router and schema shapes, so drift between documentation and behavior fails the build. Documentation generation from internal Go structures is deliberately avoided to keep the public contract independent of implementation types.

## Consequences

- Developers get a stable, provisioner-neutral Resource API while infrastructure execution remains fully asynchronous behind Operations.
- Strict concrete-generation preconditions give clients real lost-update protection; absence of wildcards trades convenience for honesty about atomicity.
- Resource-scoped header emission keeps Operation and health responses free of misleading cache/validation signals.
- Deterministic latest-operation selection makes public representations reproducible across repeated reads and across storage implementations.
- Replay responses deliberately mix old Location/Link with fresh snapshots; clients must monitor via `latestOperation.href` when they want current progress.
- No representation validator exists in v1; clients must not cache versioned responses.
- Internal machinery (phases, attempts, leases, fingerprints) staying unserialized preserves the option to evolve execution internals without breaking clients.

## Scope Exclusions

v1 does not add Resource or Operation lists, PATCH, Event APIs, ResourceType discovery, retry APIs, authentication, authorization, CLI tooling, Backstage integration, server-sent events or WebSockets, tombstone resurrection, provider-facing APIs, or new provisioners. Response caching and representation validators are deferred. Application-level any-generation semantics are deferred as a potential future concurrency model and are not simulated anywhere in v1.

## Alternatives Considered

### Wildcard Generation Precondition

Accepting `*` would let clients mutate without knowing the current generation. It cannot be implemented atomically on top of a concrete-generation mutation API — the transport would read then submit, which neither means "current" nor prevents lost updates. Rejected for v1.

### ETag Validators on Resource Responses

A weak validator derived from generation could support conditional reads. ResourceStatus mutates independently of generation, so a single-validator design would mislead clients; validators were deferred together with caching policy.

### Timestamp-Only Latest Operation Ordering

Ordering solely by `requested_at DESC` is nondeterministic when Operations share timestamps, making public `latestOperation` unstable across reads. The immutable identifier tiebreak was added instead of relying on insertion order, which differs across repositories.

### Generated OpenAPI From Go Structs

Generating documentation from DTOs couples the published contract to internal type evolution and invites accidental exposure of internal fields. A handwritten contract verified by tests was selected.
