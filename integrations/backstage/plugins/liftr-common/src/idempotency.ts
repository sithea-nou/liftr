/**
 * Idempotency-Key handling (Correction 3).
 *
 * The key is an OPAQUE identifier. Liftr's actual public contract
 * (internal/api/http requireIdempotencyKey, mirrored here) is:
 *
 *   - surrounding whitespace is trimmed;
 *   - the trimmed value must be non-empty;
 *   - there are NO charset, length, or UUID restrictions.
 *
 * The BFF therefore must not redefine the contract as "UUID only". Accepted
 * values are forwarded byte-for-byte; the BFF never generates, rewrites, or
 * regenerates a key. Keys are identifiers, not credentials: possession of one
 * is never authorization (Liftr scopes replays per principal).
 */

export const IDEMPOTENCY_KEY_HEADER = 'Idempotency-Key';

/** Upper bound for hygiene only: far above any legitimate client key. */
export const MAX_KEY_TRANSPORT_BYTES = 4096;

export type IdempotencyKeyValidation =
  | { ok: true }
  | { ok: false; reason: 'missing' | 'too-large' };

/**
 * Validate an inbound key exactly as Liftr does. Returns no normalized value:
 * callers must forward the ORIGINAL header bytes untouched.
 */
export function validateIdempotencyKey(rawHeader: string | undefined): IdempotencyKeyValidation {
  if (rawHeader === undefined || rawHeader.trim() === '') {
    return { ok: false, reason: 'missing' };
  }
  // Byte-length check (UTF-8) as transport hygiene only; Liftr imposes none,
  // so this bound exists solely to reject absurd inputs before proxying.
  const bytes = new TextEncoder().encode(rawHeader).length;
  if (bytes > MAX_KEY_TRANSPORT_BYTES) {
    return { ok: false, reason: 'too-large' };
  }
  return { ok: true };
}

/**
 * Mint a fresh key for ONE NEW logical user action. Frontend-only helper;
 * each explicit user action gets exactly one key and every internal retry of
 * that action reuses it. Not used by the BFF, which never mints keys.
 */
export function newLogicalActionKey(): string {
  const g = globalThis.crypto;
  if (!g || typeof g.randomUUID !== 'function') {
    throw new Error('crypto.randomUUID unavailable');
  }
  return g.randomUUID();
}
