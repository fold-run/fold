# Patch-level pin: the image sets GOTOOLCHAIN=local, so this base must be at
# least the version go.mod requires. See fold.Dockerfile.
FROM golang:1.26.6@sha256:640a234f4bea3e399c056b7b8f9c667c4939befae8db2f14e9785e16eccd4205 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /fold-registry ./cmd/fold-registry

# distroless/static ships CA certificates, which this one genuinely needs:
# it reaches a public registry over TLS rather than the in-cluster API.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:1b7b9f0f0e0a1d2155f531db587cc48ec26aaf97ab64364225f5bf18a054e66a
ARG VERSION=dev
ARG REVISION=unknown
LABEL org.opencontainers.image.title="fold-registry" \
      org.opencontainers.image.description="fold's MCP Registry discovery-document producer: an allowlist of registry entries becomes gateway upstreams." \
      org.opencontainers.image.source="https://github.com/fold-run/fold" \
      org.opencontainers.image.url="https://fold.run" \
      org.opencontainers.image.documentation="https://github.com/fold-run/fold/blob/main/docs/registry-discovery.md" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}"
COPY --from=build /fold-registry /fold-registry
EXPOSE 8091
ENTRYPOINT ["/fold-registry"]
