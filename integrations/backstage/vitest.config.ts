import { defineConfig } from 'vitest/config';
import { createRequire } from 'node:module';

const requireResolve = createRequire(import.meta.url);
// Resolve the CommonJS entry explicitly: under the jsdom environment Vite's
// browser condition would otherwise pick react-router-dom's UMD build, which
// expects script-tag globals.
const reactRouterDomEntry = requireResolve.resolve('react-router-dom');
let reactRouterEntry = '';
try {
  reactRouterEntry = requireResolve.resolve('react-router');
} catch {
  // react-router is a transitive dependency of react-router-dom; optional here.
}

export default defineConfig({
  resolve: {
    alias: [
      { find: /^react-router-dom$/, replacement: reactRouterDomEntry },
      ...(reactRouterEntry ? [{ find: /^react-router$/, replacement: reactRouterEntry }] : []),
    ],
  },
  test: {
    include: ['plugins/*/src/**/*.test.ts'],
    environment: 'node',
    reporters: 'default',
    passWithNoTests: false,
  },
});
