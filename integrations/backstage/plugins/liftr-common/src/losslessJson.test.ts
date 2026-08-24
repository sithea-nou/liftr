import { describe, expect, it } from 'vitest';
import {
  JsonParseError,
  LosslessNumber,
  isLosslessNumber,
  parseLosslessJson,
  quoteString,
  stringifyLosslessJson,
} from './losslessJson';

describe('lossless JSON numeric fidelity', () => {
  it('preserves integer vs decimal lexemes', () => {
    const v = parseLosslessJson('{"a":20,"b":20.0}') as Record<string, LosslessNumber>;
    expect(v['a']!.value).toBe('20');
    expect(v['b']!.value).toBe('20.0');
    expect(v['a']!.isIntegerLexeme()).toBe(true);
    expect(v['b']!.isIntegerLexeme()).toBe(false);
  });

  it('stringify reproduces lexemes verbatim', () => {
    const text = '{"storageGB":20,"other":20.0,"big":18446744073709551615,"exp":1e3}';
    const out = stringifyLosslessJson(parseLosslessJson(text));
    expect(out).toBe(text);
  });

  it('handles uint64 magnitudes exactly', () => {
    const v = parseLosslessJson('[18446744073709551615]') as LosslessNumber[];
    expect(v[0]!.toBigInt()).toBe((1n << 64n) - 1n);
  });

  it('rejects duplicate keys, trailing content, and depth abuse', () => {
    expect(() => parseLosslessJson('{"a":1,"a":2}')).toThrow(JsonParseError);
    expect(() => parseLosslessJson('{} {}')).toThrow(JsonParseError);
    const deep = '['.repeat(70) + ']'.repeat(70);
    expect(() => parseLosslessJson(deep)).toThrow(/depth/);
  });

  it('rejects malformed numbers and strings', () => {
    expect(() => parseLosslessJson('01')).toThrow();
    expect(() => parseLosslessJson('1.')).toThrow();
    expect(() => parseLosslessJson('.5')).toThrow();
    expect(() => parseLosslessJson('"\\x"')).toThrow();
    expect(() => parseLosslessJson('"unterminated')).toThrow();
  });

  it('round-trips hostile-ish strings safely', () => {
    const s = 'quote" backslash\u0007 newline\n<script>';
    expect(stringifyLosslessJson(quoteString(s) === '' ? null : { s })).toBe(
      '{"s":"quote\\" backslash\\u0007 newline\\n<script>"}',
    );
    const parsed = parseLosslessJson(stringifyLosslessJson({ s })) as { s: string };
    expect(parsed.s).toBe(s);
  });

  it('surrogate pairs survive', () => {
    const v = parseLosslessJson('"\\ud83d\\ude00"') as string;
    expect(v).toBe('\u{1F600}');
    expect(() => parseLosslessJson('"\\ud83d"')).toThrow();
    expect(() => parseLosslessJson('"\\ude00"')).toThrow();
  });

  it('isLosslessNumber discriminates', () => {
    const v = parseLosslessJson('[1,"x"]') as unknown[];
    expect(isLosslessNumber(v[0])).toBe(true);
    expect(isLosslessNumber(v[1])).toBe(false);
  });
});
