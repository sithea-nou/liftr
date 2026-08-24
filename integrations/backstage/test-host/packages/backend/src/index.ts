/**
 * Disposable host-backend compatibility fixture.
 *
 * Proves the Liftr BFF plugin registers through the standard backend system
 * (createBackend + backend.add) with current wiring types. Runtime startup is
 * intentionally not exercised here: a runnable host would also require the
 * auth-backend and database wiring that belongs to the adopting deployment,
 * not to this compile/build fixture. That boundary is documented in
 * integrations/backstage/README.md.
 */

import { createBackend } from '@backstage/backend-defaults';
import { liftrPlugin } from '@liftr/plugin-liftr-backend';

const backend = createBackend();
backend.add(liftrPlugin);

void backend.start().catch(error => {
  // eslint-disable-next-line no-console
  console.error('host backend fixture failed to start', error);
  process.exit(1);
});
