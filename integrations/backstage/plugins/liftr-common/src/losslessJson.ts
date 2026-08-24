/**
 * Lossless JSON representation for Liftr public payloads.
 *
 * Liftr preserves numeric lexical form end to end: `20` and `20.0` are
 * distinct spec values with distinct admission fingerprints (ADR-0008 /
 * ADR-0009). A client that routes payload numbers through IEEE doubles would
 * silently rewrite developer intent (`20.0` becomes `20`). This module is the
 * integration's single pinned lossless-number strategy: parsing produces
 * `LosslessNumber` nodes that keep the exact source lexeme, and serialization
 * writes those lexemes back verbatim.
 *
 * This module is hand-rolled and versioned with the repository on purpose:
 * it has zero dependencies, deterministic behavior pinned by tests in this
 * package, and it must never be swapped for a normalizing JSON parser without
 * re-proving the numeric-fidelity tests.
 */

export const MAX_JSON_DEPTH = 64;

/** A number kept exactly as it appeared in the source document. */
export class LosslessNumber {
  readonly kind = 'lossless-number' as const;
  constructor(readonly value: string) {
    if (!/^(-?)(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$/.test(value)) {
      throw new Error(`invalid number lexeme: ${value}`);
    }
  }

  /** True when the lexeme carries no fraction or exponent (an integer literal). */
  isIntegerLexeme(): boolean {
    return !/[.eE]/.test(this.value);
  }

  /** Exact integer value when the lexeme is an integer literal; undefined otherwise. */
  toBigInt(): bigint | undefined {
    if (!this.isIntegerLexeme()) return undefined;
    try {
      return BigInt(this.value);
    } catch {
      return undefined;
    }
  }

  /**
   * Convenience for display-only contexts. Lossy above 2^53; never use the
   * result to construct outgoing Liftr requests.
   */
  unsafeNumber(): number {
    return Number(this.value);
  }

  toString(): string {
    return this.value;
  }
}

export function isLosslessNumber(v: unknown): v is LosslessNumber {
  return v instanceof LosslessNumber;
}

export type JsonPrimitive = string | boolean | null | LosslessNumber;
export interface JsonObject { [key: string]: JsonValue }
export type JsonArray = JsonValue[];
export type JsonValue = JsonPrimitive | JsonArray | JsonObject;

export class JsonParseError extends Error {}

interface ParseState {
  text: string;
  pos: number;
}

export function parseLosslessJson(text: string): JsonValue {
  const s: ParseState = { text, pos: 0 };
  skipWhitespace(s);
  const v = parseValue(s, 0);
  skipWhitespace(s);
  if (s.pos !== s.text.length) {
    throw new JsonParseError('unexpected trailing content');
  }
  return v;
}

function skipWhitespace(s: ParseState): void {
  while (s.pos < s.text.length) {
    const c = s.text.charCodeAt(s.pos);
    if (c === 0x20 || c === 0x09 || c === 0x0a || c === 0x0d) {
      s.pos++;
    } else {
      break;
    }
  }
}

function peek(s: ParseState): string {
  if (s.pos >= s.text.length) throw new JsonParseError('unexpected end of input');
  return s.text[s.pos];
}

function expect(s: ParseState, ch: string): void {
  if (peek(s) !== ch) throw new JsonParseError(`expected '${ch}'`);
  s.pos++;
}

function parseValue(s: ParseState, depth: number): JsonValue {
  if (depth > MAX_JSON_DEPTH) throw new JsonParseError('maximum nesting depth exceeded');
  switch (peek(s)) {
    case '{':
      return parseObject(s, depth);
    case '[':
      return parseArray(s, depth);
    case '"':
      return parseString(s);
    case 't':
      consumeLiteral(s, 'true');
      return true;
    case 'f':
      consumeLiteral(s, 'false');
      return false;
    case 'n':
      consumeLiteral(s, 'null');
      return null;
    default:
      return parseNumber(s);
  }
}

function consumeLiteral(s: ParseState, lit: string): void {
  if (s.text.substr(s.pos, lit.length) !== lit) {
    throw new JsonParseError(`invalid literal at ${s.pos}`);
  }
  s.pos += lit.length;
}

function parseObject(s: ParseState, depth: number): JsonObject {
  expect(s, '{');
  const obj: JsonObject = {};
  skipWhitespace(s);
  if (peek(s) === '}') {
    s.pos++;
    return obj;
  }
  for (;;) {
    skipWhitespace(s);
    const key = parseString(s);
    skipWhitespace(s);
    expect(s, ':');
    skipWhitespace(s);
    const value = parseValue(s, depth + 1);
    if (Object.prototype.hasOwnProperty.call(obj, key)) {
      throw new JsonParseError(`duplicate object key: ${key}`);
    }
    obj[key] = value;
    skipWhitespace(s);
    const c = peek(s);
    if (c === ',') {
      s.pos++;
      continue;
    }
    if (c === '}') {
      s.pos++;
      return obj;
    }
    throw new JsonParseError("expected ',' or '}'");
  }
}

function parseArray(s: ParseState, depth: number): JsonArray {
  expect(s, '[');
  const arr: JsonArray = [];
  skipWhitespace(s);
  if (peek(s) === ']') {
    s.pos++;
    return arr;
  }
  for (;;) {
    skipWhitespace(s);
    arr.push(parseValue(s, depth + 1));
    skipWhitespace(s);
    const c = peek(s);
    if (c === ',') {
      s.pos++;
      continue;
    }
    if (c === ']') {
      s.pos++;
      return arr;
    }
    throw new JsonParseError("expected ',' or ']'");
  }
}

function parseString(s: ParseState): string {
  expect(s, '"');
  let out = '';
  for (;;) {
    if (s.pos >= s.text.length) throw new JsonParseError('unterminated string');
    const c = s.text[s.pos];
    if (c === '"') {
      s.pos++;
      return out;
    }
    if (c === '\\') {
      s.pos++;
      if (s.pos >= s.text.length) throw new JsonParseError('unterminated escape');
      const e = s.text[s.pos];
      switch (e) {
        case '"': out += '"'; break;
        case '\\': out += '\\'; break;
        case '/': out += '/'; break;
        case 'b': out += '\b'; break;
        case 'f': out += '\f'; break;
        case 'n': out += '\n'; break;
        case 'r': out += '\r'; break;
        case 't': out += '\t'; break;
        case 'u': {
          const hex = s.text.substr(s.pos + 1, 4);
          if (!/^[0-9a-fA-F]{4}$/.test(hex)) throw new JsonParseError('invalid \\u escape');
          let code = parseInt(hex, 16);
          s.pos += 4;
          if (code >= 0xd800 && code <= 0xdbff) {
            // High surrogate: require a following \uXXXX low surrogate.
            if (s.text[s.pos + 1] === '\\' && s.text[s.pos + 2] === 'u') {
              const hex2 = s.text.substr(s.pos + 3, 4);
              if (/^[0-9a-fA-F]{4}$/.test(hex2)) {
                const low = parseInt(hex2, 16);
                if (low >= 0xdc00 && low <= 0xdfff) {
                  code = 0x10000 + ((code - 0xd800) << 10) + (low - 0xdc00);
                  s.pos += 6;
                } else {
                  throw new JsonParseError('invalid low surrogate');
                }
              } else {
                throw new JsonParseError('invalid surrogate escape');
              }
            } else {
              throw new JsonParseError('lone high surrogate');
            }
          } else if (code >= 0xdc00 && code <= 0xdfff) {
            throw new JsonParseError('lone low surrogate');
          }
          out += String.fromCodePoint(code);
          break;
        }
        default:
          throw new JsonParseError(`invalid escape '\\${e}'`);
      }
      s.pos++;
      continue;
    }
    const code = s.text.charCodeAt(s.pos);
    if (code < 0x20) throw new JsonParseError('control character in string');
    out += c;
    s.pos++;
  }
}

const NUMBER_START = /[-0-9]/;

function parseNumber(s: ParseState): LosslessNumber {
  const start = s.pos;
  const c = peek(s);
  if (!NUMBER_START.test(c)) throw new JsonParseError(`unexpected character '${c}'`);
  if (s.text[s.pos] === '-') s.pos++;
  // int part: 0 or [1-9][0-9]*
  if (s.text[s.pos] === '0') {
    s.pos++;
  } else if (/[1-9]/.test(s.text[s.pos] ?? '')) {
    while (/[0-9]/.test(s.text[s.pos] ?? '')) s.pos++;
  } else {
    throw new JsonParseError('invalid number');
  }
  if (s.text[s.pos] === '.') {
    s.pos++;
    if (!/[0-9]/.test(s.text[s.pos] ?? '')) throw new JsonParseError('invalid fraction');
    while (/[0-9]/.test(s.text[s.pos] ?? '')) s.pos++;
  }
  if (s.text[s.pos] === 'e' || s.text[s.pos] === 'E') {
    s.pos++;
    if (s.text[s.pos] === '+' || s.text[s.pos] === '-') s.pos++;
    if (!/[0-9]/.test(s.text[s.pos] ?? '')) throw new JsonParseError('invalid exponent');
    while (/[0-9]/.test(s.text[s.pos] ?? '')) s.pos++;
  }
  return new LosslessNumber(s.text.slice(start, s.pos));
}

/** Serialize a lossless JSON tree, writing LosslessNumber lexemes verbatim. */
export function stringifyLosslessJson(value: JsonValue): string {
  return serialize(value);
}

function serialize(v: JsonValue): string {
  if (v === null) return 'null';
  if (typeof v === 'boolean') return v ? 'true' : 'false';
  if (typeof v === 'string') return quoteString(v);
  if (isLosslessNumber(v)) return v.value;
  if (Array.isArray(v)) return `[${v.map(serialize).join(',')}]`;
  const parts: string[] = [];
  for (const k of Object.keys(v)) {
    parts.push(`${quoteString(k)}:${serialize(v[k])}`);
  }
  return `{${parts.join(',')}}`;
}

export function quoteString(s: string): string {
  let out = '"';
  for (const ch of s) {
    switch (ch) {
      case '"': out += '\\"'; break;
      case '\\': out += '\\\\'; break;
      case '\b': out += '\\b'; break;
      case '\f': out += '\\f'; break;
      case '\n': out += '\\n'; break;
      case '\r': out += '\\r'; break;
      case '\t': out += '\\t'; break;
      default: {
        const code = ch.codePointAt(0) ?? 0;
        if (code < 0x20) {
          out += '\\u' + code.toString(16).padStart(4, '0');
        } else {
          out += ch;
        }
      }
    }
  }
  return out + '"';
}
