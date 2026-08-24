/**
 * Delegation subject binding (Correction 1).
 *
 * The authenticated Backstage principal and the delegation assertion are two
 * SEPARATE trust inputs. The BFF must never accept "Backstage user A presents
 * an assertion for user B" and act on B's behalf while attributing the action
 * to A.
 *
 * Reference bound-user mode establishes an explicit trusted binding between
 * the Backstage userEntityRef and the delegation assertion's issuer+subject,
 * then verifies the EXCHANGED Liftr token carries exactly that subject before
 * any upstream call. Any failure is fail-closed:
 *
 *   A. Backstage A + delegation A  -> allowed
 *   B. Backstage A + delegation B  -> rejected before any Liftr mutation
 *   C. malformed / unverifiable delegation subject -> rejected
 *   D. binding lookup unavailable  -> rejected
 *
 * Two deployment strategies are supported; both rest on trusted sign-in
 * configuration, never on email or display-name string matching:
 *
 *  - `claim`: the trusted IdP embeds the Backstage userEntityRef in a
 *    dedicated assertion claim (configured name). Binding is established
 *    because Backstage sign-in resolves the same issuer+subject to that
 *    entity ref; the STS signature-verifies the assertion during exchange.
 *  - `static`: an explicit operator-managed map of
 *    backstageRef -> {issuer, subject}.
 *
 * `none` exists ONLY for passthrough deployments that explicitly declare the
 * Liftr token as the authoritative identity; such deployments must not claim
 * verified sameness with the Backstage principal (see docs/AUTHENTICATION.md).
 */

export interface SubjectIdentity {
  issuer: string;
  subject: string;
}

export type SubjectBindingConfig =
  | { strategy: 'claim'; claimName: string; trustedIssuer: string }
  | { strategy: 'static'; entries: ReadonlyArray<{ backstageRef: string } & SubjectIdentity> }
  | { strategy: 'none' };

/** Unverified decoded JWT payload — used for binding decisions only. */
export interface DecodedAssertion {
  issuer?: unknown;
  subject?: unknown;
  claims: Record<string, unknown>;
}

export interface ParsedDelegationToken {
  ok: true;
  value: DecodedAssertion & {
    /** Raw header/payload sizes for hygiene logging (never contents). */
    segmentCount: number;
  };
}
export interface ParsedDelegationFailure {
  ok: false;
  reason: 'malformed' | 'not-a-jwt' | 'oversized';
}

const MAX_ASSERTION_BYTES = 8192; // mirrors Liftr's bearer ceiling
const MAX_CLAIM_BYTES = 64 * 1024;

/**
 * Decode a JWT-shaped delegation token WITHOUT verification. Signature
 * verification happens at the STS during exchange and at Liftr afterwards;
 * this decode feeds only binding checks, which fail closed on anything
 * malformed.
 */
export function decodeJwtPayloadUnverified(
  token: string,
): ParsedDelegationToken | ParsedDelegationFailure {
  if (new TextEncoder().encode(token).length > MAX_ASSERTION_BYTES) {
    return { ok: false, reason: 'oversized' };
  }
  const parts = token.split('.');
  if (parts.length !== 3) return { ok: false, reason: 'not-a-jwt' };
  const [headerB64, payloadB64] = parts as [string, string, string];
  if (headerB64 === '' || payloadB64 === '') return { ok: false, reason: 'malformed' };
  let payloadJson: string;
  try {
    const bytes = base64UrlDecode(payloadB64);
    if (bytes.length > MAX_CLAIM_BYTES) return { ok: false, reason: 'oversized' };
    payloadJson = new TextDecoder('utf-8', { fatal: true }).decode(bytes);
  } catch {
    return { ok: false, reason: 'malformed' };
  }
  let doc: unknown;
  try {
    doc = JSON.parse(payloadJson);
  } catch {
    return { ok: false, reason: 'malformed' };
  }
  if (typeof doc !== 'object' || doc === null || Array.isArray(doc)) {
    return { ok: false, reason: 'malformed' };
  }
  const claims = doc as Record<string, unknown>;
  return {
    ok: true,
    value: {
      issuer: claims['iss'],
      subject: claims['sub'],
      claims,
      segmentCount: 3,
    },
  };
}

function base64UrlDecode(input: string): Uint8Array {
  const normalized = input.replace(/-/g, '+').replace(/_/g, '/');
  const padded = normalized + '='.repeat((4 - (normalized.length % 4)) % 4);
  const binary = atob(padded);
  const out = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) out[i] = binary.charCodeAt(i);
  return out;
}

function asString(v: unknown): v is string {
  return typeof v === 'string' && v.length > 0 && v.length <= 512;
}

export type BindingResolution =
  | { ok: true; expected: SubjectIdentity }
  | {
      ok: false;
      reason:
        | 'binding-strategy-unavailable'
        | 'binding-lookup-unavailable'
        | 'assertion-malformed'
        | 'issuer-mismatch'
        | 'subject-binding-mismatch';
    };

/**
 * Establish the trusted binding for a request. Returns the exact subject
 * identity the eventual Liftr token MUST carry.
 */
export function resolveSubjectBinding(
  config: SubjectBindingConfig,
  backstageUserEntityRef: string,
  assertionPayload: DecodedAssertion,
): BindingResolution {
  switch (config.strategy) {
    case 'claim': {
      if (!asString(assertionPayload.issuer)) {
        return { ok: false, reason: 'assertion-malformed' };
      }
      if (assertionPayload.issuer !== config.trustedIssuer) {
        return { ok: false, reason: 'issuer-mismatch' };
      }
      const claimed = assertionPayload.claims[config.claimName];
      if (!asString(claimed)) {
        return { ok: false, reason: 'binding-lookup-unavailable' };
      }
      if (claimed !== backstageUserEntityRef) {
        return { ok: false, reason: 'subject-binding-mismatch' };
      }
      if (!asString(assertionPayload.subject)) {
        return { ok: false, reason: 'assertion-malformed' };
      }
      return {
        ok: true,
        expected: { issuer: config.trustedIssuer, subject: assertionPayload.subject },
      };
    }
    case 'static': {
      const entry = config.entries.find(e => e.backstageRef === backstageUserEntityRef);
      if (!entry) {
        return { ok: false, reason: 'binding-lookup-unavailable' };
      }
      if (!asString(assertionPayload.issuer) || !asString(assertionPayload.subject)) {
        return { ok: false, reason: 'assertion-malformed' };
      }
      if (assertionPayload.issuer !== entry.issuer) {
        return { ok: false, reason: 'issuer-mismatch' };
      }
      if (assertionPayload.subject !== entry.subject) {
        return { ok: false, reason: 'subject-binding-mismatch' };
      }
      return { ok: true, expected: { issuer: entry.issuer, subject: entry.subject } };
    }
    case 'none':
      return { ok: false, reason: 'binding-strategy-unavailable' };
  }
}

export type ExchangedSubjectCheck =
  | { ok: true }
  | { ok: false; reason: 'exchanged-token-malformed' | 'exchanged-subject-mismatch' };

/**
 * Verify the EXCHANGED token (post-STS) still names exactly the bound
 * subject+issuer before it is used to call Liftr. Local decoding is
 * unverified by design — Liftr performs full RFC 9068 validation; this check
 * enforces binding consistency so a misbehaving STS cannot silently swap
 * identity.
 */
export function verifyExchangedSubject(
  exchangedToken: string,
  expected: SubjectIdentity,
): ExchangedSubjectCheck {
  const decoded = decodeJwtPayloadUnverified(exchangedToken);
  if (!decoded.ok) return { ok: false, reason: 'exchanged-token-malformed' };
  if (!asString(decoded.value.issuer) || !asString(decoded.value.subject)) {
    return { ok: false, reason: 'exchanged-token-malformed' };
  }
  if (
    decoded.value.issuer !== expected.issuer ||
    decoded.value.subject !== expected.subject
  ) {
    return { ok: false, reason: 'exchanged-subject-mismatch' };
  }
  return { ok: true };
}
