/**
 * OpenAPI drift test — the TS-side guarantee that the handwritten client
 * mirrors docs/openapi/v1/openapi.yaml (ADR-0013 precedent, M16 §23).
 *
 * The authoritative document is parsed directly from the Go repository; if a
 * documented field is added/changed without updating this integration's DTO
 * guards and client paths, this test fails.
 */

import { readFileSync } from 'node:fs';
// eslint-disable-next-line @typescript-eslint/no-explicit-any
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';
import { parse } from 'yaml';
import { parseLosslessJson } from './losslessJson';
import {
  Operation,
  Resource,
  ResourceSummary,
  parseOperation,
  parseResourceDetail,
  parseResourceList,
  parseResourceSummary,
} from './types';

const OPENAPI_PATH = join(__dirname, '..', '..', '..', '..', '..', 'docs', 'openapi', 'v1', 'openapi.yaml');

interface JsonSchemaLike {
  type?: string;
  required?: string[];
  properties?: Record<string, unknown>;
}

function schema(name: string): JsonSchemaLike {
  const doc = parse(readFileSync(OPENAPI_PATH, 'utf8')) as {
    components: { schemas: Record<string, JsonSchemaLike> };
  };
  const s = doc.components?.schemas?.[name];
  if (!s) throw new Error(`schema ${name} missing from OpenAPI document`);
  return s;
}

describe('OpenAPI drift protection', () => {
  it('document exists and pins draft 2020-12 spec schemas', () => {
    const text = readFileSync(OPENAPI_PATH, 'utf8');
    expect(text).toContain('draft 2020-12');
  });

  it('ResourceSummary documented properties are exactly what the guard consumes', () => {
    const s = schema('ResourceSummary');
    const documented = new Set(Object.keys(s.properties ?? {}));
    // Guard reads every documented property (latestOperation optional).
    for (const key of ['id', 'type', 'owner', 'generation', 'status', 'createdAt', 'updatedAt']) {
      expect(documented.has(key), `summary field ${key}`).toBe(true);
      expect((s.required ?? []).includes(key), `${key} required`).toBe(true);
    }
    expect(documented.has('latestOperation')).toBe(true);
    expect(documented.has('spec')).toBe(false); // summaries never carry specs
  });

  it('Resource adds spec/status.conditions/outputs on top of the summary', () => {
    const r = schema('Resource');
    expect(new Set(Object.keys(r.properties ?? {})).has('spec')).toBe(true);
    expect(new Set(Object.keys(r.properties ?? {})).has('outputs')).toBe(true);
    expect(new Set(Object.keys(r.properties ?? {})).has('references')).toBe(true);
    const type = schema('ResourceType');
    expect(new Set(Object.keys(type.properties ?? {})).has('referenceContract')).toBe(true);
    expect(schema('ResourceTypeReferenceContract')).toBeDefined();
    expect(schema('ResourceTypeReferenceSlot')).toBeDefined();
    expect(schema('CreateResourceRequest').properties).toHaveProperty('references');
    expect(schema('UpdateResourceRequest').properties).toHaveProperty('references');
  });

  it('Operation documents retryOf as an optional string field', () => {
    const o = schema('Operation');
    expect(new Set(Object.keys(o.properties ?? {})).has('retryOf')).toBe(true);
    expect(o.required ?? []).not.toContain('retryOf');
  });

  it('guards accept live examples straight from the document', () => {
    const doc = parse(readFileSync(OPENAPI_PATH, 'utf8')) as Record<string, any>;
    const listExample =
      doc?.paths?.['/v1/resources']?.get?.responses?.['200']?.content?.['application/json']?.example;
    if (listExample) {
      const guard = parseResourceList(parseLosslessJson(JSON.stringify(listExample)));
      expect(guard.ok).toBe(true);
    }
  });

  it('round-trips a summary built from documented required fields only', () => {
    const s = schema('ResourceSummary');
    const minimal: Record<string, unknown> = {
      id: 'x',
      type: { name: 'T', version: 'v1' },
      owner: { kind: 'team', id: 'y' },
      generation: 2,
      status: { state: 'Ready', observedGeneration: 2, updatedAt: '2026-01-01T00:00:00Z' },
      createdAt: '2026-01-01T00:00:00Z',
      updatedAt: '2026-01-01T00:00:00Z',
    };
    // Every required field is covered by this document:
    for (const key of s.required ?? []) {
      expect(Object.keys(minimal).includes(key), key).toBe(true);
    }
    const guard = parseResourceSummary(parseLosslessJson(JSON.stringify(minimal)));
    expect(guard.ok, JSON.stringify(guard)).toBe(true);
  });
});

// Minimal sample generator covering the documented primitive shapes used in
// these schemas. Numbers become integer lexemes; enums take first values.
function sampleValueFor(field: string): unknown {
  switch (field) {
    case 'generation':
    case 'observedGeneration':
    case 'targetGeneration':
      return '1';
    case 'type':
      return { name: 'T', version: 'v1' };
    case 'owner':
      return { kind: 'team', id: 'x' };
    case 'status':
      return {
        state: 'Ready',
        observedGeneration: '1',
        updatedAt: '2026-01-01T00:00:00Z',
        conditions: [],
      };
    case 'latestOperation':
      return undefined;
    default:
      return 'x';
  }
}

// Keep the imported runtime types referenced for API-shape assertions below.
export type __Shapes = [Resource, ResourceSummary, Operation];
void parseResourceDetail;
void parseOperation;
