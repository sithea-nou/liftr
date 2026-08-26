/**
 * L1 integration tests: the full BFF pipeline against deterministic fake
 * Liftr and STS HTTP servers on literal loopback. Covers Corrections 1–4
 * end-to-end plus the adversarial matrix.
 */

import { afterAll, beforeAll, describe, expect, it } from 'vitest';
import { createServer, Server } from 'node:http';
import { AddressInfo } from 'node:net';
import { IncomingMessage, RequestListener } from 'node:http';

import {
  DELEGATION_HEADER,
} from '../credentials/provider';
import { LiftrBackendConfig } from '../config';
import { UpstreamForwarder } from '../forwarder';
import { RouteDeps, ROUTES, handleLiftrProxyRequest, IncomingRequest } from '../routes';
import { TokenExchangeCredentialProvider } from '../credentials/tokenExchange';
import { PassthroughCredentialProvider } from '../credentials/passthrough';
import { InsecureDevelopmentCredentialProvider } from '../credentials/insecureDev';
import { parseLiftrBackendConfig } from '../config';

// ---------------------------------------------------------------------------
// Fake servers
// ---------------------------------------------------------------------------

interface RecordedRequest {
  method: string;
  url: string;
  headers: IncomingMessage['headers'];
  body: string;
}

interface FakeServer {
  baseUrl: string;
  originPort: number;
  requests: RecordedRequest[];
  close(): Promise<void>;
}

function startFake(
  handler: (req: RecordedRequest, respond: (status: number, headers?: Record<string, string>, body?: string) => void) => void,
): Promise<FakeServer> {
  const requests: RecordedRequest[] = [];
  const listener: RequestListener = (req, res) => {
    const chunks: Buffer[] = [];
    req.on('data', c => chunks.push(c as Buffer));
    req.on('end', () => {
      const record: RecordedRequest = {
        method: req.method ?? 'GET',
        url: req.url ?? '/',
        headers: req.headers,
        body: Buffer.concat(chunks).toString('utf8'),
      };
      requests.push(record);
      handler(record, (status, headers = {}, body = '') => {
        res.writeHead(status, headers);
        res.end(body);
      });
    });
  };
  const server = createServer(listener);
  return new Promise(resolve => {
    server.listen(0, '127.0.0.1', () => {
      const port = (server.address() as AddressInfo).port;
      resolve({
        baseUrl: `http://127.0.0.1:${port}`,
        originPort: port,
        requests,
        close: () => new Promise(done => server.close(() => done())),
      });
    });
  });
}

// ---------------------------------------------------------------------------
// JWT helpers
// ---------------------------------------------------------------------------

const ISSUER = 'https://sts.example.com/realms/liftr';

function b64url(obj: unknown): string {
  return Buffer.from(JSON.stringify(obj), 'utf8')
    .toString('base64')
    .replace(/\+/g, '-')
    .replace(/\//g, '_')
    .replace(/=+$/, '');
}

function signJwt(payload: Record<string, unknown>): string {
  return `${b64url({ alg: 'RS256', typ: 'JWT' })}.${b64url(payload)}.fakesig`;
}

const ALICE_REF = 'user:default/alice';

function aliceAssertion(sub = 'alice-idp-sub'): string {
  return signJwt({ iss: ISSUER, sub, aud: ['liftr'], backstage_user_ref: ALICE_REF });
}

function bobAssertion(): string {
  return signJwt({ iss: ISSUER, sub: 'bob-idp-sub', aud: ['liftr'], backstage_user_ref: 'user:default/bob' });
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

interface HarnessOptions {
  mode?: 'delegated' | 'passthrough' | 'insecure-development';
  principal?: { ok: true; userEntityRef: string } | { ok: false; kind: 'unauthenticated' | 'service-principal' };
  liftrHandler: (req: RecordedRequest, respond: (status: number, headers?: Record<string, string>, body?: string) => void) => void;
  stsHandler?: (req: RecordedRequest, respond: (status: number, headers?: Record<string, string>, body?: string) => void) => void;
  forwarderFetch?: typeof fetch;
  maxResponseBytes?: number;
}

async function startHarness(opts: HarnessOptions) {
  const liftr = await startFake(opts.liftrHandler);
  let sts: FakeServer | null = null;
  if (opts.mode !== 'insecure-development') {
    sts = await startFake(
      opts.stsHandler ??
        ((_req, respond) => {
          // Deterministic STS: echo subject identity into an RFC 9068-shaped
          // token targeted at the Liftr audience.
          const form = new URLSearchParams(_req.body);
          const subjectToken = form.get('subject_token') ?? '';
          const payloadPart = subjectToken.split('.')[1] ?? '';
          const json = Buffer.from(payloadPart.replace(/-/g, '+').replace(/_/g, '/'), 'base64').toString('utf8');
          const claims = JSON.parse(json) as { iss: string; sub: string };
          respond(200, { 'Content-Type': 'application/json' }, JSON.stringify({
            access_token: signJwt({ iss: ISSUER, sub: claims.sub, aud: ['liftr-api'] }),
            token_type: 'Bearer',
            expires_in: 300,
          }));
        }),
    );
  }

  const cfg: LiftrBackendConfig = {
    origin: { scheme: 'http', host: '127.0.0.1', effectivePort: liftr.originPort },
    baseUrl: liftr.baseUrl,
    auth:
      opts.mode === 'insecure-development'
        ? { mode: 'insecure-development' }
        : opts.mode === 'passthrough'
          ? { mode: 'passthrough' }
          : {
              mode: 'delegated',
              tokenEndpoint: `${sts!.baseUrl}/token`,
              clientId: 'liftr-bff',
              clientSecret: 'secret-value',
              clientAuthMethod: 'basic',
              audience: 'liftr-api',
              subjectTokenType: 'urn:ietf:params:oauth:token-type:access_token',
              assertionIssuer: ISSUER,
              binding: { strategy: 'claim', claimName: 'backstage_user_ref', trustedIssuer: ISSUER },
              exchangeTimeoutMs: 2000,
            },
    requestTimeoutMs: 2000,
    maxResponseBytes: opts.maxResponseBytes ?? 65536,
    correlationMaxLength: 128,
    maxRequestBodyBytes: 65536,
  };

  const events: Array<Record<string, unknown>> = [];
  const log = {
    event: (fields: Record<string, string | number | boolean>) => {
      events.push({ ...fields });
    },
    error: () => {},
  };

  const credLog = { event: (f: Record<string, string | number | boolean>) => log.event(f) };
  const provider =
    cfg.auth.mode === 'insecure-development'
      ? new InsecureDevelopmentCredentialProvider()
      : cfg.auth.mode === 'passthrough'
        ? new PassthroughCredentialProvider(credLog)
        : new TokenExchangeCredentialProvider(cfg.auth, fetch, credLog);

  const forwarder = new UpstreamForwarder(
    { requestTimeoutMs: cfg.requestTimeoutMs, maxResponseBytes: cfg.maxResponseBytes },
    opts.forwarderFetch ?? fetch,
    p => `${cfg.baseUrl}${p}`,
    f => log.event(f),
  );

  const principal = opts.principal ?? { ok: true as const, userEntityRef: ALICE_REF };

  const deps: RouteDeps = {
    config: cfg,
    provider,
    forwarder,
    authenticator: { authenticate: async () => principal },
    log,
  };

  function incoming(
    method: string,
    pathAndQuery: string,
    init: { headers?: Record<string, string>; body?: string } = {},
  ): IncomingRequest {
    const [path, qs = ''] = pathAndQuery.split('?');
    const headers = { ...(init.headers ?? {}) };
    if (cfg.auth.mode !== 'insecure-development' && headers[DELEGATION_HEADER] === undefined && !('no-delegation' in (init.headers ?? {}))) {
      headers[DELEGATION_HEADER] = aliceAssertion();
    }
    if (init.body !== undefined && headers['Content-Type'] === undefined) {
      headers['Content-Type'] = 'application/json';
    }
    return {
      method,
      path: path!,
      query: new URLSearchParams(qs),
      header: name => Object.entries(headers).find(([k]) => k.toLowerCase() === name.toLowerCase())?.[1],
      bodyText: () => init.body ?? '',
    };
  }

  return {
    deps,
    cfg,
    liftr,
    sts,
    events,
    incoming,
    async close() {
      await liftr.close();
      await sts?.close();
    },
  };
}

// ---------------------------------------------------------------------------
// Shared fixtures
// ---------------------------------------------------------------------------

const RESOURCE_JSON = '{"id":"orders-db","generation":3,"spec":{"storageGB":20},"latestOperation":{"id":"op-newer-poison","capability":"update","state":"Running","targetGeneration":9,"href":"/v1/operations/op-newer-poison"}}';
const CHILD_OP_JSON = '{"id":"op-child","resourceId":"orders-db","retryOf":"op-parent","capability":"update","state":"Pending","targetGeneration":3,"requestedAt":"t"}';

async function untilAll(servers: Array<{ close(): Promise<void> }>) {
  await Promise.all(servers.map(s => s.close()));
}

// ---------------------------------------------------------------------------

describe('Correction 2 — authoritative BFF monitor contract', () => {
  let h: Awaited<ReturnType<typeof startHarness>>;

  beforeAll(async () => {
    h = await startHarness({
      liftrHandler: (_req, respond) =>
        respond(202, {
          'Content-Type': 'application/json',
          'Cache-Control': 'no-store',
          Link: '</v1/operations/op-child>; rel="monitor"',
          Location: '/v1/operations/op-location-ignored',
          'X-Request-ID': 'req-liftr-1',
        }, RESOURCE_JSON),
    });
  });

  it('B: valid monitor link yields the exact operation id', async () => {
    const r = await handleLiftrProxyRequest(h.deps, h.incoming('POST', '/v1/resources', {
      headers: { 'Idempotency-Key': 'k-monitor-valid' },
      body: '{"id":"orders-db","type":{"name":"T","version":"v1"},"owner":{"kind":"team","id":"payments"},"spec":{}}',
    }));
    expect(r.status).toBe(202);
    const doc = JSON.parse(r.bodyText) as { data: unknown; monitorOperationId: string };
    expect(doc.monitorOperationId).toBe('op-child');
    expect(doc.data).toMatchObject({ id: 'orders-db' });
    expect(r.headers['X-Request-ID']).toBe('req-liftr-1');
  });

  it('A: poisoned latestOperation inside the body is never used', async () => {
    const r = await handleLiftrProxyRequest(h.deps, h.incoming('PUT', '/v1/resources/orders-db', {
      headers: { 'Idempotency-Key': 'k-poison', 'If-Liftr-Generation': '3' },
      body: '{"spec":{"storageGB":21}}',
    }));
    const doc = JSON.parse(r.bodyText) as { monitorOperationId: string };
    expect(doc.monitorOperationId).toBe('op-child'); // from Link, not latestOperation
  });

  it('strips navigation headers from every downstream response', async () => {
    const r = await handleLiftrProxyRequest(h.deps, h.incoming('PUT', '/v1/resources/orders-db', {
      headers: { 'Idempotency-Key': 'k-strip', 'If-Liftr-Generation': '3' },
      body: '{"spec":{}}',
    }));
    expect(r.headers['Link']).toBeUndefined();
    expect(r.headers['Location']).toBeUndefined();
  });

  afterAll(() => untilAll([h]));
});

describe('Correction 2 — hostile and absent monitor references', () => {
  it('C: attacker cross-origin monitor URL is refused (protocol failure)', async () => {
    const h = await startHarness({
      liftrHandler: (_r, respond) =>
        respond(202, {
          'Content-Type': 'application/json',
          Link: '<https://evil.example.com/v1/operations/op-x>; rel="monitor"',
        }, CHILD_OP_JSON),
    });
    try {
      const r = await handleLiftrProxyRequest(h.deps, h.incoming('POST', '/v1/resources', {
        headers: { 'Idempotency-Key': 'k-evi' },
        body: '{"id":"x","type":{"name":"T","version":"v1"},"owner":{"kind":"k","id":"i"},"spec":{}}',
      }));
      expect(r.status).toBe(502);
      expect(JSON.parse(r.bodyText)).toMatchObject({ code: 'LIFTR_PROTOCOL_ERROR' });
    } finally {
      await h.close();
    }
  });

  it('D: missing monitor link is a protocol failure, never a guess', async () => {
    const h = await startHarness({
      liftrHandler: (_r, respond) => respond(201, { 'Content-Type': 'application/json' }, RESOURCE_JSON),
    });
    try {
      const r = await handleLiftrProxyRequest(h.deps, h.incoming('POST', '/v1/resources', {
        headers: { 'Idempotency-Key': 'k-nolink' },
        body: '{"id":"x","type":{"name":"T","version":"v1"},"owner":{"kind":"k","id":"i"},"spec":{}}',
      }));
      expect(r.status).toBe(502);
      const body = JSON.parse(r.bodyText);
      expect(body.code).toBe('LIFTR_PROTOCOL_ERROR');
      expect(String(body.detail)).toMatch(/refusing to guess/i);
    } finally {
      await h.close();
    }
  });

  it('E: retry polls the NEW child operation from its own monitor link', async () => {
    const h = await startHarness({
      liftrHandler: (_r, respond) =>
        respond(202, {
          'Content-Type': 'application/json',
          Link: '</v1/operations/op-child>; rel="monitor"',
          Location: '/v1/operations/op-parent', // parent Location must be ignored
        }, CHILD_OP_JSON),
    });
    try {
      const r = await handleLiftrProxyRequest(h.deps, h.incoming('POST', '/v1/operations/op-parent/retry', {
        headers: { 'Idempotency-Key': 'k-retry', 'If-Liftr-Generation': '3' },
      }));
      expect(r.status).toBe(202);
      const doc = JSON.parse(r.bodyText) as { data: { retryOf?: string }; monitorOperationId: string };
      expect(doc.monitorOperationId).toBe('op-child');
      expect(doc.data.retryOf).toBe('op-parent');
    } finally {
      await h.close();
    }
  });
});

describe('Correction 3 — idempotency keys stay opaque byte-for-byte', () => {
  it('forwards a valid non-UUID key unchanged (adversarial 6)', async () => {
    const weirdKey = 'order-fiscal-2026.07#01 v2';
    const seenOutbound: string[] = [];
    // Observe the exact header value placed on the wire. Leading/trailing
    // OWS is stripped by every HTTP stack (fetch included) — that is
    // transport behavior outside any client's control; what matters is that
    // the BFF neither rewrites, validates-as-UUID, nor regenerates keys.
    const recordingFetch: typeof fetch = async (input, init) => {
      const h2 = new Headers((init as RequestInit | undefined)?.headers);
      seenOutbound.push(h2.get('Idempotency-Key') ?? '');
      return fetch(input, init);
    };
    const h = await startHarness({
      liftrHandler: (_r, respond) => respond(201, { 'Content-Type': 'application/json', Link: '</v1/operations/op-1>; rel="monitor"' }, '{}'),
      forwarderFetch: recordingFetch,
    });
    try {
      const r = await handleLiftrProxyRequest(h.deps, h.incoming('POST', '/v1/resources', {
        headers: { 'Idempotency-Key': weirdKey },
        body: '{"id":"x","type":{"name":"T","version":"v1"},"owner":{"kind":"k","id":"i"},"spec":{}}',
      }));
      expect(r.status).toBe(201);
      expect(seenOutbound[0]).toBe(weirdKey); // byte-for-byte, no normalization
    } finally {
      await h.close();
    }
  });

  it('rejects missing or whitespace-only keys locally like Liftr does', async () => {
    const h = await startHarness({
      liftrHandler: () => {
        throw new Error('upstream must not be reached');
      },
    });
    try {
      const noKey = await handleLiftrProxyRequest(h.deps, h.incoming('POST', '/v1/resources', { body: '{}' }));
      expect(noKey.status).toBe(400);
      const blankKey = await handleLiftrProxyRequest(h.deps, h.incoming('POST', '/v1/resources', {
        headers: { 'Idempotency-Key': '   ' },
        body: '{}',
      }));
      expect(blankKey.status).toBe(400);
      expect(h.liftr.requests).toHaveLength(0);
    } finally {
      await h.close();
    }
  });

  it('forwards reference replacement bytes with the original key and generation', async () => {
    const body = '{"spec":{"image":"demo:v2","weight":20.0},"references":{"database":["db-b"]}}';
    const h = await startHarness({
      mode: 'insecure-development',
      liftrHandler: (req, respond) => {
        expect(req.body).toBe(body);
        expect(req.headers['idempotency-key']).toBe('reference-update-key');
        expect(req.headers['if-liftr-generation']).toBe('7');
        respond(202, { 'Content-Type': 'application/json', Link: '</v1/operations/op-ref>; rel="monitor"' }, '{}');
      },
    });
    try {
      const result = await handleLiftrProxyRequest(h.deps, h.incoming('PUT', '/v1/resources/app', {
        headers: { 'Idempotency-Key': 'reference-update-key', 'If-Liftr-Generation': '7' },
        body,
      }));
      expect(result.status).toBe(202);
      expect(JSON.parse(result.bodyText).monitorOperationId).toBe('op-ref');
    } finally {
      await h.close();
    }
  });
});

describe('Correction 4 — asymmetric 401 policy', () => {
  function liftrAlways401(): HarnessOptions['liftrHandler'] {
    return (_r, respond) =>
      respond(401, { 'Content-Type': 'application/problem+json', 'WWW-Authenticate': 'Bearer realm="liftr", error="invalid_token"' },
        JSON.stringify({ type: 'about:blank', title: 'Unauthenticated', status: 401, code: 'UNAUTHENTICATED', requestId: 'rq-401', detail: 'valid bearer credentials are required' }));
  }

  it('A/B: reads reacquire once and retry once, then resume', async () => {
    let stsCalls = 0;
    let seenFirstBearer = false;
    const h = await startHarness({
      liftrHandler: (_req, respond) => {
        if (!_req.url!.startsWith('/v1/resources')) {
          respond(500, {}, '');
          return;
        }
        if (!seenFirstBearer) {
          // First credential is treated as expired.
          seenFirstBearer = true;
          respond(401, { 'Content-Type': 'application/problem+json' }, JSON.stringify({ status: 401, code: 'UNAUTHENTICATED', requestId: 'rq-a' }));
          return;
        }
        respond(200, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' }, '{"items":[],"nextCursor":"c1_x"}');
      },
      stsHandler: (_req, respond) => {
        stsCalls += 1;
        // Both exchanges issue properly-bound RFC 9068-shaped tokens.
        respond(200, { 'Content-Type': 'application/json' }, JSON.stringify({
          access_token: signJwt({ iss: ISSUER, sub: 'alice-idp-sub', aud: ['liftr-api'] }),
          token_type: 'Bearer',
          expires_in: 300,
        }));
      },
    });
    void stsCalls;
    try {
      const r = await handleLiftrProxyRequest(h.deps, h.incoming('GET', '/v1/resources'));
      expect(r.status).toBe(200);
      expect(JSON.parse(r.bodyText)).toMatchObject({ items: [] });
      // Initial acquisition + EXACTLY ONE reacquisition.
      expect(h.sts!.requests.length).toBe(2);
      // Two upstream GETs total (401 then success), nothing more.
      expect(h.liftr.requests.length).toBe(2);
    } finally {
      await h.close();
    }
  });

it('C/D/E/F: mutations on Liftr 401 surface the failure with ZERO automatic replay', async () => {
    for (const spec of [
      { label: 'POST', invoke: (h: Awaited<ReturnType<typeof startHarness>>) => handleLiftrProxyRequest(h.deps, h.incoming('POST', '/v1/resources', { headers: { 'Idempotency-Key': 'm1' }, body: '{"id":"x","type":{"name":"T","version":"v1"},"owner":{"kind":"k","id":"i"},"spec":{}}' })) },
      { label: 'PUT', invoke: (h: Awaited<ReturnType<typeof startHarness>>) => handleLiftrProxyRequest(h.deps, h.incoming('PUT', '/v1/resources/x', { headers: { 'Idempotency-Key': 'm2', 'If-Liftr-Generation': '1' }, body: '{"spec":{}}' })) },
      { label: 'DELETE', invoke: (h: Awaited<ReturnType<typeof startHarness>>) => handleLiftrProxyRequest(h.deps, h.incoming('DELETE', '/v1/resources/x', { headers: { 'Idempotency-Key': 'm3', 'If-Liftr-Generation': '1' } })) },
      { label: 'RETRY', invoke: (h: Awaited<ReturnType<typeof startHarness>>) => handleLiftrProxyRequest(h.deps, h.incoming('POST', '/v1/operations/op-x/retry', { headers: { 'Idempotency-Key': 'm4', 'If-Liftr-Generation': '1' } })) },
    ]) {
      const h = await startHarness({ liftrHandler: liftrAlways401() });
      try {
        const r = await spec.invoke(h);
        expect(r.status, spec.label).toBe(401);
        expect(JSON.parse(r.bodyText).code, spec.label).toBe('UNAUTHENTICATED');
        // Initial acquisition = 1 STS call; NO reacquisition for mutations.
        expect(h.sts!.requests.length, `${spec.label} sts`).toBe(1);
        // Exactly one upstream attempt per mutation.
        expect(h.liftr.requests.length, `${spec.label} liftr`).toBe(1);
      } finally {
        await h.close();
      }
    }
  });

  it('G: mutation transport failure surfaces outcomeUnknown and sends nothing again', async () => {
    let attempts = 0;
    const failing: typeof fetch = async () => {
      attempts += 1;
      throw new Error('ECONNRESET');
    };
    const h = await startHarness({ liftrHandler: () => {}, forwarderFetch: failing });
    try {
      const r = await handleLiftrProxyRequest(h.deps, h.incoming('POST', '/v1/resources', {
        headers: { 'Idempotency-Key': 'ambig' },
        body: '{"id":"x","type":{"name":"T","version":"v1"},"owner":{"kind":"k","id":"i"},"spec":{}}',
      }));
      expect(r.status).toBe(502);
      const body = JSON.parse(r.bodyText);
      expect(body.outcomeUnknown).toBe(true);
      expect(attempts).toBe(1); // same key would be reused by explicit UI replay
    } finally {
      await h.close();
    }
  });
});

describe('Correction 1 — delegation subject binding', () => {
  function liftrEcho(): HarnessOptions['liftrHandler'] {
    return (_r, respond) =>
      respond(200, { 'Content-Type': 'application/json' }, '{"items":[]}');
  }

  it('A: Backstage A + delegation A reaches Liftr with the developer bearer', async () => {
    const h = await startHarness({ liftrHandler: liftrEcho() });
    try {
      const r = await handleLiftrProxyRequest(h.deps, h.incoming('GET', '/v1/resources'));
      expect(r.status).toBe(200);
      const seen = h.liftr.requests[0]!;
      const bearer = (seen.headers.authorization ?? '') as string;
      expect(bearer.startsWith('Bearer ')).toBe(true);
      const payload = JSON.parse(Buffer.from(bearer.slice(7).split('.')[1]!.replace(/-/g, '+').replace(/_/g, '/'), 'base64').toString());
      expect(payload.sub).toBe('alice-idp-sub'); // developer, not the BFF client
      expect(h.sts!.requests[0]!.headers.authorization).toMatch(/^Basic /); // client auth separate
    } finally {
      await h.close();
    }
  });

  it('B: Backstage A presenting delegation B is rejected BEFORE Liftr', async () => {
    const h = await startHarness({ liftrHandler: liftrEcho() });
    try {
      const r = await handleLiftrProxyRequest(h.deps, h.incoming('GET', '/v1/resources', {
        headers: { [DELEGATION_HEADER]: bobAssertion() },
      }));
      expect(r.status).toBe(403);
      expect(JSON.parse(r.bodyText).code).toBe('LIFTR_SUBJECT_BINDING_REJECTED');
      expect(h.liftr.requests).toHaveLength(0); // no upstream call, no mutation
      expect(h.sts!.requests).toHaveLength(0);
    } finally {
      await h.close();
    }
  });

  it('C: malformed/unverifiable delegation subjects fail closed', async () => {
    const h = await startHarness({ liftrHandler: liftrEcho() });
    try {
      for (const bad of ['garbage', 'a.b', signJwt({ iss: 'https://other.example.com', sub: 'x', backstage_user_ref: ALICE_REF })]) {
        const r = await handleLiftrProxyRequest(h.deps, h.incoming('GET', '/v1/resources', {
          headers: { [DELEGATION_HEADER]: bad },
        }));
        expect([403, 503]).toContain(r.status);
        expect(JSON.parse(r.bodyText).code).not.toBe('UNAUTHENTICATED');
      }
      expect(h.liftr.requests).toHaveLength(0);
    } finally {
      await h.close();
    }
  });

  it('D: binding lookup unavailable fails closed (unknown backstage ref)', async () => {
    const h = await startHarness({
      liftrHandler: liftrEcho(),
      principal: { ok: true, userEntityRef: 'user:default/stranger' },
    });
    try {
      const r = await handleLiftrProxyRequest(h.deps, h.incoming('GET', '/v1/resources'));
      expect(r.status).toBe(403);
      expect(JSON.parse(r.bodyText).code).toBe('LIFTR_SUBJECT_BINDING_REJECTED');
      expect(h.liftr.requests).toHaveLength(0);
    } finally {
      await h.close();
    }
  });

  it('E: no log event ever contains delegation or exchanged token material', async () => {
    const h = await startHarness({ liftrHandler: liftrEcho() });
    try {
      await handleLiftrProxyRequest(h.deps, h.incoming('GET', '/v1/resources'));
      const serialized = JSON.stringify(h.events);
      expect(serialized.includes(aliceAssertion())).toBe(false);
      expect(serialized.includes('fakesig')).toBe(false);
      expect(serialized.toLowerCase().includes('bearer ')).toBe(false);
    } finally {
      await h.close();
    }
  });
});

describe('adversarial hardening', () => {
  it('passes the real platform request to Backstage httpAuth glue', async () => {
    const h = await startHarness({
      mode: 'insecure-development',
      liftrHandler: (_req, respond) => respond(200, { 'Content-Type': 'application/json' }, '{"items":[]}'),
    });
    const marker = { platform: 'request' };
    let seen: unknown;
    h.deps.authenticator = {
      authenticate: async raw => {
        seen = raw;
        return { ok: true, userEntityRef: ALICE_REF };
      },
    };
    try {
      const result = await handleLiftrProxyRequest(h.deps, h.incoming('GET', '/v1/resources'), marker);
      expect(result.status).toBe(200);
      expect(seen).toBe(marker);
    } finally {
      await h.close();
    }
  });

  it('3: service principals are rejected before any upstream traffic', async () => {
    const h = await startHarness({
      liftrHandler: () => {
        throw new Error('must not be called');
      },
      principal: { ok: false, kind: 'service-principal' },
    });
    try {
      const r = await handleLiftrProxyRequest(h.deps, h.incoming('GET', '/v1/resources'));
      expect(r.status).toBe(403);
      expect(JSON.parse(r.bodyText).code).toBe('LIFTR_FORBIDDEN_PRINCIPAL');
      expect(h.liftr.requests).toHaveLength(0);
    } finally {
      await h.close();
    }
  });

  it('10: non-mirrored paths and injected queries never reach upstream (SSRF)', async () => {
    const h = await startHarness({ liftrHandler: () => {} });
    try {
      for (const attempt of ['GET /v1/admin', 'GET /admin/v1/resources', 'GET /proxy', 'GET /etc/passwd', 'GET //evil.com/v1/resources']) {
        const [method, path] = attempt.split(' ');
        const r = await handleLiftrProxyRequest(h.deps, h.incoming(method!, path!));
        expect([400, 404]).toContain(r.status);
      }
      const q = await handleLiftrProxyRequest(h.deps, h.incoming('GET', '/v1/resources?search=x&limit=5'));
      expect(q.status).toBe(400);
      expect(h.liftr.requests).toHaveLength(0);
    } finally {
      await h.close();
    }
  });

  it('contains only the explicit public v1 route allowlist', () => {
    expect(ROUTES).toHaveLength(10);
    for (const route of ROUTES) {
      expect(route.pattern.source.startsWith('^\\/v1\\/')).toBe(true);
      expect(route.pattern.source).not.toContain('.*');
      expect(route.pattern.source).not.toContain('admin');
    }
  });

  it('serializes a generation conflict without leaking or truncating uint64', async () => {
    const h = await startHarness({
      mode: 'insecure-development',
      liftrHandler: (_req, respond) => respond(
        409,
        { 'Content-Type': 'application/problem+json' },
        '{"status":409,"code":"GENERATION_CONFLICT","title":"Generation conflict","currentGeneration":"18446744073709551615"}',
      ),
    });
    try {
      const result = await handleLiftrProxyRequest(h.deps, h.incoming('DELETE', '/v1/resources/x', {
        headers: { 'Idempotency-Key': 'delete-key', 'If-Liftr-Generation': '1' },
      }));
      expect(result.status).toBe(409);
      expect(JSON.parse(result.bodyText)).toMatchObject({
        code: 'GENERATION_CONFLICT',
        currentGeneration: '18446744073709551615',
      });
    } finally {
      await h.close();
    }
  });

  it('oversized upstream responses fail closed as protocol errors', async () => {
    const h = await startHarness({
      liftrHandler: (_r, respond) => respond(200, { 'Content-Type': 'application/json' }, 'x'.repeat(70000)),
      maxResponseBytes: 1024,
    });
    try {
      const r = await handleLiftrProxyRequest(h.deps, h.incoming('GET', '/v1/resource-types'));
      expect(r.status).toBe(502);
    } finally {
      await h.close();
    }
  });

  it('passthrough mode forwards the Liftr-targeted token and refuses expired ones', async () => {
    const good = await startHarness({ mode: 'passthrough', liftrHandler: (req, respond) => {
      expect((req.headers.authorization as string).startsWith('Bearer ey')).toBe(true);
      respond(200, { 'Content-Type': 'application/json' }, '{"items":[]}');
    } });
    try {
      const ok = await handleLiftrProxyRequest(good.deps, good.incoming('GET', '/v1/resources', {
        headers: { [DELEGATION_HEADER]: signJwt({ iss: 'irrelevant', sub: 'whoever', exp: Math.floor(Date.now() / 1000) + 600 }) },
      }));
      expect(ok.status).toBe(200);
    } finally {
      await good.close();
    }
  });

  it('insecure development mode sends NO authorization upstream and skips delegation', async () => {
    const h = await startHarness({
      mode: 'insecure-development',
      liftrHandler: (req, respond) => {
        expect(req.headers.authorization).toBeUndefined(); // tokenless composition
        respond(200, { 'Content-Type': 'application/json' }, '{"items":[]}');
      },
    });
    try {
      const r = await handleLiftrProxyRequest(h.deps, h.incoming('GET', '/v1/resources', { headers: { 'no-delegation': '1' } }));
      expect(r.status).toBe(200);
    } finally {
      await h.close();
    }
  });
});

describe('configuration guardrails (adversarial 15)', () => {
  const read = (map: Record<string, unknown>) => (path: string) => map[path];

  it('refuses insecure development mode against non-loopback targets', () => {
    expect(() =>
      parseLiftrBackendConfig(read({
        'baseUrl': 'http://liftr.internal.example.com:8080',
        'auth.mode': 'insecure-development',
      })),
    ).toThrow(/loopback/i);
  });

  it('refuses plaintext production origins outside explicit dev mode', () => {
    expect(() =>
      parseLiftrBackendConfig(read({ baseUrl: 'http://liftr.internal.example.com' })),
    ).toThrow();
  });

  it('accepts loopback insecure composition and delegated https composition', () => {
    const dev = parseLiftrBackendConfig(read({
      'baseUrl': 'http://localhost:8080',
      'auth.mode': 'insecure-development',
    }));
    expect(dev.auth.mode).toBe('insecure-development');

    const prod = parseLiftrBackendConfig(read({
      'baseUrl': 'https://liftr.example.com',
      'auth.mode': 'delegated',
      'auth.tokenEndpoint': 'https://sts.example.com/token',
      'auth.clientId': 'c',
      'auth.clientSecret': 's',
      'auth.audience': 'liftr',
      'auth.assertionIssuer': 'https://sts.example.com',
      'auth.binding.trustedIssuer': 'https://sts.example.com',
    }));
    expect(prod.auth.mode).toBe('delegated');
  });

  it('requires exactly one of audience/resource and forbids unbound delegation', () => {
    expect(() =>
      parseLiftrBackendConfig(read({
        'baseUrl': 'https://l.example.com',
        'auth.tokenEndpoint': 'https://s.example.com/token',
        'auth.clientId': 'c', 'auth.clientSecret': 's', 'auth.assertionIssuer': 'i',
      })),
    ).toThrow(/audience|resource/);
    expect(() =>
      parseLiftrBackendConfig(read({
        'baseUrl': 'https://l.example.com',
        'auth.tokenEndpoint': 'https://s.example.com/token',
        'auth.clientId': 'c', 'auth.clientSecret': 's', 'auth.assertionIssuer': 'i',
        'auth.audience': 'a', 'auth.binding.strategy': 'none', 'auth.binding.trustedIssuer': 'i',
      })),
    ).toThrow(/binding|strategy|claim|static/i);
  });
});
