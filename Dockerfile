# backscroll — terminal output recorder + MCP server.
#
# The primary way to use backscroll is the static binary on your own machine
# (it wraps your interactive shell). This image exists mainly to run the MCP
# server (`backscroll mcp`) in containerized setups and for automated
# listing checks; mount your database read-only if you have one:
#
#   docker run -i --rm -v ~/.local/share/backscroll:/data/.local/share/backscroll:ro backscroll
#
# Without a mount it starts with an empty database (all tools respond, zero rows).

# --platform=$BUILDPLATFORM + GOARCH=$TARGETARCH: multi-arch images are
# cross-compiled natively under buildx instead of emulated (arm64 build in
# CI would otherwise run the Go compiler under qemu). Plain `docker build`
# still works: without buildx these args are empty and Go builds natively.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS
ARG TARGETARCH
# Version stamp: pass --build-arg VERSION=x.y.z (release CI does), or leave
# unset to fall back to `git describe` against the build context's .git
# (kept out of .dockerignore for exactly this), or "dev" as a last resort.
ARG VERSION=""
RUN apk add --no-cache git
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN V="$VERSION"; \
    if [ -z "$V" ]; then \
        V="$(git describe --tags --always 2>/dev/null || true)"; \
    fi; \
    V="${V#v}"; \
    : "${V:=dev}"; \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=$V" -o /out/backscroll .

FROM alpine:3.24
RUN adduser -D -h /data backscroll
COPY --from=build /out/backscroll /usr/local/bin/backscroll
USER backscroll
ENV HOME=/data
WORKDIR /data
ENTRYPOINT ["backscroll"]
CMD ["mcp"]
