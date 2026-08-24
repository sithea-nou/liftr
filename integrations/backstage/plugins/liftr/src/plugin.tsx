import React from 'react';
/** Liftr frontend plugin — classic frontend system entry point. */

import {
  createPlugin,
  createRoutableExtension,
  createRouteRef,
} from '@backstage/core-plugin-api';

export const liftrRootRouteRef = createRouteRef({ id: 'plugin.liftr.root' });

export const liftrPlugin = createPlugin({
  id: 'liftr',
  routes: {
    root: liftrRootRouteRef,
  },
});

export const LiftrPage = liftrPlugin.provide(
  createRoutableExtension({
    name: 'LiftrPage',
    component: async () => {
      const m = await import('./Router');
      const View = m.LiftrRouter;
      return (props: Record<string, unknown>) => <View {...props} />;
    },
    mountPoint: liftrRootRouteRef,
  }),
);

export { oauthLiftrAuth, liftrAuthApiRef } from './api/auth';
export type { LiftrAuthApi } from './api/auth';
