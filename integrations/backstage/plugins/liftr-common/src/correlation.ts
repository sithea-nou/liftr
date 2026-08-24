/**
 * X-Correlation-ID handling.
 *
 * One logical user action carries one correlation identifier spanning
 * browser -> BFF -> Liftr. Liftr echoes the value verbatim WITHOUT length or
 * content validation, so the BFF must validate and bound it before
 * forwarding. Correlation IDs are never identity, never authorization, and
 * never carry token or user data.
 */

export const CORRELATION_HEADER = 'X-Correlation-ID';

export const DEFAULT_CORRELATION_MAX_LENGTH = 128;

export type CorrelationValidation =
  | { ok: true }
  | { ok: false; reason: 'malformed' | 'too-long' };

/** Printable visible ASCII only: no whitespace, controls, DEL, or non-ASCII. */
const SAFE_CORRELATION = /^[\x21-\x7e]+$/;

export function validateCorrelationId(
  raw: string | undefined,
  maxLength = DEFAULT_CORRELATION_MAX_LENGTH,
): CorrelationValidation {
  if (raw === undefined) {
    return { ok: true }; // absence is fine; caller generates one
  }
  if (!SAFE_CORRELATION.test(raw)) {
    return { ok: false, reason: 'malformed' };
  }
  if (raw.length > maxLength) {
    return { ok: false, reason: 'too-long' };
  }
  return { ok: true };
}

/** Generate a fresh correlation identifier for a new logical user action. */
export function newCorrelationId(): string {
  const g = globalThis.crypto;
  if (g && typeof g.randomUUID === 'function') {
    return g.randomUUID();
  }
  // Deterministic fallback for exotic runtimes without randomUUID.
  const b = new Uint8Array(16);
  if (!g || typeof g.getRandomValues !== 'function') {
    throw new Error('no secure randomness available');
  }
  g.getRandomValues(b);
  b[6]! = ((b[6]! & 0x0f) | 0x40);
  b[8]! = ((b[8]! & 0x3f) | 0x80);
  const hex = Array.from(b, x => x.toString(16).padStart(2, '0')).join('');
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
}
