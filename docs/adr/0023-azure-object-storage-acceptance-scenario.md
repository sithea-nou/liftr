# ADR-0023: Azure Object Storage Acceptance Scenario

Date: 2026-09-21
Status: Accepted

- Refines: ADR-0004 (provider-neutral provisioner contract), ADR-0009
  (ResourceType contracts), ADR-0010 (implementation binding), ADR-0011
  (outputs), ADR-0020 (stateful OpenTofu CLI provisioning)

## Context

Liftr needs repeatable evidence that more than one provisioner can implement
the same developer intent without leaking a cloud or infrastructure tool into
that intent. A small Azure Storage account is suitable for an external test:
it has a bounded lifecycle, a safe non-secret endpoint output, and equivalent
Pulumi Azure Native and OpenTofu AzureRM implementations.

Shipping a production cloud composition would be a different decision. It
would require durable production routing, an operator-qualified HTTPS
OpenTofu backend, retained immutable registrations, and an operational support
claim. This scenario must not imply those decisions have been made.

## Decision

### One provider-neutral acceptance contract

The repository contains an internal `ObjectStorage/v1` contract with only:

- `accessFrequency`: `frequent` or `infrequent`;
- `permitAnonymousAccess`: a boolean capability request; and
- one normalized non-secret output, `endpoint`.

Azure names, regions, SKUs, replication codes, resource groups, account names,
Pulumi, and OpenTofu are absent from the contract. Private bindings map the
frequency values to Azure `Hot` and `Cool` tiers. Location, SKU, replication,
credentials, and implementation identity remain operator-supplied private
configuration.

### Two isolated implementations

The Pulumi implementation uses an isolated nested Go module and Azure Native
SDK `v3.28.0`.
The OpenTofu implementation is pinned to OpenTofu `1.12.6` and AzureRM
`4.46.0`; its committed dependency lock and exact offline provider package are
validated by the adapter before execution. Both implementations derive stable
lowercase account names from private Liftr identity rather than exposing names
in developer intent.

Each opt-in live test performs create, update from frequent to infrequent,
validates the HTTPS endpoint output, and deletes the owned resource group. A
best-effort cleanup handler runs after test failure. Normal verification only
compiles and tests the setup; it never contacts Azure.

### Acceptance-only scope

`ObjectStorage/v1` is not registered in the production server catalog, neither
program is added to production composition, and the HTTP API is unchanged.
The OpenTofu test uses the explicitly development-only local backend profile;
production continues to require an operator-supplied conformant HTTPS HTTP
backend. No successful live qualification is claimed until a maintainer runs
the gated tests and records that evidence separately.

## Consequences

- The repository gains a concrete parity test across Pulumi and OpenTofu while
  preserving provisioner-neutral developer intent.
- Live execution is explicit, cost-bearing, credential-gated, and excluded
  from CI and `make verify`.
- Provider binaries and cloud state remain ignored local artifacts; only
  source and dependency checksums are committed.
- Promoting this contract or either implementation to production requires a
  later ADR and production composition work.

## Rejected Alternatives

### Publish `AzureStorageAccount` as the ResourceType

Rejected because it would make a cloud implementation part of the public
developer contract.

### Share provider-specific fields in the public spec

Rejected because Azure location, SKU, replication type, and access-tier names
are platform policy and implementation configuration.

### Register the programs in the production server immediately

Rejected because acceptance evidence alone does not establish production
routing, backend qualification, historical registration retention, or an
operational support commitment.
