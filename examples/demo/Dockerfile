# syntax=docker/dockerfile:1
# Liftr DEMO server image — NON-PRODUCTION.
# Builds cmd/liftr-demo-server (deterministic demo provisioner, insecure dev
# auth) plus the OpenAPI documents it serves. Never use for real deployments.

FROM golang:1.25.11-alpine AS build
WORKDIR /src
ENV GOTOOLCHAIN=local CGO_ENABLED=0
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY docs/openapi ./docs/openapi
RUN go build -trimpath -ldflags "-s -w" -o /out/liftr-demo-server ./cmd/liftr-demo-server

FROM alpine:3.20
RUN adduser -D -H liftr-demo
COPY --from=build /out/liftr-demo-server /liftr-demo-server
COPY --from=build /src/docs/openapi /docs/openapi
COPY examples/demo/policy.json /policy.json
USER liftr-demo
EXPOSE 18080 18090 18099
ENTRYPOINT ["/liftr-demo-server"]
