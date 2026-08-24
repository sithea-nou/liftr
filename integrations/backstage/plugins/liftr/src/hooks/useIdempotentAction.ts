/**
 * Idempotent mutation action state (Correction 3 + ambiguous-transport UX).
 *
 * One cryptographically random key per NEW logical user action. Every
 * internal retry of the same logical action reuses the same key with the
 * same body bytes and the same generation. When the BFF reports
 * outcomeUnknown (connection died before a definitive Liftr response), the
 * UI offers an explicit same-key replay; starting over mints a NEW key.
 * Keys live in React state only — never persisted, never in URLs.
 */

import { useCallback, useRef, useState } from 'react';
import { newLogicalActionKey } from '@liftr/plugin-liftr-common';

export type ActionPhase = 'idle' | 'running' | 'admitted' | 'unknown-outcome' | 'failed';

export interface IdempotentAction {
  phase: ActionPhase;
  /** Key for the current/last logical action; stable across replays. */
  currentKey: string | null;
  begin(): string;
  markAdmitted(): void;
  markUnknownOutcome(): void;
  markFailed(): void;
  reset(): void;
}

export function useIdempotentAction(): IdempotentAction {
  const [phase, setPhase] = useState<ActionPhase>('idle');
  const keyRef = useRef<string | null>(null);
  const [, force] = useState(0);

  const sync = useCallback(() => force(n => n + 1), []);

  const begin = useCallback(() => {
    // A NEW explicit action always gets a fresh key.
    const key = newLogicalActionKey();
    keyRef.current = key;
    setPhase('running');
    return key;
  }, []);

  return {
    phase,
    currentKey: keyRef.current,
    begin,
    markAdmitted: () => {
      setPhase('admitted');
      sync();
    },
    markUnknownOutcome: () => {
      setPhase('unknown-outcome');
      sync();
    },
    markFailed: () => {
      setPhase('failed');
      sync();
    },
    reset: () => {
      keyRef.current = null;
      setPhase('idle');
      sync();
    },
  };
}
