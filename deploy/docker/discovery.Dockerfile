FROM golang:1.26 AS build
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
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /fold-discovery /fold-discovery
EXPOSE 8090
ENTRYPOINT ["/fold-discovery"]
