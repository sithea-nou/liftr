# ADR-0011: Resource Outputs and Secret References

- Status: Accepted
- Date: 2026-08-23
- Refines: ADR-0002 (core domain model), ADR-0003 (lifecycle engine), ADR-0004 (provisioner contract), ADR-0006 (persistence), ADR-0008 (HTTP contract v1)
- Partially supersedes: ADR-0007 (Pulumi output suppression — narrowed), ADR-0009 (output deferral and port ownership), ADR-0010 (suppressed PostgreSQL outputs; Ready semantics refined)

## Context

After Milestone 9 a developer can request `PostgreSQLDatabase` and Liftr can provision it through Pulumi. The developer cannot yet consume anything: the realized hostname and port of a database never reach Liftr, and ADR-0007/ADR-0010 deliberately suppressed all Pulumi outputs because Automation API's standard output path retrieves secret plaintext (`--show-secrets`) and raw provider data must not leak into developer contracts.

A useful resource must expose realized values (hostname, port) without turning `ResourceStatus` into provider data, returning Pulumi outputs directly, exposing secrets or provider identifiers, or weakening any domain boundary.

## Decisions

### 1. ResourceOutputs is a separate domain concept

`domain.ResourceOutputs` is an immutable value: `ObservedGeneration`, flat scalar `Values`, and `PublishedAt`. It is separate from `ResourceSpec` (desired intent) and from `ResourceStatus` (normalized lifecycle observation). It carries no worker, persistence, mapping, or operation provenance; those live in application-level records only. Values are closed scalars (string, integer, number, boolean); nested objects, arrays, null, and secret material have no representation.

### 2. Output generation semantics

Three independent dimensions: `D = Resource.Generation`, `S = ResourceStatus.ObservedGeneration`, `O = ResourceOutputs.ObservedGeneration`. When outputs exist, `0 < O <= S <= D`; equality between `O` and `S` is not required. `O` identifies the desired generation whose successful reconciliation produced the values — it is not another evaluation high-water mark and not a health observation. Pending or failed updates retain the previous snapshot with its older generation; successful updates publish atomically at their target generation. The public envelope is `{ "outputs": { "observedGeneration": N, "values": {...} } }`.

### 3. ResourceType-owned immutable output contracts

Output fields are declared by the ResourceType contract as part of its immutable identity: exact names, exact scalar JSON types, and requiredness (`requiredWhenReady`). Unknown output names are rejected; types are exact and coercion-free. M10 uses a small typed descriptor, not a second JSON Schema surface; full output schemas are deferred until real contracts need nesting or unions. Adding outputs to a released type requires a new version.

### 4. Required outputs are reconciliation postconditions

For a type whose contract declares required outputs, reconciliation success requires durable publication of validated values for the target generation. Backend execution success and Liftr Operation completion therefore become separable monotonic dimensions, tracked by durable `OutputResolution` state on the execution record:

| Backend | Resolution | Operation |
|---|---|---|
| Running | None | Running |
| Succeeded | Pending | Running (retry extraction; never re-execute the backend) |
| Succeeded | Published | Succeeded |
| Succeeded | Rejected | Failed (`OutputPostconditionRejected`, curated message) |
| Failed | None | Failed |

Transient extraction failure stays Pending. Deterministic violations (undeclared fields, wrong types, wrong identity, redacted secret markers, malformed envelopes) become Rejected. No offending key or value ever reaches Events, Conditions, Problems, logs, outbox `last_error`, or failure messages.

### 4a. Backend and Liftr evidence clocks are separate dimensions (refines ADR-0004/ADR-0005 freshness)

Backend-supplied evidence timestamps (`ObservedAt`) are judged only against previously accepted backend timestamps for the same execution; Liftr receipt instants are a separate monotonic timeline used for lifecycle transitions. This separation is required because backend clocks can be coarser than Liftr's (for example second-granular history end times): a receipt recorded after a backend completion must never strand later correlated observations of that same completion as stale. Positively correlated terminal success whose backend timestamp regresses below state Liftr advanced after launching that very execution is lifted onto the persisted frontier and applied immediately — never quarantined, and never re-executed. Resubmission conservatism is preserved: every interpreted backend observation advances the backend dimension, so a resubmission repeating an already-seen backend instant remains classified as replay-stale.

### 5. Cleanup delete from Failed

Required-output postcondition failure can leave a Failed Resource whose infrastructure exists. DELETE is admissible from `Failed` (refines ADR-0003):

- Destroy succeeds → Deleted.
- Provisioner proves conclusively — fresh pre-launch NotFound correlation with no confirmed acceptance — that the managed target is already absent → destruction objective satisfied; the Operation succeeds and the Resource becomes Deleted.
- Ambiguous destroy outcomes stay ambiguous and are observed; Liftr never concludes deletion from ambiguity.
- Definitive destroy failure fails the delete Operation; the Resource remains Failed.

Failed cleanup deletes restore the previous existence state on failure: previously Ready Resources return to Ready; cleanup deletes of Failed Resources remain Failed.

### 6. Durable versioned output-mapping identity

Every execution that may realize outputs carries a private, immutable `OutputMappingRef` persisted at the dispatch claim, before any provider work. Recovery resolves decoders through the persisted identity — never through "whatever is registered today". A missing or conflicting persisted mapping fails loudly (no fallback, no publication, no backend re-execution). Mapping versions participate in provenance only; they are never exposed publicly and never change Resource generation.

### 7. Selected non-secret Pulumi extraction (narrowing ADR-0007)

The adapter reads exactly one registered allowlisted stack export via `pulumi stack output <name> --json`. It never passes `--show-secrets`, never calls Automation API's output retrieval, and never dumps all outputs. The primary secret boundary is what can be requested at all, not textual filtering; recursive redacted-marker detection is defense-in-depth. Envelopes are strictly bounded (size, depth, keys, string length), reject duplicate keys and unknown fields, echo mapping/resource/generation identity for verification, and carry flat scalars only. Raw stdout/stderr and values never enter errors, logs, Events, or durable text columns.

### 8. Persistence

Outputs persist in a dedicated append-only `resource_outputs` table keyed by `(resource_id, observed_generation)`, tied by composite foreign key to exactly one completing Operation (capability restricted to create/update), carrying mapping ref, contract digest, canonical values digest, and publication timestamp. Contradictory evidence for one resource/generation pair fails closed; identical republication is idempotent. Publication commits atomically with lifecycle completion, status, terminal Event, execution resolution update, and outbox completion. Provider work stays outside transactions. Public reads resolve from this table — never from the provisioner.

### 9. Update and delete semantics

Updates never clear outputs at admission. Deleting Resources retain their latest published snapshot while destruction runs; a successfully deleted Resource omits outputs publicly while immutable internal history is retained. There is no output-history endpoint.

### 10. PostgreSQLDatabase/v2

Because a released ResourceTypeRef is immutable (ADR-0009) and output declarations are contract content, M10 introduces `PostgreSQLDatabase/v2` with the same input schema and transition rules plus required outputs:

    hostname  string   required
    port      integer  required (5432 is mapping-derived, not provider-derived)

v1 remains byte-compatible with no outputContract; there is no migration or relabeling; clients select v2 explicitly. Azure resource IDs, resource groups, subscriptions, URNs, stacks, provisioner refs, credentials, usernames, and connection strings are never public outputs.

### 11. SecretReference semantics (defined now, implemented later)

A future secret reference is Liftr-owned, opaque, random, non-bearer, and backend-private. It binds server-side to installation, owner/tenant, resource, output slot, credential revision, and lifecycle state; resolution authorization will be independent of Resource-read authorization. Ordinary updates preserve the reference; only explicit future rotation creates a new one. M10 implements no SecretReference field, schema, storage, or resolution: the current program keeps the generated administrator password inside Pulumi secret dataflow with no retrieval path, and no external secret store is configured yet. Secret material never crosses the Liftr API boundary.

### 12. Validation-boundary cleanup

The shared ResourceType vocabulary — contract interface, structured validation rejections, output descriptors — moves to the neutral `internal/resourcecontract` package (imports only domain + standard library). `internal/resourcetypes` no longer imports `internal/application`; application keeps its consumer-owned catalog port expressed through neutral types; architecture tests pin both directions.

## Consequences

- Resource GET/mutation representations gain optional `outputs`; ResourceType detail gains optional `outputContract`. No new endpoints.
- Core OpenAPI models the generic envelopes; discovery declares dynamic field names/types per contract.
- A create whose infrastructure succeeded but whose required outputs were rejected yields a Failed Resource that is still cleanup-deletable.
- Operators deploying decoder changes must keep old mapping versions resolvable while executions reference them.
- Without authn/authz, any caller who can reach the API can read non-secret outputs of resources it can name; network isolation remains the access boundary.
