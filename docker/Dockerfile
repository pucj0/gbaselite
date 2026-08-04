# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.24-alpine3.21 AS builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/gbaselite ./cmd/gbaselite

FROM alpine:3.21
RUN addgroup -S -g 10001 gbaselite && \
    adduser -S -D -H -u 10001 -G gbaselite gbaselite && \
    mkdir -p /app/data /app/logs && \
    chown -R gbaselite:gbaselite /app
WORKDIR /app
COPY --from=builder --chown=gbaselite:gbaselite /out/gbaselite /app/gbaselite
COPY --chown=gbaselite:gbaselite docker/config.example.yaml /app/config.yaml
ENV DB_HOST=0.0.0.0 \
    DB_DATA_PATH=/app/data
USER gbaselite
EXPOSE 3307
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=5 \
    CMD ["/app/gbaselite", "healthcheck", "--host", "127.0.0.1", "--port", "3307"]
ENTRYPOINT ["/app/gbaselite"]
CMD ["server", "--config", "/app/config.yaml"]
