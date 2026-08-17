# Patch-level pin: the image sets GOTOOLCHAIN=local, so this base must be at
# least the version go.mod requires. See fold.Dockerfile.
FROM golang:1.26.6@sha256:0d1d3a794be25f809dd2cb3160d8c73276c4056a9f8242a138e908ddeee7b6b6 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /fold-discovery ./cmd/fold-discovery

# distroless/static ships CA certificates and a nonroot user; the
# Kubernetes API CA is mounted from the service account either way.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:1b7b9f0f0e0a1d2155f531db587cc48ec26aaf97ab64364225f5bf18a054e66a
COPY --from=build /fold-discovery /fold-discovery
EXPOSE 8090
ENTRYPOINT ["/fold-discovery"]
