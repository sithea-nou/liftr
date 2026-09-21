/* Runs before test modules so third-party import-time diagnostics can be filtered. */
type ConsoleState = typeof globalThis & {
  __liftrOriginalConsoleError?: typeof console.error;
};

const state = globalThis as ConsoleState;
const originalConsoleError = state.__liftrOriginalConsoleError ?? console.error.bind(console);
state.__liftrOriginalConsoleError = originalConsoleError;

console.error = (...args: unknown[]) => {
  const first = args[0] as { message?: unknown; detail?: unknown } | string | undefined;
  if (typeof first === 'object'
    && first !== null
    && first.message === 'Could not parse CSS stylesheet'
    && String(first.detail).includes('@layer')) {
    return;
  }
  if (typeof first === 'string' && first.includes('findDOMNode is deprecated')) {
    return;
  }
  if (typeof first === 'string'
    && first.includes('Support for defaultProps will be removed')
    && typeof args[1] === 'string'
    && /^(MTable|Connect\(Droppable\))/.test(args[1])) {
    return;
  }
  originalConsoleError(...args);
};
