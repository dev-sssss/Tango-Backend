# syntax=docker/dockerfile:1.7
# Builds tango-server for any platform without emulation: Go cross-compiles
# on the build machine, the final image is a static distroless base.
FROM --platform=$BUILDPLATFORM golang:1.24-bookworm AS build
ARG TARGETOS=linux
ARG TARGETARCH=arm64
ARG VERSION=dev
# Unix seconds; the cluster places new matches on the newest build.
ARG BUILD_TIME=0
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X github.com/dev-sssss/tango_backend/server/internal/server.BuildVersion=${VERSION} -X github.com/dev-sssss/tango_backend/server/internal/server.BuildTime=${BUILD_TIME}" \
    -o /out/tango-server ./cmd/tango-server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/tango-server /usr/local/bin/tango-server
USER nonroot:nonroot
EXPOSE 7350 7351 7352 9100
ENTRYPOINT ["/usr/local/bin/tango-server"]
