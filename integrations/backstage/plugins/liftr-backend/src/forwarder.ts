/**
 * Upstream forwarder: the only code that talks to Liftr.
 *
 * Security posture (M12 lessons ported):
 *   - pinned origin; callers pass pre-validated path+query only;
 *   - redirects refused outright (redirect: 'error');
 *   - bounded timeouts and response sizes;
 *   - Authorization generated exclusively by the credential provider —
 *     inbound Backstage credentials never reach this layer;
 *   - response headers reduced to an internal allowlist; Link/Location/
 *     WWW-Authenticate/Liftr-Generation never reach browsers.
 *
 * Correction 4 — asymmetric authentication-failure policy:
 *   - READS: at most ONE server-side reacquisition + ONE retry on Liftr 401
 *     when the provider supports it. Liftr re-authorizes every request, so
 *     revocation takes effect immediately on the fresh token.
 *   - MUTATIONS: NEVER retried or replayed after ANY HTTP response. A 401 is
 *     surfaced verbatim as an authentication failure; the user deliberately
 *     retries after re-establishing auth. Idempotency scoping is
 *     (PrincipalID, key): a refreshed credential could change namespaces, so
 *     automatic mutation replay is forbidden in M16.
 *
 * Outcome taxonomy keeps ambiguity honest:
 *   - definitive HTTP responses (any status, incl. 5xx problems) are
 *     responses — even for mutations;
 *   - connection-level failures on READS get exactly one immediate retry;
 *   - connection-level failures on MUTATIONS yield outcome-unknown so the UI
 *     can offer same-key replay; no key/body regeneration exists anywhere.
 */

import { BffError } from '@liftr/plugin-liftr-common';

export interface ForwardedHeaders {
  /** Raw Idempotency-Key bytes to forward unchanged (opaque, Correction 3). */
  idempotencyKey?: string;
  generationPrecondition?: string;
  correlationId: string;
}

export type ForwardPolicy = { kind: 'read' } | { kind: 'mutation' };

export interface ForwardRequest {
  method: 'GET' | 'POST' | 'PUT' | 'DELETE';
  /** Validated path beginning /v1/, including validated query string. */
  pathWithQuery: string;
  bodyText?: string;
  headers: ForwardedHeaders;
  bearerToken: string;
  policy: ForwardPolicy;
  /**
   * Present only for reads against providers supporting reacquisition.
   * Returns a fresh token; invoked at most once per request.
   */
  reacquire?: () => Promise<string>;
}

export interface UpstreamResponse {
  status: number;
  contentType?: string;
  bodyText: string;
  requestId?: string;
  correlationEcho?: string;
  cacheControlNoStore: boolean;
  idempotencyReplayed: boolean;
  /** Raw Link header, visible ONLY inside the BFF for monitor extraction. */
  linkHeader?: string;
  /** Raw Location header, internal only; never forwarded downstream. */
  locationHeader?: string;
}

export type UpstreamOutcome =
  | { type: 'response'; response: UpstreamResponse }
  | { type: 'upstream-error'; reason: 'unreachable' | 'timeout' }
  | { type: 'outcome-unknown' };

const TRANSIENT_STATUS = new Set([502, 503, 504]);

export class UpstreamForwarder {
  constructor(
    private readonly options: {
      requestTimeoutMs: number;
      maxResponseBytes: number;
    },
    private readonly fetchImpl: typeof fetch,
    private readonly buildUrl: (pathWithQuery: string) => string,
    private readonly log?: (fields: Record<string, string | number | boolean>) => void,
  ) {}

  async send(request: ForwardRequest): Promise<UpstreamOutcome> {
    // ---- First attempt --------------------------------------------------
    let first: Response;
    try {
      first = await this.rawFetch(request, request.bearerToken);
    } catch (e) {
      return this.onTransportFailure(request, e);
    }

    // ---- Transient upstream statuses: reads tolerate one extra try ------
    if (TRANSIENT_STATUS.has(first.status)) {
      const retried = await this.retryReadOrKeep(request, first);
      if (retried.type !== '__retry__') return retried;
      first = retried.response;
    }

    // ---- Correction 4: authentication asymmetry -------------------------
    if (
      first.status === 401 &&
      request.policy.kind === 'read' &&
      request.reacquire !== undefined
    ) {
      let refreshedToken: string;
      try {
        refreshedToken = await request.reacquire();
        this.log?.({ event: 'token_reacquired', correlationId: request.headers.correlationId });
      } catch {
        this.log?.({ event: 'reacquire_failed', correlationId: request.headers.correlationId });
        return { type: 'response', response: await this.toUpstreamResponse(first) };
      }
      try {
        const second = await this.rawFetch(
          { ...request, bearerToken: refreshedToken },
          refreshedToken,
        );
        return { type: 'response', response: await this.toUpstreamResponse(second) };
      } catch {
        // Refreshed attempt hit a transport error; the FIRST definitive 401
        // response remains the honest answer.
        return { type: 'response', response: await this.toUpstreamResponse(first) };
      }
    }

    return { type: 'response', response: await this.toUpstreamResponse(first) };
  }

  private async retryReadOrKeep(
    request: ForwardRequest,
    first: Response,
  ): Promise<{ type: '__retry__'; response: Response } | Exclude<UpstreamOutcome, { type: '__retry__' }>> {
    if (request.policy.kind !== 'read') {
      return { type: 'response', response: await this.toUpstreamResponse(first) };
    }
    try {
      const second = await this.rawFetch(request, request.bearerToken);
      return { type: 'response', response: await this.toUpstreamResponse(second) };
    } catch {
      return { type: 'response', response: await this.toUpstreamResponse(first) };
    }
  }

  private async onTransportFailure(
    request: ForwardRequest,
    error: unknown,
  ): Promise<UpstreamOutcome> {
    const timedOut =
      error instanceof Error && (error.name === 'TimeoutError' || error.name === 'AbortError');
    if (request.policy.kind === 'mutation') {
      this.log?.({
        event: 'mutation_transport_ambiguous',
        correlationId: request.headers.correlationId,
      });
      return { type: 'outcome-unknown' };
    }
    // Reads: exactly one immediate retry.
    try {
      const second = await this.rawFetch(request, request.bearerToken);
      return { type: 'response', response: await this.toUpstreamResponse(second) };
    } catch (second) {
      const timedOut2 =
        second instanceof Error &&
        (second.name === 'TimeoutError' || second.name === 'AbortError');
      return {
        type: 'upstream-error',
        reason: timedOut || timedOut2 ? 'timeout' : 'unreachable',
      };
    }
  }

  private async rawFetch(request: ForwardRequest, bearer: string): Promise<Response> {
    const headers: Record<string, string> = {};
    if (bearer !== '') headers['Authorization'] = `Bearer ${bearer}`;
    headers['X-Correlation-ID'] = request.headers.correlationId;
    if (request.headers.idempotencyKey !== undefined) {
      headers['Idempotency-Key'] = request.headers.idempotencyKey;
    }
    if (request.headers.generationPrecondition !== undefined) {
      headers['If-Liftr-Generation'] = request.headers.generationPrecondition;
    }
    if (request.bodyText !== undefined) {
      headers['Content-Type'] = 'application/json';
    }
    return this.fetchImpl(this.buildUrl(request.pathWithQuery), {
      method: request.method,
      redirect: 'error',
      signal: AbortSignal.timeout(this.options.requestTimeoutMs),
      headers,
      ...(request.bodyText !== undefined ? { body: request.bodyText } : {}),
    });
  }

  private async toUpstreamResponse(response: Response): Promise<UpstreamResponse> {
    const contentType = response.headers.get('content-type') ?? undefined;
    const requestId = response.headers.get('x-request-id') ?? undefined;
    const bodyText = await readBoundedBody(response, this.options.maxResponseBytes);
    const link = response.headers.get('link');
    const location = response.headers.get('location');
    return {
      status: response.status,
      ...(contentType !== undefined ? { contentType } : {}),
      bodyText,
      ...(requestId !== undefined ? { requestId } : {}),
      correlationEcho: response.headers.get('x-correlation-id') ?? undefined,
      cacheControlNoStore: (response.headers.get('cache-control') ?? '').includes('no-store'),
      idempotencyReplayed: response.headers.get('idempotency-replayed') === 'true',
      ...(link !== null ? { linkHeader: link } : {}),
      ...(location !== null ? { locationHeader: location } : {}),
    };
  }
}

async function readBoundedBody(response: Response, maxBytes: number): Promise<string> {
  const reader = response.body?.getReader();
  if (!reader) return '';
  const chunks: Uint8Array[] = [];
  let total = 0;
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    total += value.byteLength;
    if (total > maxBytes) {
      void reader.cancel();
      throw new BffError({
        code: 'LIFTR_PROTOCOL_ERROR',
        title: 'Liftr response too large',
        detail: 'the upstream response exceeded the configured size bound',
      });
    }
    chunks.push(value);
  }
  const merged = new Uint8Array(total);
  let offset = 0;
  for (const c of chunks) {
    merged.set(c, offset);
    offset += c.byteLength;
  }
  return new TextDecoder().decode(merged);
}
