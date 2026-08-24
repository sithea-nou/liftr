/**
 * The authoritative BFF mutation envelope (Correction 2).
 *
 * Because the BFF strips raw Liftr navigation headers (Link/Location) from
 * browser responses, every successful asynchronous mutation MUST propagate
 * the admitted Operation identity through this one stable typed contract,
 * shared by liftr-backend, liftr-common, and the frontend:
 *
 *     { "data": <Liftr public response representation>,
 *       "monitorOperationId": "<id from validated Link rel=monitor>" }
 *
 * monitorOperationId is derived ONLY from the validated authoritative
 * monitor reference. It is never derived from Resource.latestOperation,
 * operation history, guessed identifiers, or unvalidated Location values.
 * A mutation response lacking a valid monitor reference is a BFF protocol
 * failure — never a guess.
 */

import { JsonValue, parseLosslessJson, stringifyLosslessJson } from './losslessJson';

export interface MutationEnvelope {
  data: JsonValue;
  monitorOperationId: string;
}

export const MONITOR_OPERATION_ID_FIELD = 'monitorOperationId';

/** Serialize an upstream success body into the wire envelope. */
export function encodeMutationEnvelope(
  dataText: string,
  monitorOperationId: string,
): { ok: true; text: string } | { ok: false; reason: 'data-not-json' } {
  let data: JsonValue;
  try {
    data = parseLosslessJson(dataText);
  } catch {
    return { ok: false, reason: 'data-not-json' };
  }
  const envelope: MutationEnvelope = { data, monitorOperationId };
  return { ok: true, text: stringifyLosslessJson(envelope as unknown as JsonValue) };
}

export type EnvelopeDecode =
  | { ok: true; value: MutationEnvelope }
  | { ok: false; reason: 'not-envelope' };

/** Decode and validate a BFF mutation envelope on the client side. */
export function decodeMutationEnvelope(bodyText: string): EnvelopeDecode {
  let doc: JsonValue;
  try {
    doc = parseLosslessJson(bodyText);
  } catch {
    return { ok: false, reason: 'not-envelope' };
  }
  if (
    typeof doc !== 'object' ||
    doc === null ||
    Array.isArray(doc) ||
    typeof (doc as Record<string, JsonValue>)['data'] === 'undefined'
  ) {
    return { ok: false, reason: 'not-envelope' };
  }
  const obj = doc as Record<string, JsonValue>;
  if (typeof obj['monitorOperationId'] !== 'string') {
    return { ok: false, reason: 'not-envelope' };
  }
  return {
    ok: true,
    value: { data: obj['data']!, monitorOperationId: obj['monitorOperationId']! },
  };
}
