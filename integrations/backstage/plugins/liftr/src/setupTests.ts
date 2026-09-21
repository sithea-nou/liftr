/*
 * jsdom used by the Backstage CLI does not yet parse Backstage UI's modern
 * CSS nesting/@layer output. Material UI v4 also calls React's deprecated
 * findDOMNode compatibility path under React 18. Both are upstream test-only
 * diagnostics; keep every other console error visible.
 */
afterAll(() => {
  const state = globalThis as typeof globalThis & {
    __liftrOriginalConsoleError?: typeof console.error;
  };
  if (state.__liftrOriginalConsoleError) {
    console.error = state.__liftrOriginalConsoleError;
  }
});
