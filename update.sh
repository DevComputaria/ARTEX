#!/usr/bin/env bash
# ARTEX update scripts: ① Docker update (re-pull mirror) ② Local compile update (re-build binary)
# Corresponds to install.sh: install is responsible for the first landing, update is responsible for
# upgrading to the new version. DB migration does not require manual execution — artex will rerun
# schema.sql (with ADD COLUMN/CREATE INDEX IF NOT EXISTS) every time it starts, so "reboot and
# migrate". Data (pgdata volume,./data,./skills) are not affected.
set -euo pipefail
cd "$(cd "$(dirname "$0")" && pwd)"

info(){ printf '\033[36m[*]\033[0m %s\n' "$*"; }
ok(){   printf '\033[32m[+]\033[0m %s\n' "$*"; }
warn(){ printf '\033[33m[!]\033[0m %s\n' "$*"; }
die(){  printf '\033[31m[x]\033[0m %s\n' "$*" >&2; exit 1; }
ask(){  local p="$1" d="${2:-}" a; read -rp "$p${d:+ [$d]}: " a; echo "${a:-$d}"; }

# ── Optional: Synchronize repository to the latest code (compose/script/local compile source code
# are updated by it) ──────
sync_repo(){
  [ -d .git ] && command -v git >/dev/null 2>&1 || { warn "非 git 工作副本，跳过 git pull"; return; }
  [ "$(ask '拉取最新代码 (git pull --ff-only)? (y/n)' y)" = y ] || return
  if ! git pull --ff-only; then
    warn "git pull 未能快进（本地有改动或分支分叉）——请手动处理后重试，本次沿用当前代码"
  fi
}

# ── ① Docker Update
# ──────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
update_docker(){
  command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1 \
    || die "未检测到 docker / docker compose，请先用 ./install.sh 安装部署"
  [ -f .env ] || die "未找到 .env，请先运行 ./install.sh 完成首次部署"

  # Optional: Upgrade to specified version tag (if left empty, ARTEX_TAG in.env is used, default is
  # latest)
  local tag; tag="$(ask '目标镜像 tag（回车沿用 .env / latest）' '')"
  if [ -n "$tag" ]; then
    if grep -q '^ARTEX_TAG=' .env; then
      sed -i.bak "s|^ARTEX_TAG=.*|ARTEX_TAG=${tag}|" .env && rm -f .env.bak
    else
      printf '\nARTEX_TAG=%s\n' "$tag" >> .env
    fi
    ok "已将 ARTEX_TAG 设为 ${tag}"
  fi

  # Only artex: postgres is fixed 16-alpine, so it doesn't need to be upgraded (pulling it down is a
  # waste of bandwidth and large version changes can pose compatibility risks). artex declares
  # depends_on postgres, so if pg is not started with the service name up, it will start
  # automatically, and if it is already running, it will remain as it is, not rebuilt.
  info "拉取新镜像（仅 artex）…"
  docker compose pull artex
  info "重建并启动（artex 重启时自动迁移 schema）…"
  docker compose up -d artex
  ok "更新完成 → http://localhost:8787"
  info "查看日志：docker compose logs -f artex"
  info "清理旧镜像（可选）：docker image prune -f"
}

# ── ② Local compilation update ────────────────────────────────────────
update_local(){
  command -v go >/dev/null 2>&1 || die "未检测到 Go（>=1.26）：https://go.dev/dl/"
  [ -f config.json ] || warn "未找到 config.json——若首次部署请改用 ./install.sh"
  ok "Go: $(go version)"

  if command -v npm >/dev/null 2>&1; then
    info "重建前端静态产物…"
    ( cd web && npm ci && npm run build:static )
    rm -rf server/webui/dist && cp -r web/out server/webui/dist
    info "重新编译内嵌单二进制…"
    CGO_ENABLED=0 go build -tags embedui -trimpath -o artex ./cmd/artex
  else
    warn "未检测到 npm：编译**不内嵌前端**的后端（前端需另跑 npm run dev）"
    CGO_ENABLED=0 go build -o artex ./cmd/artex
  fi
  ok "编译完成 → ./artex"
  warn "请重启正在运行的 artex 进程以生效（重启时会自动迁移 schema）"
}

echo "=============================="
echo "  ARTEX 更新"
echo "  1) Docker 更新（拉新镜像重建）"
echo "  2) 本地更新（go 重新编译）"
echo "=============================="
case "$(ask '选择' 1)" in
  1) sync_repo; update_docker ;;
  2) sync_repo; update_local ;;
  *) die "无效选择" ;;
esac
