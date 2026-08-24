# ADR-0016: Ownership-Scoped Resource Inventory

- Status: Accepted
- Date: 2026-08-24
- Refines: ADR-0001 (provisioner-neutral resource API), ADR-0005 (application orchestration and persistence boundary), ADR-0008 (HTTP Resource API contract v1), ADR-0012 (authentication, authorization, and actor identity), ADR-0013 (CLI as a public API client), ADR-0014 (operation history and explicit retry)

## Context

Through Milestone 15's predecessors a developer could only address Resources whose IDs they already knew: v1 had create/read/update/delete, per-Resource Operation history, outputs, discovery of ResourceType contracts, owner-scoped authorization, and a CLI — but no way to ask "which Resources can I see?". The obvious answers were all unsafe or unsound: loading every Resource and authorizing row by row is unbounded and turns response timing into an existence oracle; deriving visibility from `Principal.Memberships` inside application code would hard-code the current owner-membership policy into every future use case; and any generic search or admin enumeration is explicitly out of scope. Listing needs a bounded authorization-aware query model where **listing visibility is authorization**, not filtering after the fact.

## Decisions

### resource:list is the enumeration permission

Inventory introduces its own action, `resource:list`, decided exclusively through a new collection method on the application-owned `Authorizer` port:

```text
AuthorizeResourceList(ctx, Principal) -> (identity.ResourceVisibility, error)
```

A collection has no `ResourceTarget`, so `resource:list` never flows through single-target `Authorize` (which continues to deny it, preserving deny-by-default for unknown action/target combinations). `resource:read` remains "may read one known Resource"; `resource:list` is "may enumerate at all". The default owner-membership policy grants both from the same memberships; a future policy may grant read-without-list or list-without-read.

### List visibility never exceeds resource:read visibility

The product goal of listing is to discover Resources the principal is authorized to read. Therefore:

```text
list visibility  ⊆  resource:read visibility
```

is an invariant, not a tuning knob. `resource:list` grants enumeration; `ResourceVisibility` describes what inventory may disclose; summaries carry meaningful facts (ID, type, owner, state, generation, timestamps, latest Operation reference), so a list-only grant must never become a summary-only disclosure bypass for otherwise unreadable Resources. Making enumeration *narrower* than detail reads is supported today (a policy may return any subset of the readable scope). Making enumeration *broader* than detail reads requires a future explicit policy ADR. Regression tests pin both directions with custom policies: resources under owners the policy cannot read never appear in listings, and denying `resource:list` while granting `resource:read` leaves direct GET working while listing answers 403.

### Closed authorized visibility

`identity.ResourceVisibility` is the closed result of collection authorization: a normalized, deduplicated, sorted set of typed `OwnerRef`s plus an explicit `AllOwners` marker for unrestricted visibility. There is no expression language, no predicate AST, and no repository/HTTP/pagination vocabulary in the type — only authorization scope. Empty owners without the marker means *authorized but sees nothing*, which is a valid empty page, distinct from denial (403). The marker exists because the one shipped allow-everything policy — explicit insecure development composition — authorizes creates under any owner while its fixed principal holds no memberships; the secured `OwnerAuthorizer` can never emit it, and tests pin that. A privileged production policy would be a new decision, not a silent use of this field.

### One authoritative authorization per page

HTTP authenticates; the application authorizes — exactly once per list request, transactionally, before cursor semantics are evaluated and before the page query executes. There is no transport preflight and no second decision that could disagree with the first. Workers are unaffected: they never list.

### Application-to-repository boundary

The use case copies the authorized scope into a plain application query (`ResourceListQuery`) carrying domain values only: allowed owners, an unrestricted flag mirroring the marker, narrowing filters, Deleted policy, keyset position, and limit. Repositories execute mechanically — they evaluate no policy and receive no principals, claims, tokens, JWT material, or visibility types. Owner-set matching uses parameterized arrays (`unnest`) so SQL text never expands with input size. Visibility is never truncated: the complete normalized scope flows to persistence, with the existing M11 claim-mapper bounds as upstream protection against hostile tokens and static grants treated as trusted configuration expected to stay within similar magnitudes.

### ResourceSummary discloses discovery facts only

Inventory returns summaries — id, type, owner, generation, status (state, observedGeneration, updatedAt), optional latestOperation reference, createdAt, updatedAt — built from a dedicated application read model whose structure omits spec, conditions, and outputs entirely: the repository SELECT never loads them, so summaries cannot leak them by construction. Outputs stay behind the detail endpoint even where reads are authorized, minimizing disclosure and payload size. Provisioner bindings, execution handles, attempts, phases, fingerprints, storage versions, and the private ordering sequence have no representation. Full `GET /v1/resources/{id}` is unchanged.

### Filters narrow; they never grant

`ownerKind`+`ownerId` (supplied together), exact `type` with optional `version`, exact public `state`, and boolean `includeDeleted` are predicates applied *inside* the already-authorized visibility. A filter naming an owner outside visibility yields the same empty 200 collection as absence — never 403, which would confirm other owners' existence — and unknown-but-well-formed type values yield empty collections rather than catalog-existence signals. Unknown or duplicated query parameters are invalid. Requesting `state=Deleted` without `includeDeleted=true` is rejected as contradictory rather than silently emptied.

### Deleted tombstones default to invisible

Deleted Resources are excluded by default because developers want active inventory; `includeDeleted=true` includes them under exactly the same current authorization, with each tombstone's stored owner remaining authoritative. Retention behavior is unchanged; tombstones remain retained records for audit, idempotency, and recreation conflicts.

### Private immutable insertion sequence

Inventory orders newest-first by a private immutable `resource_seq` database identity on `resources`, backfilled deterministically by `created_at_ns ASC` then ID under byte-wise (`C`) collation — mirroring the M13 operation-history sequence. Timestamps are honest public data and are never synthesized or rewritten; insertion order wins when clocks tie. The sequence protects itself against mutation, is excluded from every public representation, and gives fake and PostgreSQL stores identical total orderings.

### Keyset cursors bound filters, visibility scope, and position

Pagination reuses the M13 discipline: opaque unsigned envelope, versioned kind byte, strict length and encoding validation, exclusive-below keyset seeking, limit default 20 and maximum 100, always-present `items`, `nextCursor` absent on the final page, no total count. The inventory cursor additionally binds two digests alongside the position: one over the canonicalized client filter tuple, one over the canonicalized closed visibility scope as of issuance.

On every page the server authenticates the current credential, derives the *current* visibility fresh from policy, and requires both digests to match exactly. Authorization changes therefore invalidate continuation instead of silently reshaping the traversal: membership loss cannot leak revoked rows mid-stream, and membership gain cannot silently skip newly visible rows above the cursor. An old cursor grants zero old permissions; possession of a cursor — like possession of a ResourceID, OperationID, or idempotency key — is never authorization. Rejection normalizes to INVALID_ARGUMENT without disclosing what changed or naming any owner. Positions beyond the signed bigint range are refused before reaching persistence.

### Owner immutability

Ownership is fixed at creation. The persistence layer now enforces what the domain already implied: `owner_kind`/`owner_id` columns reject mutation via trigger and via ordinary saves, in both durable and test stores, so authorization semantics can never shift behind an existing Resource. Owner transfer, if ever needed, requires an explicit workflow with its own migration and ADR.

## Consequences

- Developers and clients (including the future Backstage plugin surface) can discover exactly the Resources they may read, through `GET /v1/resources` and `liftr resource list`, with bounded pages and deterministic order.
- Collection denials are honestly 403; empty visibility and out-of-scope filters are indistinguishable empty collections. No endpoint reveals cardinality about invisible data.
- Every page re-prices authorization at zero trust: token refresh, group revocation, and policy swaps take effect immediately.
- The Authorizer port now carries a required collection method; replacing policy implementations must answer both questions — single-target and enumeration — at compile time.
- Cursor binding makes pagination slightly stricter: filter or authorization changes require restarting traversal from page one.

## Scope Exclusions

Backstage integration, global/admin inventory, generic search, prefix/fuzzy ID matching, tags, arbitrary sort orders, spec/output bulk listing, total counts, offset pagination, owner transfer, retention/GC changes, privileged AllOwners policies in secured composition, Events or global Operations surfaces, and analytics remain out of scope.

## Alternatives Considered

### Load Everything and Authorize Per Row

Unbounded I/O including specs, O(n) policy calls, latency correlated with invisible data, and incompatible with keyset paging. Rejected.

### Derive Visibility From Principal.Memberships in Application Code

Permanently hard-codes owner-membership policy into use cases and breaks the port-replaceability consequence of ADR-0012. Rejected in favor of asking the authorizer for a closed scope.

### Reuse resource:read or resourceType:read for Listing

Conflates known-ID reads with enumeration, or global capability discovery with owner-scoped inventory. Rejected; separate actions keep revocation precise.

### Generic Policy Query Language

No requirement exceeds expressing visibility as owner sets; a closed object keeps persistence simple and auditable. Deferred until a concrete policy demands more.

### Timestamp-Only Ordering

Equal admission clocks destroy total order and split ordering logic across stores; contra ADR-0014 precedent for history. Rejected in favor of the private sequence.

### Signed or Encrypted Cursors

Cursors carry no authority — every request re-authorizes — so signing adds key lifecycle without security benefit, per ADR-0014's standing analysis.

### Full Resource Bodies in Listings

Bulk spec dumps inflate responses and widen disclosure; conditions and outputs belong to detail reads. Rejected in favor of the summary read model.

### 403 for Out-of-Scope Owner Filters

Would confirm other owners' existence and enable enumeration probing. Rejected; filters operate inside visibility.
