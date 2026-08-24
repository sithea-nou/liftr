import { describe, expect, it } from 'vitest';
import {
  DecodedAssertion,
  SubjectIdentity,
  SubjectBindingConfig,
  decodeJwtPayloadUnverified,
  resolveSubjectBinding,
  verifyExchangedSubject,
} from './binding';

function b64url(obj: unknown): string {
  return btoa(JSON.stringify(obj)).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

function jwt(payload: Record<string, unknown>): string {
  return `${b64url({ alg: 'RS256', typ: 'JWT' })}.${b64url(payload)}.sig`;
}

const ALICE = 'user:default/alice';
const BOB = 'user:default/bob';
const ISSUER = 'https://sts.example.com/realms/liftr';

const claimConfig: SubjectBindingConfig = {
  strategy: 'claim',
  claimName: 'backstage_user_ref',
  trustedIssuer: ISSUER,
};

function assertion(sub: string, ref: string, iss: string = ISSUER): string {
  return jwt({ iss, sub, aud: ['liftr'], backstage_user_ref: ref });
}

describe('delegation subject binding (Correction 1)', () => {
  it('test A: Backstage A + delegation A is allowed', () => {
    const decoded = decodeJwtPayloadUnverified(assertion('alice-idp-sub', ALICE));
    expect(decoded.ok).toBe(true);
    if (!decoded.ok) return;
    const r = resolveSubjectBinding(claimConfig, ALICE, decoded.value);
    expect(r).toEqual({
      ok: true,
      expected: { issuer: ISSUER, subject: 'alice-idp-sub' },
    });
  });

  it('test B: Backstage A + delegation for B is rejected before Liftr', () => {
    const decoded = decodeJwtPayloadUnverified(assertion('bob-idp-sub', BOB));
    if (!decoded.ok) throw new Error('fixture broken');
    const r = resolveSubjectBinding(claimConfig, ALICE, decoded.value);
    // The assertion claims bob; alice is authenticated. Fail closed.
    expect(r).toMatchObject({ ok: false, reason: 'subject-binding-mismatch' });
  });

  it('test B2: attacker strips the binding claim -> lookup unavailable', () => {
    const decoded = decodeJwtPayloadUnverified(jwt({ iss: ISSUER, sub: 'someone' }));
    if (!decoded.ok) throw new Error('fixture broken');
    expect(resolveSubjectBinding(claimConfig, ALICE, decoded.value)).toMatchObject({
      ok: false,
      reason: 'binding-lookup-unavailable',
    });
  });

  it('test C: malformed / unverifiable delegation subjects fail closed', () => {
    expect(decodeJwtPayloadUnverified('not-a-jwt')).toMatchObject({ ok: false });
    expect(decodeJwtPayloadUnverified('a.b')).toMatchObject({ ok: false });
    expect(decodeJwtPayloadUnverified(`${b64url({})}.!!!.x`)).toMatchObject({ ok: false });
    // JSON payload that is not an object:
    const weird = `${b64url({ alg: 'none' })}.${b64url([1, 2])}.s`;
    expect(decodeJwtPayloadUnverified(weird)).toMatchObject({ ok: false, reason: 'malformed' });
    // Oversized token:
    const big = 'e'.repeat(9000);
    expect(decodeJwtPayloadUnverified(`h.${big}.s`)).toMatchObject({ ok: false, reason: 'oversized' });
    // Missing issuer/sub fields:
    const noIss = decodeJwtPayloadUnverified(jwt({ sub: 'x' }));
    if (!noIss.ok) throw new Error('unexpected');
    expect(resolveSubjectBinding(claimConfig, ALICE, noIss.value)).toMatchObject({
      ok: false,
      reason: 'assertion-malformed',
    });
  });

  it('issuer mismatch fails closed', () => {
    const hostile = decodeJwtPayloadUnverified(assertion('alice-idp-sub', ALICE, 'https://evil.example.com'));
    if (!hostile.ok) throw new Error('fixture broken');
    expect(resolveSubjectBinding(claimConfig, ALICE, hostile.value)).toMatchObject({
      ok: false,
      reason: 'issuer-mismatch',
    });
  });

  it('static strategy: exact entry matching only', () => {
    const cfg: SubjectBindingConfig = {
      strategy: 'static',
      entries: [{ backstageRef: ALICE, issuer: ISSUER, subject: 'alice-idp-sub' }],
    };
    const good = decodeJwtPayloadUnverified(assertion('alice-idp-sub', ALICE));
    if (!good.ok) throw new Error('broken');
    expect(resolveSubjectBinding(cfg, ALICE, good.value)).toMatchObject({ ok: true });
    // Unknown backstage ref (test D).
    expect(resolveSubjectBinding(cfg, BOB, good.value)).toMatchObject({
      ok: false,
      reason: 'binding-lookup-unavailable',
    });
    // Right ref, wrong subject.
    const swapped = decodeJwtPayloadUnverified(assertion('bob-idp-sub', ALICE));
    if (!swapped.ok) throw new Error('broken');
    expect(resolveSubjectBinding(cfg, ALICE, swapped.value)).toMatchObject({
      ok: false,
      reason: 'subject-binding-mismatch',
    });
  });

  it("strategy 'none' is never valid in bound-user mode", () => {
    const noneCfg: SubjectBindingConfig = { strategy: 'none' };
    const good = decodeJwtPayloadUnverified(assertion('s', ALICE));
    if (!good.ok) throw new Error('broken');
    expect(resolveSubjectBinding(noneCfg, ALICE, good.value)).toMatchObject({
      ok: false,
      reason: 'binding-strategy-unavailable',
    });
  });

  it('exchanged-token subject must equal the bound identity', () => {
    const expected: SubjectIdentity = { issuer: ISSUER, subject: 'alice-idp-sub' };
    expect(verifyExchangedSubject(assertion('alice-idp-sub', ALICE), expected)).toEqual({ ok: true });
    // STS returned a different subject than bound:
    expect(
      verifyExchangedSubject(assertion('somebody-else', ALICE), expected),
    ).toMatchObject({ ok: false, reason: 'exchanged-subject-mismatch' });
    expect(verifyExchangedSubject('garbage', expected)).toMatchObject({
      ok: false,
      reason: 'exchanged-token-malformed',
    });
  });

  it('error surfaces are enums only — no token material can leak through reasons', () => {
    const decoded = decodeJwtPayloadUnverified(assertion('sub', BOB));
    if (!decoded.ok) throw new Error('broken');
    const r = resolveSubjectBinding(claimConfig, ALICE, decoded.value);
    if (r.ok) throw new Error('expected failure');
    expect(Object.values(r).some(v => typeof v === 'string' && v.includes('.'))).toBe(false);
  });

  it('decoded assertions expose claims without throwing on exotic content', () => {
    const d = decodeJwtPayloadUnverified(jwt({ iss: ISSUER, sub: 's', nested: { a: [1, 2] } }));
    expect(d.ok).toBe(true);
    if (d.ok) {
      expect((d.value as DecodedAssertion).claims['nested']).toEqual({ a: [1, 2] });
    }
  });
});
