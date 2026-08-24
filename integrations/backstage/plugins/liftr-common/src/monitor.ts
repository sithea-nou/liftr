/**
 * Authoritative monitor-reference handling (Correction 2).
 *
 * The BFF strips raw Liftr navigation headers from browser responses, so the
 * admitted Operation identity must be extracted server-side from the one
 * authoritative source: the `Link` header entry with rel="monitor". Parsing
 * rules mirror the M12 Go client:
 *
 *  - only the configured Liftr origin participates;
 *  - relative references resolve against that origin;
 *  - the path must be exactly /v1/operations/{id} with one segment;
 *  - a present-but-invalid monitor entry is a failure — there is never a
 *    fallback to Location or to Resource.latestOperation.
 */

import { Origin, originKey, sameOrigin } from './origin';

export const MONITOR_REL = 'monitor';

export interface ParsedLink {
  uriRef: string;
  rel?: string;
}

/**
 * Parse an RFC 8288 Link header value into entries. Handles comma-separated
 * fields with quoted parameters containing commas/semicolons.
 */
export function parseLinkHeader(headerValue: string): ParsedLink[] {
  const links: ParsedLink[] = [];
  for (const field of splitUnquoted(headerValue, ',')) {
    const trimmed = field.trim();
    if (trimmed === '') continue;
    const segments = splitUnquoted(trimmed, ';');
    if (segments.length === 0) continue;
    let uriRef = segments[0]!.trim();
    if (uriRef.length >= 2 && uriRef.startsWith('<') && uriRef.endsWith('>')) {
      uriRef = uriRef.slice(1, -1);
    } else {
      continue;
    }
    let rel: string | undefined;
    for (let i = 1; i < segments.length; i++) {
      const seg = segments[i]!;
      const eq = seg.indexOf('=');
      if (eq === -1) continue;
      const name = seg.slice(0, eq).trim().toLowerCase();
      let value = seg.slice(eq + 1).trim();
      if (value.startsWith('"') && value.endsWith('"') && value.length >= 2) {
        value = value.slice(1, -1);
      }
      if (name === 'rel') {
        rel = value.toLowerCase();
      }
    }
    links.push({ uriRef, ...(rel !== undefined ? { rel } : {}) });
  }
  return links;
}

function splitUnquoted(input: string, separator: string): string[] {
  const out: string[] = [];
  let current = '';
  let inQuotes = false;
  for (let i = 0; i < input.length; i++) {
    const c = input[i];
    if (c === '"') {
      inQuotes = !inQuotes;
      current += c;
      continue;
    }
    if (c === '\\' && inQuotes && i + 1 < input.length) {
      current += c + input[i + 1];
      i++;
      continue;
    }
    if (c === separator && !inQuotes) {
      out.push(current);
      current = '';
      continue;
    }
    current += c;
  }
  out.push(current);
  return out;
}

const OPERATION_PATH = /^\/v1\/operations\/([A-Za-z0-9][A-Za-z0-9._:-]{0,127})$/;

export type MonitorExtraction =
  | { ok: true; operationId: string }
  | { ok: false; reason: 'absent' | 'invalid' };

/**
 * Extract the authoritative monitor Operation ID from a Link header.
 *
 * `upstreamOrigin` is the pinned Liftr origin this BFF was configured with.
 * Any reference that does not resolve to exactly one v1 Operation on that
 * origin makes extraction fail with reason 'invalid'; callers must treat that
 * as a protocol failure rather than guessing another identity.
 */
export function extractMonitorOperationId(
  linkHeader: string | null | undefined,
  upstreamOrigin: Origin,
): MonitorExtraction {
  if (linkHeader === null || linkHeader === undefined || linkHeader.trim() === '') {
    return { ok: false, reason: 'absent' };
  }
  const monitorEntries = parseLinkHeader(linkHeader).filter(l => l.rel === MONITOR_REL);
  if (monitorEntries.length === 0) {
    return { ok: false, reason: 'absent' };
  }
  // First rel="monitor" entry wins, mirroring the Go client.
  const ref = monitorEntries[0]!.uriRef;
  const id = resolveOperationId(ref, upstreamOrigin);
  if (id.ok) return id;
  return { ok: false, reason: 'invalid' };
}

function resolveOperationId(uriRef: string, origin: Origin): MonitorExtraction {
  if (uriRef.includes('\\')) return { ok: false, reason: 'invalid' };
  let pathname: string;
  if (/^https?:\/\//i.test(uriRef)) {
    let u: URL;
    try {
      u = new URL(uriRef);
    } catch {
      return { ok: false, reason: 'invalid' };
    }
    const parsed = (() => {
      try {
        // Re-validate through parseOrigin semantics without allowing plaintext.
        const scheme = u.protocol.replace(':', '');
        const portRaw = u.port;
        const effectivePort =
          portRaw === '' ? (scheme === 'https' ? 443 : 80) : Number(portRaw);
        return { scheme, host: u.hostname.toLowerCase(), effectivePort };
      } catch {
        return null;
      }
    })();
    if (!parsed) return { ok: false, reason: 'invalid' };
    const candidate = { scheme: parsed.scheme as Origin['scheme'], host: parsed.host, effectivePort: parsed.effectivePort };
    if (!sameOrigin(candidate, origin)) return { ok: false, reason: 'invalid' };
    if (u.search !== '' || u.hash !== '' || u.username !== '' || u.password !== '') {
      return { ok: false, reason: 'invalid' };
    }
    pathname = u.pathname;
  } else {
    if (!uriRef.startsWith('/')) return { ok: false, reason: 'invalid' };
    if (uriRef.includes('?') || uriRef.includes('#')) {
      return { ok: false, reason: 'invalid' };
    }
    pathname = uriRef;
  }
  const m = OPERATION_PATH.exec(pathname);
  if (!m) return { ok: false, reason: 'invalid' };
  return { ok: true, operationId: m[1]! };
}

/**
 * True when the given absolute URI points at the pinned origin. Used by the
 * forwarder to double-check any absolute URL it is about to construct.
 */
export function isSameOriginAbsoluteUri(uri: string, origin: Origin): boolean {
  try {
    const u = new URL(uri);
    return originKey(origin) === `${u.protocol.replace(':', '')}://${u.hostname.toLowerCase()}:${u.port === '' ? (u.protocol === 'https:' ? 443 : 80) : Number(u.port)}`;
  } catch {
    return false;
  }
}
