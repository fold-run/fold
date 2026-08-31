# Patch-level pin: the image sets GOTOOLCHAIN=local, so this base must be at
# least the version go.mod requires. See fold.Dockerfile.
FROM golang:1.27.0@sha256:65b6f280bf050ec5af12716857e8ea8439d694dbba8f31ceeb7630670071f2bb AS build
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
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /fold-discovery /fold-discovery
EXPOSE 8090
ENTRYPOINT ["/fold-discovery"]
