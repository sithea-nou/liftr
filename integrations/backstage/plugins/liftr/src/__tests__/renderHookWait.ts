/**
 * waitFor with plain predicate polling plus a re-export of renderHook —
 * avoids depending on the exact async-utils export surface across
 * @testing-library versions.
 */

import { act, renderHook as rtlRenderHook } from '@testing-library/react';

export async function waitForHook<T>(
  predicate: () => void,
  opts: { timeout?: number; interval?: number } = {},
): Promise<void> {
  const timeout = opts.timeout ?? 5000;
  const interval = opts.interval ?? 100;
  const start = Date.now();
  let lastError: unknown = new Error('predicate never ran');
  for (;;) {
    try {
      predicate();
      return;
    } catch (e) {
      lastError = e;
    }
    if (Date.now() - start > timeout) {
      throw lastError instanceof Error ? lastError : new Error(String(lastError));
    }
    await act(async () => {
      await new Promise(r => setTimeout(r, interval));
    });
  }
}

export const renderHook = rtlRenderHook;
