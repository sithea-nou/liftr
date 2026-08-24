/**
 * Liftr public /v1 DTO contracts, mirrored from docs/openapi/v1/openapi.yaml.
 *
 * Duplication is deliberate (ADR-0013 precedent): this is a client-side
 * representation of the published contract and must never import server
 * implementation packages. A drift test in this package parses the OpenAPI
 * document and pins these shapes.
 *
 * All JSON numbers are represented as LosslessNumber so that generation
 * values (uint64) and any numeric content survive without IEEE-double
 * coercion.
 */

import { JsonObject, JsonValue, LosslessNumber, isLosslessNumber } from './losslessJson';

export interface ResourceTypeRef {
  name: string;
  version: string;
}

export interface OwnerRef {
  kind: string;
  id: string;
}

/** Developer-facing capabilities advertised by a ResourceType contract. */
export type ResourceTypeCapability = 'create' | 'update' | 'delete' | 'observe' | (string & {});

export interface ResourceTypeSummary {
  name: string;
  version: string;
  displayName: string;
  description: string;
  capabilities: ResourceTypeCapability[];
  href: string;
}

export interface OutputFieldDescriptor {
  name: string;
  jsonType: 'string' | 'integer' | 'number' | 'boolean';
  requiredWhenReady: boolean;
}

export interface OutputContract {
  fields: OutputFieldDescriptor[];
}

export interface ResourceTypeDetail extends ResourceTypeSummary {
  specSchema: JsonValue;
  outputContract?: OutputContract;
}

export type ResourceState =
  | 'Unknown'
  | 'Pending'
  | 'Ready'
  | 'Deleting'
  | 'Deleted'
  | 'Failed';

export interface Condition {
  type: string;
  status: 'True' | 'False' | 'Unknown';
  reason?: string;
  message?: string;
  observedGeneration?: LosslessNumber;
  lastTransitionAt?: string;
}

export interface ResourceStatus {
  state: ResourceState;
  observedGeneration: LosslessNumber;
  conditions: Condition[];
  updatedAt: string;
}

export type OperationCapability = 'create' | 'update' | 'delete';

export type OperationState = 'Pending' | 'Running' | 'Succeeded' | 'Failed' | 'Canceled';

export interface LatestOperationRef {
  id: string;
  capability: OperationCapability;
  state: OperationState;
  targetGeneration: LosslessNumber;
  href: string;
}

/**
 * Generation-bound non-secret outputs (ADR-0011). Values are flat scalars;
 * nested objects, arrays, null, and secret material never appear.
 */
export interface ResourceOutputs {
  observedGeneration: LosslessNumber;
  values: Record<string, string | number | boolean | LosslessNumber>;
}

export interface ResourceSummary {
  id: string;
  type: ResourceTypeRef;
  owner: OwnerRef;
  generation: LosslessNumber;
  status: {
    state: ResourceState;
    observedGeneration: LosslessNumber;
    updatedAt: string;
  };
  latestOperation?: LatestOperationRef;
  createdAt: string;
  updatedAt: string;
}

export interface Resource extends Omit<ResourceSummary, 'status'> {
  spec: JsonValue;
  status: ResourceStatus;
  outputs?: ResourceOutputs;
}

export interface OperationFailure {
  reason: string;
  message?: string;
}

export interface Operation {
  id: string;
  resourceId: string;
  retryOf?: string;
  capability: OperationCapability;
  state: OperationState;
  targetGeneration: LosslessNumber;
  requestedAt: string;
  startedAt?: string;
  completedAt?: string;
  failure?: OperationFailure;
}

export interface ResourceList {
  items: ResourceSummary[];
  nextCursor?: string;
}

export interface OperationList {
  items: Operation[];
  nextCursor?: string;
}

// ---------------------------------------------------------------------------
// Runtime guards over parsed lossless JSON trees.
//
// Guards validate presence and shape of documented fields; unknown extra
// fields pass through untouched so newer servers remain readable by older
// clients (additive evolution). Malformed input yields a structured failure,
// never a thrown hostile error.
// ---------------------------------------------------------------------------

export interface GuardFailure {
  ok: false;
  path: string;
  expected: string;
}
export type GuardResult<T> = { ok: true; value: T } | GuardFailure;

function fail(path: string, expected: string): GuardFailure {
  return { ok: false, path, expected };
}

function asObject(v: JsonValue | undefined): v is JsonObject {
  return typeof v === 'object' && v !== null && !Array.isArray(v) && !isLosslessNumber(v);
}

function str(o: JsonObject, key: string, path: string): { ok: true; value: string } | GuardFailure {
  const v = o[key];
  if (typeof v !== 'string') return fail(`${path}.${key}`, 'string');
  return { ok: true, value: v };
}

function optStr(o: JsonObject, key: string, path: string): { ok: true; value?: string } | GuardFailure {
  if (!(key in o) || o[key] === undefined) return { ok: true };
  return str(o, key, path);
}

function losslessInt(
  o: JsonObject,
  key: string,
  path: string,
): { ok: true; value: LosslessNumber } | GuardFailure {
  const v = o[key];
  if (!isLosslessNumber(v)) return fail(`${path}.${key}`, 'number');
  if (!v.isIntegerLexeme()) return fail(`${path}.${key}`, 'integer');
  return { ok: true, value: v };
}

function joinItemPath(index: number, innerPath: string): string {
  const base = `$.items[${index}]`;
  return innerPath === '$' ? base : `${base}.${innerPath.slice(2)}`;
}

const RESOURCE_STATES: ReadonlySet<string> = new Set([
  'Unknown', 'Pending', 'Ready', 'Deleting', 'Deleted', 'Failed',
]);
const OPERATION_STATES: ReadonlySet<string> = new Set([
  'Pending', 'Running', 'Succeeded', 'Failed', 'Canceled',
]);
const OPERATION_CAPABILITIES: ReadonlySet<string> = new Set(['create', 'update', 'delete']);
const OUTPUT_JSON_TYPES: ReadonlySet<string> = new Set(['string', 'integer', 'number', 'boolean']);

export function parseResourceTypeSummary(v: JsonValue): GuardResult<ResourceTypeSummary> {
  if (!asObject(v)) return fail('$', 'object');
  const name = str(v, 'name', '$');
  if (!name.ok) return name;
  const version = str(v, 'version', '$');
  if (!version.ok) return version;
  const displayName = str(v, 'displayName', '$');
  if (!displayName.ok) return displayName;
  const description = str(v, 'description', '$');
  if (!description.ok) return description;
  const href = str(v, 'href', '$');
  if (!href.ok) return href;
  const caps = v['capabilities'];
  if (!Array.isArray(caps)) return fail('$.capabilities', 'array');
  for (const c of caps) {
    if (typeof c !== 'string') return fail('$.capabilities[]', 'string');
  }
  return {
    ok: true,
    value: {
      name: name.value,
      version: version.value,
      displayName: displayName.value,
      description: description.value,
      capabilities: caps as string[],
      href: href.value,
    },
  };
}

export function parseResourceTypeDetail(v: JsonValue): GuardResult<ResourceTypeDetail> {
  const summary = parseResourceTypeSummary(v);
  if (!summary.ok) return summary;
  const obj = v as JsonObject;
  if (!('specSchema' in obj)) return fail('$.specSchema', 'present');
  let outputContract: OutputContract | undefined;
  const oc = obj['outputContract'];
  if (oc !== undefined && oc !== null) {
    if (!asObject(oc)) return fail('$.outputContract', 'object');
    const fields = oc['fields'];
    if (!Array.isArray(fields)) return fail('$.outputContract.fields', 'array');
    const out: OutputFieldDescriptor[] = [];
    for (let i = 0; i < fields.length; i++) {
      const f = fields[i];
      if (!asObject(f)) return fail(`$.outputContract.fields[${i}]`, 'object');
      const n = str(f, 'name', `$.outputContract.fields[${i}]`);
      if (!n.ok) return n;
      const jt = str(f, 'jsonType', `$.outputContract.fields[${i}]`);
      if (!jt.ok) return jt;
      if (!OUTPUT_JSON_TYPES.has(jt.value)) {
        return fail(`$.outputContract.fields[${i}].jsonType`, 'string|integer|number|boolean');
      }
      const rw = f['requiredWhenReady'];
      if (typeof rw !== 'boolean') {
        return fail(`$.outputContract.fields[${i}].requiredWhenReady`, 'boolean');
      }
      out.push({ name: n.value, jsonType: jt.value as OutputFieldDescriptor['jsonType'], requiredWhenReady: rw });
    }
    outputContract = { fields: out };
  }
  return { ok: true, value: { ...summary.value, specSchema: obj['specSchema'], outputContract } };
}

function parseLatestOperationRef(v: JsonValue): GuardResult<LatestOperationRef> {
  if (!asObject(v)) return fail('latestOperation', 'object');
  const id = str(v, 'id', 'latestOperation');
  if (!id.ok) return id;
  const capability = str(v, 'capability', 'latestOperation');
  if (!capability.ok) return capability;
  if (!OPERATION_CAPABILITIES.has(capability.value)) {
    return fail('latestOperation.capability', 'create|update|delete');
  }
  const state = str(v, 'state', 'latestOperation');
  if (!state.ok) return state;
  if (!OPERATION_STATES.has(state.value)) return fail('latestOperation.state', 'OperationState');
  const tg = losslessInt(v, 'targetGeneration', 'latestOperation');
  if (!tg.ok) return tg;
  const href = str(v, 'href', 'latestOperation');
  if (!href.ok) return href;
  return {
    ok: true,
    value: {
      id: id.value,
      capability: capability.value as OperationCapability,
      state: state.value as OperationState,
      targetGeneration: tg.value,
      href: href.value,
    },
  };
}

function parseOwnerRef(v: JsonValue, path: string): GuardResult<OwnerRef> {
  if (!asObject(v)) return fail(path, 'object');
  const kind = str(v, 'kind', path);
  if (!kind.ok) return kind;
  const id = str(v, 'id', path);
  if (!id.ok) return id;
  return { ok: true, value: { kind: kind.value, id: id.value } };
}

function parseResourceTypeRef(v: JsonValue, path: string): GuardResult<ResourceTypeRef> {
  if (!asObject(v)) return fail(path, 'object');
  const name = str(v, 'name', path);
  if (!name.ok) return name;
  const version = str(v, 'version', path);
  if (!version.ok) return version;
  return { ok: true, value: { name: name.value, version: version.value } };
}

export function parseResourceSummary(v: JsonValue): GuardResult<ResourceSummary> {
  if (!asObject(v)) return fail('$', 'object');
  const id = str(v, 'id', '$');
  if (!id.ok) return id;
  const type = parseResourceTypeRef(v['type'], '$.type');
  if (!type.ok) return type;
  const owner = parseOwnerRef(v['owner'], '$.owner');
  if (!owner.ok) return owner;
  const gen = losslessInt(v, 'generation', '$');
  if (!gen.ok) return gen;
  const st = v['status'];
  if (!asObject(st)) return fail('$.status', 'object');
  const state = str(st, 'state', '$.status');
  if (!state.ok) return state;
  if (!RESOURCE_STATES.has(state.value)) return fail('$.status.state', 'ResourceState');
  const og = losslessInt(st, 'observedGeneration', '$.status');
  if (!og.ok) return og;
  const stUpd = str(st, 'updatedAt', '$.status');
  if (!stUpd.ok) return stUpd;
  let latestOperation: LatestOperationRef | undefined;
  if ('latestOperation' in v && v['latestOperation'] !== null) {
    const lo = parseLatestOperationRef(v['latestOperation']);
    if (!lo.ok) return lo;
    latestOperation = lo.value;
  }
  const created = str(v, 'createdAt', '$');
  if (!created.ok) return created;
  const updated = str(v, 'updatedAt', '$');
  if (!updated.ok) return updated;
  return {
    ok: true,
    value: {
      id: id.value,
      type: type.value,
      owner: owner.value,
      generation: gen.value,
      status: { state: state.value as ResourceState, observedGeneration: og.value, updatedAt: stUpd.value },
      ...(latestOperation ? { latestOperation } : {}),
      createdAt: created.value,
      updatedAt: updated.value,
    },
  };
}

export function parseResourceList(v: JsonValue): GuardResult<ResourceList> {
  if (!asObject(v)) return fail('$', 'object');
  const itemsRaw = v['items'];
  if (!Array.isArray(itemsRaw)) return fail('$.items', 'array');
  const items: ResourceSummary[] = [];
  for (let i = 0; i < itemsRaw.length; i++) {
    const r = parseResourceSummary(itemsRaw[i]);
    if (!r.ok) return { ...r, path: joinItemPath(i, r.path) };
    items.push(r.value);
  }
  let nextCursor: string | undefined;
  if ('nextCursor' in v && v['nextCursor'] !== undefined) {
    const nc = str(v, 'nextCursor', '$');
    if (!nc.ok) return nc;
    nextCursor = nc.value;
  }
  return { ok: true, value: { items, ...(nextCursor !== undefined ? { nextCursor } : {}) } };
}

export function parseResourceDetail(v: JsonValue): GuardResult<Resource> {
  const summary = parseResourceSummary(v);
  if (!summary.ok) return summary;
  const obj = v as JsonObject;
  if (!('spec' in obj)) return fail('$.spec', 'present');
  // Full status with conditions.
  const st = obj['status'];
  if (!asObject(st)) return fail('$.status', 'object');
  const condsRaw = st['conditions'];
  if (!Array.isArray(condsRaw)) return fail('$.status.conditions', 'array');
  const conditions: Condition[] = [];
  for (let i = 0; i < condsRaw.length; i++) {
    const c = condsRaw[i];
    if (!asObject(c)) return fail(`$.status.conditions[${i}]`, 'object');
    const t = str(c, 'type', `$.status.conditions[${i}]`);
    if (!t.ok) return t;
    const s2 = str(c, 'status', `$.status.conditions[${i}]`);
    if (!s2.ok) return s2;
    if (!['True', 'False', 'Unknown'].includes(s2.value)) {
      return fail(`$.status.conditions[${i}].status`, 'True|False|Unknown');
    }
    const cond: Condition = {
      type: t.value,
      status: s2.value as Condition['status'],
    };
    const rs = optStr(c, 'reason', `$.status.conditions[${i}]`);
    if (!rs.ok) return rs;
    if (rs.value !== undefined) cond.reason = rs.value;
    const ms = optStr(c, 'message', `$.status.conditions[${i}]`);
    if (!ms.ok) return ms;
    if (ms.value !== undefined) cond.message = ms.value;
    if ('observedGeneration' in c) {
      const og = losslessInt(c, 'observedGeneration', `$.status.conditions[${i}]`);
      if (!og.ok) return og;
      cond.observedGeneration = og.value;
    }
    const lt = optStr(c, 'lastTransitionAt', `$.status.conditions[${i}]`);
    if (!lt.ok) return lt;
    if (lt.value !== undefined) cond.lastTransitionAt = lt.value;
    conditions.push(cond);
  }
  let outputs: ResourceOutputs | undefined;
  const outsRaw = obj['outputs'];
  if (outsRaw !== undefined && outsRaw !== null) {
    if (!asObject(outsRaw)) return fail('$.outputs', 'object');
    const og = losslessInt(outsRaw, 'observedGeneration', '$.outputs');
    if (!og.ok) return og;
    const vals = outsRaw['values'];
    if (!asObject(vals)) return fail('$.outputs.values', 'object');
    const values: Record<string, string | number | boolean | LosslessNumber> = {};
    for (const k of Object.keys(vals)) {
      const raw = vals[k];
      if (
        typeof raw !== 'string' &&
        typeof raw !== 'boolean' &&
        !isLosslessNumber(raw)
      ) {
        return fail(`$.outputs.values.${k}`, 'flat scalar');
      }
      values[k] = raw;
    }
    outputs = { observedGeneration: og.value, values };
  }
  return {
    ok: true,
    value: {
      ...summary.value,
      status: {
        state: summary.value.status.state,
        observedGeneration: summary.value.status.observedGeneration,
        conditions,
        updatedAt: summary.value.status.updatedAt,
      },
      spec: obj['spec'],
      ...(outputs ? { outputs } : {}),
    },
  };
}

export function parseOperation(v: JsonValue): GuardResult<Operation> {
  if (!asObject(v)) return fail('$', 'object');
  const id = str(v, 'id', '$');
  if (!id.ok) return id;
  const rid = str(v, 'resourceId', '$');
  if (!rid.ok) return rid;
  const capability = str(v, 'capability', '$');
  if (!capability.ok) return capability;
  if (!OPERATION_CAPABILITIES.has(capability.value)) return fail('$.capability', 'create|update|delete');
  const state = str(v, 'state', '$');
  if (!state.ok) return state;
  if (!OPERATION_STATES.has(state.value)) return fail('$.state', 'OperationState');
  const tg = losslessInt(v, 'targetGeneration', '$');
  if (!tg.ok) return tg;
  const requestedAt = str(v, 'requestedAt', '$');
  if (!requestedAt.ok) return requestedAt;
  const op: Operation = {
    id: id.value,
    resourceId: rid.value,
    capability: capability.value as OperationCapability,
    state: state.value as OperationState,
    targetGeneration: tg.value,
    requestedAt: requestedAt.value,
  };
  const retryOf = optStr(v, 'retryOf', '$');
  if (!retryOf.ok) return retryOf;
  if (retryOf.value !== undefined) op.retryOf = retryOf.value;
  const startedAt = optStr(v, 'startedAt', '$');
  if (!startedAt.ok) return startedAt;
  if (startedAt.value !== undefined) op.startedAt = startedAt.value;
  const completedAt = optStr(v, 'completedAt', '$');
  if (!completedAt.ok) return completedAt;
  if (completedAt.value !== undefined) op.completedAt = completedAt.value;
  const f = v['failure'];
  if (f !== undefined && f !== null) {
    if (!asObject(f)) return fail('$.failure', 'object');
    const reason = str(f, 'reason', '$.failure');
    if (!reason.ok) return reason;
    const failure: OperationFailure = { reason: reason.value };
    const message = optStr(f, 'message', '$.failure');
    if (!message.ok) return message;
    if (message.value !== undefined) failure.message = message.value;
    op.failure = failure;
  }
  return { ok: true, value: op };
}

export function parseOperationList(v: JsonValue): GuardResult<OperationList> {
  if (!asObject(v)) return fail('$', 'object');
  const itemsRaw = v['items'];
  if (!Array.isArray(itemsRaw)) return fail('$.items', 'array');
  const items: Operation[] = [];
  for (let i = 0; i < itemsRaw.length; i++) {
    const r = parseOperation(itemsRaw[i]);
    if (!r.ok) return { ...r, path: joinItemPath(i, r.path) };
    items.push(r.value);
  }
  let nextCursor: string | undefined;
  if ('nextCursor' in v && v['nextCursor'] !== undefined) {
    const nc = str(v, 'nextCursor', '$');
    if (!nc.ok) return nc;
    nextCursor = nc.value;
  }
  return { ok: true, value: { items, ...(nextCursor !== undefined ? { nextCursor } : {}) } };
}
