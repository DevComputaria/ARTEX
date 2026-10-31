# syntax=docker/dockerfile:1
#
# Running mirrors (not compiling in mirrors): only installing common tools, with ** pre-compiled
# Linux single binary**. The binaries are cross-compiled by the CI's binaries job (Go-only, no QEMU)
# and placed in dist/<TARGETARCH>/artex of the build context for the target architecture. This way,
# arm64 can only emulate the apt layer for multi-build builds, not Next/Go compilation anymore, and
# is much faster.
#
# If you want to build a mirror locally, prepare the binary files yourself: cd web && npm run
# build:static && cd.. cp -r web/out server/webui/dist CGO_ENABLED=0 GOARCH=amd64 go build -tags
# embedui -o dist/amd64/artex./cmd/artex docker build -t artex:local.
FROM python:3.12-slim-bookworm
ARG TARGETARCH
# Common tools: ripgrep / curl / vim, plus a bunch of common recon components (add and remove as
# needed).
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates ripgrep curl wget vim git jq unzip \
      dnsutils iputils-ping netcat-openbsd inetutils-telnet whois nmap \
    && curl -fsSL https://deb.nodesource.com/setup_20.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*
# Preinstall Playwright MCP with CLI (global), no longer running npx-download. @playwright/mcp:
# browser MCP directly `npx @playwright/mcp` (globally loaded, no -y/@latest required).
# @playwright/cli: provides playwright-cli, loaded alongside --help authentication is executable.
# Reinstall playwright (provides browser management), loaded with --with-deps preloading chromium
# and its system dependencies, so that the container-based MCP/CLI is available on first startup, no
# longer running npx-download.
RUN npm install -g @playwright/mcp@latest @playwright/cli@latest playwright@latest \
    && playwright-cli --help \
    && playwright install --with-deps chromium \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
# Pre-compiled binary architecture correspondance (dist/amd64/artex or dist/arm64/artex)
COPY dist/${TARGETARCH}/artex /app/artex
# The docker stop script is responsible for forwarding the SIGTERM to artex, which is the only
# signal that docker sends to PID 1. If it fails to forward it, artex will not receive the signal
# and will be shut down elegantly, and after 10 seconds it will be killed by SIGKILL.
COPY start.sh /app/start.sh
RUN chmod +x /app/artex /app/start.sh
COPY skills/ /app/skills/
# data/(SQLite + jwt.key) persistence point
VOLUME ["/app/data"]
EXPOSE 8787 8788
ENTRYPOINT ["/app/start.sh"]
CMD ["-addr", ":8787", "-proxy", ":8788"]
