/**
 * M15 inventory query parameters. These are the ONLY list filters that exist;
 * the UI must not invent server-side search beyond them.
 */

export const RESOURCE_LIST_QUERY_KEYS = [
  'limit',
  'cursor',
  'ownerKind',
  'ownerId',
  'type',
  'version',
  'state',
  'includeDeleted',
] as const;

export type ResourceListQueryKey = (typeof RESOURCE_LIST_QUERY_KEYS)[number];

export const RESOURCE_STATES = [
  'Unknown',
  'Pending',
  'Ready',
  'Deleting',
  'Deleted',
  'Failed',
] as const;

const NAME_SAFE = /^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$/;

export interface ValidatedResourceListQuery {
  limit?: number;
  cursor?: string;
  ownerKind?: string;
  ownerId?: string;
  type?: string;
  version?: string;
  state?: (typeof RESOURCE_STATES)[number];
  includeDeleted?: boolean;
}

export type QueryValidation =
  | { ok: true; query: ValidatedResourceListQuery }
  | { ok: false; reason: string };

/**
 * Validate raw query entries against Liftr's documented M15 semantics:
 * ownerKind+ownerId are paired; version requires type; state=Deleted without
 * includeDeleted=true is contradictory; limit is 1..100.
 */
export function validateResourceListQuery(
  params: URLSearchParams,
): QueryValidation {
  const q: ValidatedResourceListQuery = {};
  for (const [k] of params) {
    if (!(RESOURCE_LIST_QUERY_KEYS as readonly string[]).includes(k)) {
      return { ok: false, reason: `unknown query parameter ${k}` };
    }
  }
  const limitRaw = params.get('limit');
  if (limitRaw !== null) {
    if (!/^[1-9][0-9]*$/.test(limitRaw)) return { ok: false, reason: 'limit must be a positive integer' };
    const n = Number(limitRaw);
    if (n < 1 || n > 100) return { ok: false, reason: 'limit must be between 1 and 100' };
    q.limit = n;
  }
  const cursor = params.get('cursor');
  if (cursor !== null) {
    if (cursor === '' || cursor.length > 128) return { ok: false, reason: 'invalid cursor' };
    q.cursor = cursor;
  }
  const ownerKind = params.get('ownerKind');
  const ownerId = params.get('ownerId');
  if (ownerKind !== null || ownerId !== null) {
    if (ownerKind === null || ownerId === null) {
      return { ok: false, reason: 'ownerKind and ownerId must be supplied together' };
    }
    if (!NAME_SAFE.test(ownerKind)) return { ok: false, reason: 'invalid ownerKind' };
    if (!NAME_SAFE.test(ownerId)) return { ok: false, reason: 'invalid ownerId' };
    q.ownerKind = ownerKind;
    q.ownerId = ownerId;
  }
  const type = params.get('type');
  if (type !== null) {
    if (!NAME_SAFE.test(type)) return { ok: false, reason: 'invalid type filter' };
    q.type = type;
  }
  const version = params.get('version');
  if (version !== null) {
    if (type === null) return { ok: false, reason: 'version requires type' };
    if (!NAME_SAFE.test(version)) return { ok: false, reason: 'invalid version filter' };
    q.version = version;
  }
  const state = params.get('state');
  const includeDeletedRaw = params.get('includeDeleted');
  let includeDeleted: boolean | undefined;
  if (includeDeletedRaw !== null) {
    if (includeDeletedRaw === 'true') includeDeleted = true;
    else if (includeDeletedRaw === 'false') includeDeleted = false;
    else return { ok: false, reason: 'includeDeleted must be true or false' };
    q.includeDeleted = includeDeleted;
  }
  if (state !== null) {
    if (!(RESOURCE_STATES as readonly string[]).includes(state)) {
      return { ok: false, reason: 'invalid state filter' };
    }
    if (state === 'Deleted' && includeDeleted !== true) {
      return { ok: false, reason: 'state=Deleted requires includeDeleted=true' };
    }
    q.state = state as ValidatedResourceListQuery['state'];
  }
  return { ok: true, query: q };
}

/** Build the upstream query string in a stable order. */
export function serializeResourceListQuery(q: ValidatedResourceListQuery): string {
  const sp = new URLSearchParams();
  if (q.limit !== undefined) sp.set('limit', String(q.limit));
  if (q.cursor !== undefined) sp.set('cursor', q.cursor);
  if (q.ownerKind !== undefined) sp.set('ownerKind', q.ownerKind);
  if (q.ownerId !== undefined) sp.set('ownerId', q.ownerId);
  if (q.type !== undefined) sp.set('type', q.type);
  if (q.version !== undefined) sp.set('version', q.version);
  if (q.state !== undefined) sp.set('state', q.state);
  if (q.includeDeleted !== undefined) sp.set('includeDeleted', String(q.includeDeleted));
  const s = sp.toString();
  return s === '' ? '' : `?${s}`;
}
