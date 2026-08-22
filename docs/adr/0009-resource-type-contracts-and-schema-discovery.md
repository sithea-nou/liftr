# ADR-0009: ResourceType Contracts and Schema Discovery

- Status: Accepted
- Date: 2026-08-22

## Context

ADR-0002 defined ResourceSpec as opaque developer intent and explicitly deferred ResourceType schema publication and schema validation until discovery and API requirements justified choosing a mechanism. Milestone 7 delivered the public HTTP Resource API; its `spec` field accepts arbitrary well-formed JSON, which leaves direct clients, a future CLI, and Backstage integration dependent on out-of-band documentation to learn what a valid spec looks like.

Liftr now needs ResourceTypes to become first-class discoverable contracts. Developers should be able to ask what ResourceTypes exist, which version applies, which capabilities the contract supports, and what JSON shape the spec accepts — without knowing anything about Pulumi, Terraform, Crossplane, provisioner registration, stack configuration, or cloud implementation. The boundary stays fixed by ADR-0001 and ADR-0004: a ResourceType describes developer intent; a provisioner describes implementation.

## Decision

### Ownership Boundary

A ResourceType contract owns exactly: identity (`name` + `version`), display metadata (`displayName`, `description`), developer-facing capabilities, a self-contained JSON Schema document for the accepted ResourceSpec, semantic validation behavior, and a stable schema `$id`. It must never contain ProvisionerRef, Pulumi projects or stacks, Terraform workspaces, Crossplane kinds, Git repositories, cloud accounts or regions, Kubernetes namespaces, credentials, execution handles, availability state, or UI metadata such as icons, tags, categories, widgets, or form layouts. Those may be layered later only if a concrete requirement appears.

The catalog answers "what developer contracts exist?"; the ProvisionerSelector answers "how will this contract be implemented?". Discovery exposes contract capabilities only. Capabilities are properties of the contract, not guarantees about current backend health, provisioner selection, or the currently supported capability subset; capability gaps surface at mutation time as PROVISIONER_UNAVAILABLE. The `observe` capability is contract metadata only — v1 defines no developer-facing observe endpoint.

### Package and Dependency Boundary

`internal/domain` remains schema-blind; nothing in it knows JSON Schema. `internal/resourcetypes` defines the developer contract (Contract, SpecSchema, semantic-validator hook) plus the deterministic in-memory registry, and is the only package permitted to depend on a JSON Schema implementation. The application consumes ResourceTypes exclusively through its consumer-owned port `application.ResourceTypeCatalog` returning the interface `application.ResourceContract`; concrete implementations satisfy that port structurally. The transport never imports the contract package and never performs ResourceType-specific validation; it only parses JSON envelopes. Dependency directions:

```text
domain  <-  resourcetypes  -> JSON Schema implementation
domain  <-  application
resourcetypes -> application (port names only)
api/http -> application
```

### Schema Standard and Representation

ResourceSpec schemas are expressed in **JSON Schema draft 2020-12**, stored as raw self-contained documents inside each ResourceType implementation, and compiled once at registration. Liftr does not invent a schema language. The choice of Go library is an implementation detail and deliberately not recorded here; swapping implementations must not change admission behavior, which tests pin at the contract level.

**M8 validation performs no network schema resolution and registered schemas must be self-contained.** Every external load attempt is refused deterministically. Local composition remains fully available: `$defs` with local `$ref: "#/$defs/..."` references are valid and tested. This is not a permanent claim that bundled or pre-registered external references can never be supported; if a future milestone needs them, that requires a new explicit decision.

Each schema declares `$id = urn:liftr:resource-type:<Name>:<Version>:spec`. The URN identifies the schema document for integrity checks and discovery; Liftr and clients never fetch it. Schemas must declare `"type": "object"` at the root and pin `$schema` to the 2020-12 meta-URI.

### Version Immutability

A `ResourceTypeRef` identifies an immutable contract once released. Breaking changes require a new version. Even additive compatible changes require a new version once any Resources persist under the ref, because two Resources sharing one version must have been admitted against one effective contract; reproducibility outranks convenience. Pre-release iteration without persisted Resources may edit in place. Versions are opaque strings with a v1/v2 convention; no semver machinery exists because no requirement does. List ordering within a name is lexicographic byte-wise; "latest version" resolution does not exist. Re-registering a ref within a process — even with identical content — fails immediately; digest-based persistence across restarts is deferred until production deployments make it meaningful.

### Validation Model

Validation is partitioned so every rule lives in exactly one place:

- Transport parsing (HTTP): well-formed JSON, envelope shape, numeric literal normalization.
- Structural rules (compiled JSON Schema): types, required properties, enums, ranges, patterns, unknown-property rejection via `additionalProperties: false`.
- Semantic rules (Go validators owned by each ResourceType implementation): cross-field and domain constraints the schema cannot express.

Structural constraints are never duplicated in Go and vice versa; consistency tests guard the partition. Validation is a pure predicate over (contract, submitted spec): no defaulting, no normalization, no mutation of intent.

Admission order after M8: request parsing → application fingerprint → idempotency replay → catalog lookup → contract validation → lifecycle admission/persistence. Consequences: replays of previously admitted requests succeed even if the catalog later degrades; an invalid first-time spec creates no Resource, Operation, Event, Execution record, Idempotency record, or outbox message; fingerprints are unchanged by validation because validation mutates nothing.

### Format, Integers, Unknown Fields, Defaults

JSON Schema `format` keywords are **annotation-only** in M8. Format assertion is never enabled implicitly; making values such as hostname or uuid admission constraints requires an explicit future contract decision, pinned by tests so a validator swap cannot flip it silently.

JSON Schema integer semantics apply: any number with zero fractional part satisfies `"type": "integer"`, regardless of Liftr's internal representation. Liftr preserves the M7 literal distinction — `20` decodes as an integer value and `20.0` as a decimal value; they remain fingerprint-distinct specs even though both validate. No hidden Go rule may require int64 storage of schema-valid numbers, and downstream code must not rely on unsafe concrete type assertions for numerics.

Unknown properties are rejected (`storagGB` fails admission). Fail-fast typo detection beats silent preservation for self-service infrastructure. Strictness is a property of each contract's schema, decided at the ResourceType level, not the envelope level.

Server-side defaulting is rejected in M8. Defaults in schemas are documentation only and inert; automatic application would mutate stored intent, disturb fingerprint semantics, and conflict with per-version immutability. Every property of PostgreSQLDatabase/v1 is therefore required.

### Discovery API

Two read-only endpoints join v1:

```text
GET /v1/resource-types                    deterministic summaries, name ASC then version ASC
GET /v1/resource-types/{name}/{version}   detail including embedded specSchema
```

List responses carry summaries only (identity, display metadata, capabilities, href); detail embeds the schema verbatim. A separate `schemaHref` route was considered and deferred until schemas grow large enough to matter. Pagination is omitted: the catalog is small, ordering is total and deterministic across implementations, and cursor machinery would be premature. Responses follow the existing no-store policy.

Errors extend the approved vocabulary minimally. GET on an unknown name/version returns 404 RESOURCE_TYPE_NOT_FOUND because the request addresses a ResourceType entity directly. Mutations referencing an unregistered type keep 422 UNSUPPORTED_RESOURCE_TYPE. Specs violating their contract return 422 RESOURCE_SPEC_INVALID with structured violations: sanitized entries of `path` (RFC 6901 JSON Pointer), `keyword`, and curated `message`, deterministically ordered, capped at ten with a boolean `truncated` overflow indicator. Raw validator-library output and submitted values are never exposed.

## Consequences

- Developers can discover what to create, in which shape, through the public API alone; CLIs and Backstage can build forms from machine-readable contracts without out-of-band knowledge.
- Platform teams keep full freedom over implementation: discovery payloads contain no provider vocabulary, enforced by tests.
- The domain stays schema-blind; the JSON Schema dependency is quarantined in one package behind a consumer-owned port.
- Contract immutability makes discovery stable but means every schema evolution ships a new version once released; consumers address explicit versions.
- Admission rejects invalid specs before any durable effect, keeping audit trails clean and replay semantics untouched.
- Format keywords and defaults carry no admission force; future activation of either is an explicit, test-pinned decision.

## Alternatives Considered

### Schema as Documentation Only

Keeping Go authoritative while publishing a passive schema invites silent divergence between published and enforced rules. Rejected in favor of executing the published schema structurally.

### Schema as Sole Authority

Removing Go validation entirely would force cross-field rules into schema extensions and expose raw validator messages. Rejected; structural and semantic layers stay separate.

### Single Source Generating Both

Codegen from one representation into schema plus Go checks is premature machinery for one example type. Deferred.

### Domain-Owned Schemas

Placing schemas on domain.ResourceType would couple the core model to a schema standard and ripple constructors everywhere. ADR-0002's deferral resolved instead outside the core.

### Server-Side Defaulting

Applying declared defaults during admission changes stored intent, fingerprint identity, generation diffs, and schema-evolution semantics. Rejected for M8.

### schemaHref Instead of Embedding

A third route plus caching questions buys nothing while documents measure in kilobytes. Deferred.

### Semver Versions and Latest Aliases

No multi-version upgrade tooling exists; opaque versions with lexicographic ordering suffice. Deferred until required.

### Status/Output Schema Symmetry

ResourceStatus is Liftr-owned normalized observation; type-specific outputs await real consumers. Not introduced for symmetry.
