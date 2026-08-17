# fold-stdio: runs one stdio MCP server and exposes it over streamable HTTP.
#
# Unlike fold and fold-discovery, this image cannot be distroless/static: its
# whole job is to execute a server, and most stdio MCP servers are delivered
# through a language runtime. The runtime is therefore a build argument, and
# the default carries Node because `npx`-delivered servers are the common case.
#
#   docker build -f Dockerfile.stdio -t fold-stdio .
#   docker build -f Dockerfile.stdio --build-arg RUNTIME_BASE=python:3.13-slim .
#
# A server that is itself a static binary wants neither — build with
# --build-arg RUNTIME_BASE=gcr.io/distroless/static-debian12:nonroot and COPY
# the server in.

# Declared before the first FROM so it can be used in one; re-declare inside a
# stage if a stage ever needs to read it.
ARG RUNTIME_BASE=node:22-slim@sha256:d649c27dae7ba0137b3cef5dd75baa422c08dc3d9e3fc0c23dfb172dc3cc6436

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
    -o /fold-stdio ./cmd/fold-stdio

FROM ${RUNTIME_BASE}
COPY --from=build /fold-stdio /usr/local/bin/fold-stdio

# The shim executes a child process, so it runs unprivileged. Node images ship
# a `node` user; other bases may not, in which case set your own USER.
USER 1000:1000
EXPOSE 8091

# Bind beyond loopback: in a container the gateway reaches the shim over the
# pod or bridge network. The binary refuses a non-loopback bind unless
# --bearer-env is set, so this entrypoint requires a token by construction —
# run it with, e.g., -e SHIM_TOKEN=... --bearer-env SHIM_TOKEN.
ENTRYPOINT ["/usr/local/bin/fold-stdio", "--host", "0.0.0.0"]
