/**
 * Foreground operation polling (bounded, visibility-aware).
 *
 * Polls the authoritative monitorOperationId returned by the BFF envelope —
 * never Resource.latestOperation. Pauses while the tab is hidden, backs off
 * gently, stops on terminal states, and always keeps a manual refresh
 * available.
 */

import { useCallback, useEffect, useRef, useState } from 'react';
import { Operation } from '@liftr/plugin-liftr-common';
import { LiftrFrontendClient } from '../api/client';

const BASE_INTERVAL_MS = 5_000;
const MAX_INTERVAL_MS = 10_000;
const SLOW_DOWN_AFTER = 6;
const AUTO_STOP_AFTER_POLLS = 120; // ~10-20 min foreground budget

export type MonitorStatus = 'idle' | 'polling' | 'succeeded' | 'failed' | 'canceled' | 'error';

export function useOperationMonitor(
  client: LiftrFrontendClient,
  monitorOperationId: string | undefined,
) {
  const [operation, setOperation] = useState<Operation | null>(null);
  const [status, setStatus] = useState<MonitorStatus>('idle');
  const [error, setError] = useState<string | null>(null);
  const pollCount = useRef(0);
  const hiddenRef = useRef(false);
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null);

  const fetchOnce = useCallback(async (): Promise<boolean> => {
    if (!monitorOperationId) return true;
    try {
      const op = await client.pollMonitor(monitorOperationId);
      setOperation(op);
      setError(null);
      if (op.state === 'Succeeded') setStatus('succeeded');
      else if (op.state === 'Failed') setStatus('failed');
      else if (op.state === 'Canceled') setStatus('canceled');
      else setStatus('polling');
      return ['Succeeded', 'Failed', 'Canceled'].includes(op.state);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
      setStatus('error');
      return true;
    }
  }, [client, monitorOperationId]);

  useEffect(() => {
    if (!monitorOperationId) {
      setStatus('idle');
      setOperation(null);
      return;
    }
    let disposed = false;
    pollCount.current = 0;

    const schedule = () => {
      if (disposed) return;
      const interval =
        pollCount.current < SLOW_DOWN_AFTER ? BASE_INTERVAL_MS : MAX_INTERVAL_MS;
      timer.current = setTimeout(tick, interval);
    };

    const tick = async () => {
      if (disposed) return;
      if (hiddenRef.current) {
        // Tab hidden: hold polling until visible again.
        schedule();
        return;
      }
      pollCount.current += 1;
      const terminal = await fetchOnce();
      if (disposed) return;
      if (terminal) return;
      if (pollCount.current >= AUTO_STOP_AFTER_POLLS) {
        // Stop auto-polling; manual refresh remains available.
        return;
      }
      schedule();
    };

    const onVisibility = () => {
      hiddenRef.current = document.visibilityState === 'hidden';
      if (!hiddenRef.current && timer.current === null) schedule();
    };
    document.addEventListener('visibilitychange', onVisibility);
    hiddenRef.current = document.visibilityState === 'hidden';

    void tick();

    return () => {
      disposed = true;
      if (timer.current !== null) clearTimeout(timer.current);
      document.removeEventListener('visibilitychange', onVisibility);
    };
  }, [monitorOperationId, fetchOnce]);

  return {
    operation,
    status,
    error,
    refresh: fetchOnce,
  };
}
