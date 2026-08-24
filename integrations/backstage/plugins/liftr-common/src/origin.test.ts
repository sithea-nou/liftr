import { describe, expect, it } from 'vitest';
import { Origin, parseOrigin } from './origin';

const HTTPS = (s: string) => parseOrigin(s).ok ? (parseOrigin(s) as { ok: true; origin: Origin }).origin : undefined;

describe('origin validation (M12 port)', () => {
  it('accepts clean https origins', () => {
    const r = parseOrigin('https://liftr.example.com');
    expect(r.ok).toBe(true);
    if (r.ok) expect(r.origin.effectivePort).toBe(443);
  });

  it('normalizes explicit default ports', () => {
    const a = HTTPS('https://liftr.example.com');
    const b = HTTPS('https://liftr.example.com:443');
    expect(a && b && `${a.host}:${a.effectivePort}` === `${b.host}:${b.effectivePort}`).toBe(true);
  });

  it('rejects userinfo, query, fragment, and any path', () => {
    for (const bad of [
      'https://user:pass@liftr.example.com',
      'https://liftr.example.com/api',
      'https://liftr.example.com?x=1',
      'https://liftr.example.com#frag',
      'ftp://liftr.example.com',
    ]) {
      expect(parseOrigin(bad).ok, bad).toBe(false);
    }
  });

  it('plaintext http requires loopback literal AND explicit dev allowance', () => {
    // Without allowance: loopback plaintext still refused (strict default).
    expect(parseOrigin('http://127.0.0.1:8080').ok).toBe(false);
    // With allowance: literals accepted...
    for (const okHost of ['http://localhost:8080', 'http://127.0.0.1:8080', 'http://[::1]:8080', 'http://127.250.1.9']) {
      expect(parseOrigin(okHost, { allowInsecureLoopback: true }).ok, okHost).toBe(true);
    }
    // ...but non-loopback http is refused even in dev mode.
    expect(parseOrigin('http://liftr.internal:8080', { allowInsecureLoopback: true }).ok).toBe(false);
  });

  it('never resolves hostnames to infer loopback', () => {
    // A DNS name pointing at 127.0.0.1 must not pass as loopback.
    expect(parseOrigin('http://localhost.attacker.dev', { allowInsecureLoopback: true }).ok).toBe(false);
  });
});
