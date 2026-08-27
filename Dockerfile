# --- build the binary ---
FROM golang:1.27-bookworm AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/newsdigest .

# --- runtime ---
# Node is here only so the Claude Code CLI can run, which is what lets the app
# authenticate with a Claude.ai subscription instead of a metered API key.
# Building with --build-arg CLAUDE_CLI=false drops it (API-key mode only).
FROM node:22-bookworm-slim

ARG CLAUDE_CLI=true

RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates tzdata \
 && if [ "$CLAUDE_CLI" = "true" ]; then npm install -g @anthropic-ai/claude-code; fi \
 && npm cache clean --force \
 && rm -rf /var/lib/apt/lists/*

# Run unprivileged. HOME must be writable: the Claude CLI keeps state there.
RUN useradd --create-home --shell /usr/sbin/nologin app
WORKDIR /app
RUN mkdir -p /app/data /app/config && chown -R app:app /app

COPY --from=build /out/newsdigest /usr/local/bin/newsdigest

USER app
ENV HOME=/home/app \
    NEWSDIGEST_CONFIG=/app/config/feeds.yaml \
    NEWSDIGEST_DATA=/app/data \
    NEWSDIGEST_ADDR=:8080

EXPOSE 8080
VOLUME ["/app/data"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD node -e "require('http').get('http://127.0.0.1:8080/healthz',r=>process.exit(r.statusCode===200?0:1)).on('error',()=>process.exit(1))"

ENTRYPOINT ["newsdigest"]
