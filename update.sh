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
  [ -d .git ] && command -v git >/dev/null 2>&1 || { warn "Não é uma cópia Git; ignorando git pull"; return; }
  [ "$(ask 'Baixar o código mais recente com git pull --ff-only? (y/n)' y)" = y ] || return
  if ! git pull --ff-only; then
    warn "git pull não pôde avançar; resolva alterações ou divergências manualmente. O código atual será mantido"
  fi
}

# ── ① Docker Update
# ──────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
update_docker(){
  command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1 \
    || die "Docker ou Docker Compose não detectado; execute ./install.sh primeiro"
  [ -f .env ] || die ".env não encontrado; execute ./install.sh primeiro"

  # Optional: Upgrade to specified version tag (if left empty, ARTEX_TAG in.env is used, default is
  # latest)
  local tag; tag="$(ask 'Tag da imagem (Enter mantém .env/latest)' '')"
  if [ -n "$tag" ]; then
    if grep -q '^ARTEX_TAG=' .env; then
      sed -i.bak "s|^ARTEX_TAG=.*|ARTEX_TAG=${tag}|" .env && rm -f .env.bak
    else
      printf '\nARTEX_TAG=%s\n' "$tag" >> .env
    fi
    ok "ARTEX_TAG definido como ${tag}"
  fi

  # Only artex: postgres is fixed 16-alpine, so it doesn't need to be upgraded (pulling it down is a
  # waste of bandwidth and large version changes can pose compatibility risks). artex declares
  # depends_on postgres, so if pg is not started with the service name up, it will start
  # automatically, and if it is already running, it will remain as it is, not rebuilt.
  info "Baixando a nova imagem do ARTEX…"
  docker compose pull artex
  info "Recriando e iniciando; o schema será migrado automaticamente…"
  docker compose up -d artex
  ok "Atualização concluída: http://localhost:8787"
  info "Logs: docker compose logs -f artex"
  info "Limpeza opcional de imagens antigas: docker image prune -f"
}

# ── ② Local compilation update ────────────────────────────────────────
update_local(){
  command -v go >/dev/null 2>&1 || die "Go >= 1.26 não detectado: https://go.dev/dl/"
  [ -f config.json ] || warn "config.json não encontrado; para a primeira instalação, use ./install.sh"
  ok "Go: $(go version)"

  if command -v npm >/dev/null 2>&1; then
    info "Gerando novamente o frontend estático…"
    ( cd web && npm ci && npm run build:static )
    rm -rf server/webui/dist && cp -r web/out server/webui/dist
    info "Recompilando o binário único com frontend embutido…"
    CGO_ENABLED=0 go build -tags embedui -trimpath -o artex ./cmd/artex
  else
    warn "npm não detectado: compilando o backend sem frontend; execute npm run dev separadamente"
    CGO_ENABLED=0 go build -o artex ./cmd/artex
  fi
  ok "Compilação concluída: ./artex"
  warn "Reinicie o processo ARTEX para aplicar a atualização e migrar o schema"
}

echo "=============================="
echo "  Atualização do ARTEX"
echo "  1) Atualizar com Docker"
echo "  2) Atualizar localmente com Go"
echo "=============================="
case "$(ask 'Escolha' 1)" in
  1) sync_repo; update_docker ;;
  2) sync_repo; update_local ;;
  *) die "Escolha inválida" ;;
esac
