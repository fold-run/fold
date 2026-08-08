FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X github.com/fold-run/fold/gateway.version=${VERSION}" \
    -o /fold ./cmd/fold

# distroless/static ships CA certificates (needed for JWKS and token
# endpoints over HTTPS) and a nonroot user.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /fold /fold
EXPOSE 8080
ENTRYPOINT ["/fold"]
