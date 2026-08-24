/**
 * Backend plugin glue for the current Backstage backend system.
 *
 * This module is the ONLY place that touches Backstage services. Everything
 * else in this plugin is framework-free and tested without Backstage.
 *
 * Wiring:
 *   - httpRouter: mounts the mirrored routes under /api/liftr;
 *   - httpAuth: user principals only ({allow:['user']}); service principals
 *     are rejected by both the credential barrier and our pipeline;
 *   - rootConfig: validated once at startup — fail-closed on insecure or
 *     malformed composition;
 *   - logger: structured, secret-free events.
 */

import {
  coreServices,
  createBackendPlugin,
} from '@backstage/backend-plugin-api';
import { LiftrBackendConfig, parseLiftrBackendConfig } from './config';
import { UpstreamForwarder } from './forwarder';
import {
  IncomingRequest,
  LoggerSink,
  RequestAuthenticator,
  RouteDeps,
  handleLiftrProxyRequest,
} from './routes';
import { LiftrCredentialProvider } from './credentials/provider';
import { InsecureDevelopmentCredentialProvider } from './credentials/insecureDev';
import { PassthroughCredentialProvider } from './credentials/passthrough';
import { CredentialLogSink, TokenExchangeCredentialProvider } from './credentials/tokenExchange';

export const liftrPlugin = createBackendPlugin({
  pluginId: 'liftr',
  register(env) {
    env.registerInit({
      deps: {
        httpRouter: coreServices.httpRouter,
        httpAuth: coreServices.httpAuth,
        config: coreServices.rootConfig,
        logger: coreServices.logger,
      },
      async init({ httpRouter, httpAuth, config, logger }) {
        // Reader over scalar leaves relative to the liftr.* root.
        const reader = (path: string): unknown =>
          config.getOptional(`liftr.${path}`);

        const cfg = parseLiftrBackendConfig(reader);

        const log: LoggerSink = {
          event: fields => logger.child(fields as Record<string, string | number | boolean>).info('liftr-bff'),
          error: (message, fields) =>
            logger.child((fields ?? {}) as Record<string, string>).error(message),
        };
        const credLog: CredentialLogSink = { event: fields => log.event(fields) };

        const provider: LiftrCredentialProvider =
          cfg.auth.mode === 'insecure-development'
            ? new InsecureDevelopmentCredentialProvider()
            : cfg.auth.mode === 'passthrough'
              ? new PassthroughCredentialProvider(credLog)
              : new TokenExchangeCredentialProvider(cfg.auth, fetch, credLog);

        const forwarder = new UpstreamForwarder(
          { requestTimeoutMs: cfg.requestTimeoutMs, maxResponseBytes: cfg.maxResponseBytes },
          fetch,
          pathWithQuery => `${cfg.baseUrl}${pathWithQuery}`,
          fields => log.event(fields),
        );

        const authenticator: RequestAuthenticator = {
          async authenticate(rawPlatformRequest) {
            try {
              // Current backend-system API: credentials(request, options).
              const credentials = await httpAuth.credentials(
                rawPlatformRequest as Parameters<typeof httpAuth.credentials>[0],
                { allow: ['user'] },
              );
              if (credentials.principal.type !== 'user') {
                return { ok: false as const, kind: 'service-principal' as const };
              }
              return {
                ok: true as const,
                userEntityRef: credentials.principal.userEntityRef,
              };
            } catch {
              return { ok: false as const, kind: 'unauthenticated' as const };
            }
          },
        };

        const deps: RouteDeps = { config: cfg, provider, forwarder, authenticator, log };
        httpRouter.use(createMirrorHandler({ deps }));
      },
    });
  },
});

/**
 * Bridge the framework-free pipeline onto an Express-compatible handler for
 * coreServices.httpRouter. Body size bounding happens here before parsing.
 */
export function createMirrorHandler(options: {
  deps: RouteDeps;
}): (req: unknown, res: unknown, next?: (e?: unknown) => void) => void {
  interface MinimalReq {
    url?: string;
    method?: string;
    headers?: Record<string, unknown>;
    on(event: string, cb: (arg?: unknown) => void): void;
  }
  const maxBody = options.deps.config.maxRequestBodyBytes;

  return (rawReq, rawRes, next) => {
    void (async () => {
      const req = rawReq as MinimalReq;
      try {
        const chunks: Buffer[] = [];
        let size = 0;
        let overflowed = false;
        req.on('data', (c: unknown) => {
          const buf = c as Buffer;
          size += buf.byteLength;
          if (size <= maxBody) chunks.push(buf);
          else overflowed = true;
        });
        req.on('end', () => {
          void (async () => {
            if (overflowed) {
              resStatus(rawRes, 413);
              resEnd(rawRes, JSON.stringify({
                code: 'LIFTR_REQUEST_INVALID',
                title: 'Body too large',
                detail: 'request body exceeds the configured bound',
              }));
              return;
            }
            const bodyText = Buffer.concat(chunks).toString('utf8');
            const url = new URL(req.url ?? '/', 'http://internal.invalid');
            const headerBag: Record<string, string> = {};
            for (const [k, v] of Object.entries(req.headers ?? {})) {
              if (typeof v === 'string') headerBag[k.toLowerCase()] = v;
              else if (Array.isArray(v) && typeof v[0] === 'string') {
                headerBag[k.toLowerCase()] = v[0];
              }
            }
            const incoming: IncomingRequest = {
              method: (req.method ?? 'GET').toUpperCase(),
              path: url.pathname.replace(/^\/api\/liftr/, '') || '/',
              query: url.searchParams,
              header: name => headerBag[name.toLowerCase()],
              bodyText: () => bodyText,
            };
            const result = await handleLiftrProxyRequest(options.deps, incoming, rawReq);
            resStatus(rawRes, result.status);
            resHeader(rawRes, 'Content-Type', result.contentType);
            resHeader(rawRes, 'Cache-Control', 'no-store');
            resHeader(rawRes, 'X-Liftr-Bff', '1');
            for (const [k, v] of Object.entries(result.headers)) resHeader(rawRes, k, v);
            resEnd(rawRes, result.bodyText);
          })().catch(e => next?.(e));
        });
        req.on('error', () => {
          if (!resHeadersSent(rawRes)) {
            resStatus(rawRes, 400);
            resEnd(rawRes);
          }
        });
      } catch (e) {
        next?.(e);
      }
    })();
  };
}

// Express-agnostic response shims keep this file free of @types/express.
function resStatus(res: unknown, status: number): void {
  (res as { status(s: number): unknown }).status(status);
}
function resHeader(res: unknown, name: string, value: string): void {
  (res as { setHeader(n: string, v: string): void }).setHeader(name, value);
}
function resHeadersSent(res: unknown): boolean {
  return Boolean((res as { headersSent?: boolean }).headersSent);
}
function resEnd(res: unknown, body?: string): void {
  const r = res as { end(b?: string): void };
  if (body !== undefined) r.end(body);
  else r.end();
}

export type { LiftrBackendConfig };
