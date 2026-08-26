import { describe, expect, it } from 'vitest';
import {
  isProblemContentType,
  parseProblemBody,
  describeProblemForLog,
} from './problem';
import { encodeMutationEnvelope, decodeMutationEnvelope } from './envelope';
import { buildCreateResourceBody, buildUpdateResourceBody } from './mutationBodies';
import { validateResourceListQuery } from './query';

describe('problem sanitization', () => {
  it('passes documented fields and extensions through', () => {
    const body = JSON.stringify({
      type: 'https://liftr.dev/problems/generation-conflict',
      title: 'Generation Conflict',
      status: 409,
      code: 'GENERATION_CONFLICT',
      requestId: 'abc123',
      detail: 'the precondition does not match the current generation',
      currentGeneration: 5,
    });
    const r = parseProblemBody(body, 409, 'hdr-rid');
    expect(r).toMatchObject({
      kind: 'problem',
      problem: {
        code: 'GENERATION_CONFLICT',
        requestId: 'abc123',
        currentGeneration: 5n,
      },
    });
  });

  it('sanitizes violation lists with bounds (422)', () => {
    const violations = Array.from({ length: 30 }, (_, i) => ({
      path: `/storageGB/${i}`,
      keyword: 'type',
      message: 'must be integer',
    }));
    const body = JSON.stringify({
      title: 'Spec Invalid', status: 422, code: 'RESOURCE_SPEC_INVALID',
      requestId: 'r1', violations, truncated: true,
    });
    const r = parseProblemBody(body, 422);
    expect(r.kind).toBe('problem');
    if (r.kind !== 'problem') return;
    expect(r.problem.violations!.length).toBeLessThanOrEqual(20);
    expect(r.problem.truncated).toBe(true);
  });

  it('strips reflected hostile fields (adversarial 9)', () => {
    const body = JSON.stringify({
      title: 'x', status: 404, code: 'RESOURCE_NOT_FOUND', requestId: 'r1',
      authorization: 'Bearer stolen-token',
      stackTrace: 'at go.runtime...',
      diagnostics: 'pulumi output --show-secrets ...',
      detail: '<script>alert(1)</script>',
    });
    const r = parseProblemBody(body, 404);
    if (r.kind !== 'problem') throw new Error('expected problem');
    const text = JSON.stringify(r);
    expect(text.includes('stolen-token')).toBe(false);
    expect(text.includes('stackTrace')).toBe(false);
    // Detail survives as TEXT data; rendering stays text-only in React.
    expect(r.problem.detail).toBe('<script>alert(1)</script>');
  });

  it('collapses hostile bodies to opaque errors carrying only status+requestId', () => {
    for (const bad of ['not json', '[1,2]', '"str"', '42']) {
      const r = parseProblemBody(bad, 404, 'req-x');
      expect(r).toEqual({ kind: 'opaque', status: 404, requestId: 'req-x' });
    }
  });

  it('detects problem media types by parsed media type', () => {
    expect(isProblemContentType('application/problem+json')).toBe(true);
    expect(isProblemContentType('application/problem+json; charset=utf-8')).toBe(true);
    expect(isProblemContentType('application/json')).toBe(false);
    expect(isProblemContentType(undefined)).toBe(false);
  });

  it('log summaries never include detail text (no wholesale problems in logs)', () => {
    const r = parseProblemBody(
      JSON.stringify({ title: 't', status: 409, code: 'OPERATION_ACTIVE', requestId: 'r9', detail: 'SECRETDETAIL' }),
      409,
    );
    if (r.kind !== 'problem') throw new Error('bad');
    const line = describeProblemForLog(r.problem);
    expect(line).not.toContain('SECRETDETAIL');
    expect(line).toContain('code=OPERATION_ACTIVE');
  });
});

describe('mutation envelope contract (Correction 2)', () => {
  it('encodes and decodes preserving numeric lexemes inside data', () => {
    const upstream = '{"generation":3,"spec":{"storageGB":20.0}}';
    const enc = encodeMutationEnvelope(upstream, 'op-child');
    expect(enc.ok).toBe(true);
    if (!enc.ok) return;
    expect(enc.text).toBe('{"data":{"generation":3,"spec":{"storageGB":20.0}},"monitorOperationId":"op-child"}');
    const dec = decodeMutationEnvelope(enc.text);
    expect(dec.ok).toBe(true);
    if (dec.ok) expect(dec.value.monitorOperationId).toBe('op-child');
  });

  it('rejects non-envelope payloads on decode', () => {
    expect(decodeMutationEnvelope('{"data":{}}').ok).toBe(false);
    expect(decodeMutationEnvelope('"str"').ok).toBe(false);
  });
});

describe('mutation bodies preserve numeric representation (adversarial 13)', () => {
  it('create splices the spec verbatim: 20.0 stays 20.0, 20 stays 20', () => {
    const r = buildCreateResourceBody({
      id: 'orders-db',
      typeName: 'PostgreSQLDatabase',
      typeVersion: 'v2',
      ownerKind: 'team',
      ownerId: 'payments',
      specText: '{ "version": "17", "storageGB": 20.0, "highAvailability": true }',
    });
    expect(r.ok).toBe(true);
    if (!r.ok) return;
    expect(r.bodyText).toContain('"storageGB":20.0');
    // The exact same editor content typed without the decimal:
    const r2 = buildCreateResourceBody({
      id: 'orders-db', typeName: 'PostgreSQLDatabase', typeVersion: 'v2',
      ownerKind: 'team', ownerId: 'payments',
      specText: '{ "version": "17", "storageGB": 20, "highAvailability": true }',
    });
    if (!r2.ok) throw new Error('broken');
    expect(r2.bodyText).toContain('"storageGB":20,');
    expect(r2.bodyText).not.toContain('20.0');
  });

  it('update wraps full replacement spec verbatim', () => {
    const r = buildUpdateResourceBody('{"storageGB":50}', '{"database":["db-b"]}');
    expect(r.ok).toBe(true);
    if (r.ok) expect(r.bodyText).toBe('{"spec":{"storageGB":50},"references":{"database":["db-b"]}}');
  });

  it('create carries references without normalizing spec numbers', () => {
    const r = buildCreateResourceBody({
      id: 'app', typeName: 'App', typeVersion: 'v1', ownerKind: 'team', ownerId: 'demo',
      specText: '{"weight":20.0}', referencesText: '{"database":["db-a"]}',
    });
    expect(r.ok).toBe(true);
    if (r.ok) expect(r.bodyText).toBe('{"id":"app","type":{"name":"App","version":"v1"},"owner":{"kind":"team","id":"demo"},"spec":{"weight":20.0},"references":{"database":["db-a"]}}');
  });

  it('rejects non-object specs and unsafe identifiers before any network call', () => {
    expect(buildUpdateResourceBody('[1]').ok).toBe(false);
    expect(buildUpdateResourceBody('not json').ok).toBe(false);
    for (const badId of ['a b', 'a/b', '', 'a\nb']) {
      expect(
        buildCreateResourceBody({
          id: badId, typeName: 'T', typeVersion: 'v1', ownerKind: 'k', ownerId: 'i',
          specText: '{}',
        }).ok,
        JSON.stringify(badId),
      ).toBe(false);
    }
  });
});

describe('M15 query validation', () => {
  it('accepts documented filters and serializes stably', () => {
    const sp = new URLSearchParams('ownerKind=team&ownerId=payments&type=PostgreSQLDatabase&version=v2&state=Ready&includeDeleted=false&limit=50');
    const r = validateResourceListQuery(sp);
    expect(r.ok).toBe(true);
  });

  it('enforces pairing, contradiction, unknown-key, and bound rules', () => {
    expect(validateResourceListQuery(new URLSearchParams('ownerKind=team')).ok).toBe(false);
    expect(validateResourceListQuery(new URLSearchParams('state=Deleted')).ok).toBe(false);
    expect(validateResourceListQuery(new URLSearchParams('search=x')).ok).toBe(false);
    expect(validateResourceListQuery(new URLSearchParams('limit=101')).ok).toBe(false);
    expect(validateResourceListQuery(new URLSearchParams('limit=0')).ok).toBe(false);
    expect(validateResourceListQuery(new URLSearchParams('version=v2')).ok).toBe(false);
  });
});
