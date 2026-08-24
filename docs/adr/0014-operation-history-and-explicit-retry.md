# ADR-0014: Resource Operation History and Explicit Retry

- Status: Accepted
- Date: 2026-08-23
- Refines: ADR-0003 (deterministic lifecycle engine), ADR-0005 (application orchestration and persistence boundary), ADR-0006 (PostgreSQL persistence and transactional outbox), ADR-0008 (HTTP Resource API contract v1), ADR-0010 (implementation binding and transition semantics), ADR-0011 (resource outputs and secret references), ADR-0012 (authentication, authorization, and actor identity), ADR-0013 (CLI as a public API client)

## Context

Liftr already retains immutable Operations, Events, provisioning executions, and submission attempts, but v1 exposed only one Operation by ID and the latest Operation reference on a Resource. A developer could not inspect a Resource's lifecycle history or explicitly recover from a failed request. ADR-0003 established that retry creates a new Operation rather than reopening a terminal one, while deferring durable admission details. M13 supplies those details without exposing execution internals or creating a global activity feed.

Output postconditions introduce one important recovery case. Infrastructure may have completed successfully while output extraction failed permanently under an old private mapping. Re-running infrastructure would be incorrect, but silently interpreting old state with whichever mapping is current would violate ADR-0011's frozen-provenance rule. Explicit retry therefore needs one tightly constrained observe-only recovery path in addition to ordinary execution retry.

## Decisions

### History is scoped to one Resource

The only history endpoint is `GET /v1/resources/{id}/operations`; v1 has no global Operation list. It authorizes `resource:read` against the addressed Resource's stored owner. Missing Resources and denials are the same hidden `RESOURCE_NOT_FOUND` 404. Authentication and Resource authorization happen before query parsing, so an invalid limit or cursor cannot disclose Resource existence.

History is newest-first by durable insertion order, not by caller-supplied `requestedAt`. Every Operation receives a private positive `operation_seq` from a database identity. Migration backfill orders existing rows by `requested_at_ns ASC`, then Operation ID under PostgreSQL `C` collation, and assigns a deterministic oldest-to-newest sequence before advancing the identity. It does not synthesize, rewrite, or perturb `requestedAt`; equal or non-monotonic request timestamps remain honest API data and never become the ordering authority. The sequence is immutable, unique, and private.

Pages default to 20 items and accept `limit` from 1 through 100. `items` is always an array, including on an empty page. `nextCursor` is absent on the final page; there is no total count. Inserts after a cursor is issued cannot shift the continuation because the query seeks strictly below the cursor's insertion sequence.

The cursor is an opaque `c1_` envelope followed by unpadded base64url. Its binary payload is a one-byte kind/version discriminator, the SHA-256 digest of the Resource ID, and the next-page boundary as a big-endian unsigned 64-bit sequence. The decoded boundary must be greater than zero. Encoded input is bounded to 64 characters and malformed versions, lengths, encodings, zero positions, and cross-Resource use are rejected. The cursor is deliberately unsigned: it is neither a credential nor an integrity-protected claim, and it contains no authority. Resource authorization precedes validation on every page; the Resource digest provides binding, not access control. Internal sequence values never appear in Operation JSON.

### Retry admits a new exact-generation Operation

`POST /v1/operations/{id}/retry` accepts only a latest `Failed` source Operation that targets the owning Resource's current generation, with no active Operation. It creates a new exact-generation Operation with a new ID and audit trail, preserves the source capability and immutable execution intent, does not increment Resource generation, and persists `retryOf` immediately at admission. The source remains terminal and unchanged. Public Operation JSON gains only the optional `retryOf` source ID.

This narrows ADR-0003's earlier statement that a retry might target a newer current generation than its source. The implemented explicit retry requires source target generation, submitted `If-Liftr-Generation`, and current Resource generation to match exactly. New desired intent is an update, not a retry. ADR-0003 remains otherwise unchanged and is not rewritten.

Capability and normalized-state compatibility remains structural: failed create retries only from `Failed`, failed update only from `Ready`, and failed delete from `Ready` or `Failed`. A delete retry is an ordinary new delete execution and follows the existing conclusive-absence, definitive-failure, and ambiguity rules; output recovery never applies to delete. Retry of `Canceled` Operations is deferred because M13 has no admitted cancellation path or approved canceled-state recovery semantics.

The owning ResourceType must still advertise the source capability. The retry uses the source execution's exact Resource, ResourceType, capability, target generation, spec, stable private provisioner binding, and matching current Resource intent. Missing or contradictory provenance fails closed as not retryable; no best-effort reconstruction is allowed.

### Retry has independent authorization and replay ordering

Retry introduces `ActionResourceRetry` / `resource:retry`; permission to create, update, delete, or read a Resource does not imply permission to retry it. Operations have no independent ACL. The source is resolved through its owning Resource and authorized against the stored owner. Missing Operations, missing owning Resources, and denials collapse to the same hidden `OPERATION_NOT_FOUND` 404.

The transport performs that hidden-404 authorization preflight before parsing `Idempotency-Key`, `If-Liftr-Generation`, or the body. The application repeats authorization transactionally before idempotency lookup or lifecycle evaluation. This prevents malformed input and possession of an idempotency key from becoming existence or replay oracles.

After authorization, `Idempotency-Key` is required and scoped to the authenticated principal as in ADR-0012. The retry fingerprint is versioned over source Operation ID and the submitted exact generation. Replay resolution precedes current-generation, terminal-state, latest-source, and active-Operation checks, so a valid replay keeps returning its original child Operation after later lifecycle progress. Reusing the key with a different retry fingerprint yields `IDEMPOTENCY_CONFLICT`.

`If-Liftr-Generation` is required, concrete, greater than zero, and parsed as an unsigned 64-bit integer; wildcard and non-integer values are invalid. A missing header yields `PRECONDITION_REQUIRED`; a stale concrete value yields `GENERATION_CONFLICT` with `currentGeneration`. The endpoint has no request document. An absent or whitespace-only body is tolerated; non-whitespace and oversized bodies are invalid.

### Rejected outputs have an observe-only repair path

An exact output-recovery retry is recognized only when a failed create or update has `OutputPostconditionRejected`, the source execution durably succeeded, output resolution is `Rejected`, failure details match exactly, and the source has a real attempt, provisioner binding, and frozen output mapping. All ordinary provenance and exact-generation checks still apply.

Admission asks the same provisioner implementation for a newly selected mapping that explicitly declares compatibility with the source mapping. Selection must succeed, be non-empty, and differ from the rejected mapping. The selected repair mapping is frozen immediately on the child execution. There is no fallback to the current mapping, no mutation of the source mapping, and no reinterpretation under an undeclared decoder. Ordinary M10/M11 executions retain ADR-0011 semantics: their selected mapping is frozen on that execution and every later observation resolves that exact identity.

The child privately references the source execution's exact Operation and submission-attempt number. Once referenced, the source execution and exact source attempt are durably immutable; admission locks both rows and creates the reference only after source terminalization. The child starts with backend execution already `Succeeded`, output resolution `Pending`, and no child attempt. Worker structure enforces observe-only processing: the child can enter only the applying path, cannot dispatch, cannot create a submission attempt, and asks `Observe` about the exact source Operation/attempt/handle while supplying the child repair mapping. Source and child provisioner, Resource, ResourceType, capability, target generation, and spec must continue to match. Only positively correlated terminal success can advance output resolution. Every other observation, including later `Found`/terminal `Failed` backend evidence that contradicts the frozen durable success, remains ambiguous for output recovery and schedules another Observe without mutating the source or failing the child. Valid successful evidence publishes and completes the child; deterministic output rejection from successful execution fails the child. Infrastructure is never submitted again through this path.

### Execution detail remains private

Operation history reuses the existing public Operation representation. `operation_seq`, output mappings, recovery source attempt, provisioning executions, submission attempts, outbox records, lifecycle phases, and phase timestamps are private implementation state. Events also remain append-only private audit records in M13; there is no Event or attempt endpoint. History and retry responses carry `Cache-Control: no-store` and no `Liftr-Generation` because they represent Operations, not a Resource snapshot.

### HTTP and CLI behavior remain one contract

Retry returns `202 Accepted` with the child Operation body, `Location` and `Link rel="monitor"` pointing to that child, and `Idempotency-Replayed: true` only on replay. Errors include hidden `OPERATION_NOT_FOUND`, `INVALID_ARGUMENT`, `PRECONDITION_REQUIRED`, `GENERATION_CONFLICT`, `IDEMPOTENCY_CONFLICT`, `OPERATION_ACTIVE`, `OPERATION_NOT_RETRYABLE`, `PROVISIONER_UNAVAILABLE`, persistence unavailability, and internal failure under the existing security response conventions.

The pure public client adds Resource-scoped history and explicit retry; no server package crosses the CLI dependency boundary. `liftr operation list --resource ID` passes through optional `--limit` and opaque `--cursor`. `liftr operation retry OPERATION_ID` creates one idempotency key unless explicitly supplied. By default it reads the source Operation and owning Resource to obtain the current generation; explicit `--generation` skips both convenience reads without weakening the server precondition. Without `--wait`, retry always prints the admitted child Operation even if monitor metadata is absent, malformed, or mismatched; that metadata is not needed to return the admitted representation. With `--wait`, exact equality between the child body and authoritative monitor reference is mandatory before polling, and in both text and JSON modes the CLI prints the terminal child Operation rather than reading or printing a Resource. JSON output remains the server's verbatim Operation document, including `retryOf`; failed/canceled child terminal states and timeout use the existing exit-code contract, although canceled sources themselves are not retryable.

## Consequences

- Developers can inspect stable per-Resource history and deliberately retry a failed latest intent without gaining a global activity feed.
- Audit identity is explicit: source and child Operations remain independently immutable, while `retryOf` makes provenance immediately visible.
- Pagination remains stable even when request timestamps collide or new Operations arrive, at the cost of one private database sequence.
- Authorization and idempotency ordering preserve hidden-existence and per-principal replay guarantees.
- Output decoder repairs can recover already-created infrastructure without re-execution, but only through explicit compatibility and strict persisted provenance.
- Internal Events, attempts, mappings, sequences, and phases remain available to operators and persistence logic without becoming compatibility commitments.

## Alternatives Considered

### Global Operation Listing

A global feed introduces cross-owner filtering, information-disclosure, retention, and pagination policy not needed for Resource troubleshooting. Rejected; history is Resource-scoped.

### Ordering by requestedAt

Caller-controlled and equal timestamps do not define total insertion order, and rewriting them would falsify public history. Rejected in favor of a private deterministic sequence with migration backfill.

### Signed or Encrypted Cursors

The cursor grants no authority and every request re-authorizes the Resource before parsing it. Signing or encryption adds key lifecycle and rotation policy without a security benefit for this state. Rejected; use a bounded, versioned, Resource-bound unsigned envelope and validate strictly.

### Reopen the Failed Operation

Mutating a terminal Operation destroys the source outcome and audit boundary. Rejected consistently with ADR-0003; retry creates a child Operation.

### Retry a Failed Source Against Newer Intent

Applying old execution provenance to a newer Resource generation is ambiguous and can overwrite intent. Rejected; source, precondition, and current generation must match exactly.

### Retry Canceled Operations

There is no public cancellation admission or approved meaning for potentially partial canceled execution. Deferred.

### Re-run Infrastructure to Repair Outputs

The backend already succeeded, so another submission can duplicate or mutate infrastructure. Rejected in favor of observe-only output recovery.

### Decode With Whatever Mapping Is Current

This silently changes persisted execution meaning and violates ADR-0011. Rejected; a repair mapping must explicitly declare source compatibility and is frozen on the child.
