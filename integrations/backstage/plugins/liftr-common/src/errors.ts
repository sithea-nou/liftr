/**
 * BFF error taxonomy shared by backend and frontend.
 *
 * Upstream Liftr problems pass through as problem+json with sanitized fields;
 * everything the BFF itself fails on is expressed as one of these stable
 * codes so the frontend can distinguish:
 *   - Backstage session problems (handled by Backstage core, never here)
 *   - delegation acquisition failures (fail closed; no service fallback)
 *   - upstream transport failures
 *   - BFF protocol failures (e.g., mutation without a valid monitor link)
 */

export type BffErrorCode =
  | 'LIFTR_DELEGATION_UNAVAILABLE'
  | 'LIFTR_SUBJECT_BINDING_REJECTED'
  | 'LIFTR_UPSTREAM_UNREACHABLE'
  | 'LIFTR_UPSTREAM_TIMEOUT'
  | 'LIFTR_PROTOCOL_ERROR'
  | 'LIFTR_REQUEST_INVALID'
  | 'LIFTR_AUTHENTICATION_REQUIRED'
  | 'LIFTR_FORBIDDEN_PRINCIPAL';

export interface BffErrorBody {
  code: BffErrorCode;
  title: string;
  detail: string;
  correlationId?: string;
  /** Authoritative X-Request-ID from Liftr when one was received. */
  requestId?: string;
  /** HTTP status Liftr returned, when a response was received. */
  upstreamStatus?: number;
  /**
   * True for mutations whose transport failed before a definitive response;
   * the logical action MAY be safely replayed under the SAME idempotency key
   * by explicit user choice. Never set for authentication failures.
   */
  outcomeUnknown?: boolean;
}

export class BffError extends Error {
  readonly body: BffErrorBody;

  constructor(body: BffErrorBody) {
    super(`${body.code}: ${body.detail}`);
    this.name = 'BffError';
    this.body = body;
  }
}
