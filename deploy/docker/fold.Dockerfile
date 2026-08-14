# The tag names the patch, not just the minor. The image sets
# GOTOOLCHAIN=local, so it cannot fetch a newer toolchain than it ships: this
# base must be at least the version go.mod requires, and bumping go.mod is
# what makes this line stale. Pinned as golang:1.26 it read as current while
# the digest held an older patch, and the build broke silently — the image is
# only built on release tags, so CI stayed green until the tag was pushed.
FROM golang:1.26.6@sha256:640a234f4bea3e399c056b7b8f9c667c4939befae8db2f14e9785e16eccd4205 AS build
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
