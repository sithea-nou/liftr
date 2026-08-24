/**
 * Origin validation, ported from the M12 Go client's rules
 * (internal/client/origin.go). A configured Liftr address is an origin —
 * scheme, host, effective port — and nothing else. Hostnames are never
 * resolved to infer loopback membership.
 */

export interface Origin {
  scheme: 'http' | 'https';
  /** Lowercased host as written; IPv6 retains brackets. */
  host: string;
  /** Effective port after scheme-default normalization. */
  effectivePort: number;
}

export type OriginResult =
  | { ok: true; origin: Origin }
  | { ok: false; reason: string };

function isLiteralLoopbackHost(host: string): boolean {
  const h = host.toLowerCase();
  if (h === 'localhost' || h === '[::1]' || h === '::1') return true;
  if (/^127\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}$/.test(h)) {
    const parts = h.split('.').slice(1).map(Number);
    return parts.every(p => p >= 0 && p <= 255);
  }
  return false;
}

/** Literal-only loopback test; performs no DNS resolution. */
export function isLoopbackLiteral(host: string): boolean {
  return isLiteralLoopbackHost(host);
}

/**
 * Parse a Liftr base URL into an Origin.
 *
 * Rules (mirroring M12):
 *  - only http and https schemes;
 *  - no userinfo, query string, fragment, or any path component;
 *  - plaintext http is accepted only for syntactic loopback hosts AND when
 *    the caller explicitly allowed insecure development composition;
 *  - hostnames are never resolved.
 */
export function parseOrigin(
  raw: string,
  opts: { allowInsecureLoopback?: boolean } = {},
): OriginResult {
  let u: URL;
  try {
    u = new URL(raw);
  } catch {
    return { ok: false, reason: 'not a valid URL' };
  }
  if (u.protocol !== 'http:' && u.protocol !== 'https:') {
    return { ok: false, reason: `unsupported scheme ${u.protocol}` };
  }
  if (u.username !== '' || u.password !== '') {
    return { ok: false, reason: 'userinfo is not permitted in an origin' };
  }
  if (u.search !== '' || u.hash !== '') {
    return { ok: false, reason: 'query and fragment are not permitted in an origin' };
  }
  if (u.pathname !== '/' && u.pathname !== '') {
    return { ok: false, reason: 'path prefixes are not supported; configure an origin only' };
  }
  const scheme = u.protocol === 'https:' ? 'https' : 'http';
  const host = u.hostname.toLowerCase();
  if (host === '') return { ok: false, reason: 'missing host' };
  if (
    scheme === 'http' &&
    !(opts.allowInsecureLoopback === true && isLiteralLoopbackHost(host))
  ) {
    return {
      ok: false,
      reason:
        'plaintext HTTP requires both explicit development mode and a syntactic loopback host',
    };
  }
  const portRaw = u.port;
  let effectivePort: number;
  if (portRaw === '') {
    effectivePort = scheme === 'https' ? 443 : 80;
  } else {
    const p = Number(portRaw);
    if (!Number.isInteger(p) || p <= 0 || p > 65535) {
      return { ok: false, reason: `invalid port ${portRaw}` };
    }
    effectivePort = p;
  }
  return { ok: true, origin: { scheme, host, effectivePort } };
}

export function originKey(o: Origin): string {
  return `${o.scheme}://${o.host}:${o.effectivePort}`;
}

export function sameOrigin(a: Origin, b: Origin): boolean {
  return (
    a.scheme === b.scheme && a.host === b.host && a.effectivePort === b.effectivePort
  );
}
