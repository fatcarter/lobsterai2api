FROM golang:1.26.1-alpine3.23 AS builder

WORKDIR /src
COPY go.mod ./
COPY cmd/ ./cmd/
COPY internal/ ./internal/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/lobsterai2api ./cmd/server \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/credit ./cmd/credit

FROM alpine:3.23.3

RUN apk add --no-cache ca-certificates tzdata
RUN mkdir -p /app/auths /app/data

WORKDIR /app
COPY --from=builder /out/ /usr/local/bin/
COPY config.example.json /app/config.json
COPY --chmod=755 docker/entrypoint.sh /usr/local/bin/docker-entrypoint.sh

ENV LB2A_LISTEN=:8367 \
    LB2A_AUTH_DIR=/app/auths \
    LB2A_STATE_FILE=/app/data/state.json \
    LB2A_CHECKIN_FILE=/app/data/checkin.json \
    LB2A_SCHEDULE_FILE=/app/data/schedule.json \
    TZ=Asia/Shanghai

EXPOSE 8367
HEALTHCHECK --interval=15s --timeout=5s --start-period=5s --retries=3 \
    CMD wget -q -T 3 -O /dev/null "http://127.0.0.1:${LB2A_LISTEN##*:}/healthz" || exit 1

USER 0:0
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
CMD ["lobsterai2api", "-config", "/app/config.json"]
