/**
 * Disposable host-application compatibility fixture (New Frontend System).
 *
 * Proves the Liftr frontend plugin is discoverable as an NFS feature in a
 * real Backstage app workspace: feature import, registration, route wiring,
 * and production bundling all execute through the standard @backstage/cli
 * app build. This is NOT a distributed portal.
 */

import { createApp } from '@backstage/frontend-defaults';
import { liftrFrontendPlugin } from '@liftr/plugin-liftr';

export default createApp({
  features: [liftrFrontendPlugin],
});
