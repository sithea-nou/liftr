/**
 * RFC 9457 Problem handling.
 *
 * Upstream Liftr problems are passed through to the browser after strict
 * field allowlisting: only documented Problem members survive. Anything else
 * a hostile or misbehaving upstream reflects (headers, tokens, HTML) is
 * dropped. Legitimate JSON Resource data is never touched by this sanitizer —
 * it applies exclusively to problem+json documents.
 */

export interface SpecViolation {
  path: string;
  keyword: string;
  message: string;
}

export interface LiftrProblem {
  type?: string;
  title?: string;
  status: number;
  code?: string;
  requestId?: string;
  detail?: string;
  instance?: string;
  currentGeneration?: bigint;
  violations?: SpecViolation[];
  truncated?: boolean;
}

export const PROBLEM_MEDIA_TYPE = 'application/problem+json';

const MAX_STRING = 2048;
const MAX_VIOLATIONS = 20;

function boundedString(v: unknown): string | undefined {
  if (typeof v !== 'string') return undefined;
  if (v.length > MAX_STRING) return v.slice(0, MAX_STRING);
  return v;
}

export type ParsedUpstreamBody =
  | { kind: 'problem'; problem: LiftrProblem }
  | { kind: 'opaque'; status: number; requestId?: string };

/** True when a Content-Type header identifies an RFC 9457 problem document. */
export function isProblemContentType(contentType: string | undefined): boolean {
  if (!contentType) return false;
  const mediaType = contentType.split(';')[0]!.trim().toLowerCase();
  return mediaType === PROBLEM_MEDIA_TYPE || mediaType === 'application/problem+json';
}

/**
 * Parse and sanitize an upstream body believed to be a Problem. Never throws
 * on hostile input; anything unparseable collapses to the opaque form, which
 * carries only the HTTP status and the authoritative X-Request-ID.
 */
export function parseProblemBody(
  bodyText: string,
  status: number,
  requestIdHeader?: string,
): ParsedUpstreamBody {
  let doc: unknown;
  try {
    // Problems contain no numeric content the UI must reproduce lexically;
    // plain JSON.parse is acceptable ONLY here because every surviving field
    // is re-typed explicitly below. Resource data never goes through this
    // path.
    doc = JSON.parse(bodyText);
  } catch {
    return { kind: 'opaque', status, ...(requestIdHeader ? { requestId: requestIdHeader } : {}) };
  }
  if (typeof doc !== 'object' || doc === null || Array.isArray(doc)) {
    return { kind: 'opaque', status, ...(requestIdHeader ? { requestId: requestIdHeader } : {}) };
  }
  const d = doc as Record<string, unknown>;
  const problem: LiftrProblem = { status };
  const type = boundedString(d['type']);
  if (type !== undefined) problem.type = type;
  const title = boundedString(d['title']);
  if (title !== undefined) problem.title = title;
  const code = boundedString(d['code']);
  if (code !== undefined) problem.code = code;
  let rid = boundedString(d['requestId']);
  if (rid === undefined && requestIdHeader) rid = requestIdHeader;
  if (rid !== undefined) problem.requestId = rid;
  const detail = boundedString(d['detail']);
  if (detail !== undefined) problem.detail = detail;
  const instance = boundedString(d['instance']);
  if (instance !== undefined) problem.instance = instance;
  const cg = d['currentGeneration'];
  if (typeof cg === 'number' && Number.isSafeInteger(cg) && cg > 0) {
    problem.currentGeneration = BigInt(cg);
  } else if (typeof cg === 'string' && /^[0-9]+$/.test(cg)) {
    problem.currentGeneration = BigInt(cg);
  }
  const violationsRaw = d['violations'];
  if (Array.isArray(violationsRaw)) {
    const violations: SpecViolation[] = [];
    for (const raw of violationsRaw.slice(0, MAX_VIOLATIONS)) {
      if (typeof raw !== 'object' || raw === null) continue;
      const r = raw as Record<string, unknown>;
      const path = boundedString(r['path']);
      const keyword = boundedString(r['keyword']);
      const message = boundedString(r['message']);
      if (path === undefined || keyword === undefined || message === undefined) continue;
      violations.push({ path, keyword, message });
    }
    if (violations.length > 0) problem.violations = violations;
  }
  if (typeof d['truncated'] === 'boolean') problem.truncated = d['truncated'];
  return { kind: 'problem', problem };
}

/**
 * One-line safe summary for logs. Deliberately excludes `detail`, violation
 * messages, and instance: log lines must not become a wholesale channel for
 * upstream text.
 */
export function describeProblemForLog(p: LiftrProblem): string {
  const parts = [
    `status=${p.status}`,
    p.code ? `code=${p.code}` : undefined,
    p.requestId ? `requestId=${p.requestId}` : undefined,
    p.violations ? `violations=${p.violations.length}${p.truncated ? '+' : ''}` : undefined,
  ].filter(x => x !== undefined);
  return parts.join(' ');
}
