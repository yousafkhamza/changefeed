# syntax=docker/dockerfile:1.7
# Multi-arch, CGO-free build into a distroless, non-root image (~20 MB).

FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG TARGETOS TARGETARCH
ARG VERSION=dev COMMIT=none DATE=unknown
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags="-s -w -X github.com/yousafkhamza/changefeed/internal/version.Version=${VERSION} -X github.com/yousafkhamza/changefeed/internal/version.Commit=${COMMIT} -X github.com/yousafkhamza/changefeed/internal/version.Date=${DATE}" \
      -o /out/changefeed ./cmd/changefeed \
 && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.source="https://github.com/yousafkhamza/changefeed" \
      org.opencontainers.image.description="One timeline of every production change, with ranked likely causes" \
      org.opencontainers.image.licenses="Apache-2.0"
COPY --from=build /out/changefeed /changefeed
COPY --from=build --chown=65532:65532 /out/data /data
ENV CHANGEFEED_DB=/data/changefeed.db
USER 65532:65532
EXPOSE 8080
VOLUME ["/data"]
ENTRYPOINT ["/changefeed"]
CMD ["serve"]
