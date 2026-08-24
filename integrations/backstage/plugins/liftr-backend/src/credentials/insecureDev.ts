/**
 * Insecure development credential provider.
 *
 * Composes with `LIFTR_AUTH_MODE=insecure` on a LITERAL-LOOPBACK Liftr only
 * (startup validation enforces both). Sends NO Authorization header upstream,
 * mirroring the CLI's tokenless-loopback development composition. Never
 * selectable for remote targets; never a fallback when delegation fails.
 */

import {
  AcquiredCredential,
  DelegatedCredentialRequest,
  LiftrCredentialProvider,
} from './provider';

export class InsecureDevelopmentCredentialProvider implements LiftrCredentialProvider {
  readonly identityAuthority = 'insecure-development' as const;
  readonly supportsServerSideReacquisition = false as const;

  async acquire(_request: DelegatedCredentialRequest): Promise<AcquiredCredential> {
    void _request;
    return { token: '', expiresAtEpochMs: Number.MAX_SAFE_INTEGER };
  }
}
