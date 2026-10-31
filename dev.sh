#!/usr/bin/env bash
# Developer mode: Backend (:8787) + Traffic Agent (:8788) runs with front end next dev (:5173).
#
# For single binary (front-end embedded) mode, see the README section "Single binary" and do not run
# this script.
set -euo pipefail
cd "$(dirname "$0")"

# Exit terminates all subprocesses (backend + frontend) within the process group.
cleanup() { kill 0 2>/dev/null || true; }
trap cleanup EXIT INT TERM

# Backend (normal go run, no embedded front-end); the number of concurrent work agents is configured
# in System Settings.
go run ./cmd/artex -addr :8787 -proxy 127.0.0.1:8788 &

# Front-end hot-update (Vite/Next dev server, /api backport to :8787).
( cd web && npm run dev ) &

echo "[dev] 后端 :8787 / 代理 :8788 / 前端 http://localhost:5173  (Ctrl-C 退出)"
wait
