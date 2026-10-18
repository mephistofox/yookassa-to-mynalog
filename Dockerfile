# fxTunnel Go port. Original runtime: Dockerfile.python.
FROM golang:1.26-alpine AS build
RUN apk add --no-cache build-base
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY pkg/ ./pkg/
ARG VERSION=dev
RUN CGO_ENABLED=1 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /receipts ./cmd/receipts

FROM alpine:3.23
RUN apk add --no-cache ca-certificates tzdata libgcc \
    && addgroup -g 10001 receipts \
    && adduser -D -u 10001 -G receipts receipts \
    && mkdir /data && chown receipts:receipts /data && chmod 700 /data
COPY --from=build /receipts /usr/local/bin/receipts
COPY LICENSE NOTICE /usr/share/licenses/receipts/
USER receipts:receipts
WORKDIR /data
ENV STATE_DB=/data/receipts-go.db
EXPOSE 8099
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s \
  CMD wget -q -O /dev/null http://127.0.0.1:8099/healthz || exit 1
ENTRYPOINT ["/usr/local/bin/receipts"]
CMD ["serve"]
