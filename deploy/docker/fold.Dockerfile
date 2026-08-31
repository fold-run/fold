# The tag names the patch, not just the minor. The image sets
# GOTOOLCHAIN=local, so it cannot fetch a newer toolchain than it ships: this
# base must be at least the version go.mod requires, and bumping go.mod is
# what makes this line stale. Pinned as golang:1.26 it read as current while
# the digest held an older patch, and the build broke silently — the image is
# only built on release tags, so CI stayed green until the tag was pushed.
FROM golang:1.27.0@sha256:65b6f280bf050ec5af12716857e8ea8439d694dbba8f31ceeb7630670071f2bb AS build
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
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /fold /fold
EXPOSE 8080
ENTRYPOINT ["/fold"]
