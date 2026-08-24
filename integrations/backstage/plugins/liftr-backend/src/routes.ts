/**
 * Explicit finite route mirror + request pipeline.
 *
 * Exactly the Liftr /v1 routes M16 uses are mirrored; there is no wildcard
 * proxying and no client-selected upstream. The pipeline order is:
 *
 *   match route -> authenticate Backstage user -> correlation -> delegation
 *   header -> input validation (opaque key, generation, query, body) ->
 *   credential acquisition (subject binding inside) -> forward (Correction 4
 *   policy inside) -> response shaping (Correction 2 envelope on mutations;
 *   sanitized problem passthrough; verbatim JSON otherwise).
 *
 * The BFF never makes authorization decisions and never holds state.
 */

import {
  BffError,
  CORRELATION_HEADER,
  GENERATION_PRECONDITION_HEADER,
  IDEMPOTENCY_KEY_HEADER,
  LiftrProblem,
  MutationEnvelope,
  Origin,
  PROBLEM_MEDIA_TYPE,
  encodeMutationEnvelope,
  extractMonitorOperationId,
  isValidResourceTransportId,
  parseProblemBody,
  serializeResourceListQuery,
  validateCorrelationId,
  validateGenerationPrecondition,
  validateIdempotencyKey,
  validateResourceListQuery,
  newCorrelationId,
} from '@liftr/plugin-liftr-common';
import { LiftrBackendConfig } from './config';
import {
  DELEGATION_HEADER,
  LiftrCredentialProvider,
} from './credentials/provider';
import { ForwardRequest, UpstreamForwarder, UpstreamOutcome } from './forwarder';

export interface LoggerSink {
  event(fields: Record<string, string | number | boolean>): void;
  error(message: string, fields?: Record<string, string | number | boolean>): void;
}

export interface RequestAuthenticator {
  /**
   * Authenticate the incoming Backstage credential. Ordinary plugin endpoints
   * accept ONLY user principals; service principals are rejected. The raw
   * platform request is passed through so glue can call
   * httpAuth.credentials(req, { allow: ['user'] }).
   */
  authenticate(rawPlatformRequest?: unknown): Promise<
    { ok: true; userEntityRef: string } | { ok: false; kind: 'unauthenticated' | 'service-principal' }
  >;
}

export interface IncomingRequest {
  method: string;
  /** Decoded pathname of the mirrored Liftr path, e.g. /v1/resources/db. */
  path: string;
  query: URLSearchParams;
  /** Case-insensitive single header value. */
  header(name: string): string | undefined;
  /** Already size-bounded raw body text; empty string when absent. */
  bodyText(): string;
}

export interface HandlerResult {
  status: number;
  contentType: string;
  bodyText: string;
  headers: Record<string, string>;
}

export interface RouteDeps {
  config: LiftrBackendConfig;
  provider: LiftrCredentialProvider;
  forwarder: UpstreamForwarder;
  authenticator: RequestAuthenticator;
  log: LoggerSink;
}

interface RouteDef {
  method: string;
  pattern: RegExp;
  paramNames?: string[];
  kind: 'read' | 'mutation';
  requiresKey?: boolean;
  requiresGeneration?: boolean;
  hasBody?: boolean;
  listQuery?: 'resources' | 'history';
}

export const ROUTES: RouteDef[] = [
  { method: 'GET', pattern: /^\/v1\/resource-types$/, kind: 'read' },
  {
    method: 'GET',
    pattern: /^\/v1\/resource-types\/([^/]+)\/([^/]+)$/,
    paramNames: ['typeName', 'typeVersion'],
    kind: 'read',
  },
  { method: 'GET', pattern: /^\/v1\/resources$/, kind: 'read', listQuery: 'resources' },
  { method: 'POST', pattern: /^\/v1\/resources$/, kind: 'mutation', requiresKey: true, hasBody: true },
  { method: 'GET', pattern: /^\/v1\/resources\/([^/]+)$/, paramNames: ['resourceId'], kind: 'read' },
  {
    method: 'PUT',
    pattern: /^\/v1\/resources\/([^/]+)$/,
    paramNames: ['resourceId'],
    kind: 'mutation',
    requiresKey: true,
    requiresGeneration: true,
    hasBody: true,
  },
  {
    method: 'DELETE',
    pattern: /^\/v1\/resources\/([^/]+)$/,
    paramNames: ['resourceId'],
    kind: 'mutation',
    requiresKey: true,
    requiresGeneration: true,
  },
  {
    method: 'GET',
    pattern: /^\/v1\/resources\/([^/]+)\/operations$/,
    paramNames: ['resourceId'],
    kind: 'read',
    listQuery: 'history',
  },
  { method: 'GET', pattern: /^\/v1\/operations\/([^/]+)$/, paramNames: ['operationId'], kind: 'read' },
  {
    method: 'POST',
    pattern: /^\/v1\/operations\/([^/]+)\/retry$/,
    paramNames: ['sourceOperationId'],
    kind: 'mutation',
    requiresKey: true,
    requiresGeneration: true,
  },
];

const NAME_SAFE = /^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$/;

function bff(status: number, body: unknown, headers: Record<string, string> = {}): HandlerResult {
  return { status, contentType: 'application/json; charset=utf-8', bodyText: JSON.stringify(body), headers };
}

function bffErrorResult(
  status: number,
  err: BffError,
  extraHeaders: Record<string, string> = {},
): HandlerResult {
  return bff(status, err.body, extraHeaders);
}

export async function handleLiftrProxyRequest(
  deps: RouteDeps,
  req: IncomingRequest,
  rawPlatformRequest?: unknown,
): Promise<HandlerResult> {
  try {
    return await pipeline(deps, req, rawPlatformRequest);
  } catch (e) {
    if (e instanceof BffError) {
      const status = mapBffStatus(e);
      return bffErrorResult(status, e, e.body.correlationId ? { [CORRELATION_HEADER]: e.body.correlationId } : {});
    }
    // Unknown internal failure: opaque, no token/spec material.
    deps.log.error('internal bff failure', { reason: e instanceof Error ? e.name : 'unknown' });
    return bff(500, {
      code: 'LIFTR_PROTOCOL_ERROR',
      title: 'Internal integration failure',
      detail: 'the Backstage Liftr integration failed internally',
    });
  }
}

function mapBffStatus(e: BffError): number {
  switch (e.body.code) {
    case 'LIFTR_REQUEST_INVALID':
      return 400;
    case 'LIFTR_FORBIDDEN_PRINCIPAL':
      return 403;
    case 'LIFTR_SUBJECT_BINDING_REJECTED':
      return 403;
    case 'LIFTR_AUTHENTICATION_REQUIRED':
      return 401;
    case 'LIFTR_DELEGATION_UNAVAILABLE':
      return 503;
    case 'LIFTR_UPSTREAM_TIMEOUT':
      return 504;
    case 'LIFTR_UPSTREAM_UNREACHABLE':
    case 'LIFTR_PROTOCOL_ERROR':
      return 502;
    default:
      return 502;
  }
}

async function pipeline(
  deps: RouteDeps,
  req: IncomingRequest,
  rawPlatformRequest?: unknown,
): Promise<HandlerResult> {
  const { config, log } = deps;

  // 1. Route matching — explicit mirror only.
  let matched: { route: RouteDef; params: Record<string, string> } | null = null;
  for (const route of ROUTES) {
    if (route.method !== req.method.toUpperCase()) continue;
    const m = route.pattern.exec(req.path);
    if (!m) continue;
    const params: Record<string, string> = {};
    (route.paramNames ?? []).forEach((name, i) => {
      params[name] = decodeURIComponent(m[i + 1]!);
    });
    matched = { route, params };
    break;
  }
  if (!matched) {
    throw new BffError({
      code: 'LIFTR_REQUEST_INVALID',
      title: 'Not found',
      detail: `no Liftr integration route mirrors ${req.method} ${req.path}`,
    });
  }
  const { route, params } = matched;

  for (const v of Object.values(params)) {
    if (!isValidResourceTransportId(v)) {
      throw new BffError({
        code: 'LIFTR_REQUEST_INVALID',
        title: 'Invalid path parameter',
        detail: 'path parameters must be single URL-segment-safe identifiers',
      });
    }
  }

  // 2. Backstage authentication — user principals only.
  const auth = await deps.authenticator.authenticate(req);
  if (!auth.ok) {
    if (auth.kind === 'unauthenticated') {
      throw new BffError({
        code: 'LIFTR_AUTHENTICATION_REQUIRED',
        title: 'Backstage sign-in required',
        detail: 'sign in to Backstage before using the Liftr integration',
      });
    }
    // Adversarial 3: a service credential must never reach Liftr through us.
    log.event({ event: 'service_principal_rejected', path: req.path });
    throw new BffError({
      code: 'LIFTR_FORBIDDEN_PRINCIPAL',
      title: 'Service principals are not accepted',
      detail: 'Liftr plugin endpoints act on behalf of signed-in users only',
    });
  }

  // 3. Correlation identity for this logical action hop.
  const inboundCorrelation = req.header(CORRELATION_HEADER);
  const cv = validateCorrelationId(inboundCorrelation, config.correlationMaxLength);
  if (!cv.ok && cv.reason === 'malformed') {
    throw new BffError({
      code: 'LIFTR_REQUEST_INVALID',
      title: 'Invalid correlation id',
      detail: `${CORRELATION_HEADER} must be printable ASCII without whitespace`,
    });
  }
  if (!cv.ok && cv.reason === 'too-long') {
    throw new BffError({
      code: 'LIFTR_REQUEST_INVALID',
      title: 'Invalid correlation id',
      detail: `${CORRELATION_HEADER} exceeds ${config.correlationMaxLength} characters`,
    });
  }
  const correlationId = inboundCorrelation ?? newCorrelationId();

  // 4. Delegation assertion (absent in insecure development mode).
  let delegationAssertion = '';
  if (config.auth.mode !== 'insecure-development') {
    const raw = req.header(DELEGATION_HEADER);
    if (raw === undefined || raw.trim() === '') {
      log.event({ event: 'delegation_missing', correlationId });
      throw new BffError({
        code: 'LIFTR_REQUEST_INVALID',
        title: 'Delegation assertion missing',
        detail:
          'this request cannot obtain Liftr access: no delegation assertion was supplied by the frontend',
        correlationId,
      });
    }
    delegationAssertion = raw;
  }

  // 5. Input validation mirroring Liftr's public rules exactly.

  // Opaque idempotency key (Correction 3): trim-check only, forward raw bytes.
  let rawKey: string | undefined;
  if (route.requiresKey) {
    const kv = validateIdempotencyKey(req.header(IDEMPOTENCY_KEY_HEADER));
    if (!kv.ok) {
      throw new BffError({
        code: 'LIFTR_REQUEST_INVALID',
        title: 'Idempotency-Key required',
        detail:
          kv.reason === 'missing'
            ? 'the Idempotency-Key header is required'
            : 'the Idempotency-Key header exceeds the transport bound',
        correlationId,
      });
    }
    rawKey = req.header(IDEMPOTENCY_KEY_HEADER)!; // original bytes preserved
  }

  // Generation precondition: forward the RAW header value; server trims.
  let rawGeneration: string | undefined;
  if (route.requiresGeneration) {
    const gv = validateGenerationPrecondition(req.header(GENERATION_PRECONDITION_HEADER));
    if (!gv.ok) {
      throw new BffError({
        code: 'LIFTR_REQUEST_INVALID',
        title: gv.reason === 'missing' ? 'Generation precondition required' : 'Invalid generation precondition',
        detail:
          gv.reason === 'missing'
            ? 'If-Liftr-Generation carrying a concrete generation is required (428 semantics)'
            : 'If-Liftr-Generation must be a concrete unsigned 64-bit decimal greater than zero',
        correlationId,
      });
    }
    rawGeneration = req.header(GENERATION_PRECONDITION_HEADER)!;
  }

  // List queries.
  let upstreamQuery = '';
  if (route.listQuery === 'resources') {
    const qv = validateResourceListQuery(req.query);
    if (!qv.ok) {
      throw new BffError({ code: 'LIFTR_REQUEST_INVALID', title: 'Invalid query', detail: qv.reason, correlationId });
    }
    upstreamQuery = serializeResourceListQuery(qv.query);
  } else if (route.listQuery === 'history') {
    const limit = req.query.get('limit');
    const cursor = req.query.get('cursor');
    if (limit !== null && !/^[1-9][0-9]*$/.test(limit)) {
      throw new BffError({ code: 'LIFTR_REQUEST_INVALID', title: 'Invalid limit', detail: 'limit must be 1..100', correlationId });
    }
    if (limit !== null && Number(limit) > 100) {
      throw new BffError({ code: 'LIFTR_REQUEST_INVALID', title: 'Invalid limit', detail: 'limit must be 1..100', correlationId });
    }
    if (cursor !== null && (cursor === '' || cursor.length > 64)) {
      throw new BffError({ code: 'LIFTR_REQUEST_INVALID', title: 'Invalid cursor', detail: 'cursor too long', correlationId });
    }
    const sp = new URLSearchParams();
    if (limit !== null) sp.set('limit', limit);
    if (cursor !== null) sp.set('cursor', cursor);
    upstreamQuery = sp.toString() === '' ? '' : `?${sp.toString()}`;
  }

  // Body handling.
  let bodyText: string | undefined;
  if (route.hasBody) {
    const text = req.bodyText();
    if (text.trim() === '') {
      throw new BffError({ code: 'LIFTR_REQUEST_INVALID', title: 'Body required', detail: 'a JSON document is required', correlationId });
    }
    if (new TextEncoder().encode(text).length > config.maxRequestBodyBytes) {
      throw new BffError({ code: 'LIFTR_REQUEST_INVALID', title: 'Body too large', detail: 'request body exceeds the configured bound', correlationId });
    }
    const ct = req.header('content-type');
    const media = ct?.split(';')[0]?.trim().toLowerCase() ?? '';
    if (media !== 'application/json') {
      throw new BffError({ code: 'LIFTR_REQUEST_INVALID', title: 'Unsupported media type', detail: 'Content-Type must be application/json', correlationId });
    }
    bodyText = text;
  }

  // 6. Credential acquisition (binding enforced inside delegated provider).
  let bearerToken = '';
  const acquire = async (): Promise<string> => {
    const cred = await deps.provider.acquire(
      { backstageUserEntityRef: auth.userEntityRef, delegationAssertion },
      { correlationId },
    );
    return cred.token;
  };
  try {
    bearerToken = await acquire();
  } catch (e) {
    if (e instanceof BffError) throw e;
    // Passthrough hygiene failures arrive as generic errors; sanitize them.
    log.event({ event: 'credential_failed', mode: config.auth.mode, correlationId });
    throw new BffError({
      code: 'LIFTR_DELEGATION_UNAVAILABLE',
      title: 'Unable to obtain access to Liftr',
      detail: 'delegated access to Liftr could not be established from your session',
      correlationId,
    });
  }

  // 7. Forward under Correction 4 policy.
  const forwardRequest: ForwardRequest = {
    method: req.method.toUpperCase() as ForwardRequest['method'],
    pathWithQuery: `${req.path}${upstreamQuery}`,
    ...(bodyText !== undefined ? { bodyText } : {}),
    headers: {
      ...(rawKey !== undefined ? { idempotencyKey: rawKey } : {}),
      ...(rawGeneration !== undefined ? { generationPrecondition: rawGeneration } : {}),
      correlationId,
    },
    bearerToken,
    policy: { kind: route.kind },
    ...(deps.provider.supportsServerSideReacquisition && route.kind === 'read' && config.auth.mode !== 'insecure-development'
      ? {
          reacquire: async () => {
            // Fresh acquisition; binding re-verified inside the provider.
            return await acquire();
          },
        }
      : {}),
  };

  const outcome: UpstreamOutcome = await deps.forwarder.send(forwardRequest);

  const baseHeaders: Record<string, string> = { [CORRELATION_HEADER]: correlationId };
  if (outcome.type === 'upstream-error') {
    throw new BffError({
      code: outcome.reason === 'timeout' ? 'LIFTR_UPSTREAM_TIMEOUT' : 'LIFTR_UPSTREAM_UNREACHABLE',
      title: 'Liftr unavailable',
      detail:
        outcome.reason === 'timeout'
          ? 'Liftr did not answer in time'
          : 'Liftr could not be reached',
      correlationId,
    });
  }
  if (outcome.type === 'outcome-unknown') {
    throw new BffError({
      code: 'LIFTR_UPSTREAM_UNREACHABLE',
      title: 'Outcome unknown',
      detail:
        'the connection failed before Liftr returned a result; you may safely replay this exact action with its original key',
      correlationId,
      outcomeUnknown: true,
    });
  }

  const upstream = outcome.response;
  if (upstream.requestId !== undefined) baseHeaders['X-Request-ID'] = upstream.requestId;

  // Mutations: authoritative monitor propagation (Correction 2).
  if (route.kind === 'mutation' && upstream.status >= 200 && upstream.status < 300) {
    const monitor = extractMonitorOperationId(upstream.linkHeader, config.origin as Origin);
    if (!monitor.ok) {
      log.event({
        event: 'protocol_failure',
        reason: `monitor_${monitor.reason}`,
        requestId: upstream.requestId ?? '',
        correlationId,
      });
      throw new BffError({
        code: 'LIFTR_PROTOCOL_ERROR',
        title: 'Missing authoritative operation reference',
        detail:
          'Liftr accepted the mutation but did not supply a valid monitor reference; refusing to guess which Operation to follow',
        correlationId,
        ...(upstream.requestId !== undefined ? { requestId: upstream.requestId } : {}),
        upstreamStatus: upstream.status,
      });
    }
    const enc = encodeMutationEnvelope(upstream.bodyText, monitor.operationId);
    if (!enc.ok) {
      log.event({ event: 'protocol_failure', reason: 'data-not-json', correlationId });
      throw new BffError({
        code: 'LIFTR_PROTOCOL_ERROR',
        title: 'Unreadable Liftr response',
        detail: 'the mutation response could not be interpreted',
        correlationId,
      });
    }
    return {
      status: upstream.status,
      contentType: 'application/json; charset=utf-8',
      bodyText: enc.text,
      headers: baseHeaders,
    };
  }

  // Problems: sanitize then re-serialize as problem+json.
  if (upstream.status >= 400 || isProblemMedia(upstream.contentType)) {
    const parsed = parseProblemBody(upstream.bodyText, upstream.status, upstream.requestId);
    let doc: LiftrProblem | { status: number; requestId?: string };
    if (parsed.kind === 'problem') doc = parsed.problem;
    else doc = { status: parsed.status, ...(parsed.requestId ? { requestId: parsed.requestId } : {}) };
    return {
      status: upstream.status,
      contentType: `${PROBLEM_MEDIA_TYPE}; charset=utf-8`,
      bodyText: JSON.stringify(doc),
      headers: baseHeaders,
    };
  }

  // Success reads: verbatim passthrough (numeric fidelity preserved).
  return {
    status: upstream.status,
    contentType: upstream.contentType ?? 'application/json; charset=utf-8',
    bodyText: upstream.bodyText,
    headers: baseHeaders,
  };
}

function isProblemMedia(ct: string | undefined): boolean {
  if (!ct) return false;
  return ct.split(';')[0]!.trim().toLowerCase() === PROBLEM_MEDIA_TYPE;
}

// Re-exported for glue/tests that need envelope typing context.
export type { MutationEnvelope };
