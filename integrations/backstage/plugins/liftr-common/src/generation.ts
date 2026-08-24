/**
 * If-Liftr-Generation precondition validation, mirroring Liftr's transport
 * rules exactly (internal/api/http parseGenerationPrecondition):
 *   - missing header => PRECONDITION_REQUIRED (callers decide status);
 *   - after trimming, must parse as an unsigned 64-bit decimal > 0;
 *   - wildcards do not exist anywhere in v1.
 */

export const GENERATION_PRECONDITION_HEADER = 'If-Liftr-Generation';

export type GenerationPrecondition =
  | { ok: true; value: bigint }
  | { ok: false; reason: 'missing' | 'malformed' };

const UINT64_MAX = (1n << 64n) - 1n;

export function validateGenerationPrecondition(
  rawHeader: string | undefined,
): GenerationPrecondition {
  if (rawHeader === undefined || rawHeader.trim() === '') {
    return { ok: false, reason: 'missing' };
  }
  const value = rawHeader.trim();
  if (!/^[0-9]+$/.test(value)) {
    return { ok: false, reason: 'malformed' };
  }
  let parsed: bigint;
  try {
    parsed = BigInt(value);
  } catch {
    return { ok: false, reason: 'malformed' };
  }
  if (parsed === 0n || parsed > UINT64_MAX) {
    return { ok: false, reason: 'malformed' };
  }
  return { ok: true, value: parsed };
}
