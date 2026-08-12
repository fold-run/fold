FROM golang:1.26@sha256:7caba5286b4c3613a337b709c573047d8ae62ee76106647313b61e72b99f20af AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X github.com/fold-run/fold/gateway.ldflagsVersion=${VERSION}" \
    -o /fold ./cmd/fold

# distroless/static ships CA certificates (needed for JWKS and token
# endpoints over HTTPS) and a nonroot user.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:1b7b9f0f0e0a1d2155f531db587cc48ec26aaf97ab64364225f5bf18a054e66a
COPY --from=build /fold /fold
EXPOSE 8080
ENTRYPOINT ["/fold"]
