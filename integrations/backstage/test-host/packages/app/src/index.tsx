/**
 * Disposable host-application compatibility fixture (New Frontend System).
 *
 * Proves the Liftr frontend plugin is discoverable as an NFS feature in a
 * real Backstage app workspace: feature import, registration, route wiring,
 * and production bundling all execute through the standard @backstage/cli
 * app build. It is also runnable as a loopback-only M21.6 demo host, but is
 * not a distributed or production portal.
 */

import { createApp } from '@backstage/frontend-defaults';
import { liftrFrontendPlugin } from '@liftr/plugin-liftr';
import ReactDOM from 'react-dom/client';

const app = createApp({
  features: [liftrFrontendPlugin],
});

ReactDOM.createRoot(document.getElementById('root')!).render(app.createRoot());
