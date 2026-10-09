# syntax=docker/dockerfile:1
# The module backend image: the Go binary only. Spark itself is not in this image; it runs only in
# each run's driver and executor pods, from the Spark runtime image (docs/design-v0.md item 1).
# Every base image is pinned by digest (resolved 2026-10-09; test/contract checks it).

FROM golang:1.26-bookworm@sha256:d9c68c2c51161e12fd77e4c6320687c9cd86e1af1e3ad6e6cd63ff970641453c AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/booth-spark ./cmd/spark

# Distroless static + nonroot (uid/gid 65532): no shell, no package manager; the chart pins the
# same uid.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/booth-spark /booth-spark
USER nonroot:nonroot
ENTRYPOINT ["/booth-spark"]
