# Backstage Full-System Demo (M21.6)

This walkthrough uses Backstage as a developer-plane client of Liftr's public
`/v1` API. It does not route the admin API or demo control listener through the
Backstage BFF.

## Prerequisites

- Docker with Compose
- Go toolchain from `go.mod`
- Node.js 20 or newer (the tested local version is in
  `integrations/backstage/.nvmrc`)
- Corepack, bash, and curl

No cloud credentials, Kubernetes cluster, or infrastructure CLI is required.

## Start

Start from a clean demo database:

```bash
make demo-down
make demo-backstage-up
make demo-backstage
```

Open:

```text
http://localhost:3000/liftr
```

The local host uses Backstage's guest **user** provider. The constrained BFF
still calls Backstage `httpAuth` and accepts user principals only. Between the
BFF and `liftr-demo-server`, `insecure-development` sends no bearer token and
is accepted only because both endpoints are literal-loopback HTTP. This is an
explicit demo composition, not a fallback. Production Liftr and production
Backstage authentication are unchanged.

## Architecture

```text
Browser :3000
  -> Backstage guest user session
  -> constrained Liftr BFF :7007 (/api/liftr only)
  -> Liftr developer API :18080 (/v1 only)
  -> PostgreSQL, outbox workers, lifecycle
  -> deterministic demo provisioner

Operator API:       http://127.0.0.1:18090/admin/v1
Demo control:       http://127.0.0.1:18099/release/{resourceID}
```

The BFF has ten explicit public routes. It has no generic URL proxy, wildcard
forwarding, `/admin/v1` route, or `:18099` route.

## Browser Walkthrough

The existing M21.5 types provide the requested roles without adding aliases or
new provisioner behavior:

| Demo role | ResourceType |
|---|---|
| dependency anchor | `DemoDatabase/v1` |
| dependent resource | `DemoApp/v1` |

Use owner `team/demo` throughout.

### 1. Inventory and discovery

Open the Liftr page. Inventory is read from `GET /v1/resources`, not the
Backstage Catalog. Open each ResourceType from the list. `DemoApp/v1` shows:

- create, update, and delete capabilities
- its draft 2020-12 spec contract
- reference slot `database`, cardinality `1..1`
- allowed target `DemoDatabase/v1`
- an output contract when the selected type declares one

No provisioner or backend metadata is shown.

### 2. Create AnchorA

Choose **Create Resource** and enter:

```text
ResourceType: DemoDatabase/v1
Resource ID: backstage-db-a
Owner: team/demo
```

Spec:

```json
{"engine":"demo-postgres","sizeGB":20,"hold":true}
```

References:

```json
{}
```

Create it. The Resource page shows ID, type, `Pending`, generation, observed
generation, Conditions, and latest Operation. The page polls bounded
authoritative Resource and Operation reads while work is active.

### 3. Create the dependent

Create `DemoApp/v1`:

```text
Resource ID: backstage-app
Owner: team/demo
```

Spec:

```json
{"image":"demo:v1","hold":false,"holdDelete":true}
```

References:

```json
{"database":["backstage-db-a"]}
```

Expected state:

```text
Pending
DependenciesReady=False
reason=WaitingForDependencies
```

The public API does not expose submission counts. To prove the private
execution fact from the separate operator plane, copy the create Operation ID
shown in Backstage and run:

```bash
curl -fsS http://127.0.0.1:18090/admin/v1/operations/OPERATION_ID/diagnostics
```

Expected: `attemptCount` is `0`. This admin request is deliberately outside
Backstage.

### 4. Release AnchorA

In a terminal, simulate backend convergence outside normal Liftr semantics:

```bash
curl -fsS -X POST http://127.0.0.1:18099/release/backstage-db-a
```

Do not click any action on `backstage-app`. Its page should progress through
normal reads:

```text
backstage-db-a Ready
-> WakeDependents
-> backstage-app dependency gate passes
-> create submission
-> backstage-app Ready
```

Open **Operations** on the app to read
`GET /v1/resources/backstage-app/operations` through the BFF.

### 5. Delete protection

Open `backstage-db-a` and choose **Delete**. Confirm the prompt.

Expected:

```text
HTTP 409
Code: RESOURCE_IN_USE
```

The error explains how to remove the reference without exposing private
dependent lists.

### 6. Advanced reference update

Create `DemoDatabase/v1` as `backstage-db-b` with the same held spec as
AnchorA. On `backstage-app`, choose **Update** and change only the references
editor to:

```json
{"database":["backstage-db-b"]}
```

Keep this spec:

```json
{"image":"demo:v2","hold":false,"holdDelete":true}
```

The generation precondition is the generation displayed on the page. While
AnchorB is Pending, the app can retain the Ready state of its already-applied
generation while `DependenciesReady=False` shows that the newer desired
generation is waiting. Its desired reference shows AnchorB. Deleting either
AnchorA or AnchorB returns `RESOURCE_IN_USE`: AnchorA is protected by applied
evidence and AnchorB by desired intent. Applied references remain private.

Release AnchorB:

```bash
curl -fsS -X POST http://127.0.0.1:18099/release/backstage-db-b
```

After the app update succeeds, AnchorA deletion succeeds while AnchorB remains
protected.

### 7. Cleanup

Delete `backstage-app` in Backstage. Because `holdDelete` is true, it remains
`Deleting`; AnchorB must still return `RESOURCE_IN_USE`. Release the app:

```bash
curl -fsS -X POST http://127.0.0.1:18099/release/backstage-app
```

Wait for `Deleted`, then delete AnchorB. Include deleted Resources in inventory
if you want to verify terminal records. Finish with:

```bash
make demo-down
```

## Error Rendering

The plugin provides developer guidance for `RESOURCE_IN_USE`,
`REFERENCE_INVALID`, `DEPENDENCY_CYCLE`, `POLICY_DENIED`, `QUOTA_EXCEEDED`, and
generation conflicts. Practical local triggers include:

| Problem | Local trigger |
|---|---|
| `REFERENCE_INVALID` | create `DemoApp/v1` with a missing or wrong-type `database` ID |
| `POLICY_DENIED` | create `DemoFault/v1` with `{"scenario":"clean"}`, wait for Ready, then update it under the demo policy |
| `QUOTA_EXCEEDED` | exceed four non-deleted `team/demo` Resources |
| generation conflict | submit an update from a stale browser tab after another update succeeds |

The current demo contracts form a one-way `DemoApp -> DemoDatabase` type graph,
so they cannot produce a real dependency cycle without adding a new
relationship contract. M21.6 deliberately does not add one; the
`DEPENDENCY_CYCLE` rendering path is covered by the frontend error tests.

## Automated Qualification

With a clean M21.5 server running:

```bash
make demo-backstage-test
```

This drives the frontend client through the constrained BFF pipeline against
the real demo server and verifies discovery, create, dependency waiting,
release/wake, Operations, delete protection, generation-safe reference update,
desired/applied protection, cleanup, idempotency forwarding, and rejection of
admin/generic proxy paths.

## Troubleshooting

- Backstage app log: `.demo/backstage-app.log`
- Backstage backend log: `.demo/backstage-backend.log`
- Liftr native log: `.demo/server.log`
- Liftr Compose log: `docker compose --profile demo logs demo-server`
- If ports `3000` or `7007` are occupied, stop that process; the demo does not
  weaken loopback restrictions or select remote endpoints.
- If fixed walkthrough IDs already exist, run `make demo-down` before startup.
- If the browser defaults to delegated auth, verify the frontend config exposes
  `liftr.auth.mode: insecure-development` and restart both Backstage processes.

## Plane Separation

Backstage is the developer plane: public ResourceTypes, Resources, Operations,
and lifecycle mutations through `/v1`.

The admin API on `:18090` is the operator plane: diagnostics and recovery. It
is not linked, forwarded, or authenticated by the developer BFF. The control
listener on `:18099` is an even narrower demo-only simulation endpoint and is
not a Liftr platform capability.
