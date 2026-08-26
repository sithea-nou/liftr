/**
 * M16 frontend wiring smoke (release-integrity requirement).
 *
 * 1. Inventory renders against a fake BFF and a ResourceSummary appears;
 *    selecting it (router link) renders the detail view, including outputs.
 * 2. An accepted mutation's envelope monitorOperationId is the ONLY polling
 *    target: a poisoned data.latestOperation is provably ignored.
 *
 * Uses the canonical @backstage/core-app-api ApiProvider/ApiRegistry so the
 * plugin's real useApi wiring executes unmocked. RTL/router are imported
 * dynamically inside tests, matching how vitest transforms this graph.
 */

/**
 * @vitest-environment jsdom
 */
import React from 'react';
import { configApiRef, discoveryApiRef, fetchApiRef, identityApiRef } from '@backstage/core-plugin-api';
import { ApiProvider } from '@backstage/core-app-api';
import { render, screen, fireEvent } from '@testing-library/react';
import { MemoryRouter, Route, Routes, useParams } from 'react-router-dom';
import { InventoryPage } from '../components/InventoryPage';
import { ResourceDetailPage } from '../components/ResourceDetailPage';
import { liftrAuthApiRef } from '../api/auth';
import { LiftrApiError, LiftrFrontendClient } from '../api/client';
import { ProblemView } from '../components/common';

function json(body: unknown, contentType = 'application/json'): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'Content-Type': contentType, 'Cache-Control': 'no-store' },
  });
}

function makeRegistry(fetchImpl: (url: string, init?: RequestInit) => Promise<Response>) {
  const config = {
    getOptionalString: (key: string) =>
      key === 'liftr.auth.mode' ? 'delegated' : undefined,
    getOptionalConfig: () => undefined,
    keys: () => [] as string[],
  };
  // Structural ApiHolder: ApiProvider only calls .get(apiRef).
  const impls = new Map<unknown, unknown>([
    [fetchApiRef, { fetch: fetchImpl }],
    [discoveryApiRef, { getBaseUrl: async () => '/api/liftr' }],
    [configApiRef, config],
    [
      identityApiRef,
      {
        getBackstageIdentity: async () => ({
          ownershipEntityRefs: ['group:default/payments'],
        }),
      },
    ],
    [liftrAuthApiRef, { getDelegationAssertion: async () => 'fake.assertion.token' }],
  ]);
  return { get: (apiRef: unknown) => impls.get(apiRef) };
}

const SUMMARY_FIXTURE = {
  items: [
    {
      id: 'orders-db',
      type: { name: 'PostgreSQLDatabase', version: 'v2' },
      owner: { kind: 'team', id: 'payments' },
      generation: 3,
      status: { state: 'Ready', observedGeneration: 3, updatedAt: '2026-08-24T09:30:00Z' },
      latestOperation: {
        id: 'op-list-ref',
        capability: 'update',
        state: 'Succeeded',
        targetGeneration: 3,
        href: '/v1/operations/op-list-ref',
      },
      createdAt: '2026-08-20T10:00:00Z',
      updatedAt: '2026-08-24T09:30:00Z',
    },
  ],
};

const DETAIL_FIXTURE = {
  ...SUMMARY_FIXTURE.items[0],
  spec: { storageGB: 20 },
  status: {
    state: 'Ready',
    observedGeneration: 3,
    updatedAt: '2026-08-24T09:30:00Z',
    conditions: [{ type: 'Reconciled', status: 'True', reason: 'ok', observedGeneration: 3 }],
  },
  outputs: { observedGeneration: 3, values: { hostname: 'db.example.com', port: 5432 } },
};

if (!(globalThis as Record<string, unknown>).matchMedia) {
  Object.defineProperty(globalThis, 'matchMedia', {
    writable: true,
    value: (query: string) => ({
      matches: false,
      media: query,
      onchange: null,
      addListener: () => {},
      removeListener: () => {},
      addEventListener: () => {},
      removeEventListener: () => {},
      dispatchEvent: () => false,
    }),
  });
}

const DetailBridge = () => {
  const { id } = useParams<{ id: string }>();
  return <ResourceDetailPage resourceId={id ?? ''} />;
};

async function waitForPredicate(predicate: () => void, timeoutMs = 15000): Promise<void> {
  const start = Date.now();
  let lastError: unknown = new Error('predicate never ran');
  while (Date.now() - start < timeoutMs) {
    try {
      predicate();
      return;
    } catch (e) {
      lastError = e;
    }
    await new Promise(r => setTimeout(r, 100));
  }
  throw lastError instanceof Error ? lastError : new Error(String(lastError));
}

describe('frontend wiring smoke', () => {
  it('renders bounded developer guidance for lifecycle and admission Problems', () => {
    const cases = [
      ['RESOURCE_IN_USE', 'Remove the desired reference'],
      ['REFERENCE_INVALID', 'Review the ResourceType reference contract'],
      ['DEPENDENCY_CYCLE', 'do not create a dependency cycle'],
      ['POLICY_DENIED', 'satisfy platform admission policy'],
      ['QUOTA_EXCEEDED', 'request a quota change'],
      ['GENERATION_CONFLICT', 'Refresh and review the current generation'],
    ];
    for (const [code, guidance] of cases) {
      const error = new LiftrApiError(
        {
          status: 409,
          code,
          title: 'Request refused',
          detail: 'curated public detail',
          ...(code === 'GENERATION_CONFLICT' ? { currentGeneration: 9n } : {}),
        },
        null,
        409,
      );
      const view = render(<ProblemView error={error} />);
      expect(view.baseElement.textContent).toContain(code);
      expect(view.baseElement.textContent).toContain(guidance);
      if (code === 'GENERATION_CONFLICT') expect(view.baseElement.textContent).toContain('9');
      view.unmount();
    }
  });

  it('renders inventory from GET /v1/resources, then renders detail on selection', async () => {
    const fetchImpl = jest.fn(async (url: string) => {
      if (/^\/api\/liftr\/v1\/resources(\?.*)?$/.test(url)) return json(SUMMARY_FIXTURE);
      if (url.startsWith('/api/liftr/v1/resource-types')) return json({ items: [] });
      if (/^\/api\/liftr\/v1\/resources\/orders-db$/.test(url)) return json(DETAIL_FIXTURE);
      throw new Error(`unexpected fetch ${url}`);
    });
    // prop-types' ReactNodeLike vs React's ReactNode: loosen the component
    // type for the fixture only.
    const Provider = ApiProvider as unknown as React.FC<{
      apis: unknown;
      children?: React.ReactNode;
    }>;
    const view = render(
      React.createElement(
        Provider,
        { apis: makeRegistry(fetchImpl as unknown as typeof fetch) as never },
        React.createElement(
          MemoryRouter,
          { initialEntries: ['/liftr'] },
          React.createElement(
            Routes,
            null,
            React.createElement(Route, {
              path: '/liftr',
              element: React.createElement(InventoryPage),
            }),
            React.createElement(Route, {
              path: '/liftr/resources/:id',
              element: React.createElement(DetailBridge),
            }),
          ),
        ),
      ),
    );

    // ResourceSummary appears.
    await waitForPredicate(() => {
      expect(view.baseElement.textContent).toContain('orders-db');
    });

    // Select the resource via its router link -> detail renders.
    fireEvent.click(screen.getByRole('link', { name: 'orders-db' }));

    await waitForPredicate(() => {
      expect(view.baseElement.textContent).toContain('Resource orders-db');
    });

    // Open the Outputs tab and verify generation-bound output data renders.
    fireEvent.click(screen.getByText('Outputs'));
    await waitForPredicate(() => {
      expect(view.baseElement.textContent).toContain('db.example.com');
      expect(view.baseElement.textContent).toContain('5432');
    });

    // Detail came from the authoritative detail endpoint.
    expect(fetchImpl).toHaveBeenCalledWith(
      '/api/liftr/v1/resources/orders-db',
      expect.anything(),
    );
  }, 30000);

  it('polls the admitted child operation and ignores poisoned latestOperation', async () => {
    const polledUrls: string[] = [];
    let pollsForChild = 0;
    const fetchImpl = jest.fn(async (url: string, init?: RequestInit) => {
      const method = (init?.method ?? 'GET').toUpperCase();
      if (method === 'POST' && url === '/api/liftr/v1/resources') {
        return new Response(
          JSON.stringify({
            data: {
              id: 'orders-db',
              generation: 1,
              latestOperation: {
                id: 'op-newer-poison',
                capability: 'create',
                state: 'Running',
                targetGeneration: 9,
                href: '/v1/operations/op-newer-poison',
              },
            },
            monitorOperationId: 'op-child',
          }),
          { status: 201, headers: { 'Content-Type': 'application/json' } },
        );
      }
      if (method === 'GET' && url.startsWith('/api/liftr/v1/operations/')) {
        polledUrls.push(url);
        if (url.includes('op-child')) {
          pollsForChild += 1;
          return json({
            id: 'op-child',
            resourceId: 'orders-db',
            retryOf: 'op-parent',
            capability: 'create',
            state: pollsForChild >= 2 ? 'Succeeded' : 'Pending',
            targetGeneration: 1,
            requestedAt: 't',
          });
        }
        return json({
          id: 'op-newer-poison',
          state: 'Failed',
          targetGeneration: 9,
          requestedAt: 't',
        });
      }
      throw new Error(`unexpected fetch ${method} ${url}`);
    });

    const client = new LiftrFrontendClient(
      { fetch: fetchImpl as unknown as typeof fetch } as never,
      { getDelegationAssertion: async () => 'fake.assertion.token' },
    );

    const env = await client.create({
      bodyText:
        '{"id":"orders-db","type":{"name":"T","version":"v1"},"owner":{"kind":"team","id":"payments"},"spec":{}}',
      idempotencyKey: '11111111-2222-3333-4444-555555555555',
    });
    expect(env.monitorOperationId).toBe('op-child');
    expect((env.data as { latestOperation?: { id: string } }).latestOperation?.id).toBe(
      'op-newer-poison',
    );

    const { renderHook } = await import('@testing-library/react');
    const { useOperationMonitor } = await import('../hooks/useOperationMonitor');
    const { result } = renderHook(() => useOperationMonitor(client, env.monitorOperationId));

    await waitForPredicate(() => {
      expect(result.current.status).toBe('succeeded');
    });

    expect(polledUrls.length).toBeGreaterThanOrEqual(2);
    for (const u of polledUrls) {
      if (!u.endsWith('/operations/op-child')) {
        throw new Error(`polled wrong operation: ${u}`);
      }
    }
    expect(polledUrls.some(u => u.includes('op-newer-poison'))).toBe(false);
  }, 30000);
});
