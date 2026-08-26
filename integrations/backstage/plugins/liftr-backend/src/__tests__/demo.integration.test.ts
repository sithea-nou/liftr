/**
 * Opt-in M21.6 qualification against the real cmd/liftr-demo-server. The test
 * places the handwritten frontend client behind the constrained BFF pipeline;
 * it never calls developer mutations directly.
 */

import { createServer } from 'node:http';
import { AddressInfo } from 'node:net';
import { afterAll, beforeAll, describe, expect, it } from 'vitest';
import {
  buildCreateResourceBody,
  buildUpdateResourceBody,
  Operation,
  Resource,
} from '@liftr/plugin-liftr-common';
import { LiftrFrontendClient } from '@liftr/plugin-liftr/src/api/client';
import { LiftrBackendConfig } from '../config';
import { InsecureDevelopmentCredentialProvider } from '../credentials/insecureDev';
import { UpstreamForwarder } from '../forwarder';
import { IncomingRequest, RouteDeps, handleLiftrProxyRequest } from '../routes';

const DEMO_BASE_URL = process.env['LIFTR_BACKSTAGE_DEMO_BASE_URL'];
const CONTROL_BASE_URL = process.env['LIFTR_BACKSTAGE_DEMO_CONTROL_URL'] ?? 'http://127.0.0.1:18099';
const liveDescribe = DEMO_BASE_URL ? describe : describe.skip;

interface RecordedUpstream {
  url: string;
  headers: Headers;
  body: string;
}

function key(label: string): string {
  return `m21-6-${label}-${Date.now()}-${Math.random().toString(16).slice(2)}`;
}

function body(result: ReturnType<typeof buildCreateResourceBody> | ReturnType<typeof buildUpdateResourceBody>): string {
  if (!result.ok) throw new Error(result.error);
  return result.bodyText;
}

async function waitForResource(
  client: LiftrFrontendClient,
  id: string,
  predicate: (resource: Resource) => boolean,
  timeoutMs = 30_000,
): Promise<Resource> {
  const deadline = Date.now() + timeoutMs;
  let last: Resource | undefined;
  while (Date.now() < deadline) {
    last = await client.getResource(id);
    if (predicate(last)) return last;
    await new Promise(resolve => setTimeout(resolve, 200));
  }
  throw new Error(`timed out waiting for ${id}; last state ${last?.status.state ?? 'unknown'}`);
}

async function waitForOperation(
  client: LiftrFrontendClient,
  id: string,
  predicate: (operation: Operation) => boolean,
  timeoutMs = 30_000,
): Promise<Operation> {
  const deadline = Date.now() + timeoutMs;
  let last: Operation | undefined;
  while (Date.now() < deadline) {
    last = await client.getOperation(id);
    if (predicate(last)) return last;
    await new Promise(resolve => setTimeout(resolve, 200));
  }
  throw new Error(`timed out waiting for operation ${id}; last state ${last?.state ?? 'unknown'}`);
}

liveDescribe('M21.6 real demo BFF/client qualification', () => {
  let server: ReturnType<typeof createServer>;
  let bffBaseUrl = '';
  let client: LiftrFrontendClient;
  const upstream: RecordedUpstream[] = [];
  const suffix = `${Date.now()}-${Math.random().toString(16).slice(2, 8)}`;
  const anchorA = `bs-db-a-${suffix}`;
  const anchorB = `bs-db-b-${suffix}`;
  const dependent = `bs-app-${suffix}`;

  beforeAll(async () => {
    const origin = new URL(DEMO_BASE_URL!);
    const config: LiftrBackendConfig = {
      origin: {
        scheme: origin.protocol.slice(0, -1) as 'http',
        host: origin.hostname,
        effectivePort: Number(origin.port || 80),
      },
      baseUrl: DEMO_BASE_URL!,
      auth: { mode: 'insecure-development' },
      requestTimeoutMs: 5_000,
      maxResponseBytes: 1_048_576,
      correlationMaxLength: 128,
      maxRequestBodyBytes: 1_048_576,
    };
    const recordingFetch: typeof fetch = async (input, init) => {
      upstream.push({
        url: String(input),
        headers: new Headers(init?.headers),
        body: typeof init?.body === 'string' ? init.body : '',
      });
      return fetch(input, init);
    };
    const deps: RouteDeps = {
      config,
      provider: new InsecureDevelopmentCredentialProvider(),
      forwarder: new UpstreamForwarder(
        { requestTimeoutMs: config.requestTimeoutMs, maxResponseBytes: config.maxResponseBytes },
        recordingFetch,
        path => `${config.baseUrl}${path}`,
        () => {},
      ),
      authenticator: { authenticate: async () => ({ ok: true, userEntityRef: 'user:default/guest' }) },
      log: { event: () => {}, error: () => {} },
    };

    server = createServer((request, response) => {
      const chunks: Buffer[] = [];
      request.on('data', chunk => chunks.push(chunk as Buffer));
      request.on('end', () => {
        const url = new URL(request.url ?? '/', 'http://bff.invalid');
        const incoming: IncomingRequest = {
          method: request.method ?? 'GET',
          path: url.pathname,
          query: url.searchParams,
          header: name => {
            const value = request.headers[name.toLowerCase()];
            return Array.isArray(value) ? value[0] : value;
          },
          bodyText: () => Buffer.concat(chunks).toString('utf8'),
        };
        void handleLiftrProxyRequest(deps, incoming, request).then(result => {
          response.writeHead(result.status, { 'Content-Type': result.contentType, ...result.headers });
          response.end(result.bodyText);
        });
      });
    });
    await new Promise<void>(resolve => server.listen(0, '127.0.0.1', resolve));
    bffBaseUrl = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
    client = new LiftrFrontendClient({ fetch } as never, { baseUrl: bffBaseUrl });
  });

  afterAll(async () => {
    await new Promise<void>(resolve => server.close(() => resolve()));
  });

  it('proves discovery, relationships, wake-up, operations, protection, update, and cleanup', async () => {
    const types = await client.listResourceTypes();
    expect(types.items.map(type => `${type.name}/${type.version}`)).toEqual([
      'DemoApp/v1',
      'DemoDatabase/v1',
      'DemoFault/v1',
    ]);
    const appType = await client.getResourceType('DemoApp', 'v1');
    expect(appType.referenceContract?.slots).toEqual([{
      name: 'database',
      allowedTargetTypes: [{ name: 'DemoDatabase', version: 'v1' }],
      minItems: 1,
      maxItems: 1,
    }]);

    const createA = await client.create({
      idempotencyKey: key('create-a'),
      bodyText: body(buildCreateResourceBody({
        id: anchorA,
        typeName: 'DemoDatabase',
        typeVersion: 'v1',
        ownerKind: 'team',
        ownerId: 'demo',
        specText: '{"engine":"demo-postgres","sizeGB":20,"hold":true}',
        referencesText: '{}',
      })),
    });
    expect(createA.monitorOperationId).toBeTruthy();
    await waitForResource(client, anchorA, resource => resource.status.state === 'Pending');

    const createDependentKey = key('create-dependent');
    const dependentBody = body(buildCreateResourceBody({
      id: dependent,
      typeName: 'DemoApp',
      typeVersion: 'v1',
      ownerKind: 'team',
      ownerId: 'demo',
      specText: '{"image":"demo:v1","hold":false}',
      referencesText: `{"database":["${anchorA}"]}`,
    }));
    const createDependent = await client.create({
      idempotencyKey: createDependentKey,
      bodyText: dependentBody,
    });
    expect(createDependent.monitorOperationId).toBeTruthy();
    const waiting = await waitForResource(client, dependent, resource =>
      resource.status.conditions.some(condition =>
        condition.type === 'DependenciesReady' &&
        condition.status === 'False' &&
        condition.reason === 'WaitingForDependencies',
      ),
    );
    expect(waiting.references).toEqual({ database: [anchorA] });
    const forwardedCreate = upstream.find(request => request.headers.get('Idempotency-Key') === createDependentKey);
    expect(forwardedCreate?.body).toBe(dependentBody);

    const releaseA = await fetch(`${CONTROL_BASE_URL}/release/${encodeURIComponent(anchorA)}`, { method: 'POST' });
    expect(releaseA.status).toBe(204);
    await waitForResource(client, anchorA, resource => resource.status.state === 'Ready');
    const readyDependent = await waitForResource(client, dependent, resource => resource.status.state === 'Ready');
    expect(readyDependent.status.conditions.find(condition => condition.type === 'DependenciesReady')?.status).toBe('True');
    const history = await client.listOperations(dependent, 20);
    expect(history.items.some(operation => operation.id === createDependent.monitorOperationId)).toBe(true);

    await expect(client.remove({
      resourceId: anchorA,
      idempotencyKey: key('protected-a'),
      viewedGeneration: (await client.getResource(anchorA)).generation.toString(),
    })).rejects.toMatchObject({ status: 409, problem: { code: 'RESOURCE_IN_USE' } });

    await client.create({
      idempotencyKey: key('create-b'),
      bodyText: body(buildCreateResourceBody({
        id: anchorB,
        typeName: 'DemoDatabase',
        typeVersion: 'v1',
        ownerKind: 'team',
        ownerId: 'demo',
        specText: '{"engine":"demo-postgres","sizeGB":20,"hold":true}',
        referencesText: '{}',
      })),
    });
    await waitForResource(client, anchorB, resource => resource.status.state === 'Pending');

    const beforeUpdate = await client.getResource(dependent);
    const updateKey = key('update-reference');
    const updateBody = body(buildUpdateResourceBody(
      '{"image":"demo:v2","hold":false}',
      `{"database":["${anchorB}"]}`,
    ));
    const update = await client.update({
      resourceId: dependent,
      bodyText: updateBody,
      idempotencyKey: updateKey,
      viewedGeneration: beforeUpdate.generation.toString(),
    });
    const updateForward = upstream.find(request => request.headers.get('Idempotency-Key') === updateKey);
    expect(updateForward?.body).toBe(updateBody);
    expect(updateForward?.headers.get('If-Liftr-Generation')).toBe(beforeUpdate.generation.toString());
    const repointed = await waitForResource(client, dependent, resource =>
      resource.references?.database?.[0] === anchorB &&
      resource.status.conditions.some(condition =>
        condition.type === 'DependenciesReady' && condition.status === 'False',
      ),
    );

    for (const protectedAnchor of [anchorA, anchorB]) {
      await expect(client.remove({
        resourceId: protectedAnchor,
        idempotencyKey: key(`protected-${protectedAnchor}`),
        viewedGeneration: (await client.getResource(protectedAnchor)).generation.toString(),
      })).rejects.toMatchObject({ status: 409, problem: { code: 'RESOURCE_IN_USE' } });
    }

    const releaseB = await fetch(`${CONTROL_BASE_URL}/release/${encodeURIComponent(anchorB)}`, { method: 'POST' });
    expect(releaseB.status).toBe(204);
    await waitForOperation(client, update.monitorOperationId, operation => operation.state === 'Succeeded');
    const converged = await waitForResource(client, dependent, resource =>
      resource.status.state === 'Ready' &&
      resource.status.observedGeneration.toString() === repointed.generation.toString() &&
      resource.status.conditions.some(condition =>
        condition.type === 'DependenciesReady' && condition.status === 'True',
      ),
    );
    expect(converged.references).toEqual({ database: [anchorB] });

    await client.remove({
      resourceId: anchorA,
      idempotencyKey: key('delete-a'),
      viewedGeneration: (await client.getResource(anchorA)).generation.toString(),
    });
    await waitForResource(client, anchorA, resource => resource.status.state === 'Deleted');
    await client.remove({
      resourceId: dependent,
      idempotencyKey: key('delete-dependent'),
      viewedGeneration: converged.generation.toString(),
    });
    await waitForResource(client, dependent, resource => resource.status.state === 'Deleted');
    await client.remove({
      resourceId: anchorB,
      idempotencyKey: key('delete-b'),
      viewedGeneration: (await client.getResource(anchorB)).generation.toString(),
    });
    await waitForResource(client, anchorB, resource => resource.status.state === 'Deleted');

    const beforeRefusals = upstream.length;
    for (const path of ['/admin/v1/resources', '/proxy?url=http://127.0.0.1:18090/admin/v1/resources']) {
      const response = await fetch(`${bffBaseUrl}${path}`);
      expect(response.status).toBe(400);
    }
    expect(upstream).toHaveLength(beforeRefusals);
  }, 120_000);
});
