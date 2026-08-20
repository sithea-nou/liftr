# ADR-0006: PostgreSQL Persistence and Transactional Outbox

- Status: Accepted
- Date: 2026-08-16

## Context

ADR-0005 defines repository and transaction ports but intentionally leaves durable storage, dispatch ownership, and crash recovery to Milestone 5. The in-memory implementation cannot enforce lifecycle invariants across processes or reliably recover work after a process exits.

Liftr must atomically preserve desired and observed resource state, Operations, Events, idempotency decisions, immutable provisioning attempts, and dispatch intent. Provider calls cannot participate in a database transaction, so submission may remain ambiguous after a timeout or process failure. Recovery must not turn that ambiguity into duplicate work or erase the history of an earlier attempt.

Persistence must also preserve ResourceSpec exactly. ResourceSpec accepts multiple concrete integer and floating-point types that ordinary JSON decoding can collapse or change. A stored private intent snapshot must therefore round-trip without changing its accepted value types.

## Decision

Milestone 5 uses PostgreSQL as the first durable persistence implementation. The adapter uses `pgx` and explicit SQL. Repository contracts remain application ports; PostgreSQL types, SQL, transaction handles, and outbox details do not enter the domain, lifecycle, provisioning, or developer-facing Resource contracts.

### Transactions and Invariants

Application mutations atomically persist every affected Resource and status snapshot, Operation, Event, idempotency record, provisioning attempt, and outbox entry. Optimistic versions reject stale writes.

PostgreSQL foreign keys, unique indexes, check constraints, and partial indexes enforce invariants that can be expressed locally, including unique identities, idempotency keys, Event identities, and at most one nonterminal Operation for a Resource. Invariants spanning multiple rows are checked inside the same transaction while locking their owning Resource or Operation row. This includes matching Resource and Operation ownership, target generation, stable ProvisionerRef, and the legal relationship between an Operation and its attempts. Repository methods do not weaken these cross-row rules when called in a transaction.

ResourceSpec is stored through a private, versioned, lossless codec. The codec preserves every ResourceSpec value accepted by the domain, including concrete numeric types and nested values, and rejects malformed or unknown encodings. It is a persistence format, not a public API or provisioner contract.

### Provisioning Attempts and Correlation

Each provider submission is an immutable, separately identified provisioning attempt associated with one OperationID. Retries of outbox delivery for that attempt do not create another attempt.

RequestCorrelation Found, NotFound, and Unknown are stored independently from the optional observed Execution. A nil Execution only reports that no current execution is visible. It does not authorize resubmission.

After an ambiguous submission, only a fresh observation with explicit RequestCorrelation NotFound may create a new submission attempt. The new attempt retains the same OperationID and immutable submitted intent but receives its own attempt identity. The old attempt is never reset to Pending, rewound, or overwritten. Found prevents a new submission attempt, including when Execution is nil; Unknown remains recoverable only through later observation. This refines the provisional single-record recovery description in ADR-0005.

### Transactional Outbox and Leases

Work that must occur outside PostgreSQL is represented by an outbox row committed in the same transaction as the state that requires it. Every logical outbox item has a stable dedupe key protected by a uniqueness constraint. Redelivery retains that key, while a genuinely new submission attempt receives a distinct key. Consumers must use the dedupe key and persisted attempt state to make repeated delivery safe. The outbox provides at-least-once delivery, not an exactly-once claim across PostgreSQL and a provider.

Workers claim work with bounded leases based exclusively on PostgreSQL server time. Claim, renewal, expiry, and completion compare the persisted lease owner and fencing token in atomic SQL operations. Worker clocks do not decide whether a lease has expired, and a stale worker cannot finalize work after ownership has changed.

Outbox identities are derived from durable aggregate versions: `drive:<OperationID>:<OperationVersion>`, `dispatch:<OperationID>:<AttemptNumber>`, `observe:<OperationID>:<ObservationSequence>`, and `passive-observe:<ResourceID>:<ObservationSequence>`. PostgreSQL partial unique indexes permit at most one Pending or Leased message of each kind for its aggregate. Drive, Dispatch, and Observe are different responsibilities and may briefly coexist for one Operation during a transactional handoff or recovery; PassiveObserve may coexist with Operation work because it refreshes normalized Resource facts and cannot complete an Operation. Same-kind duplicates may not coexist. Workers fence every result using the expected aggregate version or sequence, so stale coexistence cannot apply old evidence.

Stale observation evidence (evidence that does not advance the execution's persisted observation timeline) is settled without applying any transition: the worker persists the bumped observation sequence, completes the observe message with the `StaleObservation` terminal reason, and enqueues a follow-up observe with a bounded retry delay. The observe loop therefore stays alive for an active execution even when a backend repeatedly reports old or equal-timestamped evidence, and an adversarial stale terminal observation cannot fail or complete an Operation.

Completed and Dead messages are immutable and cannot be returned to Pending. Administrative redrive, if introduced later, must create a new explicit work identity rather than revive a terminal row.

Provider Submit and Observe calls always occur outside database transactions. A worker first commits its claim, performs the provider call, and then records the result in a new transaction. A crash or timeout between those steps produces an ambiguous attempt that must be observed according to ADR-0004; it is not treated as NotFound.

### Schema Migrations

Schema changes are ordered, explicit SQL migrations. The migration runner serializes execution with a PostgreSQL advisory lock, records a checksum for each applied migration, and refuses to continue when an applied migration's checksum has changed. Applicable migrations run transactionally. Liftr does not use runtime schema inference or automatic ORM-generated schema changes.

## Consequences

- Domain and application semantics gain durable transactions, optimistic concurrency, and database-enforced uniqueness without depending on PostgreSQL packages.
- ResourceSpec intent snapshots can be recovered without numeric type loss.
- Events and outbox work are committed with the state changes that caused them.
- Multiple workers can recover expired work without trusting synchronized host clocks.
- Provider ambiguity remains explicit. At-least-once dispatch does not become unsafe resubmission, and every submission attempt remains auditable.
- Explicit SQL and migrations make schema behavior reviewable, at the cost of maintaining PostgreSQL-specific statements and integration tests.
- Dedupe keys prevent duplicate logical outbox records; they do not promise exactly-once effects in external systems.

## Scope Exclusions

Milestone 5 does not add a public Resource API, authentication or authorization, real provisioner adapters, provider-specific policy, cancellation, or Resource migration between provisioners. It does not introduce an external message broker or a general-purpose workflow engine. Multi-region database operation, partitioning, archival and physical tombstone deletion, automated migration rollback, and zero-downtime migration policy are deferred until concrete requirements exist.

## Alternatives Considered

### ORM or Generated Persistence Model

An ORM-owned model would introduce another representation of domain state and obscure locking and constraint behavior. Explicit SQL through pgx was selected so transaction and concurrency semantics remain visible.

### JSON ResourceSpec Encoding

Ordinary JSON cannot preserve all concrete numeric types accepted by ResourceSpec. Using it for private intent snapshots could change provider input after recovery, so a lossless versioned codec was selected.

### Call Providers Inside Transactions

A PostgreSQL transaction cannot make an external provider call atomic. Holding locks during a slow or ambiguous call would increase contention without removing the failure window. Claims and results are committed on either side of the provider call instead.

### Reset an Ambiguous Attempt to Pending

Reusing an old attempt would erase its ambiguity and weaken the audit trail. A conclusive NotFound creates a new attempt under the same OperationID; all other correlation outcomes preserve the existing attempt.

### Worker-Local Lease Time

Host clock skew could allow concurrent owners to believe the same lease is valid. PostgreSQL server time and fenced ownership were selected as the single authority.

### Automatic Schema Synchronization

Automatic schema changes are difficult to review and cannot safely encode Liftr's migration intent. Checksummed, advisory-locked SQL migrations were selected instead.
