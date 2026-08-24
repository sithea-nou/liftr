/**
 * Passthrough credential provider.
 *
 * For deployments whose authorization server issues RFC 9068 access tokens
 * with the Liftr audience directly to the Backstage SPA session. The BFF
 * performs hygiene checks only (shape, size, expiry) and forwards verbatim;
 * Liftr remains the sole validator (M11 unchanged).
 *
 * IDENTITY SEMANTICS — documented distinction required by Correction 1:
 * this mode does NOT verify that the Backstage principal and the Liftr
 * principal are the same person. The Liftr token is the authoritative user
 * identity; logs and errors must never claim bound-user equivalence.
 */

import { decodeJwtPayloadUnverified } from '@liftr/plugin-liftr-common';
import {
  AcquiredCredential,
  DelegatedCredentialRequest,
  LiftrCredentialProvider,
} from './provider';
import { CredentialLogSink } from './tokenExchange';

export class PassthroughCredentialProvider implements LiftrCredentialProvider {
  readonly identityAuthority = 'passthrough-liftr-token' as const;
  readonly supportsServerSideReacquisition = false as const;

  constructor(private readonly log: CredentialLogSink, private readonly now: () => number = () => Date.now()) {}

  async acquire(
    request: DelegatedCredentialRequest,
    ctx: { correlationId: string },
  ): Promise<AcquiredCredential> {
    const decoded = decodeJwtPayloadUnverified(request.delegationAssertion);
    if (!decoded.ok) {
      this.log.event({ event: 'passthrough_rejected', reason: `assertion_${decoded.reason}`, correlationId: ctx.correlationId });
      throw new Error('delegation assertion is malformed'); // mapped by routes to LIFTR_REQUEST_INVALID
    }
    // Hygiene-only expiry glance; Liftr enforces exp authoritatively.
    const exp = decoded.value.claims['exp'];
    if (
      typeof exp === 'number' &&
      exp * 1000 <= this.now()
    ) {
      this.log.event({ event: 'passthrough_expired', correlationId: ctx.correlationId });
      throw new Error('delegation assertion has expired');
    }
    this.log.event({ event: 'token_passthrough', correlationId: ctx.correlationId });
    return {
      token: request.delegationAssertion,
      expiresAtEpochMs:
        typeof exp === 'number' ? exp * 1000 : this.now() + 60_000,
    };
  }
}
