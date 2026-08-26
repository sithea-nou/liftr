/**
 * Disposable host-backend compatibility fixture.
 *
 * The fixture also supports the loopback-only M21.6 demo with Backstage's
 * guest user provider. Production adopters supply their own auth composition.
 */

import { createBackend } from '@backstage/backend-defaults';
import { liftrPlugin } from '@liftr/plugin-liftr-backend';

const backend = createBackend();
backend.add(import('@backstage/plugin-auth-backend'));
backend.add(import('@backstage/plugin-auth-backend-module-guest-provider'));
backend.add(liftrPlugin);

void backend.start().catch(error => {
  // eslint-disable-next-line no-console
  console.error('host backend fixture failed to start', error);
  process.exit(1);
});
