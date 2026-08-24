import { describe, expect, it } from 'vitest';
import { Origin } from './origin';
import { extractMonitorOperationId, parseLinkHeader } from './monitor';

const LIFTR: Origin = { scheme: 'https', host: 'liftr.example.com', effectivePort: 443 };

describe('monitor link extraction (Correction 2)', () => {
  it('extracts the exact operation id from a relative monitor link', () => {
    const r = extractMonitorOperationId('</v1/operations/op-abc123>; rel="monitor"', LIFTR);
    expect(r).toEqual({ ok: true, operationId: 'op-abc123' });
  });

  it('accepts absolute same-origin references', () => {
    const r = extractMonitorOperationId(
      '<https://liftr.example.com/v1/operations/op-9f8e>; rel="monitor"',
      LIFTR,
    );
    expect(r).toEqual({ ok: true, operationId: 'op-9f8e' });
  });

  it('refuses attacker cross-origin monitor URLs (adversarial 11)', () => {
    for (const hostile of [
      '<https://evil.example.com/v1/operations/op-x>; rel="monitor"',
      '<http://liftr.example.com/v1/operations/op-x>; rel="monitor"', // scheme differs
      '<https://liftr.example.com:8443/v1/operations/op-x>; rel="monitor"', // port differs
    ]) {
      const r = extractMonitorOperationId(hostile, LIFTR);
      expect(r).toEqual({ ok: false, reason: 'invalid' });
    }
  });

  it('refuses non-operation paths and malformed ids', () => {
    for (const bad of [
      '</v1/resources/orders-db>; rel="monitor"',
      '</v1/operations/>; rel="monitor"',
      '</v1/operations/op%20x>; rel="monitor"',
      '</v1/operations/op-x?trace=1>; rel="monitor"',
      '</v1/operations/../secret>; rel="monitor"',
    ]) {
      expect(extractMonitorOperationId(bad, LIFTR), bad).toEqual({ ok: false, reason: 'invalid' });
    }
  });

  it('absent header is absent, not invalid', () => {
    expect(extractMonitorOperationId(null, LIFTR)).toEqual({ ok: false, reason: 'absent' });
    expect(extractMonitorOperationId('   ', LIFTR)).toEqual({ ok: false, reason: 'absent' });
    expect(extractMonitorOperationId('</v1/resources/x>; rel="self"', LIFTR)).toEqual({
      ok: false,
      reason: 'absent',
    });
  });

  it('ignores latestOperation-style hrefs even if present in Link (test A)', () => {
    // A poisoned server emits a "monitor" rel pointing at a resource path:
    // extraction must fail rather than yield a wrong identity.
    const r = extractMonitorOperationId(
      '</v1/resources/db/latestOperation>; rel="monitor"',
      LIFTR,
    );
    expect(r.ok).toBe(false);
  });

  it('first monitor entry wins among multiple links', () => {
    const r = extractMonitorOperationId(
      '</v1/resources/x>; rel="self", </v1/operations/op-first>; rel="monitor", </v1/operations/op-second>; rel="monitor"',
      LIFTR,
    );
    expect(r).toEqual({ ok: true, operationId: 'op-first' });
  });

  it('parses quoted parameters containing separators', () => {
    const links = parseLinkHeader('</v1/operations/op-1>; rel="monitor"; title="ops, x"');
    expect(links).toHaveLength(1);
    expect(links[0]!.rel).toBe('monitor');
  });
});
