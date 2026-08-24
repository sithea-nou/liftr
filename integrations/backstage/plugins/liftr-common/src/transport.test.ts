import { describe, expect, it } from 'vitest';
import { validateIdempotencyKey } from './idempotency';
import { validateGenerationPrecondition } from './generation';
import { newCorrelationId, validateCorrelationId } from './correlation';

describe('idempotency keys are opaque (Correction 3)', () => {
  it('accepts non-UUID keys exactly as Liftr does (adversarial 6)', () => {
    for (const key of [
      'order-fiscal-2026.07#01',
      'k',
      '0123456789abcdef-very-long-opaque-key-without-uuid-shape',
      'key with internal spaces',
      'ключ', // non-ASCII: Liftr imposes no charset restriction
    ]) {
      expect(validateIdempotencyKey(key), key).toEqual({ ok: true });
    }
  });

  it('rejects only what Liftr rejects: missing or whitespace-only', () => {
    expect(validateIdempotencyKey(undefined)).toEqual({ ok: false, reason: 'missing' });
    expect(validateIdempotencyKey('   ')).toEqual({ ok: false, reason: 'missing' });
    expect(validateIdempotencyKey('\t\n')).toEqual({ ok: false, reason: 'missing' });
  });

  it('does not normalize: validation returns no rewritten value', () => {
    // The API deliberately returns {ok:true} with NO value so callers can
    // only forward the original bytes.
    const r = validateIdempotencyKey('  padded-key  ');
    expect(r).toEqual({ ok: true });
    expect((r as { value?: string }).value).toBeUndefined();
  });

  it('bounds absurd inputs before proxying', () => {
    expect(validateIdempotencyKey('x'.repeat(5000))).toEqual({ ok: false, reason: 'too-large' });
  });
});

describe('generation preconditions mirror the server', () => {
  it('accepts concrete decimal uint64 > 0 including leading zeros', () => {
    expect(validateGenerationPrecondition('1')).toMatchObject({ ok: true, value: 1n });
    expect(validateGenerationPrecondition(' 007 ')).toMatchObject({ ok: true, value: 7n });
    expect(validateGenerationPrecondition(String(2n ** 64n - 1n))).toMatchObject({ ok: true });
  });

  it('refuses missing, zero, wildcards, and out-of-range values', () => {
    expect(validateGenerationPrecondition(undefined)).toMatchObject({ ok: false, reason: 'missing' });
    expect(validateGenerationPrecondition('')).toMatchObject({ ok: false, reason: 'missing' });
    expect(validateGenerationPrecondition('0')).toMatchObject({ ok: false, reason: 'malformed' });
    expect(validateGenerationPrecondition('*')).toMatchObject({ ok: false, reason: 'malformed' });
    expect(validateGenerationPrecondition('1.0')).toMatchObject({ ok: false, reason: 'malformed' });
    expect(validateGenerationPrecondition('-3')).toMatchObject({ ok: false, reason: 'malformed' });
    expect(validateGenerationPrecondition(String(2n ** 64n))).toMatchObject({
      ok: false,
      reason: 'malformed',
    });
  });
});

describe('correlation ids are bounded and printable', () => {
  it('validates safe values and generates UUID-shaped ones', () => {
    expect(validateCorrelationId('abc-DEF_123:45/x')).toEqual({ ok: true });
    const generated = newCorrelationId();
    expect(validateCorrelationId(generated)).toEqual({ ok: true });
    expect(generated).toMatch(/^[0-9a-f-]{36}$/);
  });

  it('rejects control characters, whitespace, and oversize values', () => {
    expect(validateCorrelationId('bad\nnewline').ok).toBe(false);
    expect(validateCorrelationId('has space').ok).toBe(false);
    expect(validateCorrelationId('x'.repeat(129)).ok).toBe(false);
  });
});
