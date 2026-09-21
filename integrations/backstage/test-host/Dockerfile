# syntax=docker/dockerfile:1
# Disposable, non-production Backstage host images for the Liftr demo.

FROM node:25-bookworm-slim AS backstage-build
WORKDIR /app
ENV COREPACK_ENABLE_DOWNLOAD_PROMPT=0 \
    NODE_OPTIONS=--max-old-space-size=4096

# better-sqlite3 may need a native build when no matching prebuild is present.
RUN apt-get update \
    && apt-get install -y --no-install-recommends python3 make g++ \
    && rm -rf /var/lib/apt/lists/* \
    && corepack enable

COPY . .
RUN --mount=type=cache,target=/root/.yarn/berry/cache \
    corepack yarn@4.9.2 install --immutable
RUN corepack yarn@4.9.2 workspace @liftr/testhost-backend build \
    && corepack yarn@4.9.2 workspace @liftr/testhost-app build \
        --config ../../app-config.yaml \
    && grep -Fq '<title>Liftr host fixture</title>' \
        test-host/packages/app/dist/index.html

FROM backstage-build AS backstage-prod-deps
RUN corepack yarn@4.9.2 workspaces focus @liftr/testhost-backend --production

FROM node:25-bookworm-slim AS backstage-backend
WORKDIR /app
ENV NODE_ENV=production

COPY --from=backstage-prod-deps --chown=node:node /app/node_modules ./node_modules
COPY --from=backstage-build --chown=node:node /app/test-host/packages/backend/dist/bundle.tar.gz ./
RUN tar xzf bundle.tar.gz \
    && rm bundle.tar.gz \
    && chmod 0755 /app
COPY --chown=node:node test-host/app-config.yaml test-host/app-config.container.yaml ./test-host/

USER node
EXPOSE 7007
CMD ["node", "test-host/packages/backend", "--config", "test-host/app-config.yaml", "--config", "test-host/app-config.container.yaml"]

FROM nginx:1.27-alpine AS backstage-app
COPY test-host/nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=backstage-build /app/test-host/packages/app/dist /usr/share/nginx/html
EXPOSE 80
