/**
 * Client construction from Backstage APIs. The delegation assertion is
 * fetched per request from the configured auth adapter; in explicit insecure
 * development mode no delegation header is sent (BFF skips it too).
 */

import {
  configApiRef,
  discoveryApiRef,
  fetchApiRef,
  useApi,
} from '@backstage/core-plugin-api';
import { useMemo } from 'react';
import { LiftrFrontendClient } from '../api/client';
import { liftrAuthApiRef } from '../api/auth';

export function useLiftrClient(): LiftrFrontendClient {
  const fetchApi = useApi(fetchApiRef);
  const discoveryApi = useApi(discoveryApiRef);
  const configApi = useApi(configApiRef);
  const mode = configApi.getOptionalString('liftr.auth.mode') ?? 'delegated';
  // In insecure development the BFF needs no delegation; skip the lookup so
  // guest sessions work without an OAuth provider wired in.
  const auth =
    mode === 'insecure-development' ? undefined : useApi(liftrAuthApiRef);

  // Memoized deliberately: components place the client in effect
  // dependencies, so identity must be stable across renders.
  return useMemo(
    () =>
      new LiftrFrontendClient(fetchApi, {
        getBaseUrl: () => discoveryApi.getBaseUrl('liftr'),
        getDelegationAssertion: auth
          ? () => auth.getDelegationAssertion()
          : undefined,
      }),
    [fetchApi, discoveryApi, auth],
  );
}
