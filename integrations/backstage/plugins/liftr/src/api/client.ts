/**
 * Frontend client for the Liftr BFF (/api/liftr).
 *
 * A narrow handwritten client over the stable BFF contract shared with
 * liftr-backend via @liftr/plugin-liftr-common. Responsibilities:
 *   - attach the Backstage credential (fetchApi) and delegation assertion;
 *   - decode mutation envelopes (monitorOperationId, Correction 2);
 *   - parse sanitized Liftr problems and BFF errors into typed failures.
 *
 * Numeric fidelity: response documents are parsed with the lossless parser,
 * never with JSON.parse + Number coercion.
 */

import { FetchApi } from '@backstage/core-plugin-api';
import {
  BffErrorBody,
  JsonValue,
  LiftrProblem,
  MutationEnvelope,
  Operation,
  OperationList,
  PROBLEM_MEDIA_TYPE,
  Resource,
  ResourceList,
  ResourceSummary,
  ResourceTypeDetail,
  ResourceTypeSummary,
  ValidatedResourceListQuery,
  decodeMutationEnvelope,
  isLosslessNumber,
  parseLosslessJson,
  parseOperation,
  parseOperationList,
  parseResourceDetail,
  parseResourceList,
  parseResourceTypeDetail,
  parseResourceTypeList,
} from '@liftr/plugin-liftr-common';

export class LiftrApiError extends Error {
  constructor(
    public readonly problem: LiftrProblem | null,
    public readonly bff: BffErrorBody | null,
    public readonly status: number,
    public readonly outcomeUnknown: boolean = false,
  ) {
    super(bff ? `${bff.code}: ${bff.detail}` : `Liftr request failed (${status})`);
    this.name = 'LiftrApiError';
  }
}

export interface ListResult<T> {
  items: T[];
  nextCursor?: string;
}

export class LiftrFrontendClient {
  constructor(
    private readonly fetchApi: FetchApi,
    private readonly options: {
      /** Supplies the delegation assertion per request (bound-user mode). */
      getDelegationAssertion?: () => Promise<string>;
      baseUrl?: string; // default /api/liftr
      getBaseUrl?: () => Promise<string>;
    },
  ) {}

  private async request(
    method: string,
    pathWithQuery: string,
    init: { bodyText?: string; idempotencyKey?: string; generation?: string } = {},
  ): Promise<{ status: number; contentType: string; text: string }> {
    const headers: Record<string, string> = {};
    if (
      this.options.getDelegationAssertion &&
      !(init as { skipDelegation?: boolean }).skipDelegation
    ) {
      // Failure here means "unable to obtain access to Liftr": fail closed.
      headers['X-Liftr-Delegation'] = await this.options.getDelegationAssertion();
    }
    if (init.idempotencyKey !== undefined) headers['Idempotency-Key'] = init.idempotencyKey;
    if (init.generation !== undefined) headers['If-Liftr-Generation'] = init.generation;
    if (init.bodyText !== undefined) headers['Content-Type'] = 'application/json';

    const baseUrl = this.options.getBaseUrl
      ? await this.options.getBaseUrl()
      : this.options.baseUrl ?? '/api/liftr';
    const res = await this.fetchApi.fetch(`${baseUrl}${pathWithQuery}`, {
      method,
      headers,
      ...(init.bodyText !== undefined ? { body: init.bodyText } : {}),
    });
    const contentType = res.headers.get('content-type') ?? 'application/json';
    const text = await res.text();
    if (!res.ok) throw await this.toError(res.status, contentType, text);
    return { status: res.status, contentType, text };
  }

  private async toError(status: number, contentType: string, text: string): Promise<LiftrApiError> {
    const media = contentType.split(';')[0]?.trim().toLowerCase();
    let doc: JsonValue | null = null;
    try {
      doc = parseLosslessJson(text);
    } catch {
      doc = null;
    }
    if (media === PROBLEM_MEDIA_TYPE && typeof doc === 'object' && doc !== null && !Array.isArray(doc)) {
      const o = doc as Record<string, JsonValue>;
      const problem: LiftrProblem = {
        status,
        code: typeof o['code'] === 'string' ? o['code'] : undefined,
        title: typeof o['title'] === 'string' ? o['title'] : undefined,
        detail: typeof o['detail'] === 'string' ? o['detail'] : undefined,
        requestId: typeof o['requestId'] === 'string' ? o['requestId'] : undefined,
        currentGeneration:
          isLosslessNumber(o['currentGeneration']) ? o['currentGeneration'].toBigInt() : undefined,
        truncated: typeof o['truncated'] === 'boolean' ? o['truncated'] : undefined,
        violations: Array.isArray(o['violations'])
          ? (o['violations']
              .map(v =>
                typeof v === 'object' && v !== null && !Array.isArray(v) && !isLosslessNumber(v)
                  ? {
                      path: String((v as Record<string, JsonValue>)['path'] ?? ''),
                      keyword: String((v as Record<string, JsonValue>)['keyword'] ?? ''),
                      message: String((v as Record<string, JsonValue>)['message'] ?? ''),
                    }
                  : null,
              )
              .filter((x): x is NonNullable<typeof x> => x !== null))
          : undefined,
      };
      return new LiftrApiError(problem, null, status);
    }
    const bff =
      typeof doc === 'object' && doc !== null && !Array.isArray(doc)
        ? (doc as unknown as BffErrorBody)
        : null;
    return new LiftrApiError(null, bff?.code ? bff : null, status, Boolean(bff?.outcomeUnknown));
  }

  private async envelope(text: string): Promise<MutationEnvelope> {
    const dec = decodeMutationEnvelope(text);
    if (!dec.ok) {
      throw new LiftrApiError(null, {
        code: 'LIFTR_PROTOCOL_ERROR',
        title: 'Protocol failure',
        detail: 'mutation response was not a valid monitor envelope',
      }, 502);
    }
    return dec.value;
  }

  // ---- Discovery ---------------------------------------------------------

  async listResourceTypes(): Promise<ListResult<ResourceTypeSummary>> {
    const r = await this.request('GET', '/v1/resource-types');
    const g = parseResourceTypeList(parseLosslessJson(r.text));
    if (!g.ok) throw new LiftrApiError(null, null, r.status);
    return { items: g.value.items };
  }

  async getResourceType(name: string, version: string): Promise<ResourceTypeDetail> {
    const r = await this.request('GET', `/v1/resource-types/${encodeURIComponent(name)}/${encodeURIComponent(version)}`);
    const g = parseResourceTypeDetail(parseLosslessJson(r.text));
    if (!g.ok) throw new LiftrApiError(null, null, r.status);
    return g.value;
  }

  // ---- Inventory ---------------------------------------------------------

  async listResources(query: ValidatedResourceListQuery = {}): Promise<ListResult<ResourceSummary>> {
    const sp = new URLSearchParams();
    if (query.limit !== undefined) sp.set('limit', String(query.limit));
    if (query.cursor !== undefined) sp.set('cursor', query.cursor);
    if (query.ownerKind !== undefined) sp.set('ownerKind', query.ownerKind);
    if (query.ownerId !== undefined) sp.set('ownerId', query.ownerId);
    if (query.type !== undefined) sp.set('type', query.type);
    if (query.version !== undefined) sp.set('version', query.version);
    if (query.state !== undefined) sp.set('state', query.state);
    if (query.includeDeleted !== undefined) sp.set('includeDeleted', String(query.includeDeleted));
    const qs = sp.toString();
    const r = await this.request('GET', `/v1/resources${qs ? `?${qs}` : ''}`);
    const g = parseResourceList(parseLosslessJson(r.text));
    if (!g.ok) throw new LiftrApiError(null, null, r.status);
    return { items: g.value.items, ...(g.value.nextCursor !== undefined ? { nextCursor: g.value.nextCursor } : {}) };
  }

  async getResource(id: string): Promise<Resource> {
    const r = await this.request('GET', `/v1/resources/${encodeURIComponent(id)}`);
    const g = parseResourceDetail(parseLosslessJson(r.text));
    if (!g.ok) throw new LiftrApiError(null, null, r.status);
    return g.value;
  }

  // ---- Operations --------------------------------------------------------

  async listOperations(resourceId: string, limit?: number, cursor?: string): Promise<ListResult<Operation>> {
    const sp = new URLSearchParams();
    if (limit !== undefined) sp.set('limit', String(limit));
    if (cursor !== undefined) sp.set('cursor', cursor);
    const qs = sp.toString();
    const r = await this.request('GET', `/v1/resources/${encodeURIComponent(resourceId)}/operations${qs ? `?${qs}` : ''}`);
    const g = parseOperationList(parseLosslessJson(r.text));
    if (!g.ok) throw new LiftrApiError(null, null, r.status);
    return { items: g.value.items, ...(g.value.nextCursor !== undefined ? { nextCursor: g.value.nextCursor } : {}) };
  }

  async getOperation(operationId: string): Promise<Operation> {
    const r = await this.request('GET', `/v1/operations/${encodeURIComponent(operationId)}`);
    const g = parseOperation(parseLosslessJson(r.text));
    if (!g.ok) throw new LiftrApiError(null, null, r.status);
    return g.value;
  }

  /** Poll an operation by its authoritative id from a mutation envelope. */
  async pollMonitor(monitorOperationId: string): Promise<Operation> {
    return this.getOperation(monitorOperationId);
  }

  // ---- Mutations (enveloped; Correction 2) -------------------------------

  async create(input: { bodyText: string; idempotencyKey: string }): Promise<MutationEnvelope> {
    const r = await this.request('POST', '/v1/resources', {
      bodyText: input.bodyText,
      idempotencyKey: input.idempotencyKey,
    });
    return this.envelope(r.text);
  }

  async update(input: {
    resourceId: string;
    bodyText: string;
    idempotencyKey: string;
    viewedGeneration: string;
  }): Promise<MutationEnvelope> {
    const r = await this.request('PUT', `/v1/resources/${encodeURIComponent(input.resourceId)}`, {
      bodyText: input.bodyText,
      idempotencyKey: input.idempotencyKey,
      generation: input.viewedGeneration,
    });
    return this.envelope(r.text);
  }

  async remove(input: {
    resourceId: string;
    idempotencyKey: string;
    viewedGeneration: string;
  }): Promise<MutationEnvelope> {
    const r = await this.request('DELETE', `/v1/resources/${encodeURIComponent(input.resourceId)}`, {
      idempotencyKey: input.idempotencyKey,
      generation: input.viewedGeneration,
    });
    return this.envelope(r.text);
  }

  async retry(input: {
    sourceOperationId: string;
    idempotencyKey: string;
    viewedGeneration: string;
  }): Promise<MutationEnvelope> {
    const r = await this.request('POST', `/v1/operations/${encodeURIComponent(input.sourceOperationId)}/retry`, {
      idempotencyKey: input.idempotencyKey,
      generation: input.viewedGeneration,
    });
    return this.envelope(r.text);
  }
}
