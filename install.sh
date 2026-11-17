#!/usr/bin/env bash
# ARTEX Installation Script: ① All Docker ② Locally compiled run
set -euo pipefail
cd "$(cd "$(dirname "$0")" && pwd)"

info(){ printf '\033[36m[*]\033[0m %s\n' "$*"; }
ok(){   printf '\033[32m[+]\033[0m %s\n' "$*"; }
warn(){ printf '\033[33m[!]\033[0m %s\n' "$*"; }
die(){  printf '\033[31m[x]\033[0m %s\n' "$*" >&2; exit 1; }
ask(){  local p="$1" d="${2:-}" a; read -rp "$p${d:+ [$d]}: " a; echo "${a:-$d}"; }
rand(){ head -c 18 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 24; }

# ── docker environment detection / automatic installation ──────────────────
ensure_docker(){
  if command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then
    ok "Docker e Docker Compose detectados"; return
  fi
  warn "Docker ou Docker Compose não detectado"
  case "$(uname -s)" in
    Linux)
      if [ "$(ask 'Instalar o Docker automaticamente? (y/n)' y)" = y ]; then
        curl -fsSL https://get.docker.com | sh
        sudo usermod -aG docker "$USER" || true
        ok "Docker instalado; entre novamente na sessão para usar sem sudo"
      else
        die "Instale o Docker e tente novamente"
      fi ;;
    Darwin) die "No macOS, instale o Docker Desktop: https://www.docker.com/products/docker-desktop/" ;;
    *)      die "Instale o Docker e tente novamente" ;;
  esac
}

# ── ① All Docker
# ──────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
install_docker(){
  ensure_docker
  if [ ! -f .env ]; then
    cp .env.example .env 2>/dev/null || true
    local pw key
    pw="$(ask 'Senha do PostgreSQL (Enter gera uma senha aleatória)' "$(rand)")"
    key="$(ask 'ANTHROPIC_API_KEY (opcional; pode ser configurada depois na interface)' '')"
    sed -i.bak "s|^POSTGRES_PASSWORD=.*|POSTGRES_PASSWORD=${pw}|" .env
    sed -i.bak "s|^ANTHROPIC_API_KEY=.*|ANTHROPIC_API_KEY=${key}|" .env
    rm -f .env.bak
    ok ".env criado com POSTGRES_PASSWORD"
  else
    info "Usando o arquivo .env existente"
  fi
  info "Baixando imagens e iniciando os serviços…"
  docker compose pull || true
  docker compose up -d
  ok "ARTEX iniciado em http://localhost:8787"
  info "Logs: docker compose logs -f artex"
}

# ── ② Local compilation run
# ────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
install_local(){
  echo "Modo do banco de dados:"
  echo "  1) Conectar a um PostgreSQL existente"
  echo "  2) Iniciar PostgreSQL com Docker"
  case "$(ask 'Escolha' 1)" in
    2)
      ensure_docker
      local pw; pw="$(ask 'Senha do PostgreSQL (Enter gera uma senha aleatória)' "$(rand)")"
      docker run -d --name artex-pg -p 5432:5432 \
        -e POSTGRES_USER=artex -e POSTGRES_PASSWORD="$pw" -e POSTGRES_DB=artex \
        -v artex-pg:/var/lib/postgresql/data postgres:16-alpine
      DB_HOST=127.0.0.1 DB_PORT=5432 DB_USER=artex DB_PASS="$pw" DB_NAME=artex DB_SSL=disable ;;
    *)
      DB_HOST="$(ask 'Endereço do banco' 127.0.0.1)"
      DB_PORT="$(ask 'Porta' 5432)"
      DB_USER="$(ask 'Usuário' artex)"
      DB_PASS="$(ask 'Senha' '')"
      DB_NAME="$(ask 'Nome do banco' artex)"
      DB_SSL="$(ask 'sslmode (disable/require)' disable)" ;;
  esac

  # Generate config.json
  cat > config.json <<JSON
{
  "database": {
    "host": "${DB_HOST}",
    "port": ${DB_PORT},
    "user": "${DB_USER}",
    "password": "${DB_PASS}",
    "dbname": "${DB_NAME}",
    "sslmode": "${DB_SSL}"
  }
}
JSON
  ok "config.json criado"

  # go Environmental Inspection
  command -v go >/dev/null 2>&1 || die "Go >= 1.26 não detectado: https://go.dev/dl/"
  ok "Go: $(go version)"

  # The embedded front-end requires node to output a static product
  if command -v npm >/dev/null 2>&1; then
    info "Gerando o frontend estático…"
    ( cd web && npm ci && npm run build:static )
    rm -rf server/webui/dist && cp -r web/out server/webui/dist
    info "Compilando o binário único com frontend embutido…"
    CGO_ENABLED=0 go build -tags embedui -trimpath -o artex ./cmd/artex
  else
    warn "npm não detectado: o backend será compilado sem frontend; execute npm run dev separadamente"
    CGO_ENABLED=0 go build -o artex ./cmd/artex
  fi
  ok "Compilação concluída: ./artex"

  info "Iniciando… (Ctrl-C para sair)"
  ./artex
}

echo "=============================="
echo "  Instalação do ARTEX"
echo "  1) Instalação completa com Docker"
echo "  2) Execução local (compilar com Go)"
echo "=============================="
case "$(ask 'Escolha' 1)" in
  1) install_docker ;;
  2) install_local ;;
  *) die "Escolha inválida" ;;
esac
