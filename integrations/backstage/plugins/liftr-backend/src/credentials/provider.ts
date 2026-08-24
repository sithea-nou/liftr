/**
 * LiftrCredentialProvider: the deliberately narrow credential abstraction.
 *
 * Three implementations exist and no general OAuth framework:
 *   - TokenExchangeCredentialProvider  (reference, RFC 8693, bound user)
 *   - PassthroughCredentialProvider    (IdP already issues Liftr tokens)
 *   - InsecureDevelopmentCredentialProvider (explicit local dev only)
 *
 * Results are MEMORY-ONLY for the duration of one request. No implementation
 * caches, persists, or logs token material.
 */

export const DELEGATION_HEADER = 'x-liftr-delegation';

export interface DelegatedCredentialRequest {
  /** Authenticated Backstage principal entity ref (e.g. user:default/alice). */
  backstageUserEntityRef: string;
  /** Raw delegation assertion supplied by the frontend. */
  delegationAssertion: string;
}

export interface AcquiredCredential {
  /** Bearer token for Liftr; empty string means "send no Authorization". */
  token: string;
  expiresAtEpochMs: number;
}

/**
 * How identity is established between Backstage and Liftr. Passthrough
 * deployments MUST NOT claim verified sameness with the Backstage principal
 * (documented distinction; see docs/AUTHENTICATION.md).
 */
export type IdentityAuthority =
  | 'bound-backstage-user'
  | 'passthrough-liftr-token'
  | 'insecure-development';

export interface LiftrCredentialProvider {
  readonly identityAuthority: IdentityAuthority;

  /**
   * Whether a fresh credential can be minted server-side when Liftr answers
   * 401 on a READ. True only for providers that perform an exchange with the
   * STS; passthrough depends on browser-held assertions and cannot refresh
   * here (Correction 4).
   */
  readonly supportsServerSideReacquisition: boolean;

  acquire(
    request: DelegatedCredentialRequest,
    ctx: { correlationId: string },
  ): Promise<AcquiredCredential>;
}
