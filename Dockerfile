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

FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/backscroll .

FROM alpine:3.20
RUN adduser -D -h /data backscroll
COPY --from=build /out/backscroll /usr/local/bin/backscroll
USER backscroll
ENV HOME=/data
WORKDIR /data
ENTRYPOINT ["backscroll"]
CMD ["mcp"]
