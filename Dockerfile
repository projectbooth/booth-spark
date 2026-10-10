# syntax=docker/dockerfile:1
# The module backend image: the Go binary and the module's built UI. Spark itself is not in this image; it runs only in
# each run's driver and executor pods, from the Spark runtime image (docs/design-v0.md item 1).
# Every base image is pinned by digest (resolved 2026-10-09; test/contract checks it).

# The module's own UI (web/): its lint and tests run here, so a failing UI never ships. Built on
# the build machine's platform; its output is the same files for every target.
FROM --platform=$BUILDPLATFORM node:22-bookworm-slim@sha256:c3de60bf2f9dd0ac6370e6117950ff62d6e339527e7472301c9c78a017978392 AS web
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN npm run lint && npm test && npm run build

# The Go build runs on the build machine's own platform and cross-compiles (release.yml builds
# linux/amd64 and linux/arm64).
FROM --platform=$BUILDPLATFORM golang:1.26-bookworm@sha256:d9c68c2c51161e12fd77e4c6320687c9cd86e1af1e3ad6e6cd63ff970641453c AS build
ARG TARGETOS=linux
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/booth-spark ./cmd/spark

# Distroless static + nonroot (uid/gid 65532): no shell, no package manager; the chart pins the
# same uid.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/booth-spark /booth-spark
COPY --from=web /web/dist /web
ENV BOOTH_WEB_DIR=/web
USER nonroot:nonroot
ENTRYPOINT ["/booth-spark"]
