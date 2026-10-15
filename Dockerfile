# syntax=docker/dockerfile:1

# ---------------------------------------------------------------------------
# Build stage
#
# The frontend is embedded in the Go binary, so there is no Node step, no
# asset pipeline and no second image to keep in sync. One command builds
# the entire product.
# ---------------------------------------------------------------------------
FROM golang:1.26-alpine AS build

WORKDIR /src

# Dependencies are copied first so that a source change does not refetch
# the module graph.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
# CGO_ENABLED=0 matters: it produces a static binary, because the SQLite
# driver is pure Go. That is what allows the tiny runtime image below.
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags "-s -w -X main.version=${VERSION} -X github.com/nelvixboy/eye-of-lotus/internal/web.Version=${VERSION}" \
      -o /out/eye-of-lotus ./cmd/eye-of-lotus

# ---------------------------------------------------------------------------
# Runtime stage
#
# No shell, no package manager, no curl: an image with nothing in it is an
# image with nothing to exploit.
# ---------------------------------------------------------------------------
FROM alpine:3.21

# ca-certificates is here only for the DNS probe, which performs a real
# name resolution.
RUN apk add --no-cache ca-certificates tzdata \
 && addgroup -g 10001 -S lotus \
 && adduser -u 10001 -S lotus -G lotus -h /var/lib/eye-of-lotus

COPY --from=build /out/eye-of-lotus /usr/local/bin/eye-of-lotus

# The history database lives here; it is the only writable path the panel
# needs.
RUN mkdir -p /var/lib/eye-of-lotus /etc/eye-of-lotus \
 && chown -R lotus:lotus /var/lib/eye-of-lotus /etc/eye-of-lotus

COPY deploy/config.example.json /etc/eye-of-lotus/config.example.json

USER lotus
EXPOSE 9110

# The binary probes its own HTTP health endpoint, which is deliberately
# unauthenticated, so the image needs neither curl nor wget.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/usr/local/bin/eye-of-lotus", "-healthcheck"]

ENTRYPOINT ["/usr/local/bin/eye-of-lotus"]
CMD ["-config", "/etc/eye-of-lotus/config.json"]
