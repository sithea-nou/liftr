/**
 * Delegation credential plumbing for the frontend.
 *
 * The Liftr BFF requires a short-lived upstream delegation assertion from the
 * signed-in user's session (Correction 1). Which Backstage auth provider
 * supplies it is deployment-specific; this module defines the tiny API
 * surface plugins consume plus one ready-made adapter for generic OAuth2/OIDC
 * providers.
 *
 * The assertion is held in component/request scope only: it is sent per
 * request to the same-origin BFF, never persisted, never placed in URLs, and
 * never logged by plugin code.
 */

import {
  createApiRef,
  OAuthApi,
} from '@backstage/core-plugin-api';

export interface LiftrAuthApi {
  /**
   * Return the current delegation assertion for the signed-in identity.
   * Implementations should request minimal scopes and rely on the provider
   * session cache. Throws when no suitable provider session exists; callers
   * surface "Unable to obtain access to Liftr" and fail closed.
   */
  getDelegationAssertion(): Promise<string>;
}

export const liftrAuthApiRef = createApiRef<LiftrAuthApi>({
  id: 'plugin.liftr.auth',
});

/**
 * Adapter over a generic OAuthApi (oauth2 / oidc provider instances).
 * `scope` must name the minimal scope the deployment's STS requires for
 * token exchange toward Liftr.
 */
export function oauthLiftrAuth(oauth: OAuthApi, scope?: string): LiftrAuthApi {
  return {
    async getDelegationAssertion() {
      return await oauth.getAccessToken(scope ? [scope] : undefined, {
        optional: false,
      });
    },
  };
}
