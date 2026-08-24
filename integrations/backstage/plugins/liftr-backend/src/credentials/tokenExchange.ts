/**
 * RFC 8693 token-exchange credential provider (reference implementation).
 *
 * Flow per request (Correction 1 semantics):
 *   1. decode the delegation assertion (unverified locally — the STS and
 *      Liftr perform all signature validation);
 *   2. resolve the trusted subject binding for the authenticated Backstage
 *      user; any mismatch/malformation/unavailable lookup fails closed;
 *   3. exchange at the configured STS:
 *        - OAuth CLIENT authentication via clientId/clientSecret
 *          (basic header or body parameters) — this is NOT actor_token;
 *        - subject_token = the user's delegation assertion;
 *        - audience OR resource = Liftr, as configured for STS support;
 *        - actor_token ONLY when a real actor-token provider is configured.
 *          A client secret is never synthesized into an actor token;
 *   4. verify the exchanged token names exactly the bound issuer+subject
 *      before it is used; mismatches fail closed;
 *   5. return a memory-only credential.
 *
 * Token material never appears in errors or logs: log events carry outcome
 * categories only.
 */

import {
  BffError,
  SubjectIdentity,
  decodeJwtPayloadUnverified,
  resolveSubjectBinding,
  verifyExchangedSubject,
} from '@liftr/plugin-liftr-common';
import { DelegatedAuthConfig } from '../config';
import {
  AcquiredCredential,
  DelegatedCredentialRequest,
  LiftrCredentialProvider,
} from './provider';

export interface CredentialLogSink {
  /** Structured, secret-free event sink. */
  event(fields: Record<string, string | number | boolean>): void;
}

const MAX_TOKEN_RESPONSE_BYTES = 64 * 1024;
/** Refresh margin: treat tokens as expiring slightly early. */
const EXPIRY_MARGIN_MS = 30_000;

export class TokenExchangeCredentialProvider implements LiftrCredentialProvider {
  readonly identityAuthority = 'bound-backstage-user' as const;
  readonly supportsServerSideReacquisition = true as const;

  constructor(
    private readonly config: DelegatedAuthConfig,
    private readonly fetchImpl: typeof fetch,
    private readonly log: CredentialLogSink,
    private readonly now: () => number = () => Date.now(),
  ) {}

  async acquire(
    request: DelegatedCredentialRequest,
    ctx: { correlationId: string },
  ): Promise<AcquiredCredential> {
    // 1-2. Binding resolution (fail closed on everything).
    const decoded = decodeJwtPayloadUnverified(request.delegationAssertion);
    if (!decoded.ok) {
      this.log.event({
        event: 'binding_rejected',
        reason: `assertion_${decoded.reason}`,
        correlationId: ctx.correlationId,
      });
      throw new BffError({
        code: 'LIFTR_SUBJECT_BINDING_REJECTED',
        title: 'Delegation rejected',
        detail: 'the delegation assertion could not be interpreted; access to Liftr is denied',
        correlationId: ctx.correlationId,
      });
    }
    const binding = resolveSubjectBinding(
      this.config.binding,
      request.backstageUserEntityRef,
      decoded.value,
    );
    if (!binding.ok) {
      this.log.event({
        event: 'binding_rejected',
        reason: binding.reason,
        correlationId: ctx.correlationId,
        backstageRef: request.backstageUserEntityRef,
      });
      throw new BffError({
        code: 'LIFTR_SUBJECT_BINDING_REJECTED',
        title: 'Delegation rejected',
        detail:
          'the delegation assertion is not bound to your Backstage identity; access to Liftr is denied',
        correlationId: ctx.correlationId,
      });
    }

    // 3. Exchange. Client auth and subject/actor inputs kept strictly apart.
    let response: Response;
    try {
      response = await this.fetchImpl(this.config.tokenEndpoint, {
        method: 'POST',
        redirect: 'error',
        signal: AbortSignal.timeout(this.config.exchangeTimeoutMs),
        headers: await this.buildRequestHeaders(),
        body: await this.buildRequestBody(request.delegationAssertion),
      });
    } catch (e) {
      const timedOut = e instanceof Error && e.name === 'TimeoutError';
      this.log.event({
        event: 'exchange_failed',
        reason: timedOut ? 'timeout' : 'network',
        correlationId: ctx.correlationId,
      });
      throw new BffError({
        code: 'LIFTR_DELEGATION_UNAVAILABLE',
        title: 'Unable to obtain access to Liftr',
        detail: timedOut
          ? 'the authorization server did not answer in time'
          : 'the authorization server could not be reached',
        correlationId: ctx.correlationId,
      });
    }
    if (!response.ok) {
      this.log.event({
        event: 'exchange_failed',
        reason: 'sts-error',
        stsStatus: response.status,
        correlationId: ctx.correlationId,
      });
      throw new BffError({
        code: 'LIFTR_DELEGATION_UNAVAILABLE',
        title: 'Unable to obtain access to Liftr',
        detail: 'the authorization server refused the delegated token request',
        correlationId: ctx.correlationId,
        upstreamStatus: response.status,
      });
    }

    // Enforce response size before parsing.
    const text = await readBounded(response, MAX_TOKEN_RESPONSE_BYTES);
    let parsed: { access_token?: unknown; expires_in?: unknown };
    try {
      parsed = JSON.parse(text);
    } catch {
      this.log.event({ event: 'exchange_failed', reason: 'bad-sts-body', correlationId: ctx.correlationId });
      throw new BffError({
        code: 'LIFTR_DELEGATION_UNAVAILABLE',
        title: 'Unable to obtain access to Liftr',
        detail: 'the authorization server returned an unreadable response',
        correlationId: ctx.correlationId,
      });
    }
    if (typeof parsed['access_token'] !== 'string' || parsed['access_token'].length === 0) {
      this.log.event({ event: 'exchange_failed', reason: 'no-access-token', correlationId: ctx.correlationId });
      throw new BffError({
        code: 'LIFTR_DELEGATION_UNAVAILABLE',
        title: 'Unable to obtain access to Liftr',
        detail: 'the authorization server returned no access token',
        correlationId: ctx.correlationId,
      });
    }
    const token = parsed['access_token'];
    if (new TextEncoder().encode(token).length > 8192) {
      // Mirrors Liftr's bearer ceiling; oversized credentials are useless.
      this.log.event({ event: 'exchange_failed', reason: 'oversized-token', correlationId: ctx.correlationId });
      throw new BffError({
        code: 'LIFTR_SUBJECT_BINDING_REJECTED',
        title: 'Delegation rejected',
        detail: 'the issued Liftr credential exceeded the accepted size ceiling',
        correlationId: ctx.correlationId,
      });
    }

    // 4. Exchanged-subject consistency check (fail closed).
    const check = verifyExchangedSubject(token, binding.expected as SubjectIdentity);
    if (!check.ok) {
      this.log.event({
        event: 'binding_rejected',
        reason: check.reason,
        correlationId: ctx.correlationId,
        backstageRef: request.backstageUserEntityRef,
      });
      throw new BffError({
        code: 'LIFTR_SUBJECT_BINDING_REJECTED',
        title: 'Delegation rejected',
        detail: 'the issued Liftr credential does not match your bound identity; access to Liftr is denied',
        correlationId: ctx.correlationId,
      });
    }

    const expiresIn =
      typeof parsed['expires_in'] === 'number' && parsed['expires_in'] > 0
        ? parsed['expires_in'] * 1000
        : 120_000;
    this.log.event({ event: 'token_acquired', correlationId: ctx.correlationId });

    return {
      token,
      expiresAtEpochMs: this.now() + Math.max(expiresIn - EXPIRY_MARGIN_MS, 1000),
    };
  }

  private async buildRequestHeaders(): Promise<Record<string, string>> {
    const headers: Record<string, string> = {
      'Content-Type': 'application/x-www-form-urlencoded',
      Accept: 'application/json',
    };
    if (this.config.clientAuthMethod === 'basic') {
      const cred = Buffer.from(`${this.config.clientId}:${this.config.clientSecret}`, 'utf8');
      headers['Authorization'] = `Basic ${cred.toString('base64')}`;
    }
    return headers;
  }

  private async buildRequestBody(subjectToken: string): Promise<string> {
    const form = new URLSearchParams();
    form.set('grant_type', 'urn:ietf:params:oauth:grant-type:token-exchange');
    form.set('subject_token', subjectToken);
    form.set('subject_token_type', this.config.subjectTokenType);
    if (this.config.audience !== undefined) {
      form.set('audience', this.config.audience);
    } else {
      form.set('resource', this.config.resource!);
    }
    if (this.config.clientAuthMethod === 'body') {
      form.set('client_id', this.config.clientId);
      form.set('client_secret', this.config.clientSecret);
    }
    // RFC 8693 actor_token: sent ONLY when a real actor security token is
    // provided by deployment configuration. Never derived from client auth.
    if (this.config.actorTokenProvider !== undefined) {
      form.set('actor_token', await this.config.actorTokenProvider());
      form.set('actor_token_type', 'urn:ietf:params:oauth:token-type:access_token');
    }
    return form.toString();
  }
}

async function readBounded(response: Response, maxBytes: number): Promise<string> {
  const reader = response.body?.getReader();
  if (!reader) return '';
  const chunks: Uint8Array[] = [];
  let total = 0;
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    total += value.byteLength;
    if (total > maxBytes) {
      void reader.cancel();
      throw new BffError({
        code: 'LIFTR_DELEGATION_UNAVAILABLE',
        title: 'Unable to obtain access to Liftr',
        detail: 'the authorization server response exceeded the size bound',
      });
    }
    chunks.push(value);
  }
  const merged = new Uint8Array(total);
  let offset = 0;
  for (const c of chunks) {
    merged.set(c, offset);
    offset += c.byteLength;
  }
  return new TextDecoder().decode(merged);
}
