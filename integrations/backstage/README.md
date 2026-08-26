# Liftr Backstage Integration

Backstage is a **client** of the Liftr public `/v1` API — nothing more.

```
Backstage SPA ──► plugins/liftr-backend (constrained BFF) ──► Liftr GET/POST/PUT/DELETE /v1/*
```

* `plugins/liftr-common` — shared contracts: DTO guards mirroring
  `docs/openapi/v1/openapi.yaml`, lossless-JSON numeric fidelity (`20` ≠
  `20.0`), monitor-link validation, opaque idempotency handling, origin
  validation, Problem sanitization, delegation subject binding.
* `plugins/liftr-backend` — user-only BFF: delegated credential acquisition,
  explicit finite route mirror, mutation envelope
  (`{data, monitorOperationId}`), asymmetric 401 policy.
* `plugins/liftr` — frontend: discovery, authoritative inventory
  (`GET /v1/resources`), detail with output freshness, operations history,
  create/update/delete/retry with generation + idempotency safety.

## Tested compatibility baseline (exactly what CI exercises)

M16 targets ONE reference baseline; no other Backstage release line is claimed
or tested. "Backstage" is an umbrella of independently versioned packages —
the versions below are the resolved, locked, and tested set
(`yarn.lock` is authoritative). The earlier phrase "backend-plugin-api 1.10"
referred to that single package's own version, not a Backstage product
release.

| Component | Tested version |
| --- | --- |
| Node.js | 24.x (`.nvmrc`; engines `>=20`) |
| Yarn | 4.9.2 via `packageManager` + Corepack |
| @backstage/backend-plugin-api | 1.10.0 |
| @backstage/backend-defaults | ^0.8 line (resolved in yarn.lock) |
| @backstage/plugin-auth-node | 0.7.4 |
| @backstage/config | 1.3.8 |
| @backstage/errors | 1.3.1 |
| @backstage/core-plugin-api | 1.12.9 |
| @backstage/core-components | 0.16.4 |
| @backstage/frontend-plugin-api | 0.18.0 |
| @backstage/frontend-defaults | ^0.5 line (resolved in yarn.lock) |
| @backstage/integration-react | 1.2.21 |
| @backstage/cli | ^0.32 line (dist-workspace model) |
| react / react-dom | 18.3.1 |
| react-router-dom | 6.30.x |

Host applications are expected to resolve these peers against their own
workspace (standard Backstage monorepo dedupe); the host fixture in this
repository pins and proves exactly the set above. Do not assume compatibility
with materially older or newer generations without re-running
`make verify-backstage`.

## Reproducible install

`yarn.lock` is committed and authoritative. Both CI and
`make verify-backstage` install with `--immutable` and finish with a
`git diff --exit-code` gate: a clean checkout must never modify
`yarn.lock`, manifests, or `.yarnrc.yml`. Deleting the lockfile or editing a
manifest without regenerating it makes installation fail by design.

## Install into an existing Backstage app

This tree is a self-contained Yarn (Berry) workspace using contemporary
Backstage package conventions (`backstage.role`, config schemas,
dist-workspace builds). Two supported consumption paths:

1. Copy/symlink the three plugin packages into your app's monorepo and add
   them to your root workspaces; register `liftrPlugin` in
   `packages/backend/src/index.ts`
   (`backend.add(import('@liftr/plugin-liftr-backend'))`) and mount
   `LiftrPage` at a route (classic) or import `liftrFrontendPlugin`
   (default export of `@liftr/plugin-liftr`) as a New Frontend System
   feature.
2. Publish privately from this workspace and depend on it from your app.

### Host compatibility fixture

`test-host/packages/{app,backend}` is a minimal disposable host workspace
(NOT a distributable portal) wired into the same yarn project. It proves:
immutable workspace install; frontend plugin feature registration under the
New Frontend System; backend plugin registration via `createBackend().add`;
host typecheck; frontend package build (webpack bundle); backend dist-workspace
packaging; and that operator configuration loads against both plugins'
`config-schema.json`. Run it with `yarn verify:host`. For M21.6 only, the same
fixture is runnable on loopback with Backstage's guest user provider, an
in-memory SQLite database, and explicit Liftr `insecure-development` mode via
`make demo-backstage-up`. This does not define a production auth composition;
adopters still provide their own identity, database, and deployment wiring.

Configuration reference: [docs/AUTHENTICATION.md](docs/AUTHENTICATION.md) and
[examples/app-config.liftr.example.yaml](examples/app-config.liftr.example.yaml).

## Development

```bash
corepack enable                       # Yarn 4.9.2 via packageManager field
yarn install --immutable
yarn typecheck && yarn lint && yarn test && yarn verify:host
```

From the repository root, `make verify-backstage` runs the same pipeline with
a prerequisite check plus the tree-purity gate; `make verify` remains Go-only
and never needs Node.

## Full-auth composition status

The Keycloak profile (`examples/docker-compose.keycloak.yaml`) provides a
reference Standard Token Exchange setup for local development and manual
acceptance. It has **not** been automated or fully acceptance-tested in CI;
deterministic RFC 8693 exchange coverage lives in the offline test suite
(fake STS). Enable "Standard Token Exchange" on the `liftr-backstage-bff`
client in the Keycloak admin console before exercising delegation manually.
