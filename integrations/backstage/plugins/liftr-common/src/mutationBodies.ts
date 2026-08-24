/**
 * Mutation envelope construction with strict numeric fidelity (Correction 3 /
 * JSON-editor-first decision).
 *
 * The user's spec is TEXT. It is validated to be well-formed JSON and then
 * spliced VERBATIM into the outgoing document, mirroring the M12 CLI's
 * technique. The spec text never round-trips through JS numbers, so `20` and
 * `20.0` keep their distinct lexical forms — and therefore their distinct
 * admission fingerprints — end to end.
 */

import {
  JsonObject,
  JsonValue,
  LosslessNumber,
  isLosslessNumber,
  parseLosslessJson,
  quoteString,
} from './losslessJson';

/** Transport ID rules mirrored from internal/api/http validateTransportID. */
const RESOURCE_ID_SAFE = /^[^\s/\x7f\x00-\x20]+$/;

export function isValidResourceTransportId(id: string): boolean {
  return id.length > 0 && id.length <= 512 && RESOURCE_ID_SAFE.test(id);
}

function requirePlainObject(text: string, what: string): JsonObject {
  let v: JsonValue;
  try {
    v = parseLosslessJson(text);
  } catch {
    throw new Error(`${what} is not well-formed JSON`);
  }
  if (typeof v !== 'object' || v === null || Array.isArray(v) || isLosslessNumber(v)) {
    throw new Error(`${what} must be a JSON object`);
  }
  return v as JsonObject;
}

/**
 * Build the POST /v1/resources request body with the spec spliced verbatim.
 * Throws on malformed input; callers surface that before any network call.
 */
export function buildCreateResourceBody(input: {
  id: string;
  typeName: string;
  typeVersion: string;
  ownerKind: string;
  ownerId: string;
  /** Raw spec JSON text exactly as the developer wrote it. */
  specText: string;
}): { ok: true; bodyText: string } | { ok: false; error: string } {
  if (!isValidResourceTransportId(input.id)) {
    return { ok: false, error: 'resource id must be a single URL-segment-safe string' };
  }
  if (!isValidResourceTransportId(input.typeName) || !isValidResourceTransportId(input.typeVersion)) {
    return { ok: false, error: 'invalid resource type reference' };
  }
  if (!isValidResourceTransportId(input.ownerKind) || !isValidResourceTransportId(input.ownerId)) {
    return { ok: false, error: 'invalid owner reference' };
  }
  let spec: string;
  try {
    const obj = requirePlainObject(input.specText, 'spec');
    spec = stringifySpecVerbatim(obj);
  } catch (e) {
    return { ok: false, error: e instanceof Error ? e.message : 'invalid spec' };
  }
  const body =
    '{' +
    `"id":${quoteString(input.id)},` +
    '"type":{' +
    `"name":${quoteString(input.typeName)},"version":${quoteString(input.typeVersion)}` +
    '},' +
    '"owner":{' +
    `"kind":${quoteString(input.ownerKind)},"id":${quoteString(input.ownerId)}` +
    '},' +
    `"spec":${spec}` +
    '}';
  // Round-trip sanity: the assembled document must itself parse.
  try {
    parseLosslessJson(body);
  } catch {
    return { ok: false, error: 'assembled create document failed validation' };
  }
  return { ok: true, bodyText: body };
}

/**
 * Build the PUT /v1/resources/{id} body: full replacement, `{spec: ...}`,
 * with the same verbatim-splice guarantee.
 */
export function buildUpdateResourceBody(specText: string): { ok: true; bodyText: string } | { ok: false; error: string } {
  try {
    const obj = requirePlainObject(specText, 'spec');
    const body = `{"spec":${stringifySpecVerbatim(obj)}}`;
    parseLosslessJson(body);
    return { ok: true, bodyText: body };
  } catch (e) {
    return { ok: false, error: e instanceof Error ? e.message : 'invalid spec' };
  }
}

// ---------------------------------------------------------------------------
// Verbatim spec serialization.
//
// The parsed tree keeps LosslessNumber nodes holding exact lexemes, so
// re-serializing reproduces the developer's numeric representation byte for
// byte while normalizing only whitespace outside of strings.
// ---------------------------------------------------------------------------

function stringifySpecVerbatim(value: JsonValue): string {
  return serializeSpec(value);
}

function serializeSpec(v: JsonValue): string {
  if (v === null) return 'null';
  if (typeof v === 'boolean') return v ? 'true' : 'false';
  if (typeof v === 'string') return quoteString(v);
  if (isLosslessNumber(v)) return (v as LosslessNumber).value;
  if (Array.isArray(v)) return `[${v.map(serializeSpec).join(',')}]`;
  const parts: string[] = [];
  for (const k of Object.keys(v)) {
    parts.push(`${quoteString(k)}:${serializeSpec((v as JsonObject)[k]!)}`);
  }
  return `{${parts.join(',')}}`;
}
