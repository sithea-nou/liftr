/**
 * Typed, validated BFF configuration.
 *
 * Parsing is framework-free: the injected reader resolves dot-paths relative
 * to the "liftr" prefix, so this module tests without @backstage/config. The
 * glue in plugin.ts adapts Backstage config to it. Startup validation is
 * fail-closed: production modes require HTTPS origins; insecure development
 * mode requires an explicit literal-loopback HTTP origin and can never target
 * anything else (adversarial 15).
 */

import { Origin, SubjectBindingConfig, parseOrigin } from '@liftr/plugin-liftr-common';

export type LiftrAuthMode = 'delegated' | 'passthrough' | 'insecure-development';

export interface DelegatedAuthConfig {
  mode: 'delegated';
  tokenEndpoint: string;
  clientId: string;
  clientSecret: string;
  clientAuthMethod: 'basic' | 'body';
  audience?: string;
  resource?: string;
  subjectTokenType:
    | 'urn:ietf:params:oauth:token-type:access_token'
    | 'urn:ietf:params:oauth:token-type:id_token'
    | 'urn:ietf:params:oauth:token-type:jwt';
  assertionIssuer: string;
  binding: SubjectBindingConfig;
  exchangeTimeoutMs: number;
  /**
   * Optional provider of a REAL actor security token (RFC 8693 §1.1).
   * OAuth client authentication (clientId/clientSecret) is entirely separate
   * from actor tokens; a client secret is never synthesized into actor_token.
   */
  actorTokenProvider?: () => Promise<string>;
}

export interface PassthroughAuthConfig {
  mode: 'passthrough';
}

export interface InsecureDevAuthConfig {
  mode: 'insecure-development';
}

export type LiftrAuthConfig =
  | DelegatedAuthConfig
  | PassthroughAuthConfig
  | InsecureDevAuthConfig;

export interface LiftrBackendConfig {
  origin: Origin;
  baseUrl: string;
  auth: LiftrAuthConfig;
  requestTimeoutMs: number;
  maxResponseBytes: number;
  correlationMaxLength: number;
  maxRequestBodyBytes: number;
}

export class ConfigError extends Error {}

/** Resolves dot-paths relative to the liftr.* configuration root. */
export type ConfigReader = (path: string) => unknown | undefined;

function reqString(read: ConfigReader, path: string): string {
  const v = read(path);
  if (typeof v !== 'string' || v.trim() === '') {
    throw new ConfigError(`liftr.${path} must be a non-empty string`);
  }
  return v;
}

function optString(read: ConfigReader, path: string): string | undefined {
  const v = read(path);
  if (v === undefined || v === null) return undefined;
  if (typeof v !== 'string') throw new ConfigError(`liftr.${path} must be a string`);
  return v;
}

function optInt(
  read: ConfigReader,
  path: string,
  min: number,
  max: number,
  fallback: number,
): number {
  const v = read(path);
  if (v === undefined) return fallback;
  if (typeof v !== 'number' || !Number.isInteger(v) || v < min || v > max) {
    throw new ConfigError(`liftr.${path} must be an integer between ${min} and ${max}`);
  }
  return v;
}

const LITERAL_LOOPBACK = /^(localhost|\[::1\]|::1|127(\.[0-9]{1,3}){3})$/;

function isLoopbackHost(host: string): boolean {
  const h = host.toLowerCase();
  if (!LITERAL_LOOPBACK.test(h)) return false;
  if (/^127\./.test(h)) {
    return h.split('.').slice(1).every(p => Number(p) <= 255);
  }
  return true;
}

function parseBinding(read: ConfigReader): SubjectBindingConfig {
  const s = optString(read, 'auth.binding.strategy') ?? 'claim';
  if (s === 'claim') {
    const trustedIssuer = reqString(read, 'auth.binding.trustedIssuer');
    const claimName = optString(read, 'auth.binding.claimName') ?? 'backstage_user_ref';
    return { strategy: 'claim', claimName, trustedIssuer };
  }
  if (s === 'static') {
    const raw = read('auth.binding.entries');
    if (!Array.isArray(raw) || raw.length === 0) {
      throw new ConfigError('liftr.auth.binding.entries must be a non-empty array');
    }
    const entries: Array<{ backstageRef: string; issuer: string; subject: string }> = [];
    for (const e of raw) {
      if (typeof e !== 'object' || e === null) {
        throw new ConfigError('each binding entry must be an object');
      }
      const o = e as Record<string, unknown>;
      for (const k of ['backstageRef', 'issuer', 'subject']) {
        if (typeof o[k] !== 'string' || (o[k] as string).length === 0) {
          throw new ConfigError(`binding entry.${k} must be a non-empty string`);
        }
      }
      entries.push({
        backstageRef: o['backstageRef'] as string,
        issuer: o['issuer'] as string,
        subject: o['subject'] as string,
      });
    }
    return { strategy: 'static', entries };
  }
  throw new ConfigError(
    'liftr.auth.binding.strategy must be "claim" or "static"; bound-user delegation cannot disable subject binding',
  );
}

function parseDelegated(read: ConfigReader): DelegatedAuthConfig {
  const tokenEndpoint = reqString(read, 'auth.tokenEndpoint');
  let endpointUrl: URL;
  try {
    endpointUrl = new URL(tokenEndpoint);
  } catch {
    throw new ConfigError('liftr.auth.tokenEndpoint must be a valid URL');
  }
  if (endpointUrl.protocol !== 'https:') {
    const host = endpointUrl.hostname.toLowerCase();
    // Plaintext STS endpoints only on literal loopback (local dev STS).
    if (!isLoopbackHost(host)) {
      throw new ConfigError('liftr.auth.tokenEndpoint must be HTTPS except on literal loopback');
    }
  }
  if (endpointUrl.search !== '' || endpointUrl.hash !== '' || endpointUrl.username || endpointUrl.password) {
    throw new ConfigError('liftr.auth.tokenEndpoint must not contain query/fragment/userinfo');
  }
  const clientId = reqString(read, 'auth.clientId');
  const clientSecret = reqString(read, 'auth.clientSecret');
  const clientAuthMethod = optString(read, 'auth.clientAuthMethod') ?? 'basic';
  if (clientAuthMethod !== 'basic' && clientAuthMethod !== 'body') {
    throw new ConfigError('liftr.auth.clientAuthMethod must be "basic" or "body"');
  }
  const audience = optString(read, 'auth.audience');
  const resource = optString(read, 'auth.resource');
  if ((audience === undefined) === (resource === undefined)) {
    throw new ConfigError('configure exactly one of liftr.auth.audience or liftr.auth.resource');
  }
  const allowedSTT = [
    'urn:ietf:params:oauth:token-type:access_token',
    'urn:ietf:params:oauth:token-type:id_token',
    'urn:ietf:params:oauth:token-type:jwt',
  ];
  const subjectTokenType = (optString(read, 'auth.subjectTokenType') ?? allowedSTT[0]) as DelegatedAuthConfig['subjectTokenType'];
  if (!(allowedSTT as string[]).includes(subjectTokenType)) {
    throw new ConfigError(`liftr.auth.subjectTokenType must be one of: ${allowedSTT.join(', ')}`);
  }
  const assertionIssuer = reqString(read, 'auth.assertionIssuer');
  return {
    mode: 'delegated',
    tokenEndpoint: tokenEndpoint.replace(/\/$/, ''),
    clientId,
    clientSecret,
    clientAuthMethod: clientAuthMethod as 'basic' | 'body',
    ...(audience !== undefined ? { audience } : { resource: resource! }),
    subjectTokenType,
    assertionIssuer,
    binding: parseBinding(read),
    exchangeTimeoutMs: optInt(read, 'auth.exchangeTimeoutMs', 1000, 60000, 10000),
  };
}

export function parseLiftrBackendConfig(read: ConfigReader): LiftrBackendConfig {
  const baseUrl = reqString(read, 'baseUrl');
  const modeRaw = optString(read, 'auth.mode') ?? 'delegated';

  let auth: LiftrAuthConfig;
  switch (modeRaw) {
    case 'insecure-development': {
      let u: URL;
      try {
        u = new URL(baseUrl);
      } catch {
        throw new ConfigError('liftr.baseUrl must be a valid URL');
      }
      if (u.protocol !== 'http:') {
        throw new ConfigError(
          'liftr.auth.mode=insecure-development requires a plaintext HTTP liftr.baseUrl',
        );
      }
      if (!isLoopbackHost(u.hostname)) {
        throw new ConfigError(
          'liftr.auth.mode=insecure-development may only target literal loopback hosts (localhost / 127.0.0.0/8 / ::1); remote or hostname-based targets are refused',
        );
      }
      auth = { mode: 'insecure-development' };
      break;
    }
    case 'delegated':
      auth = parseDelegated(read);
      break;
    case 'passthrough':
      auth = { mode: 'passthrough' };
      break;
    default:
      throw new ConfigError(
        `liftr.auth.mode "${modeRaw}" is unsupported (use delegated, passthrough, or insecure-development)`,
      );
  }

  const originResult = parseOrigin(baseUrl, {
    allowInsecureLoopback: auth.mode === 'insecure-development',
  });
  if (!originResult.ok) {
    throw new ConfigError(`liftr.baseUrl invalid: ${originResult.reason}`);
  }

  return {
    origin: originResult.origin,
    baseUrl,
    auth,
    requestTimeoutMs: optInt(read, 'requestTimeoutMs', 1000, 120000, 30000),
    maxResponseBytes: optInt(read, 'maxResponseBytes', 1024, 33554432, 4 * 1024 * 1024),
    correlationMaxLength: optInt(read, 'correlationMaxLength', 16, 512, 128),
    maxRequestBodyBytes: optInt(read, 'maxRequestBodyBytes', 1024, 2 * 1024 * 1024, 1024 * 1024),
  };
}
