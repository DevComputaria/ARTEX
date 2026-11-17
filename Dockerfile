# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM node:22-bookworm-slim AS frontend
ENV NEXT_TELEMETRY_DISABLED=1
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build:static

FROM --platform=$BUILDPLATFORM golang:1.26.3-bookworm AS builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . ./
COPY --from=frontend /src/web/out /src/server/webui/dist
RUN chmod +x ./docker-entrypoint.sh \
    && GOOS="$TARGETOS" GOARCH="$TARGETARCH" ./docker-entrypoint.sh --prepare /out/artex

FROM python:3.12-slim-bookworm
# Common tools: ripgrep / curl / vim, plus a bunch of common recon components (add and remove as
# needed).
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates ripgrep curl wget vim git jq unzip \
      dnsutils iputils-ping netcat-openbsd inetutils-telnet whois nmap \
    && rm -rf /var/lib/apt/lists/*
COPY --from=frontend /usr/local/ /usr/local/
# Preinstall Playwright MCP with CLI (global), no longer running npx-download. @playwright/mcp:
# browser MCP directly `npx @playwright/mcp` (globally loaded, no -y/@latest required).
# @playwright/cli: provides playwright-cli, loaded alongside --help authentication is executable.
# Reinstall playwright (provides browser management), loaded with --with-deps preloading chromium
# and its system dependencies, so that the container-based MCP/CLI is available on first startup, no
# longer running npx-download.
RUN npm install -g @playwright/mcp@0.0.85 @playwright/cli@0.1.24 playwright@1.65.1 \
    && playwright-cli --help \
    && playwright install --with-deps chromium \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=builder /out/artex /app/artex
COPY docker-entrypoint.sh /app/docker-entrypoint.sh
COPY skills/ /app/skills/
RUN groupadd --gid 1000 artex \
    && useradd --uid 1000 --gid artex --home-dir /app --shell /usr/sbin/nologin artex \
    && mkdir -p /app/data \
    && chmod 0755 /app/artex /app/docker-entrypoint.sh \
    && chown -R artex:artex /app
# data/(SQLite + jwt.key) persistence point
VOLUME ["/app/data"]
EXPOSE 8787 8788
USER artex
ENTRYPOINT ["/app/docker-entrypoint.sh"]
CMD ["-addr", ":8787", "-proxy", ":8788"]
