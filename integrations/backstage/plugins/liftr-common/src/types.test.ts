import { describe, expect, it } from 'vitest';
import { parseLosslessJson } from './losslessJson';
import {
  parseOperation,
  parseOperationList,
  parseResourceDetail,
  parseResourceList,
  parseResourceTypeDetail,
  parseResourceTypeSummary,
  parseResourceSummary,
} from './types';

const SUMMARY_FIXTURE = `{
  "name": "PostgreSQLDatabase",
  "version": "v2",
  "displayName": "PostgreSQL Database",
  "description": "A managed PostgreSQL database.",
  "capabilities": ["create", "update", "delete"],
  "href": "/v1/resource-types/PostgreSQLDatabase/v2"
}`;

const RESOURCE_SUMMARY = `{
  "id": "orders-db",
  "type": {"name": "PostgreSQLDatabase", "version": "v2"},
  "owner": {"kind": "team", "id": "payments"},
  "generation": 3,
  "status": {"state": "Ready", "observedGeneration": 3, "updatedAt": "2026-08-24T09:30:00Z"},
  "latestOperation": {"id": "op-123", "capability": "update", "state": "Succeeded", "targetGeneration": 3, "href": "/v1/operations/op-123"},
  "createdAt": "2026-08-20T10:00:00Z",
  "updatedAt": "2026-08-24T09:30:00Z"
}`;

const OPERATION = `{
  "id": "op-child",
  "resourceId": "orders-db",
  "retryOf": "op-parent",
  "capability": "update",
  "state": "Failed",
  "targetGeneration": 4,
  "requestedAt": "2026-08-24T09:00:00Z",
  "completedAt": "2026-08-24T09:05:00Z",
  "failure": {"reason": "OutputPostconditionRejected", "message": "curated sentence"}
}`;

describe('DTO guards accept documented shapes', () => {
  it('resource type summary and detail', () => {
    const s = parseResourceTypeSummary(parseLosslessJson(SUMMARY_FIXTURE));
    expect(s.ok).toBe(true);
    const detailText = SUMMARY_FIXTURE.replace(/}$/, `,
      "specSchema": {"$schema": "https://json-schema.org/draft/2020-12/schema", "type": "object"},
      "outputContract": {"fields": [
        {"name": "hostname", "jsonType": "string", "requiredWhenReady": true},
        {"name": "port", "jsonType": "integer", "requiredWhenReady": true}
      ]}
    }`);
    const d = parseResourceTypeDetail(parseLosslessJson(detailText));
    expect(d.ok).toBe(true);
    if (d.ok) expect(d.value.outputContract!.fields).toHaveLength(2);
  });

  it('resource list with cursor', () => {
    const list = parseResourceList(
      parseLosslessJson(`{"items":[${RESOURCE_SUMMARY}],"nextCursor":"c1_abc"}`),
    );
    expect(list.ok).toBe(true);
    if (list.ok) {
      expect(list.value.items[0]!.latestOperation!.targetGeneration.toBigInt()).toBe(3n);
      expect(list.value.nextCursor).toBe('c1_abc');
    }
  });

  it('resource detail with outputs and conditions', () => {
    const detailText = `{
      "id": "orders-db",
      "type": {"name": "PostgreSQLDatabase", "version": "v2"},
      "owner": {"kind": "team", "id": "payments"},
      "generation": 5,
      "spec": {"storageGB": 20},
      "status": {"state": "Ready", "observedGeneration": 4, "updatedAt": "u",
        "conditions": [{"type": "Reconciled", "status": "True", "reason": "ok", "observedGeneration": 4}]},
      "outputs": {"observedGeneration": 4, "values": {"hostname": "db.example.com", "port": 5432}},
      "createdAt": "c", "updatedAt": "2026-08-24T09:30:00Z"
    }`;
    const detail = parseResourceDetail(parseLosslessJson(detailText));
    expect(detail.ok).toBe(true);
    if (detail.ok) {
      expect(detail.value.outputs!.values['port']!.toString()).toBe('5432');
      expect(detail.value.status.conditions).toHaveLength(1);
    }
  });

  it('operation incl retryOf lineage', () => {
    const op = parseOperation(parseLosslessJson(OPERATION));
    expect(op.ok).toBe(true);
    if (op.ok) expect(op.value.retryOf).toBe('op-parent');
    const list = parseOperationList(parseLosslessJson(`{"items":[${OPERATION}]}`));
    expect(list.ok).toBe(true);
  });
});

describe('DTO guards reject malformed shapes without throwing', () => {
  it('missing required fields report paths', () => {
    const r = parseResourceSummary(parseLosslessJson('{"id":"x"}'));
    expect(r.ok).toBe(false);
    if (!r.ok) expect(r.path).toBe('$.type');
  });

  it('wrong enum values are rejected', () => {
    const bad = RESOURCE_SUMMARY.replace('"state": "Succeeded"', '"state": "Exploded"');
    const r = parseResourceList(parseLosslessJson(`{"items":[${bad}]}`));
    expect(r.ok).toBe(false);
  });

  it('non-object inputs fail cleanly', () => {
    for (const bad of ['[]', 'null', '"x"', '5']) {
      expect(parseOperation(parseLosslessJson(bad)).ok, bad).toBe(false);
    }
  });

  it('outputs reject nested or null values', () => {
    const makeDetail = (valuesJson: string) => `{
      "id": "x", "type": {"name":"T","version":"v1"}, "owner": {"kind":"k","id":"i"},
      "generation": 1, "spec": {},
      "status": {"state": "Ready", "observedGeneration": 1, "updatedAt": "u", "conditions": []},
      "outputs": {"observedGeneration": 1, "values": ${valuesJson}},
      "createdAt": "c", "updatedAt": "u"
    }`;
    expect(parseResourceDetail(parseLosslessJson(makeDetail('{"bad": {"deep": true}}'))).ok).toBe(false);
    expect(parseResourceDetail(parseLosslessJson(makeDetail('{"bad": null}'))).ok).toBe(false);
    expect(parseResourceDetail(parseLosslessJson(makeDetail('{"ok": "str", "n": 2}'))).ok).toBe(true);
  });
});
